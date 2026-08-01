//! Byte-level stores backing a drive.
//!
//! The library never touches bytes except through [`Store`]; implementations
//! decide what backs it: a byte vector ([`MemStore`]), a device or image file
//! ([`FileStore`]), or — on the host — a remote machine's memory.
//!
//! There is no HTTP-backed store here because the Rust standard library has
//! no HTTP client and this crate takes zero dependencies. Hosts that read a
//! live (parked) machine implement `Store` themselves over the
//! cartesi-jsonrpc-machine protocol's `machine.read_memory` — a pure state
//! read that never executes the machine — returning [`Error::ReadOnly`] from
//! `write_at`, and per spec §11 using it only against a quiescent machine.

use crate::Error;
use std::fs::File;
use std::io::{Read, Seek, SeekFrom, Write};
use std::path::Path;

/// Byte-level access to a drive. Offsets are relative to the drive's first
/// byte. Reads and writes are exact: short transfers are errors.
pub trait Store {
    /// Reads `buf.len()` bytes at `off`.
    fn read_at(&self, off: u64, buf: &mut [u8]) -> Result<(), Error>;
    /// Writes all of `data` at `off`.
    fn write_at(&mut self, off: u64, data: &[u8]) -> Result<(), Error>;
}

/// An in-memory drive image: the whole drive as one byte vector.
#[derive(Debug, Clone)]
pub struct MemStore {
    bytes: Vec<u8>,
}

impl MemStore {
    /// Allocates a zeroed drive image of the given length.
    pub fn new(length: u64) -> Self {
        MemStore {
            bytes: vec![0u8; length as usize],
        }
    }

    /// Wraps an existing image (e.g. bytes copied out of a stored machine).
    pub fn from_bytes(bytes: Vec<u8>) -> Self {
        MemStore { bytes }
    }

    /// The full drive image.
    pub fn bytes(&self) -> &[u8] {
        &self.bytes
    }

    /// Consumes the store, returning the image.
    pub fn into_bytes(self) -> Vec<u8> {
        self.bytes
    }

    fn bounds(&self, off: u64, n: usize) -> Result<usize, Error> {
        let end = off.checked_add(n as u64).ok_or(Error::OutOfBounds {
            off,
            len: n as u64,
            size: self.bytes.len() as u64,
        })?;
        if end > self.bytes.len() as u64 {
            return Err(Error::OutOfBounds {
                off,
                len: n as u64,
                size: self.bytes.len() as u64,
            });
        }
        Ok(off as usize)
    }
}

impl Store for MemStore {
    fn read_at(&self, off: u64, buf: &mut [u8]) -> Result<(), Error> {
        let o = self.bounds(off, buf.len())?;
        buf.copy_from_slice(&self.bytes[o..o + buf.len()]);
        Ok(())
    }

    fn write_at(&mut self, off: u64, data: &[u8]) -> Result<(), Error> {
        let o = self.bounds(off, data.len())?;
        self.bytes[o..o + data.len()].copy_from_slice(data);
        Ok(())
    }
}

/// A drive accessed through a file — in the guest, the raw device
/// (`/dev/pmemN`).
///
/// Spec §10 guest requirement: on a pmem-backed flash drive, file I/O goes
/// through the guest kernel's page cache, and un-synced bytes are **not** in
/// machine state at the yield. A guest MUST therefore call [`FileStore::sync`]
/// before finishing each input; otherwise the drive bytes are not current at
/// the quiescent state hosts and verifiers read.
#[derive(Debug)]
pub struct FileStore {
    file: File,
}

impl FileStore {
    /// Opens a device or image file read-write.
    pub fn open<P: AsRef<Path>>(path: P) -> Result<Self, Error> {
        let file = File::options()
            .read(true)
            .write(true)
            .open(path)
            .map_err(Error::Io)?;
        Ok(FileStore { file })
    }

    /// Wraps an already-open file.
    pub fn from_file(file: File) -> Self {
        FileStore { file }
    }

    /// Flushes written bytes to the device (`fdatasync`). Guests on a
    /// pmem-backed drive MUST call this before finishing each input (spec §10).
    pub fn sync(&self) -> Result<(), Error> {
        self.file.sync_data().map_err(Error::Io)
    }
}

impl Store for FileStore {
    fn read_at(&self, off: u64, buf: &mut [u8]) -> Result<(), Error> {
        // Seek-then-read on &File (both impls exist for &File), so read_at
        // needs no &mut self; every access seeks first, so the shared cursor
        // carries no state between calls.
        let mut f = &self.file;
        f.seek(SeekFrom::Start(off)).map_err(Error::Io)?;
        f.read_exact(buf).map_err(Error::Io)
    }

    fn write_at(&mut self, off: u64, data: &[u8]) -> Result<(), Error> {
        let mut f = &self.file;
        f.seek(SeekFrom::Start(off)).map_err(Error::Io)?;
        f.write_all(data).map_err(Error::Io)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{balance_from_u64, Config, Drive};

    /// A drive image on a FileStore survives a close/reopen, and sync works.
    #[test]
    fn file_store_roundtrip() {
        // Keep the image under target/ so the test writes only build output.
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join("filestore-test.img");
        let image = File::create(&path).unwrap();
        image.set_len(1 << 16).unwrap();
        drop(image);

        let store = FileStore::open(&path).unwrap();
        let mut d = Drive::format(
            store,
            Config {
                drive_length: 1 << 16,
                capacity: 8,
                ..Config::default()
            },
        )
        .unwrap();
        let addr = [0xbbu8; 20];
        d.set_account(&addr, 7, &balance_from_u64(5)).unwrap();
        d.store_mut().sync().unwrap();
        drop(d);

        let d = Drive::open(FileStore::open(&path).unwrap()).unwrap();
        let a = d.get_account(&addr).unwrap().unwrap();
        assert_eq!(a.nonce, 7);
        assert_eq!(a.balance, balance_from_u64(5));
        std::fs::remove_file(&path).ok();
    }
}
