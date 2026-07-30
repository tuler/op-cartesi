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

rm -rf "$SNAPSHOT_DIR"
cartesi-machine \
  --flash-drive="label:root,data_filename:$IMAGES_DIR/rootfs.ext2" \
  --append-init-file="$GUEST_APP" \
  --store="$SNAPSHOT_DIR"

echo "stored machine snapshot in $SNAPSHOT_DIR" >&2
echo "run it with: cartesi-jsonrpc-machine --server-address=127.0.0.1:6000" >&2
echo "then: ./devnet/start-shim.sh with MACHINE_REMOTE=http://127.0.0.1:6000 MACHINE_SNAPSHOT=$SNAPSHOT_DIR" >&2
