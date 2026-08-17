# Devnet

Running op-cartesi as a real OP Stack L2 needs four pieces:

1. **An L1 chain** — any post-Dencun Ethereum devnet (`anvil`, `geth --dev`, or kurtosis).
2. **L1 contracts** — `OptimismPortal`, `SystemConfig`, `DisputeGameFactory`/`L2OutputOracle`, and the batch inbox address. These are the stock OP Stack contracts, deployed with [`op-deployer`](https://docs.optimism.io/builders/chain-operators/tools/op-deployer). op-cartesi does not deploy or modify them.
3. **op-cartesi** — the execution engine, serving the Engine API.
4. **op-node** — in sequencer mode, pointed at the L1 and at op-cartesi.

Steps 1 and 2 are ordinary OP Stack chain-bringup and are not automated here: `op-deployer` needs a funded L1 deployer key and produces the addresses that go into `rollup.json`. What *is* automated is step 3 and the configuration that ties it to step 4, which is where op-cartesi differs from an op-geth chain.

You do not need to build op-node or op-batcher: `start-devnet.ts` will run the official docker images if the binaries are not on your `PATH`. See [below](#where-op-node-and-op-batcher-come-from).

## Quick start: the whole stack on anvil

`start-devnet.ts` brings up anvil as L1, a Cartesi Machine, op-cartesi as the
execution engine, and op-node sequencing on top:

```sh
bun install                    # once, at the repo root (bun workspace)
./scripts/build-snapshot.ts    # once — `cartesi build` of demo (see that script)
./devnet/start-devnet.ts
```

Every piece runs in a pane of its own under
[mprocs](https://github.com/pvolok/mprocs), which `bun install` provides:

```
info              op-cartesi devnet
l1                =================
l1-contracts
genesis             L1 (anvil)        http://127.0.0.1:8600        chain 900
machine             L2 (op-cartesi)   http://127.0.0.1:8545        chain 901
engine              op-node           http://127.0.0.1:9545
guest               op-batcher        http://127.0.0.1:8548
op-node             op-proposer       http://127.0.0.1:8560
op-batcher          verifier L2       http://127.0.0.1:8565
op-proposer         verifier op-node  http://127.0.0.1:9555
verifier-machine
verifier-engine   panes
verifier-node     -----
outputs             machine   the emulator's console: Linux booting, then …
```

Up and down pick a pane, `s` starts one, `x` stops it, `r` restarts it, `q`
quits and stops everything. Every pane is also written to
`devnet/logs/<pane>.log`, so `grep` still works.

Three of the panes are not long-running processes but steps that finish and
stay down: `l1-contracts` (op-deployer), `genesis` (the rollup config), and
`outputs`, which does not start on its own at all — see
[below](#how-the-bring-up-is-organized).

### Where op-node and op-batcher come from

The OP monorepo publishes **no binaries** — its releases carry source archives
only — so unless you want to compile Go, docker is the official way to get
them. `start-devnet.ts` uses whichever is available:

| `OP_RUNTIME` | Behaviour |
|---|---|
| `auto` (default) | binaries on `PATH` if both are there, otherwise docker |
| `native` | `op-node` and `op-batcher` from `PATH` |
| `docker` | the images below, pulled on first use |

```sh
OP_RUNTIME=docker ./devnet/start-devnet.ts
```

The images are pinned in `lib/env.ts` and versioned independently upstream, so the
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

The guest is [`demo`](../demo/README.md) — the routed guest of
[docs/EVM-COMPAT.md](../docs/EVM-COMPAT.md), TypeScript on `@cartesi/rollup`, its
ledger on the accounts drive. `build-snapshot.ts` wraps `cartesi build`, which
stores the booted machine under `demo/.cartesi/image`; that directory is the
chain's genesis state.

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

### How the bring-up is organized

`start-devnet.ts` starts nothing. It checks that the run can succeed — the
tools on `PATH`, the snapshot, the ports, the JWT secret — compiles
op-cartesi once into `bin/`, clears what a previous run left behind, writes
an mprocs config for the panes this run wants, and hands over. One script per
process, under `devnet/procs/`:

| pane | what it is |
|---|---|
| `info` | the endpoint summary; prints and stays down |
| `l1` | anvil, not `--silent`: L1 blocks and transactions as they land |
| `l1-contracts` | `deploy-l1.ts` (op-deployer). Optional, runs once |
| `genesis` | anchors the rollup and writes `rollup.json`. Runs once |
| `machine` | `cartesi-jsonrpc-machine`: the guest's console |
| `engine` | op-cartesi, the sequencer's engine |
| `guest` | what the guest says about each transaction — its reports |
| `op-node` `op-batcher` `op-proposer` | the OP tools, native or in docker |
| `verifier-*` | the second node's machine, engine and op-node |
| `outputs` | `deploy-outputs.ts`. Does not autostart — press `s` |

mprocs starts every pane at once and has no notion of dependencies, so the
ordering the old single script did by hand each process now does for itself:
`op-node` blocks until the engine's port answers, `engine` blocks until
`genesis` has written the rollup config, `genesis` blocks until the contracts
are deployed. Two kinds of signal, no log-scraping:

- **A port that answers.** For op-cartesi this is exact rather than
  approximate: it binds its listeners only after the chain is open and the
  machine has booted, so a port that answers is an engine that can serve.
  (This is what the old "op-node must not start before the engine logs
  `chain initialized`" note was about; it is now the engine's own business.)
- **A marker under `devnet/.state`,** for the steps that finish rather than
  run — a file exists from its first byte, so the deploy announces itself
  when it is done rather than when it starts writing.

Because each pane's waits are its own, any pane restarts on its own: `r` on
`op-node` re-runs the same waits and comes back up against the running
engine. `x` then `s` on `engine` restarts the chain against the same machine
server.

The one thing that has to travel between panes is consensus-relevant:
`genesis` writes the L1 anchor and the L2 genesis timestamp to
`devnet/chain-genesis.env`, which `lib/env.ts` reads back — that is how the
engine, started from a different pane, ends up on exactly the genesis the
rollup config was generated with. The deployment outputs travel the same way,
and are re-read rather than cached, so a process that started before the
deploy it waited for sees what the deploy wrote.

Quitting mprocs stops every process it started, killing each one's whole
process group, which is what the machine servers need: op-cartesi forks a
server per block and the ones it prunes reparent to init, out of reach of any
parent-to-child walk but never out of their process group. (bun's children
share their parent's group, so a pane is one group however many processes
deep it goes.) If mprocs is killed rather than quit, `start-devnet.ts` tears
the stack down on its way out; run `stop-devnet.ts` yourself if a terminal
died and left the stack behind:

```sh
./devnet/stop-devnet.ts
```

### Watching the guest

The guest program inside the machine is visible in two panes, and they show
different things:

- **`machine`** is the emulator's console — Linux booting, then anything the
  guest writes to stdout. The servers op-cartesi forks per block inherit
  these file descriptors, so their output lands in the same pane.
- **`guest`** is the guest's account of the chain: the reports it emitted for
  each transaction, read back over `cartesi_getTransactionEmissions`. Reports
  are diagnostic and explicitly not provable, so they never appear in a
  receipt — and for a rejected input they are the only account of why it
  failed.

```
block 41 0x9f2c7a13…4b0e21 1 tx, 3.4M cycles
  accepted 0x77c1ab90…5d1f42 to 0x42000000…000015 — 3.4M cycles
  report   ledger: credited 0xa11ce with 1000000000000000000
  output#7 0x…                       # a voucher or notice, with its tree index
```

Reports carry the router's one-byte tag ([EVM-COMPAT §8](../docs/EVM-COMPAT.md)),
so the pane can tell an app diagnostic from `eth_call` return data from revert
data — a revert is shown decoded, `Error("nonce too low")` rather than a
selector. Printable payloads are shown as text, everything else as hex.

`WITH_GUEST_LOG=0` drops the pane. It is an ordinary client script, so it also
runs standalone against any node — the verifier included:

```sh
L2_RPC=http://127.0.0.1:8565 bun devnet/guest-log.ts
```

### L1 contracts, and proposals

The `l1-contracts` pane deploys the full OP Stack L1 suite with `op-deployer`,
and everything downstream waits for it — the rollup has to be anchored at a
block where the SystemConfig already exists. `op-proposer` then runs against
it. Set
`WITH_CONTRACTS=0` for a faster bring-up on placeholder addresses — a
sequencing-only smoke mode: blocks, derivation, restarts, but no deposits
(there is no portal to call) and therefore no funded accounts, withdrawals
or tokens. Set `WITH_PROPOSER=0` to deploy but not propose.

Two things about a devnet L1 that the standard path does not handle:

- Its chain id is not one `op-deployer` knows, and the standard intent resolves
  OPCM from a per-chain table. `deploy-l1.ts` uses `--intent-type custom`,
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

Deploying `OptimismPortal` does not make OP's own withdrawal path usable:
`proveWithdrawalTransaction` verifies an MPT storage proof of the
L2ToL1MessagePasser account against the L2 state root, and ours is a Cartesi
hash tree. Withdrawals go through Cartesi vouchers instead — see below and
[DESIGN.md §4 and §7c](../docs/DESIGN.md).

### Two packages: bringing it up, and using it

`devnet/` is one job — put the stack on its feet and keep it there. Talking
to the stack once it is up is the other, and it is [`scripts/`](../scripts/README.md):
deposits, withdrawals, balances, the snapshot they all run against. Both are
members of the repo's bun workspace (`bun install` at the repo root once), and
every file in either is directly executable.

The split is a dependency direction, not a folder. A client script imports
the devnet's configuration and its viem clients through the two entry points
this package publishes —

```ts
import { config, usage } from "devnet/env";
import { l1Public, l2Chain } from "devnet/wallet";
```

— and nothing goes the other way: the devnet never imports a script. That is
the right direction, because the values are the devnet's own. The ports it
binds and the addresses its deploys wrote are what a client has to agree
with, and they are written into `devnet/*.env` by the panes that produced
them.

| | |
|---|---|
| `start-devnet.ts` `stop-devnet.ts` `procs/*.ts` | the stack |
| `deploy-l1.ts` `deploy-outputs.ts` | the deploys, and two of the panes |
| `generate-config.ts` `start-shim.ts` | op-cartesi on its own |
| `guest-log.ts` | the `guest` pane, and a tail for any node |
| `lib/env.ts` | all configuration — `devnet/env` |
| `lib/wallet.ts` | the chains and the viem clients — `devnet/wallet` |
| `lib/proc.ts` `lib/optools.ts` `lib/opcartesi.ts` | running and waiting |

There is no shell left in either package — not the client scripts, not the
orchestration. For the transactional half this was always the argument: the Ethereum
plumbing the shell did with `cast` and hex string surgery — ABI encoding,
receipt polling, deposit payload packing — is what viem is for. The
orchestration half followed for a different reason. It had grown a second
language of its own: `python3` inline for JSON and TOML, `cast` subprocesses
for what viem does in a line, and a configuration file (`env.sh`) that
existed only because the shell could not read the TypeScript one. Deploying
the L1 suite now reads `SystemConfig.scalar()` with `readContract` instead
of shelling out to `cast call` and decoding the result in a heredoc of
Python, and `devnet/lib/env.ts` is the only place any variable is named.

What is left of the shell's job — spawning a program, waiting for a port,
signalling a process group — is `lib/proc.ts`, and it is smaller than the
shell version: bun's children share their parent's process group, so the
teardown that mattered most comes for free.

User overrides go in a `.env` at the repo root — `SENDER_KEY=…`, `L1_RPC=…`,
anything `lib/env.ts` reads. It is loaded from the repo root explicitly
rather than left to bun's cwd-relative auto-loading, so it applies wherever a
script is run from, and it sits below real environment variables. The
machine-written files (`l1-addresses.env`, `chain-genesis.env`,
`outputs-addresses.env`, `token.env`) stay separate and stronger — they are
deployment outputs, not preferences, and they are re-read rather than cached,
so a process that started before a deploy still sees what it wrote. `.env` is
gitignored; keys live there, not in command lines.

The scripts ([`scripts/`](../scripts/README.md)) and the guest speak the same standard: `withdraw.ts` and
`withdraw-erc20.ts` call the bridge predeploy, `balance.ts` reads the drive
and the ERC-20 façades, `contracts.ts` asks `cartesi_getContracts` what the
guest routes — every recorded contract with its ABI, every token façade with
its L1 token, straight off the machine's drives — and the guest they address
is the one `build-snapshot.ts` builds — the routed guest
([demo](../demo/README.md), [docs/EVM-COMPAT.md](../docs/EVM-COMPAT.md)).
Nothing here speaks a private dialect anymore; an app that wants one still
can, through `cartesi_inspect` and `send-l2-tx.ts` with raw payloads.

### Deposits

`deposit.ts` sends an L1 deposit and lets op-node derive it into the L2 chain:

```sh
bun scripts/deposit.ts 0x00000000000000000000000000000000000a11ce 1000000000000000000
# ...the script follows the deposit to its derived L2 transaction, then:
bun scripts/balance.ts 0x00000000000000000000000000000000000a11ce
```

The balance is kept by the guest, not by the shim, on the accounts drive of
[docs/ACCOUNTS.md](../docs/ACCOUNTS.md), so it is part of the state the
machine's Merkle root commits to — and readable two ways: plain
`eth_getBalance` reads the drive record straight out of machine memory with
no execution at all (`eth_getTransactionCount` likewise), while `eth_call`
runs the guest's inspect protocol on a discarded fork. Since the shim
learned EVM-COMPAT §7's envelope, `eth_call` wraps every query as
`EvmCall(chainId, from, to, value, data)` and unwraps the guest's tagged
reports — return data on accept, a code-3 revert error with data on reject —
which is what lets `readContract` and `cast call` treat guest handlers as
ordinary contracts. An app-private inspect dialect is still reachable
through `cartesi_inspect`, which stays a raw passthrough in both
directions.

This calls `OptimismPortal.depositTransaction` — the path a real user takes —
and requires the deployed contract suite; on a `WITH_CONTRACTS=0` devnet
there is no portal, and `deposit.ts` says so and stops. (An earlier version
carried a contract-less fallback: a minimal `TransactionDeposited` emitter
installed with `anvil_setCode`, exploiting the fact that derivation reads
the log rather than the contract that produced it. It was the last of the
hand-packed hex, anvil-only, and the only thing the contract-less mode
funded — retired once `WITH_CONTRACTS=1` became the default.)

### Withdrawals

`deploy-outputs.ts` puts the L1 half in place: the validator that opens an OP
proposal's root claim, the executor that runs a proven output, and the two
portals. It also funds the executor and registers the portals with the guest.

It is the `outputs` pane — the one step of the flow that waits for you rather
than for another process, since not every run wants to move assets. Select it
and press `s`, or run it yourself:

```sh
./devnet/deploy-outputs.ts
bun scripts/withdraw.ts 0x00000000000000000000000000000000000a11ce 500000000000000000
```

`withdraw.ts` asks the guest for a withdrawal — a `withdrawEther` call on the
routed guest's bridge predeploy, carrying the wei as `msg.value` — and
`scripts/lib/voucher.ts` does the rest: wait for a proposal covering the voucher's
block, open that proposal's root claim on L1, and prove the voucher against
the outputs root inside it with Cartesi's own libraries. Nothing about op-node, op-batcher or op-proposer is
modified or aware of any of this — the root claim they already publish is the
commitment the proof runs against.

The proof is built against the *proposed* block, not the block that emitted the
voucher. The outputs tree is cumulative over the chain, so a withdrawal has to
stay provable as the tree grows past it; `cartesi_getOutputProof` takes both the
output index and the block to prove it as of.

### Tokens

ERC-20 goes through a Cartesi-style portal, not `L1StandardBridge`:

```sh
bun scripts/deposit-erc20.ts 1000000000000000000      # deploys a test token first time
bun scripts/balance.ts 0x70997970C51812dc3A010C7d01b50e0d17dc79C8 $TEST_TOKEN_ADDRESS
bun scripts/withdraw-erc20.ts 0x70997970C51812dc3A010C7d01b50e0d17dc79C8 500000000000000000
```

The portal escrows the tokens in the application contract and hands the guest
Cartesi's own packed deposit payload as the data of an `OptimismPortal`
deposit; the withdrawal is a voucher calling `transfer` on the token from that
same contract. The standard bridge is avoided deliberately: it escrows in
itself and releases only against the MPT proof this chain cannot produce, so
tokens sent through it would be stuck. [DESIGN.md §7d](../docs/DESIGN.md).

The guest credits a portal deposit only if the deposit's `from` is a portal it
was told about — `alias(portal)`, since `OptimismPortal` aliases contract
callers. It learns those addresses from `GUEST_OWNER`, an address baked into
the snapshot and therefore into the genesis state root, whose deposits it
treats as configuration. That indirection exists because the portals do not
exist when the snapshot is built, and trusting any sender would let any
contract mint claims against tokens the application really holds.

### Testing the guest

The guest never runs a machine on the host, but its logic does:

```sh
bun run test    # at the repo root: app + abi-drive suites
```

The vitest suite drives the router with hand-built deposits and signed
transactions over an in-memory accounts drive — including malformed ones,
since an error inside the guest halts the machine, and a halted machine is a
halted chain. (Its Lua predecessor had the same discipline in
`test-guest.lua`, retired with the bank app.)

Two operational notes, both easy to trip over by hand — and both handled by
`devnet/procs/`:

- A machine server holds exactly one machine, and `machine.load` refuses to
  replace it. Config generation and the node each need their own server,
  which is why `genesis` starts one of its own and stops it again.
- op-node must not be started before the engine is serving. `procs/op-node.ts`
  waits for the engine's port, which op-cartesi binds only once the chain is
  open.

## Persistence

`DATA_DIR` gives each node a store, and the chain survives a restart:

```sh
DATA_DIR=/tmp/op-cartesi-data CHECKPOINT_INTERVAL=25 ./devnet/start-devnet.ts
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

`build-snapshot.ts` stores the machine where `cartesi-machine --store` leaves
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

`start-shim.ts` runs op-cartesi alone against the in-memory mock machine. Nothing derives from L1, but the Engine API is live and can be driven by hand, which is enough to explore the RPC surface:

```sh
./devnet/start-shim.ts
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
./devnet/generate-config.ts

# 4. Start op-cartesi
./devnet/start-shim.ts

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
