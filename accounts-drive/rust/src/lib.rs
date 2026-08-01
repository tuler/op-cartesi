//! Pure-Rust implementation of the Cartesi Accounts Drive Format, version 1
//! (`docs/ACCOUNTS-DRIVE-SPEC.md`): a fixed-layout accounts database inside a
//! Cartesi Machine flash drive, written by the guest as part of the state
//! transition and readable — and provable — from outside without executing
//! the machine.
//!
//! The same code serves both sides of the drive. A guest opens it over a
//! [`FileStore`] on the raw device; a host opens it over a [`MemStore`]
//! holding a copied image, or its own [`Store`] implementation over the
//! machine API's `machine.read_memory` (see [`store`] for why this crate
//! ships no HTTP-backed store). All layout, hashing and probing logic is
//! shared, which is what keeps two independent parties byte-for-byte agreed
//! about what the drive means.
//!
//! The crate has zero third-party dependencies, including its own Keccak-256
//! ([`keccak256`]).

mod drive;
pub mod keccak;
pub mod store;

pub use drive::{
    Account, Config, Drive, Token, MAGIC, PROFILE_SINGLE_ASSET, PROFILE_SPARSE, PROFILE_WIDE,
    VERSION,
};
pub use keccak::keccak256;
pub use store::{FileStore, MemStore, Store};

/// A balance: 32 bytes, big-endian — byte-identical to an EVM ABI `uint256`
/// word (spec §2).
pub type Balance = [u8; 32];

/// Encodes a `u64` as a 32-byte big-endian balance.
pub fn balance_from_u64(v: u64) -> Balance {
    balance_from_u128(v as u128)
}

/// Encodes a `u128` as a 32-byte big-endian balance.
pub fn balance_from_u128(v: u128) -> Balance {
    let mut b = [0u8; 32];
    b[16..].copy_from_slice(&v.to_be_bytes());
    b
}

/// Decodes a balance as `u64`; `None` when the value exceeds `u64::MAX`.
pub fn balance_to_u64(b: &Balance) -> Option<u64> {
    if b[..24].iter().any(|&x| x != 0) {
        return None;
    }
    let mut w = [0u8; 8];
    w.copy_from_slice(&b[24..]);
    Some(u64::from_be_bytes(w))
}

/// Decodes a balance as `u128`; `None` when the value exceeds `u128::MAX`.
pub fn balance_to_u128(b: &Balance) -> Option<u128> {
    if b[..16].iter().any(|&x| x != 0) {
        return None;
    }
    let mut w = [0u8; 16];
    w.copy_from_slice(&b[16..]);
    Some(u128::from_be_bytes(w))
}

/// Everything that can go wrong. The first nine variants map 1:1 to the
/// golden vectors' `expect` kinds; in a guest, [`Error::TableFull`],
/// [`Error::RegistryFull`] and [`Error::Overflow`] are exactly the
/// conditions the spec requires to be answered by rejecting the input.
#[derive(Debug)]
pub enum Error {
    /// The table is at its load limit (`tableFull`); reject the input.
    /// Also raised when a wide-profile registration's columns would exceed
    /// the slot's `slotSize − 64` budget (spec §7).
    TableFull,
    /// The token registry is at capacity (`registryFull`); reject the input.
    RegistryFull,
    /// A balance exceeds its declared width (`overflow`); reject the input.
    Overflow,
    /// The record's nonzero nonce protects it from deletion
    /// (`nonceProtected`); pass `force` to delete anyway (spec §7).
    NonceProtected,
    /// The token address is already registered (`duplicateToken`).
    DuplicateToken,
    /// The token id is not registered (`unknownToken`).
    UnknownToken,
    /// The balance width is not 8, 16 or 32 — or not 8 on a drive with
    /// 32-byte sparse slots (`badWidth`, spec §§8–9).
    BadWidth,
    /// The zero address is invalid in every record type (`zeroAddress`).
    ZeroAddress,
    /// The operation is not available in this drive's profile (`profile`).
    Profile,
    /// The store's first bytes are not the `ctsiacct` magic.
    NotFormatted,
    /// The drive declares a version (or nonzero flags) this crate does not
    /// interpret (spec §13).
    UnsupportedVersion(u32),
    /// The geometry handed to [`Drive::format`] violates spec §5.
    Config(String),
    /// The drive's bytes contradict its own header (e.g. token count past
    /// registry capacity).
    Corrupt(String),
    /// An access outside the store's bounds.
    OutOfBounds {
        /// Requested offset.
        off: u64,
        /// Requested length.
        len: u64,
        /// The store's (or table's) size.
        size: u64,
    },
    /// The store cannot write (host-side reads).
    ReadOnly,
    /// An I/O failure from a [`FileStore`] (or other store).
    Io(std::io::Error),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::TableFull => write!(f, "table at load limit"),
            Error::RegistryFull => write!(f, "token registry full"),
            Error::Overflow => write!(f, "balance exceeds declared width"),
            Error::NonceProtected => write!(
                f,
                "record has nonzero nonce (replay protection); pass force to delete"
            ),
            Error::DuplicateToken => write!(f, "token already registered"),
            Error::UnknownToken => write!(f, "token not registered"),
            Error::BadWidth => write!(f, "balance width must be 8, 16 or 32"),
            Error::ZeroAddress => write!(f, "zero address is invalid"),
            Error::Profile => write!(f, "operation not available in this profile"),
            Error::NotFormatted => write!(f, "not an accounts drive (bad magic)"),
            Error::UnsupportedVersion(v) => {
                write!(f, "unsupported accounts drive version/flags ({v})")
            }
            Error::Config(m) => write!(f, "bad drive config: {m}"),
            Error::Corrupt(m) => write!(f, "corrupt drive: {m}"),
            Error::OutOfBounds { off, len, size } => {
                write!(f, "access [{off},{}) outside {size} bytes", off + len)
            }
            Error::ReadOnly => write!(f, "store is read-only"),
            Error::Io(e) => write!(f, "store i/o: {e}"),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::Io(e) => Some(e),
            _ => None,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn balance_helpers_roundtrip() {
        assert_eq!(balance_to_u64(&balance_from_u64(0)), Some(0));
        assert_eq!(
            balance_to_u64(&balance_from_u64(u64::MAX)),
            Some(u64::MAX)
        );
        assert_eq!(balance_to_u64(&balance_from_u128(1u128 << 64)), None);
        assert_eq!(
            balance_to_u128(&balance_from_u128(u128::MAX)),
            Some(u128::MAX)
        );
        let mut top = [0u8; 32];
        top[0] = 1;
        assert_eq!(balance_to_u128(&top), None);
        // Big-endian placement: 5 lands in the last byte.
        assert_eq!(balance_from_u64(5)[31], 5);
    }
}
