# Devnet

Running op-cartesi as a real OP Stack L2 needs four pieces:

1. **An L1 chain** — any post-Dencun Ethereum devnet (`anvil`, `geth --dev`, or kurtosis).
2. **L1 contracts** — `OptimismPortal`, `SystemConfig`, `DisputeGameFactory`/`L2OutputOracle`, and the batch inbox address. These are the stock OP Stack contracts, deployed with [`op-deployer`](https://docs.optimism.io/builders/chain-operators/tools/op-deployer). op-cartesi does not deploy or modify them.
3. **op-cartesi** — the execution engine, serving the Engine API.
4. **op-node** — in sequencer mode, pointed at the L1 and at op-cartesi.

Steps 1 and 2 are ordinary OP Stack chain-bringup and are not automated here: `op-deployer` needs a funded L1 deployer key and produces the addresses that go into `rollup.json`. What *is* automated is step 3 and the configuration that ties it to step 4, which is where op-cartesi differs from an op-geth chain.

You do not need to build op-node or op-batcher: `start-devnet.sh` will run the official docker images if the binaries are not on your `PATH`. See [below](#where-op-node-and-op-batcher-come-from).

## Quick start: the whole stack on anvil

`start-devnet.sh` brings up anvil as L1, a Cartesi Machine, op-cartesi as the
execution engine, and op-node sequencing on top:

```sh
./devnet/build-snapshot.sh     # once, needs the guest images (see that script)
./devnet/start-devnet.sh
```

### Where op-node and op-batcher come from

The OP monorepo publishes **no binaries** — its releases carry source archives
only — so unless you want to compile Go, docker is the official way to get
them. `start-devnet.sh` uses whichever is available:

| `OP_RUNTIME` | Behaviour |
|---|---|
| `auto` (default) | binaries on `PATH` if both are there, otherwise docker |
| `native` | `op-node` and `op-batcher` from `PATH` |
| `docker` | the images below, pulled on first use |

```sh
OP_RUNTIME=docker ./devnet/start-devnet.sh
```

The images are pinned in `env.sh` and versioned independently upstream, so the
tags do not match:

```
us-docker.pkg.dev/oplabs-tools-artifacts/images/op-node:v1.19.3
us-docker.pkg.dev/oplabs-tools-artifacts/images/op-batcher:v1.16.11
```

Two consequences of running them in containers, both handled by the script but
worth knowing:

- anvil and op-cartesi bind `0.0.0.0` instead of loopback, because a container
  reaches the host over the host gateway. On a shared network that exposes
  them, which is why it is not how the native path runs. macOS may also raise a
  firewall prompt the first time.
- op-batcher and op-node share a user-defined docker network and address each
  other by container name. Publishing op-node's RPC to the host's loopback is
  not enough — loopback is not reachable from the bridge gateway.

`build-snapshot.sh` builds the ledger guest (`bank-app.sh`) by default. Pass
`GUEST_APP=$PWD/devnet/echo-app.sh` for one that only echoes, or `probe-app.sh`
to see the raw request JSON a guest receives — which is how the other two were
written.

op-node then drives block production: every L2 block carries the L1-attributes
deposit it injects, that deposit is wrapped in an `EvmAdvance` envelope and fed
to the machine, and the machine's Merkle root becomes the block's state root.

`op-batcher` posts those blocks to L1 as calldata, which advances the safe
head, and a second node — its own machine, engine and op-node — rebuilds the
chain from that L1 data alone. Set `WITH_BATCHER=0` or `WITH_VERIFIER=0` to
leave either out.

```sh
cast block-number --rpc-url http://127.0.0.1:8545
cast rpc cartesi_getOutputsRoot latest --rpc-url http://127.0.0.1:8545
cast rpc optimism_syncStatus --rpc-url http://127.0.0.1:9545

# the verifier, which sequences nothing, must agree block for block
cast block 10 --rpc-url http://127.0.0.1:8545 | grep -E 'hash|stateRoot'
cast block 10 --rpc-url http://127.0.0.1:8565 | grep -E 'hash|stateRoot'
```

### L1 contracts, and proposals

`start-devnet.sh` deploys the full OP Stack L1 suite with `op-deployer` before
starting anything else, then runs `op-proposer` against it. Set
`WITH_CONTRACTS=0` for the older, faster bring-up on placeholder addresses, or
`WITH_PROPOSER=0` to deploy but not propose.

Two things about a devnet L1 that the standard path does not handle:

- Its chain id is not one `op-deployer` knows, and the standard intent resolves
  OPCM from a per-chain table. `deploy-l1.sh` uses `--intent-type custom`,
  which deploys the superchain contracts and implementations along the way —
  so the separate `op-deployer bootstrap` step turns out not to be needed.
- Only the L1 half of the output is used. `inspect genesis` and `inspect
  rollup` describe an op-geth L2 with predeploys and an allocs file, none of
  which exists here; this chain's genesis is the machine's root hash. So
  `inspect l1` supplies the addresses, the fee scalars are read back off the
  deployed SystemConfig, and the L2 side is generated as before.

The rollup is anchored at the L1 block the contracts landed in, not at L1
genesis — before that block there is no SystemConfig for op-node to read.

```sh
FACTORY=$(grep DISPUTE_GAME_FACTORY devnet/l1-addresses.env | cut -d= -f2)
cast call "$FACTORY" 'gameCount()(uint256)' --rpc-url http://127.0.0.1:8600
```

Proposals are made into the **permissioned** game (type 1), by the proposer
address the deployment authorised, and are never disputed — there is no fault
proof VM that can execute a Cartesi Machine. That is how OP chains launch;
real disputes are the settlement track.

Withdrawals still do not work, and deploying `OptimismPortal` does not change
that: `proveWithdrawalTransaction` verifies an MPT storage proof of the
L2ToL1MessagePasser account against the L2 state root, and ours is a Cartesi
hash tree. See [DESIGN.md §4 and §7](../docs/DESIGN.md).

### Deposits

`deposit.sh` sends an L1 deposit and lets op-node derive it into the L2 chain:

```sh
./devnet/deposit.sh 0x00000000000000000000000000000000000a11ce 1000000000000000000

# a few L2 blocks later, on either node:
cast rpc eth_call \
  '{"to":"0x0000000000000000000000000000000000000000",
    "data":"0x00000000000000000000000000000000000a11ce"}' latest \
  --rpc-url http://127.0.0.1:8545
# "0x...0de0b6b3a7640000"
```

`cast rpc eth_call` rather than `cast call`: the latter reads the caller's
nonce first, and this chain serves no `eth_getTransactionCount` because its
guest has no account model to take a nonce from.

The balance is kept by the guest (`bank-app.sh`), not by the shim, so it is
part of the state the machine's Merkle root commits to. Reading it goes through
`eth_call`, which the shim answers by running the machine's inspect protocol on
a fork it then discards.

With the contracts deployed, this calls `OptimismPortal.depositTransaction` —
the path a real user takes. With `WITH_CONTRACTS=0` there is no portal, so
`deposit.sh` installs a minimal `TransactionDeposited` emitter at the
configured address with `anvil_setCode` instead. Derivation reads the log
rather than the contract that produced it, so as far as the chain is concerned
the two are the same, and the guest is credited either way.

Two operational notes the script encodes, both easy to trip over by hand:

- A machine server holds exactly one machine, and `machine.load` refuses to
  replace it. Config generation and the node each need their own server.
- op-node must not be started before the engine logs `chain initialized`.

## Persistence

`DATA_DIR` gives each node a store, and the chain survives a restart:

```sh
DATA_DIR=/tmp/op-cartesi-data CHECKPOINT_INTERVAL=25 ./devnet/start-devnet.sh
ls /tmp/op-cartesi-data/checkpoints
```

Blocks and the machine's emissions go into a pebble database; the machine
itself is checkpointed whole every `CHECKPOINT_INTERVAL` blocks and at every
finalized block. A checkpoint is around 400 MB with nothing deduplicating
between them, so `-checkpoint-retention` (default 3) is the disk budget.

The write comes off a fork of the machine the chain already keeps, so it does
not stall block production — measured at exactly 2.00 s per block across a
checkpoint. Restarting loads the newest checkpoint and re-executes the blocks
after it, checking each against the state root it was stored with.

Each node needs its own directory: a pebble database is held by one process,
so the verifier gets `$DATA_DIR-verifier` automatically.

## The snapshot is stored already booted

`build-snapshot.sh` stores the machine where `cartesi-machine --store` leaves
it: booted, and parked at its first input yield. This is how Cartesi Rollups
distributes templates, and op-cartesi requires it — it refuses a machine stored
at mcycle 0 rather than booting one itself:

```
the stored machine is not parked at an input yield — store it with
`cartesi-machine ... --store=<dir>`, which runs to the first yield,
rather than with --max-mcycle=0
```

The reason is genesis. The chain's genesis state root is the machine's root
hash, so if the node did the booting, genesis would depend on how the node ran
it rather than on the snapshot — and two operators handed the same template
could compute different genesis blocks, and so different rollup configs. With
the machine stored after boot there is nothing to disagree about: genesis is
the stored machine's own root hash. It is also the hash Cartesi anchors on
chain as the template hash.

Node startup drops from tens of seconds to about one as a side effect.

## Quick start (no L1, no contracts)

`start-shim.sh` runs op-cartesi alone against the in-memory mock machine. Nothing derives from L1, but the Engine API is live and can be driven by hand, which is enough to explore the RPC surface:

```sh
./devnet/start-shim.sh
```

It writes a JWT secret to `devnet/jwt.hex`, starts the engine port on `:8551` and the public `eth_*` port on `:8545`, and prints the genesis block hash.

## Full devnet

```sh
# 1. L1 (example: anvil with Cancun support)
anvil --host 0.0.0.0 --port 8545 --chain-id 900 --block-time 4

# 2. Deploy the OP Stack L1 contracts with op-deployer, then note:
#      - OptimismPortal proxy address
#      - SystemConfig proxy address
#      - batch inbox address
#      - batcher address
#      - the L1 block hash/number to anchor the rollup to

# 3. Generate rollup.json. The chain flags here MUST match the ones given to
#    `run` below, since they determine the genesis block hash.
./devnet/generate-config.sh

# 4. Start op-cartesi
./devnet/start-shim.sh

# 5. Start op-node in sequencer mode
op-node \
  --l1=http://127.0.0.1:8545 \
  --l1.beacon=http://127.0.0.1:5052 \
  --l2=http://127.0.0.1:8551 \
  --l2.jwt-secret=./devnet/jwt.hex \
  --rollup.config=./devnet/rollup.json \
  --sequencer.enabled \
  --sequencer.l1-confs=0 \
  --p2p.disable \
  --rpc.addr=127.0.0.1 --rpc.port=9545
```

`op-batcher` and `op-proposer` then attach to op-node exactly as they would on any OP chain; nothing about them is op-cartesi-specific.

## Fork support

The fork schedule is fixed: every fork through **Isthmus** is active from
genesis, and none of them is configurable. A new chain has no pre-fork history
to preserve, and Isthmus is not optional — op-node computes the L2 output root
pre-Isthmus by proving the L2ToL1MessagePasser account against the block's
state root, which cannot work here because that state root is a Cartesi hash
tree, not an Ethereum MPT. A pre-Isthmus chain could never be proposed, so the
shim does not offer one.

Accordingly op-cartesi serves `engine_forkchoiceUpdatedV3` plus the **V4**
payload methods, which is exactly what op-node calls for an Isthmus chain.

Jovian and later are not supported: Jovian adds a minimum-base-fee field the
shim does not implement.

## Genesis hash consistency

The L2 genesis block hash is derived from the Cartesi Machine's root hash plus
the chain configuration. Two consequences:

- Changing the machine (a different guest program, a different snapshot) changes
  the genesis hash, and `rollup.json` must be regenerated.
- The chain flags passed to `genesis` and to `run` must be identical. If they
  differ, op-node will refuse to start, complaining that the engine's genesis
  block does not match the rollup config — which is the failure mode you want,
  rather than a chain that silently disagrees with its own configuration.
