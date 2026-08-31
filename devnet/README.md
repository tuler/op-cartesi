# Devnet

Running op-cartesi as a real OP Stack L2 needs four pieces:

1. **An L1 chain** — any post-Dencun Ethereum devnet (`anvil`, `geth --dev`, or kurtosis).
2. **L1 contracts** — `OptimismPortal`, `SystemConfig`, `DisputeGameFactory`/`L2OutputOracle`, and the batch inbox address. These are the stock OP Stack contracts, deployed with [`op-deployer`](https://docs.optimism.io/builders/chain-operators/tools/op-deployer). op-cartesi does not deploy or modify them.
3. **op-cartesi** — the execution engine, serving the Engine API.
4. **op-node** — in sequencer mode, pointed at the L1 and at op-cartesi.

All four are automated here, as a docker compose stack: `docker compose up` at
the repo root puts up the whole chain, and the only things it needs on your
machine are docker and a machine snapshot. There is no second way in — the
compose file is the bring-up.

## Quick start: the whole stack on anvil

```sh
cartesi build          # once — the guest snapshot, the chain's genesis state
docker compose up      # the stack
```

Two prerequisites: docker, and [@cartesi/cli](https://github.com/cartesi/cli)
2.0 alpha for that first command (`npm i -g @cartesi/cli@alpha`). The build is
driven by [`cartesi.toml`](../cartesi.toml) at the repo root and stores the
machine under `.cartesi/image`.

anvil as L1, the OP Stack L1 suite deployed with op-deployer, a Cartesi
Machine, op-cartesi as the execution engine, op-node sequencing on top, a
batcher, a proposer, and a second node verifying from L1 alone — one service
each:

```
l1               anvil            :8600     L1, chain 900
l1-contracts     op-deployer                runs once, exits
genesis-machine  the emulator               a machine server for the step below
genesis          the rollup config          runs once, exits
machine          the emulator     :6300     the guest's console
engine           op-cartesi       :8545     the L2 RPC, :8551 the Engine API
op-node          op-node          :9545     sequencing
op-batcher       op-batcher       :8548     posting to L1 as calldata
op-proposer      op-proposer      :8560     a game per proposal
verifier-*       the second node  :8565     deriving from L1 alone, :9555 its node
outputs          deploy-outputs.ts          manual
guest            guest-log.ts               manual
```

Nothing else is needed on the host: no bun, no go, no foundry, no op-deployer.
The two images this builds carry them, and everything else is an image someone
else publishes.

Two services do not start with the stack, because each is a step you take
rather than a process that runs:

```sh
docker compose run --rm outputs    # the outputs suite, once a proposal exists
docker compose run --rm guest      # what the guest says about each transaction
```

Naming a service brings up that service and what it depends on, which is how
you run less than everything:

```sh
docker compose up op-node        # sequencer only: no batcher, no proposer, no verifier
docker compose up op-batcher     # ...and the batcher
```

The one thing you cannot ask for is a chain without L1 contracts. The compose
file describes one topology, and in it the rollup is always anchored at a block
where the SystemConfig exists.

```sh
docker compose logs -f machine        # the emulator's console: Linux, then the guest
docker compose logs -f engine op-node # or several at once
docker compose restart op-node        # one piece, without disturbing the rest
docker compose down -v                # stop everything and forget it
```

Running `docker compose up` again is a no-op rather than a new chain: see
[below](#how-the-bring-up-is-organized).

### What runs in which image

Two images are built from [`devnet/compose/Dockerfile`](compose/Dockerfile),
and everything else is used as its publisher ships it:

```
node    the emulator's own image + the op-cartesi binary + curl
tools   bun + the repo + op-deployer + forge
```

`node` runs the machine servers and the engines — one image for both, because
an engine is only meaningful next to a machine server and the emulator's image
is where a matching emulator comes from. `tools` runs the repo's own TypeScript:
`deploy-l1.ts`, `deploy-outputs.ts` and `guest-log.ts`, plus the two thin steps
in `compose/` that drive them. curl is in `node` for the healthchecks, since
compose runs those inside the container being checked.

### The engine is a slot

`op-node` drives a service that speaks
[docs/ENGINE-RPC-SPEC.md](../docs/ENGINE-RPC-SPEC.md); which implementation
does the speaking is `ENGINE_IMAGE`, and the verifier's is
`VERIFIER_ENGINE_IMAGE`. Both default to the `node` image built here, so
nothing changes unless you set them:

```sh
# a second implementation as the verifier, the Go engine still sequencing
VERIFIER_ENGINE_IMAGE=someone/op-cartesi-rs:v0 docker compose up
```

The verifier is the low-risk slot for a new implementation: it sequences
nothing, learns the chain only from what the batcher posted to L1, and must
reach byte-identical blocks — so a divergence surfaces immediately, as a
refused payload, and nothing downstream depends on it.

An image standing in that slot must:

- run `engine.sh` on its PATH (the service's `command`), or ship its own
  entrypoint of that name;
- read its configuration from the environment the service sets —
  `MACHINE_REMOTE`, `MACHINE_SNAPSHOT`, `DATA_DIR`, `ENGINE_ADDR`,
  `HTTP_ADDR`, `JWT_SECRET_FILE`, and `CHAIN_FLAGS_FILE`, whose file the
  `genesis` step writes with the consensus parameters every node of the chain
  must agree on;
- serve the authenticated Engine API on `ENGINE_ADDR` and the public `eth_`
  and `cartesi_` surface on `HTTP_ADDR`;
- answer `eth_blockNumber` on the public port once it can serve, which is
  what the healthcheck asks;
- carry `curl`, since compose runs healthchecks inside the container.

The chain-flags file is currently a list of *Go command-line flags*, which a
second implementation cannot consume — turning it into a configuration
document is [BLOCKS-SPEC §16.1](../docs/BLOCKS-SPEC.md#16-known-underspecification).

The OP monorepo publishes **no binaries** — its releases carry source archives
only — so its images are the official way to get op-node, op-batcher and
op-proposer, and anvil and forge come from foundry's. They are pinned in
`compose.yaml`, and versioned independently upstream, so the tags do not match:

```
us-docker.pkg.dev/oplabs-tools-artifacts/images/op-node:v1.19.3
us-docker.pkg.dev/oplabs-tools-artifacts/images/op-batcher:v1.16.11
us-docker.pkg.dev/oplabs-tools-artifacts/images/op-proposer:v1.16.3
us-docker.pkg.dev/oplabs-tools-artifacts/images/op-deployer:v0.7.1
```

Because every piece is a container on one network, there is no host gateway
anywhere in the stack: services address each other by service name, on the same
port numbers `lib/env.ts` defaults to, and only the ports you might want to
reach from a laptop are published — to loopback.

The guest is [`demo`](../demo/README.md) — the routed guest of
[docs/EVM-COMPAT.md](../docs/EVM-COMPAT.md), TypeScript on `@cartesi/rollup`, its
ledger on the accounts drive. `cartesi build` stores the booted machine under
`.cartesi/image`; that directory is the chain's genesis state.

op-node then drives block production: every L2 block carries the L1-attributes
deposit it injects, that deposit is wrapped in an `EvmAdvance` envelope and fed
to the machine, and the machine's Merkle root becomes the block's state root.

`op-batcher` posts those blocks to L1 as calldata, which advances the safe
head, and a second node — its own machine, engine and op-node — rebuilds the
chain from that L1 data alone.

```sh
cast block-number --rpc-url http://127.0.0.1:8545
cast rpc cartesi_getOutputsRoot latest --rpc-url http://127.0.0.1:8545
cast rpc optimism_syncStatus --rpc-url http://127.0.0.1:9545

# the verifier, which sequences nothing, must agree block for block
cast block 10 --rpc-url http://127.0.0.1:8545 | grep -E 'hash|stateRoot'
cast block 10 --rpc-url http://127.0.0.1:8565 | grep -E 'hash|stateRoot'
```

### How the bring-up is organized

Compose does the sequencing, and it does it with the three notions this used to
carry code for: `depends_on` for order, a healthcheck for readiness, and a
container that exits for a step that finishes rather than runs.

| service | what it is |
|---|---|
| `l1` | anvil, not `--silent`: L1 blocks and transactions as they land |
| `l1-contracts` | `compose/l1-contracts.ts` → `deploy-l1.ts` (op-deployer). Exits |
| `genesis-machine` | a machine server for the step below, and only for it |
| `genesis` | `compose/genesis.ts`: anchors the rollup, writes `rollup.json`. Exits |
| `machine` | `cartesi-jsonrpc-machine`: the guest's console |
| `engine` | op-cartesi, the sequencer's engine, through `compose/engine.sh` |
| `op-node` `op-batcher` `op-proposer` | the OP tools, from their published images |
| `verifier-*` | the second node's machine, engine and op-node |
| `outputs` `guest` | `deploy-outputs.ts` and `guest-log.ts`; started by hand |

The readiness checks are the checks the old bring-up made from the outside,
made from within instead. op-cartesi binds its listeners only once the chain is
open and the machine has booted, so the engine's healthcheck — one
`eth_blockNumber` — is exact rather than approximate: an engine that answers is
an engine that can serve. The two steps that finish announce themselves by
exiting 0, and what waits for them says `condition:
service_completed_successfully`.

Two of the OP images carry no shell at all, which is why op-node and op-batcher
have no healthcheck; `restart: on-failure` covers the seconds between op-node's
container starting and its RPC answering.

Values still travel between the pieces through `devnet/*.env`, which
`lib/env.ts` reads back — that is how the engine, in a container of its own,
ends up on exactly the genesis the rollup config was generated with. Two of
them cross a container boundary as files that are read rather than merged:

- **`devnet/chain-flags`**, written by `compose/genesis.ts` and read by
  `compose/engine.sh`. The chain flags determine the L2 genesis block hash, so
  an engine that runs with different ones than `rollup.json` was generated with
  is a chain op-node rejects. In one process that would be `chainFlags()`
  called twice; in two containers it is this file — generated by the step that
  committed to them, run by the step that has to match.
- **`devnet/l1-addresses.env`**, sourced by `compose/proposer.sh`. Every other
  command line is fixed before anything starts, because everything op-node and
  op-batcher need is inside `rollup.json`. op-proposer takes its
  DisputeGameFactory as a flag, and that address does not exist until
  op-deployer has run.

`docker compose up` is a statement about what should be up rather than a run,
and running it again — to add the batcher to a sequencing stack, or after a
`stop` — does not build a new chain. The two steps that produce the chain ask
L1 whether their own output is still true: is the recorded OptimismPortal still
code, is the anchored block still that hash. When it is, they exit 0 without
repeating themselves. When it is not, which is what a restarted anvil looks
like, they redeploy and re-anchor — and the anchor step first clears what
described the chain being replaced (`rollup.json`, `outputs-addresses.env`,
`token.env`), because a new chain starts from nothing.

Everything the stack keeps is in the containers and two volumes, so
`docker compose down -v` is the whole teardown, including the machine servers
op-cartesi forks per block.

One thing outside the compose files exists because of them. A machine server in
a container of its own needed `machine.Remote.Fork` to rewrite the address a
fork reports: the server answers with the address its child bound, which is the
parent's own bind address on a fresh port — `0.0.0.0:42693` for a server
started with `--server-address=0.0.0.0:6300`. Dialing that verbatim means
dialing your own machine, which is right only when the server is on it. `Fork`
substitutes the host it reached the parent on, so `machine:6300` forks to
`machine:42693` (`machine/jsonrpc.go`, `TestForkEndpoint`).

### Watching the guest

The guest program inside the machine is visible two ways, and they show
different things:

- **`docker compose logs -f machine`** is the emulator's console — Linux
  booting, then anything the guest writes to stdout. The servers op-cartesi
  forks per block inherit these file descriptors, so their output lands in the
  same log.
- **`docker compose run --rm guest`** is the guest's account of the chain: the
  reports it emitted for
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
so the log can tell an app diagnostic from `eth_call` return data from revert
data — a revert is shown decoded, `Error("nonce too low")` rather than a
selector. Printable payloads are shown as text, everything else as hex.

It is an ordinary client script, so it also runs on the host against any node —
the verifier included:

```sh
L2_RPC=http://127.0.0.1:8565 bun devnet/guest-log.ts
```

### L1 contracts, and proposals

The `l1-contracts` service deploys the full OP Stack L1 suite with
`op-deployer`, and everything downstream waits for it — the rollup has to be
anchored at a block where the SystemConfig already exists. `op-proposer` then
runs against it, and `docker compose up op-batcher` is how you get the chain
without one.

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

Deploying `OptimismPortal` also makes OP's own withdrawal path work:
`proveWithdrawalTransaction` wants a storage proof of the
L2ToL1MessagePasser's `sentMessages` trie, and the shim maintains exactly
that trie as the header's `withdrawalsRoot`
([DESIGN.md §5](../docs/DESIGN.md)). Ether and ERC-20 withdrawals ride it —
see below. Cartesi vouchers remain for application outputs the portal has
no notion of.

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
with, and they are written into `devnet/*.env` by the services that produced
them — bind-mounted back out of the containers into the repository, which is
why a script on the host finds them with no configuration at all. (The
containers run as root, so on Linux those files come back owned by root; on
macOS, where docker maps ownership, they do not.)

| | |
|---|---|
| `../compose.yaml` | the stack: every service, and what waits for what |
| `compose/Dockerfile` `compose/engine.sh` `compose/proposer.sh` | what the containers are and what they run |
| `compose/l1-contracts.ts` `compose/genesis.ts` `compose/live.ts` | the two steps that produce the chain, and the question they ask first |
| `deploy-l1.ts` `deploy-outputs.ts` | the deploys themselves |
| `generate-config.ts` `start-shim.ts` | op-cartesi on its own, no chain around it |
| `guest-log.ts` | the `guest` service, and a tail for any node |
| `lib/env.ts` | all configuration — `devnet/env` |
| `lib/wallet.ts` | the chains and the viem clients — `devnet/wallet` |
| `lib/genesis.ts` `lib/opcartesi.ts` `lib/proc.ts` | anchoring, invoking the engine, running things |

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

What is left of the shell's job — spawning a program, failing loudly, removing
a file — is `lib/proc.ts`, and it is smaller than it has ever been: the waiting
and the teardown it used to carry are `depends_on`, a healthcheck and
`docker compose down`.

User overrides go in a `.env` at the repo root — `SENDER_KEY=…`, `L1_RPC=…`,
anything `lib/env.ts` reads. It is loaded from the repo root explicitly
rather than left to bun's cwd-relative auto-loading, so it applies wherever a
script is run from, and it sits below real environment variables. The
machine-written files (`l1-addresses.env`, `chain-genesis.env`,
`outputs-addresses.env`, `token.env`) stay separate and stronger — they are
deployment outputs, not preferences, and they are re-read rather than cached,
so a process that started before a deploy still sees what it wrote. `.env` is
gitignored; keys live there, not in command lines.

The scripts ([`scripts/`](../scripts/README.md)) and the guest speak the same standard: `withdraw.ts`
calls the message-passer predeploy through viem's `initiateWithdrawal`,
`withdraw-erc20.ts` the `L2StandardBridge`
predeploy, `balance.ts` reads the drive
and the ERC-20 façades, `contracts.ts` asks `cartesi_getContracts` what the
guest routes — every recorded contract with its ABI, every token façade with
its L1 token, straight off the machine's drives — and the guest they address
is the one `cartesi build` builds — the routed guest
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

This calls `OptimismPortal.depositTransaction` — the path a real user takes.
Without the deployed contract suite there is no portal, and `deposit.ts` says
so and stops. (An earlier version carried a contract-less fallback: a minimal
`TransactionDeposited` emitter installed with `anvil_setCode`, exploiting the
fact that derivation reads the log rather than the contract that produced it.
It was the last of the hand-packed hex, anvil-only, and the only thing a
contract-less devnet funded — retired along with the option of running one.)

### Withdrawals

Ether withdrawals are the stock OP flow, driven with viem's op-stack actions
exactly as [the withdrawal guide](https://viem.sh/op-stack/guides/withdrawals)
writes it:

```sh
bun scripts/withdraw.ts 0x00000000000000000000000000000000000a11ce 500000000000000000
```

`initiateWithdrawal` sends the L2 transaction to the message-passer
predeploy, which the routed guest serves: it burns the value and emits the
OP `Withdrawal` message whose hash enters the block's withdrawal trie. Then
`waitToProve` finds the dispute game covering the block,
`buildProveWithdrawal` fetches the storage proof with `eth_getProof`, and
`proveWithdrawal` / `finalizeWithdrawal` drive `OptimismPortal` — with one
devnet substitution: the guide's `waitToFinalize` measures the proof's age
against the caller's wall clock, and the devnet advances *anvil's* clock
past the week of delays instead, resolving the permissioned game by hand
along the way (`scripts/lib/portal.ts`). Nothing about op-node, op-batcher
or op-proposer is modified or aware of any of this — the root claim
op-proposer already publishes is what the proof runs against.

The voucher path remains for application outputs the portal has no notion
of. `deploy-outputs.ts` puts its L1 half in place — the validator that opens
an OP proposal's root claim, the executor that runs a proven output — and
registers the standard messenger pair with the guest (which the ERC-20
bridging below needs). It is the `outputs` service, the one step of the flow
that waits for you rather than for another process:
`docker compose run --rm outputs`. `execute-voucher.ts` and
`scripts/lib/voucher.ts` then prove a voucher against the outputs root,
which lives one storage slot inside the same withdrawal trie. The proof is
built against the *proposed* block, not the block that emitted the voucher:
the outputs tree is cumulative over the chain, so a withdrawal has to stay
provable as the tree grows past it, and `cartesi_getOutputProof` takes both
the output index and the block to prove it as of.

### Tokens

ERC-20 goes through `L1StandardBridge`, the standard OP path (DESIGN §6):

```sh
bun scripts/deposit-erc20.ts 1000000000000000000      # deploys a test token first time
bun scripts/balance.ts 0x70997970C51812dc3A010C7d01b50e0d17dc79C8 $TEST_TOKEN_ADDRESS
bun scripts/withdraw-erc20.ts 0x70997970C51812dc3A010C7d01b50e0d17dc79C8 500000000000000000
```

The bridge escrows the tokens in itself and sends `finalizeBridgeERC20`
through the messengers; the guest's `L2StandardBridge` predeploy credits the
ledger under the token's derived façade address. The withdrawal is
`bridgeERC20To` on that same predeploy: the guest debits the ledger and sends
the finalize message back through its `L2CrossDomainMessenger`, riding an OP
`Withdrawal` the portal proves against the withdrawal trie — after which the
L1 bridge releases its own escrow. No voucher, no executor, no custom
contract on the path. This repo ships no Cartesi-style portals;
the guest still enforces escrow exclusivity — one fungible balance backed
by two escrows would let a deposit into one drain the other — so a
deployment that registers its own ERC-20 portal cannot also register the
messenger.

The guest credits a relayed deposit only if the deposit's `from` is the
aliased `L1CrossDomainMessenger` it was told about. It learns that address
from
`GUEST_OWNER`, an address baked into the snapshot and therefore into the
genesis state root, whose deposits it treats as configuration. That
indirection exists because the L1 contracts do not exist when the snapshot
is built, and trusting any sender would let any contract mint claims
against tokens the bridge really holds.

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

Two operational notes, both easy to trip over by hand — and both settled by the
compose file:

- A machine server holds exactly one machine, and `machine.load` refuses to
  replace it. Config generation and the node each need their own server, which
  is why `genesis-machine` exists and why the genesis step shuts it down again
  when it is finished with it.
- op-node must not be started before the engine is serving, which is what the
  engine's healthcheck and op-node's `depends_on` say: op-cartesi answers on
  its RPC only once the chain is open.

## Persistence

`DATA_DIR` gives each node a store, and the chain survives a restart. It is a
path inside the containers, and each node's engine and machine server share a
volume at it — the engine keeps the blocks, the machine server writes the
checkpoints:

```sh
DATA_DIR=/data docker compose up
docker compose exec engine ls /data/checkpoints
```

Blocks and the machine's emissions go into a pebble database; the machine
itself is checkpointed whole every `CHECKPOINT_INTERVAL` blocks and at every
finalized block. A checkpoint is around 400 MB with nothing deduplicating
between them, so `-checkpoint-retention` (default 3) is the disk budget.

The write comes off a fork of the machine the chain already keeps, so it does
not stall block production — measured at exactly 2.00 s per block across a
checkpoint. Restarting loads the newest checkpoint and re-executes the blocks
after it, checking each against the state root it was stored with.

Each node needs its own store, because a pebble database is held by one
process: the sequencer and the verifier have a volume each, which is the same
rule expressed as compose says it.

## The snapshot is stored already booted

`cartesi build` stores the machine where `cartesi-machine --store` leaves it:
booted, and parked at its first input yield. This is how Cartesi Rollups
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

## op-cartesi on its own

Not a devnet — no L1, no contracts, no op-node — but the engine alone is
sometimes what you want to poke at. `start-shim.ts` runs it against the
in-memory mock machine, writes a JWT secret to `devnet/jwt.hex`, starts the
Engine API on `:8551` and the public `eth_*` port on `:8545`, and prints the
genesis block hash:

```sh
bun install            # this one does need the workspace
./devnet/start-shim.ts
```

`generate-config.ts` is the other half, generating a `rollup.json` against a
machine server you point `MACHINE_REMOTE` at. The chain flags it uses must
match the ones the engine runs with, which is why both come from
`chainFlags()` rather than from two lists — and why, in the devnet, the genesis
step writes them to a file the engine is started from.

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
