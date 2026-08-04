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

# Here the image's platform does not necessarily have to be riscv64.
# If any needed dependencies rely on native binaries, you must use
# a riscv64 image such as cartesi/node:20-jammy for the build stage,
# to ensure that the appropriate binaries will be generated.
FROM --platform=$BUILDPLATFORM node:24.17.0-trixie AS build-stage

WORKDIR /opt/cartesi/dapp

RUN corepack enable pnpm
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY . .
RUN pnpm run build

# Stage the runtime files: the JS bundle plus @deroll/cmio's native addon,
# which esbuild cannot inline and must be required at runtime. `cp -RL` flattens
# pnpm's self-contained store dir for the addon into a real node_modules (the
# addon, its node-gyp-build loader, and node-addon-api).
RUN mkdir -p rootfs \
 && cp dist/index.js rootfs/index.js \
 && cp -RL node_modules/.pnpm/@deroll+cmio@*/node_modules rootfs/node_modules


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

WORKDIR /opt/cartesi/dapp
COPY --from=build-stage /opt/cartesi/dapp/rootfs .

ENTRYPOINT ["node"]
CMD ["index.js"]
