# syntax=docker.io/docker/dockerfile:1

# This enforces that the packages downloaded from the repositories are the same
# for the defined date, no matter when the image is built.
ARG APT_UPDATE_SNAPSHOT=20260415T030400Z
ARG MACHINE_GUEST_TOOLS_VERSION=0.18.0
ARG MACHINE_GUEST_TOOLS_SHA256SUM=204d4260defd68e11b957ae1f1b511b6c2c74345c918748be06f592733b72dcd

################################################################################
# riscv64 base stage
FROM --platform=linux/riscv64 cartesi/node:24.17.0-noble-slim AS base

ARG APT_UPDATE_SNAPSHOT
ARG DEBIAN_FRONTEND=noninteractive
RUN <<EOF
set -eu
apt-get update
apt-get install -y --no-install-recommends ca-certificates
apt-get update --snapshot=${APT_UPDATE_SNAPSHOT}
apt-get remove -y --purge ca-certificates
apt-get autoremove -y --purge
EOF

################################################################################
# build stage: includes resources necessary for installing dependencies

# Here the image's platform does not necessarily have to be riscv64: the
# native addon ships prebuilds and node-gyp-build picks the riscv64 one at
# runtime. The build context is the REPO ROOT (cartesi.toml sets
# context = ".."), because the app is a bun workspace member whose
# @cartesi/accounts dependency lives in accounts-drive/js.
FROM --platform=$BUILDPLATFORM oven/bun:1.3 AS build-stage

WORKDIR /opt/cartesi

# Manifests first, for layer caching; bun resolves the whole workspace.
COPY package.json bun.lock ./
COPY accounts-drive/js/package.json accounts-drive/js/
COPY abi-drive/js/package.json abi-drive/js/
COPY evm-compat/js/package.json evm-compat/js/
COPY routed-guest/package.json routed-guest/
COPY ledger-app/package.json ledger-app/
COPY devnet/package.json devnet/
RUN bun install --frozen-lockfile

COPY accounts-drive/ accounts-drive/
COPY abi-drive/ abi-drive/
COPY evm-compat/ evm-compat/
COPY routed-guest/ routed-guest/
COPY ledger-app/ ledger-app/
RUN cd ledger-app && bun run build

# Stage the runtime files: the JS bundle plus @deroll/cmio's native addon,
# which esbuild cannot inline and must be required at runtime. The script
# locates the packages through module resolution rather than assuming a
# node_modules layout — bun 1.3's isolated linker keeps workspace members'
# dependencies out of the root node_modules.
RUN bun ledger-app/stage.mts rootfs


################################################################################
# runtime stage: produces final image that will be executed

# Here the image's platform MUST be linux/riscv64.
# Give preference to small base images, which lead to better start-up
# performance when loading the Cartesi Machine.
FROM base

ARG MACHINE_GUEST_TOOLS_VERSION
ARG MACHINE_GUEST_TOOLS_SHA256SUM
ADD --checksum=sha256:${MACHINE_GUEST_TOOLS_SHA256SUM} \
  https://github.com/cartesi/machine-guest-tools/releases/download/v${MACHINE_GUEST_TOOLS_VERSION}/machine-guest-tools_riscv64.deb \
  /tmp/machine-guest-tools_riscv64.deb

ARG DEBIAN_FRONTEND=noninteractive
RUN <<EOF
set -e
apt-get install -y --no-install-recommends \
  busybox-static \
  /tmp/machine-guest-tools_riscv64.deb

rm /tmp/machine-guest-tools_riscv64.deb
rm -rf /var/lib/apt/lists/* /var/log/* /var/cache/*
EOF

ENV PATH="/opt/cartesi/bin:${PATH}"

# Genesis parameters of the routed guest (docs/EVM-COMPAT.md): baked into the
# image environment, which `cartesi build` passes to the machine
# (use_docker_env defaults to true), so they are covered by the genesis state
# root like every other consensus parameter. Defaults match devnet/env.sh.
ARG CHAIN_ID=901
ARG OWNER=0x90F79bf6EB2c4f870365E785982E1f101E93b906
ENV CHAIN_ID=${CHAIN_ID}
ENV OWNER=${OWNER}
ENV ACCOUNTS_DRIVE=/dev/pmem1
ENV ABI_DRIVE=/dev/pmem2

WORKDIR /opt/cartesi/dapp
COPY --from=build-stage /opt/cartesi/rootfs .

ENTRYPOINT ["node"]
CMD ["index.js"]
