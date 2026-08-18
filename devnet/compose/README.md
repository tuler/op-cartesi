# The devnet under docker compose

An alternative bring-up, not an alternative chain. The stack is the same one
[`start-devnet.ts`](../start-devnet.ts) puts up under mprocs — anvil, the OP
Stack L1 suite, a Cartesi Machine, op-cartesi, op-node, a batcher, a proposer,
and a second node verifying from L1 alone — and the same scripts deploy and
configure it. What differs is who does the waiting.

```sh
./scripts/build-snapshot.ts    # once — `cartesi build` of demo/
docker compose up
```

Nothing else is needed on the host: no bun, no go, no foundry, no op-deployer.
The two images this builds carry them.

## What compose replaces

`devnet/lib/proc.ts` exists because mprocs starts every pane at once and has no
notion of dependencies, so each process waits for what it needs: a port that
answers, a marker file under `devnet/.state`, a pid record so a stack outliving
its terminal can still be stopped. Compose has all three notions built in, and
the compose file is what they turn into:

| the mprocs bring-up | here |
|---|---|
| `waitForPort(port, …)` | `healthcheck` + `depends_on: {condition: service_healthy}` |
| `markReady` / `waitReady` in `devnet/.state` | a container that exits 0 + `condition: service_completed_successfully` |
| `requireFreePorts`, the pid records, `stop-devnet.ts` | `docker compose down` |
| `requireCommands`, `requireOpTools` | the images |
| the generated mprocs config, `WITH_*` | naming the services you want |
| a pane | a service, and `docker compose logs -f <service>` |

The readiness checks are the same checks, made from the other side. op-cartesi
binds its listeners only once the chain is open and the machine has booted, so
the engine's healthcheck — one `eth_blockNumber` — is exact rather than
approximate, exactly as the port wait was.

The two steps that finish rather than run are containers that exit:
`l1-contracts` (op-deployer) and `genesis` (the rollup config). Everything
downstream declares `service_completed_successfully` on them, which is the
marker file without the file.

## The services

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

The ports are the ones `devnet/lib/env.ts` defaults to, and the files the
deploys write land in `devnet/` through a bind mount. Between them, everything
that talks to a running devnet works against this one with no configuration:

```sh
cast block-number --rpc-url http://127.0.0.1:8545
bun scripts/deposit.ts 0x00000000000000000000000000000000000a11ce 1000000000000000000
```

Two services do not start with the stack — the same two that are a step you
take rather than a process that runs:

```sh
docker compose run --rm outputs    # the outputs suite, once a proposal exists
docker compose run --rm guest      # what the guest says about each transaction
```

And naming a service brings up that service and what it depends on, which is
what the `WITH_*` flags were for:

```sh
docker compose up op-node        # sequencer only: no batcher, no proposer, no verifier
docker compose up op-batcher     # ...and the batcher
```

`WITH_CONTRACTS=0` has no equivalent: the compose file describes one topology,
and in it the rollup is always anchored at a block where the SystemConfig
exists.

## Two images, and everything else as published

```
node    the emulator's own image + the op-cartesi binary + curl
tools   bun + the repo + op-deployer + forge
```

`node` runs the machine servers and the engines — one image for both, because
an engine is only meaningful next to a machine server and the emulator's image
is where a matching emulator comes from. `tools` runs the repo's own TypeScript
deploy steps unchanged: `deploy-l1.ts`, `compose/genesis.ts`,
`deploy-outputs.ts`, `guest-log.ts` are the same files the mprocs panes run.
curl is in `node` because a healthcheck runs inside the container it checks, so
the container has to carry the client — which is also why op-node and
op-batcher have no healthcheck: their published images carry no shell at all.
`restart: on-failure` covers the seconds between op-node's container starting
and its RPC answering.

Everything else is an image someone else publishes, used as it comes: anvil
from foundry, op-node, op-batcher and op-proposer from the OP monorepo's
registry, pinned by the same tags `lib/env.ts` pins.

## The two things that travel between containers

Under mprocs the consensus-relevant values travel through `devnet/*.env`, which
`lib/env.ts` re-reads. Here two of them cross a container boundary as files:

- **`devnet/chain-flags`**, written by `compose/genesis.ts` and read by
  `compose/engine.sh`. The chain flags determine the L2 genesis block hash, so
  an engine that runs with different ones than `rollup.json` was generated with
  is a chain op-node rejects. In one process that is `chainFlags()` called
  twice; in two containers it is this file — generated by the step that
  committed to them, run by the step that has to match.
- **`devnet/l1-addresses.env`**, sourced by `compose/proposer.sh`. Every other
  command line is fixed before anything starts, because everything op-node and
  op-batcher need is inside `rollup.json`. op-proposer takes its
  DisputeGameFactory as a flag, and that address does not exist until
  op-deployer has run.

## One change outside the compose files

A machine server in a container of its own needed `machine.Remote.Fork` to
rewrite the address a fork reports. The server answers with the address its
child bound, which is the parent's own bind address on a fresh port:
`0.0.0.0:42693` for a server started with `--server-address=0.0.0.0:6300`.
Dialing that verbatim means dialing your own machine — right when the server is
on it, wrong when it is a container away. `Fork` now substitutes the host it
reached the parent on, so `machine:6300` forks to `machine:42693`
(`machine/jsonrpc.go`, `TestForkEndpoint`). Nothing else about the engine, the
guest or the chain differs between the two bring-ups.

## Operating it

```sh
docker compose logs -f machine        # the emulator's console: Linux, then the guest
docker compose logs -f engine op-node # or several at once
docker compose restart op-node        # one piece, like `r` on a pane
docker compose down -v                # stop everything and forget it
```

A run starts from nothing, and here that is your `down -v` rather than
something the bring-up does for you: `docker compose up` a second time over a
running anvil redeploys the L1 suite and re-anchors the rollup under an engine
that is already serving the old one. `down -v` between runs; the `-v` is the
chain stores.

Persistence works the way it does under mprocs, with one wrinkle: the engine
keeps the blocks and the machine server writes the checkpoints, so a node's
store is a volume shared by its two containers. `DATA_DIR` is a path inside
them, and each node has a volume of its own because a pebble database is held
by one process.

```sh
DATA_DIR=/data docker compose up
```

Bind mounts are how the deploys' output reaches the repo, so on Linux the files
under `devnet/` come back owned by root. On macOS, where docker maps ownership,
they do not.
