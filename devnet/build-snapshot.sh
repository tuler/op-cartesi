#!/usr/bin/env bash
# Builds the Cartesi Machine snapshot that becomes the chain's genesis state.
#
# The machine is stored where `cartesi-machine --store` leaves it: booted, and
# parked at its first input yield. That is how Cartesi Rollups distributes
# templates, and it is what makes genesis reproducible — the chain's genesis
# state root is the stored machine's own root hash, rather than something that
# depends on how each node happened to run the boot.
#
# Images are not vendored. Fetch them once with:
#   curl -L -o "$IMAGES_DIR/linux.bin"   <machine-linux-image release asset>
#   curl -L -o "$IMAGES_DIR/rootfs.ext2" <machine-guest-tools rootfs-tools.ext2>

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${IMAGES_DIR:=/usr/share/cartesi-machine/images}"
: "${SNAPSHOT_DIR:=$DEVNET_DIR/snapshot}"
: "${GUEST_APP:=$DEVNET_DIR/bank-app.sh}"

for image in "$IMAGES_DIR/rootfs.ext2"; do
  if [ ! -f "$image" ]; then
    echo "missing $image — see the header of this script for where to get it" >&2
    exit 1
  fi
done

# The guest's owner is a genesis parameter: the one address whose inputs it
# takes as configuration, which is how it learns the portal addresses that do
# not exist yet when the snapshot is built. Substituting it here rather than
# editing the app keeps it a chain parameter — and, being part of the stored
# machine, it is covered by the genesis state root like every other one.
#
# The init file also carries the accounts-drive Lua library into the guest
# (written to /var/lib, where bank-app.sh require()s it from), the same way
# the app itself travels: as guest bytes in the snapshot, so the genesis state
# root pins the exact library the chain runs.
ACCOUNTS_LIB="$REPO_DIR/accounts-drive/lua/accounts_drive.lua"
APP="$(mktemp "${TMPDIR:-/tmp}/op-cartesi-guest.XXXXXX")"
trap 'rm -f "$APP"' EXIT
{
  echo "cat > /var/lib/accounts_drive.lua <<'ACCOUNTS_DRIVE_LUA'"
  cat "$ACCOUNTS_LIB"
  echo "ACCOUNTS_DRIVE_LUA"
  sed "s/__OWNER__/$(echo "${GUEST_OWNER#0x}" | tr 'A-Z' 'a-z')/" "$GUEST_APP"
} > "$APP"

# The accounts drive (docs/ACCOUNTS-DRIVE-SPEC.md): shipped zero-filled — no
# image file, just a length — and formatted by the guest at boot, before the
# first yield, so the header lands inside the stored snapshot and the genesis
# state root covers it. The geometry is consensus: the start is explicit at
# the spec's recommended 0x80000000000000 (2^55) because 0.21's auto-placement
# depends on RAM length and drive order — nothing a consensus constant should
# depend on (ACCOUNTS.md §5.1) — and the length is 1 MiB (2^20), a power of
# two so the drive is a single aligned subtree of the machine's hash tree.
# mke2fs:false matters: 0.21's init otherwise formats a file-less drive as
# ext2, and the accounts format requires an all-zero device (with no ext2 the
# mount default is already off, but the intent is worth spelling out).
rm -rf "$SNAPSHOT_DIR"
cartesi-machine \
  --flash-drive="label:root,data_filename:$IMAGES_DIR/rootfs.ext2" \
  --flash-drive="label:accounts,start:0x80000000000000,length:1048576,mke2fs:false,mount:false" \
  --append-init-file="$APP" \
  --store="$SNAPSHOT_DIR"

echo "stored machine snapshot in $SNAPSHOT_DIR" >&2
echo "run it with: cartesi-jsonrpc-machine --server-address=127.0.0.1:6000" >&2
echo "then: ./devnet/start-shim.sh with MACHINE_REMOTE=http://127.0.0.1:6000 MACHINE_SNAPSHOT=$SNAPSHOT_DIR" >&2
