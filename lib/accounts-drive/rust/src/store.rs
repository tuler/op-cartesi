//! Byte-level stores backing a drive.
//!
//! The library never touches bytes except through [`Store`]; implementations
//! decide what backs it: a byte vector ([`MemStore`]), a device or image file
//! ([`FileStore`]), or — on the host — a remote machine's memory.
//!
//! There is no HTTP-backed store here because the Rust standard library has
//! no HTTP client and this crate keeps its dependency footprint to the
//! RustCrypto hashes. Hosts that read a
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

/// A drive read out of a Cartesi Machine through whatever machine client the
/// host already uses — this crate deliberately does not speak the machine's
/// JSON-RPC itself. The store only translates drive-relative offsets into
/// the `(address, length)` arguments of the client's `machine.read_memory`.
///
/// Read-only, and per spec §11 only for a quiescent (parked or stored)
/// machine.
pub struct MachineStore<F>
where
    F: Fn(u64, u64) -> Result<Vec<u8>, Error>,
{
    base: u64,
    read_memory: F,
}

impl<F> MachineStore<F>
where
    F: Fn(u64, u64) -> Result<Vec<u8>, Error>,
{
    /// `base` is the drive's start address in the machine's address space;
    /// `read_memory` returns `length` bytes at an absolute machine address.
    /// Wrap a client method in a closure (map its error into [`Error::Io`]).
    pub fn new(base: u64, read_memory: F) -> Self {
        Self { base, read_memory }
    }
}

impl<F> Store for MachineStore<F>
where
    F: Fn(u64, u64) -> Result<Vec<u8>, Error>,
{
    fn read_at(&self, off: u64, buf: &mut [u8]) -> Result<(), Error> {
        let data = (self.read_memory)(self.base + off, buf.len() as u64)?;
        if data.len() != buf.len() {
            return Err(Error::Io(std::io::Error::new(
                std::io::ErrorKind::UnexpectedEof,
                format!("read_memory returned {} bytes, want {}", data.len(), buf.len()),
            )));
        }
        buf.copy_from_slice(&data);
        Ok(())
    }

    fn write_at(&mut self, _off: u64, _data: &[u8]) -> Result<(), Error> {
        Err(Error::ReadOnly)
    }
}

#[cfg(test)]
mod machine_store_tests {
    use super::*;
    use crate::drive::{Config, Drive};
    use crate::{balance_from_u64, balance_to_u64};

    #[test]
    fn translates_offsets_and_refuses_writes() {
        let mem = MemStore::new(1 << 16);
        let mut d = Drive::format(
            mem,
            Config {
                drive_length: 1 << 16,
                capacity: 8,
                ..Default::default()
            },
        )
        .unwrap();
        d.set_account(&[0xbb; 20], 7, &balance_from_u64(5)).unwrap();
        let image = d.into_store().into_bytes();

        const BASE: u64 = 0x80000000000000;
        let ms = MachineStore::new(BASE, move |address, length| {
            assert!(address >= BASE && address + length <= BASE + (1 << 16));
            let off = (address - BASE) as usize;
            Ok(image[off..off + length as usize].to_vec())
        });
        let mut h = Drive::open(ms).unwrap();
        let a = h.get_account(&[0xbb; 20]).unwrap().unwrap();
        assert_eq!(a.nonce, 7);
        assert_eq!(balance_to_u64(&a.balance), Some(5));
        assert!(h.get_account(&[0x99; 20]).unwrap().is_none());
        assert!(matches!(
            h.set_account(&[0xbb; 20], 8, &balance_from_u64(6)),
            Err(Error::ReadOnly)
        ));
    }
}
