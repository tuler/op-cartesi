# syntax=docker.io/docker/dockerfile:1

# The two images the compose devnet builds. Everything else it runs is an image
# someone else publishes, used as it comes: anvil from foundry, op-node,
# op-batcher and op-proposer from the OP monorepo's own registry.
#
#   node    the emulator's image plus the op-cartesi binary. It runs the
#           machine servers and the engines — one image for both, because an
#           engine is only ever meaningful next to a machine server, and
#           because the emulator's image is where a matching emulator comes
#           from. curl is added for the healthchecks: compose runs those
#           inside the container, so the container carries the client.
#
#   tools   bun, the repo, op-deployer and forge. It runs the steps that
#           deploy and configure the chain, which are the repo's own
#           TypeScript — deploy-l1.ts, compose/genesis.ts, deploy-outputs.ts —
#           unchanged from the way mprocs runs them.
#
# Build context is the repo root: the Go build needs the whole module, and the
# app is a bun workspace whose members live outside devnet/.

ARG MACHINE_IMAGE=cartesi/machine-emulator:0.21.0
ARG BUN_IMAGE=oven/bun:1.3
ARG GO_IMAGE=golang:1.25-trixie
ARG FOUNDRY_IMAGE=ghcr.io/foundry-rs/foundry:v1.3.0
ARG OP_DEPLOYER_IMAGE=us-docker.pkg.dev/oplabs-tools-artifacts/images/op-deployer:v0.7.1

################################################################################
# The engine, built once and copied into both images.
FROM ${GO_IMAGE} AS go-build
WORKDIR /src
# The manifests first, for layer caching — the two drive libraries among them,
# since go.mod replaces them with directories in this repository.
COPY go.mod go.sum ./
COPY accounts-drive/go/go.mod accounts-drive/go/
COPY abi-drive/go/go.mod abi-drive/go/
RUN go mod download
COPY . .
# Static, because the two images it lands in have different libcs.
RUN CGO_ENABLED=0 go build -o /out/op-cartesi ./cmd/op-cartesi

FROM ${FOUNDRY_IMAGE} AS foundry
FROM ${OP_DEPLOYER_IMAGE} AS op-deployer

################################################################################
# node: machine servers and engines.
FROM ${MACHINE_IMAGE} AS node

USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=go-build /out/op-cartesi /usr/local/bin/op-cartesi
COPY devnet/compose/engine.sh /usr/local/bin/engine.sh

# A named volume mounted here takes this directory's ownership, which is how
# the store stays writable by the user the emulator's image runs as.
RUN mkdir -p /data && chown cartesi:cartesi /data

USER cartesi

################################################################################
# tools: the deploy and configuration steps.
FROM ${BUN_IMAGE} AS tools

# op-deployer is a static Go binary, so it travels; forge and cast come from
# foundry's own image rather than from an installer, which pins them to a tag
# instead of to whatever the installer resolves today.
COPY --from=op-deployer /usr/local/bin/op-deployer /usr/local/bin/op-deployer
COPY --from=foundry /usr/local/bin/forge /usr/local/bin/forge
COPY --from=foundry /usr/local/bin/cast /usr/local/bin/cast
COPY --from=go-build /out/op-cartesi /usr/local/bin/op-cartesi
# Otherwise lib/opcartesi.ts falls back to `go run`, and there is no Go here.
ENV OP_CARTESI_BIN=/usr/local/bin/op-cartesi

WORKDIR /repo

# Manifests first, for layer caching; bun resolves the whole workspace.
COPY package.json bun.lock ./
COPY accounts-drive/js/package.json accounts-drive/js/
COPY abi-drive/js/package.json abi-drive/js/
COPY evm-compat/js/package.json evm-compat/js/
COPY app/package.json app/
COPY demo/package.json demo/
COPY devnet/package.json devnet/
COPY scripts/package.json scripts/
RUN bun install --frozen-lockfile

COPY . .

# Root, not bun's own user: the generated files land in a bind mount of the
# repo, and a devnet that writes them as a uid the host does not have is a
# devnet you need sudo to clean up after.
USER root
