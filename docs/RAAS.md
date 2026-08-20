# Rollup-as-a-Service: a chain from a snapshot

A user hands over a stored Cartesi Machine and a page of parameters. Some
minutes later there is a chain on Sepolia or on Ethereum mainnet: an RPC
endpoint, a bridge, a batcher posting to L1, a proposer creating games, and a
verifier anyone can run to check the whole thing from L1 data alone. That is the
product. This document is about what stands between the repository as it is and
that sentence being true.

The devnet already performs the sequence. `docker compose up` deploys the
OP Stack L1 suite with op-deployer, boots a machine on the snapshot, derives the
genesis block from its root hash, writes the `rollup.json` op-node derives from,
and starts five services plus an independent verifier that reaches
byte-identical blocks. Every step of a chain launch is in there somewhere. What
is *not* in there is anything that survives being run twice, by two tenants,
against an L1 that reorgs and charges real money — because the devnet was built
to demonstrate a protocol, not to operate a fleet.

So this is mostly a document about the difference between a demonstration and a
service, with one genuinely new component in the middle of it: something has to
accept a stranger's machine image and decide whether it is fit to be a chain's
genesis.

Two things are assumed throughout and stated plainly in §9: this chain has no
fault proofs, and its inputs are free. Neither is a reason not to build a
service. Both are reasons the mainnet tier looks different from the testnet one.

---

## 1. What a chain actually is

Before enumerating what to build, it is worth being precise about what a launch
produces, because the list is shorter than it looks and the constraints between
its items are what most of the work is about.

1. **A stored Cartesi Machine**, booted and parked at its first input yield. Its
   Merkle root hash *is* the L2 genesis state root — the node never boots a
   machine, it adopts one (`machine.Remote.CheckReady`, `chain.New`). Genesis is
   therefore reproducible in the strongest sense available: the same directory
   yields the same chain.
2. **The consensus parameters** — `chainFlags` in `cmd/op-cartesi/common.go`:
   chain id, genesis timestamp, gas limit, `MaxCyclesPerInput`, the Holocene
   EIP-1559 pair, and the checkpoint policy. The fork schedule is deliberately
   not among them; every fork through Isthmus is active from genesis and none is
   optional.
3. **An L1 deployment**: the OP Stack suite through op-deployer — `OptimismPortal`,
   `SystemConfig`, `DisputeGameFactory`, `L1StandardBridge`,
   `L1CrossDomainMessenger`, `AnchorStateRegistry` — plus this repository's two
   contracts, `OPOutputsMerkleRootValidator` and `OutputExecutor`.
4. **`rollup.json`**, which `op-cartesi genesis` computes from (1), (2) and (3)
   together.
5. **Five processes**: a `cartesi-jsonrpc-machine` server, the op-cartesi engine,
   op-node in sequencer mode, op-batcher, op-proposer — and a sixth that should
   never be optional, the verifier.
6. **Secrets**: the engine's JWT, and three L1 signing keys (sequencer, batcher,
   proposer).

The load-bearing fact is the coupling between (1), (2) and (4). The genesis
block hash is a function of the snapshot's root hash and the consensus
parameters; `rollup.json` commits to that hash; and op-node refuses to start
against an engine whose genesis disagrees with its rollup config. Today that
agreement is maintained by `chainFlags()` in `devnet/lib/opcartesi.ts` being
*the single copy* of a command line, from which both the `genesis` invocation
and the `run` invocation are built. That is a property of a devnet with one
chain in it. With two tenants it is a bug waiting for a maintenance window.

---

## 2. The chain spec, and the snapshot registry

**A chain spec.** One canonical document per chain, written once at creation and
read by both `op-cartesi genesis` and `op-cartesi run`, replacing a command
line that must be passed identically to two subcommands. The point is not ergonomics; it
is that a chain becomes a thing that can be stored, versioned, diffed and
*hashed*. Publishing the spec hash alongside the genesis hash is what lets a
third party demonstrate they are running the same chain rather than a chain that
looks like it — and running third-party verifiers is the entire trust story
until there are proofs.

The spec is also where the parameters that are *not* currently parameters have
to land: the L1 network and its contract addresses, the batch inbox, the
sequencing window, the block time. Today those live in `rollup.json` and in
environment files the devnet writes; a service needs one document that produces
both.

**A snapshot registry.** Content-addressed storage for stored machines, keyed by
digest, with the derived genesis root hash recorded next to the bytes. Two
things must be recorded that are easy to forget because the devnet gets them for
free:

- **The emulator version.** The repository pins machine-emulator 0.21.0 by
  probing a running server. A stored machine is not portable across emulator
  versions in the way a genesis file is portable across geth versions, so the
  version is part of the chain's identity, not part of its deployment.
- **The parameters baked into the image.** `CHAIN_ID` and `OWNER` are
  Dockerfile build arguments — defaulted in the Dockerfile, overridable from
  `cartesi.toml` — which `cartesi build` bakes into the machine environment. They are covered by the genesis state root
  like every other consensus parameter, and they are invisible from outside the
  snapshot. A registry that stores the bytes but not the build inputs stores
  something that cannot be rebuilt.

**Chain-id allocation.** Unglamorous and necessary: a registry, collision
avoidance against the public chain lists, and the eventual submission. A service
that lets two tenants pick 901 has shipped a support ticket.

---

## 3. Snapshot intake: the component with no precedent here

This is the piece that has no analogue anywhere in the repository, and it is the
one that makes this a service rather than a deployment script. A customer's
snapshot is untrusted input that becomes consensus. Admitting one means
answering, before any L1 transaction is sent, whether this machine can be a
chain.

The checks divide into three kinds.

**Can it run at all.** Load it on the pinned emulator. Confirm it is parked at a
first input yield rather than mid-boot or halted — `CheckReady` already does
exactly this and is the reason the node never boots a machine. Compute the root
hash and record it as the chain's genesis state root. Enforce limits on stored
size and load time, both of which are per-chain infrastructure costs (a
checkpoint of the devnet's machine is ~380 MiB on disk; a customer's may be
larger).

**Does it produce a usable chain.** This is the part that will surprise people.
Several `eth_*` methods do not execute anything — they read the guest's accounts
drive directly out of machine memory. `eth_getBalance` and
`eth_getTransactionCount` come straight off that drive, the mempool's nonce gate
reads it at the head block, and `cartesi_getContracts` reads the ABI drive
beside it. A snapshot without those drives still makes a *chain* — blocks build,
the root advances, batches post — but it makes a chain whose wallet surface
silently reports zero balances and zero nonces for everyone. That is a support
catastrophe discovered by the customer, in production, and it is entirely
preventable by checking for the drives at intake and failing loudly.

**Is it the customer's chain to configure.** The guest carries one baked-in
`OWNER` address, and it is the only party that can register the L1 messenger
after deployment (§4). If the owner baked into the snapshot is not an address
the customer controls, the chain is undeployable in a way that cannot be fixed
without rebuilding genesis. Intake must extract it and have the customer prove
control of it — a signature is enough.

Finally, a **smoke advance**: feed one synthetic input and confirm the machine
accepts or rejects it within `MaxCyclesPerInput` rather than halting or running
away. A guest that blows its cycle budget on its first input produces a chain
that rejects every transaction ever sent to it. Better to learn that at upload
than at block 1.

---

## 4. Deploying to an L1 that is not anvil

`devnet/deploy-l1.ts` is the seed of this component and roughly half of it
survives contact with a real network. What changes:

**The intent.** The devnet uses `--intent-type custom`, which deploys the
superchain contracts and every implementation along the way. That is correct for
an L1 op-deployer has never heard of, and wrong for Sepolia and mainnet, where
the standard intent resolves the shared OPCM and the published implementations
from a per-chain table. Using `custom` on mainnet would mean paying to deploy a
private copy of the OP Stack for every tenant and forfeiting the shared
audit and upgrade surface that is the main reason to be on the OP Stack at all.

**The `anvil_setCode` Multicall3 placement** drops out; Multicall3 is canonical
on real networks.

**Roles become real.** `fillIntent()` currently sets every role — the superchain
proxy admin owner, the guardian, the challenger, both proxy admin owners, the
`SystemConfig` owner, the unsafe block signer, the batcher, the proposer, and
four fee vault recipients — to one anvil private key. For a hosted mainnet chain
these have to be separated across Safes and KMS-held signers, and the document
that describes the service has to state, in the open, **who holds upgrade
rights**: the service operator or the customer. That is a product decision with
a security disclosure attached, and it is the question a serious customer asks
second (the first is about proofs).

**The application contracts.** `OutputExecutor` and
`OPOutputsMerkleRootValidator` are deployed by a forge script the devnet drives
with `OUTPUT_MATURITY_DELAY=0` and `OUTPUT_REQUIRE_RESOLVED=false`. Those
defaults are the honest posture for a chain nothing can dispute — a maturity
delay guarding a game nobody can challenge is theatre. On mainnet they are still
the *technically* honest values and they are nevertheless unacceptable as
product defaults, because a nonzero air gap is what gives a human the chance to
intervene when the trusted proposer is wrong. §9 returns to this. Add source
verification while we are here; an unverified bridge contract on mainnet is not
a serious offering.

**Anchoring has to survive reorgs.** `devnet/lib/genesis.ts` reads back the L1
block the deployment recorded, compares hashes, and dies with *"the L1 reorged;
redeploy"* if they differ. On anvil that is a sound assertion about an
impossible event. On Sepolia it is a Tuesday. The anchor has to wait for a
confirmation depth before the chain commits to it, and the pipeline has to be
able to resume rather than restart when it does not get one.

**Bootstrapping the guest.** After the L1 suite exists, the chain is still not
usable for ERC-20 bridging, because the guest does not know its counterparties.
[DESIGN §6](DESIGN.md) explains why this is unavoidable — the L1 contracts do not exist when
the snapshot that *is* genesis is built, so the circle is cut by baking in an
owner and letting it register the addresses as an input. Concretely: a deposit
from the owner, unaliased, carrying `registerMessenger`, answered by the guest
with a notice rather than a report because the registration is consensus state.
The service has to send this transaction, wait for the notice, and verify it.

Bundled with it is a choice the customer must make at creation time and can
never unmake: **which escrow model**. The standard bridge escrows in
`L1StandardBridge`; a Cartesi-style portal escrows in the application contract.
The guest speaks either and refuses both at once, because one fungible ledger
backed by two escrows is a cross-drain. That refusal is enforced in the guest,
in consensus state. A launch form that does not surface this choice will produce
chains that cannot bridge the way their operator assumed.

**A deployment record.** op-deployer's own state, the resulting addresses, every
transaction hash, per chain, durable, so a partially-completed launch can be
resumed rather than begun again.

---

## 5. Running the nodes

**Images and a version matrix.** op-cartesi, `cartesi-jsonrpc-machine`, op-node,
op-batcher, op-proposer — pinned, reproducible, and versioned as a *set*, since
the emulator version is part of the chain's identity and the OP tool versions
determine which Engine API methods are called. The repository has run against
op-node v1.19.3 and op-batcher v1.16.11; a service needs to know which chain is
on which set and to be able to move one without moving all.

**Orchestration.** One compose project is a laptop's stack: excellent for
watching a chain, unusable for supervising a hundred. Per-chain Helm releases or
the equivalent, with health probes and a restart policy, replace it — and the
translation is less than it once was, since `depends_on` conditions and
healthchecks are the shapes a scheduler already speaks. One specific trap
carries over: op-cartesi forks a machine server per block for snapshots, and
those forks reparent to init, out of reach of any process-tree walk. In compose
they are inside the machine server's container and go when it does; anywhere
else, teardown must be scoped by cgroup or process group, or every restart leaks
half a gigabyte of resident machine.

**Storage, sized from measurements rather than guesses.** [DESIGN §7](DESIGN.md) has the
numbers: a stored machine is ~532 MiB apparent and ~380 MiB on disk, takes about
1.8 seconds to write, and there is **no reflink or dedup between checkpoints** —
every one is a full copy. Restart time is one block re-execution per block since
the last checkpoint (~1.9 s each on the devnet). So checkpoint interval and
retention are a per-chain trade between disk spend and recovery time, and they
belong in the chain spec as operational parameters with a default the service
can justify rather than a constant.

**Replica bootstrap is a gap, not a configuration.** A new node replays from
genesis or from a checkpoint it already has; there is no snapshot sync. The
store is also single-writer, so a second node needs its own data directory —
this is why the devnet's verifier gets one. Adding a read replica to a chain
that has been running for a month therefore requires shipping it a checkpoint,
which nothing currently does. For a service that promises redundancy, this is
work, not a flag.

**The verifier is a product, not a test fixture.** The devnet's second node
rebuilds the chain from L1 data alone and reaches byte-identical blocks: same
hash, same machine root, same outputs commitment. Packaged as a one-command
image and pointed at a published chain spec, it is the strongest trust statement
available while there are no fault proofs — *don't trust us, re-derive it* — and
it costs almost nothing to ship because it already exists and already runs in
CI's spirit if not its letter.

---

## 6. The control plane

The part that makes it a service rather than a runbook. Nothing here is
Cartesi-specific, which is precisely why it should be built with the least
possible novelty.

- **API and data model**: chain, spec, snapshot, deployment, node, key,
  endpoint. A chain is the aggregate; everything else hangs off it.
- **A durable workflow engine** over the launch sequence, every step idempotent
  and resumable. The devnet's sequence is already a dependency graph — each
  process waits for what it needs rather than being started in order — and that
  shape is the right one; it just has to survive the process that is driving it
  going away, which marker files in a temp directory do not.
- **Secrets**: a JWT per chain (the devnet's `ensureJwt()` writes one to disk),
  three signing keys per chain, rotation, and a custody story that matches the
  answer given in §4 about upgrade rights.
- **Tenancy, quotas, billing.** The dominant recurring cost is L1 gas — the
  batcher posts continuously and the proposer posts games — and it is
  attributable per chain, so it should be metered per chain from the first day
  rather than discovered from an invoice.
- **A console** that shows a customer their chain: endpoints, addresses, head,
  safe head, last batch, last proposal, and the balances that will halt it.

---

## 7. The chain's public surface

**A gateway** in front of the sequencer's RPC: TLS, authentication, rate
limiting, load balancing across replicas. This is not the ordinary
defence-in-depth argument. There is no public L2 mempool, so the sequencer's RPC
is the *only* ingress to the chain; and inputs are free, so nothing downstream
of that endpoint charges an attacker for using it. Until there is a fee market
(§9), the gateway is the only thing standing between a chain and a stranger with
a loop.

**The RPC surface has real gaps for anything that is not a wallet.** The served
namespaces are `eth_`, `cartesi_`, `miner_` and `engine_`, and the `eth_` subset
is deliberately the methods op-node, op-batcher and ordinary wallets actually
call. There are no filter methods, no subscriptions, no log queries, no `debug_`
and no `txpool_`. `cast send` works; viem's withdrawal flow works; an indexer
does not, and neither does an explorer. The missing piece is a log index: the
receipts are already synthesized from the guest's emissions, and the guest's
`EvmLog` notices are already decoded into real logs with real topics, so
`eth_getLogs` is an indexing problem over data the node already produces rather
than a new semantic. Subscriptions are a separate, smaller piece of work on top.

**Then the things customers assume exist**: an explorer or indexer, a bridge UI,
and a faucet on testnet. An off-the-shelf explorer will want a much larger RPC
surface than this chain serves — including tracing methods that have no meaning
when the execution layer is a Linux machine rather than an EVM — so this is a
build-versus-adapt decision that should be made deliberately and early. The
Cartesi-native view (`cartesi_getTransactionEmissions`, reports, outputs, cycle
counts) is genuinely more informative than an EVM explorer's, and is unavailable
anywhere else.

**Metadata publication**: the chain spec, `rollup.json`, the genesis hash, the
L1 addresses, in a superchain-registry-shaped manifest at a stable URL. This is
what makes running a third-party verifier a `docker run` rather than a
correspondence.

---

## 8. Operations

**Metrics.** op-cartesi exposes none today. The ones that matter are the ones
that are specific to this execution layer: block build time, mcycles per input,
checkpoint duration, live machine fork count, mempool depth. Beside them go the
ordinary OP Stack ones the tools already export, and the two balances whose
exhaustion silently stops a chain — the batcher's and the proposer's.

**Alerting** on safe-head lag, proposal cadence, batch submission failures, and
those balances. A chain whose batcher ran out of ETH looks healthy from the
sequencer's RPC for a long time.

**Backup and restore** of the pebble store and the checkpoints, which given the
absence of dedup is a real storage bill and needs a retention policy of its own.

**A nondeterminism watchdog.** Run the verifier for every chain, permanently,
and alarm on any divergence between what it derives and what the sequencer
served. This is cheap — the verifier exists and the devnet proves it reaches
byte-identical blocks — and it is the only automated defence against a guest or
emulator nondeterminism bug, which is the failure mode most likely to be
discovered by a customer rather than by us. It doubles as the honest version of
the trust story in §5.

**How does a chain upgrade its guest?** The question every customer asks within
a month. Today a new snapshot is a new genesis state root, which is a new chain
— acceptable for a testnet, unacceptable for anything holding value. [DESIGN §9](DESIGN.md)
sketches the alternative: in-band deployment, where an input addressed to a
supervisor inside the guest carries the new code, so the artifact travels
through the same DA path as any other input and is covered by the same
commitments. The service does not have to settle this to launch, but it has to
*answer* it, because "you cannot" is an answer that loses mainnet customers and
"you can, by relaunching" is an answer that must be said out loud before someone
bridges into a chain.

---

## 9. Mainnet: what has to be true first

Everything above is engineering. This section is disclosure, and it is the part
of the document that should be written last and edited most.

**There are no fault proofs.** Proposals go into OP's permissioned game type and
nobody can challenge them, because no fault-proof VM can execute a Cartesi
Machine today. This is the chain's one remaining trust assumption and it is a
constructor argument rather than a hidden one. [DESIGN §8](DESIGN.md) sets out exactly what
closing it takes, and the honest summary is that it is not close: the three
requirements — an on-chain way to check an input, the outputs root living inside
the machine, and a settled definition of the state transition function — are
shared by every settlement track and none of them is currently met.

For a mainnet tier this means, concretely: an explicit Stage-0-style trust
disclosure published with the chain; a guardian that can pause; a real
`maturityDelay` on `OutputExecutor` so there is an air gap in which a human can
act; and `requireDefenderWins` set once there is a game whose resolution means
something. The first three are available today and should be mandatory on
mainnet regardless of the fourth.

**Inputs are free**, and this is the largest blocker that has nothing to do with
proofs. There is no fee market and no metering charged to anyone.
`MaxCyclesPerInput` bounds one input's execution; it does not bound a sender.
The per-transaction fee the guest debits is owner-settable and zero on the
devnet. The consequences
compound: a mainnet chain with free ingress is a DoS target, it cannot pay for
its own L1 costs, and it has no way to price the resource it actually consumes.
The neighbouring gaps in the README — gas as a constant rather than a
measurement, `eth_estimateGas` returning the cycle budget as an upper bound, a
nonce that is enforced but free to mint — are the same problem seen from three
sides, and they resolve together or not at all. A hosted mainnet offering needs
this work; a hosted testnet offering can ship without it behind a rate limiter.

**Blob DA is unexercised.** Batches are calldata only. On mainnet that is the
dominant recurring cost and the gap between calldata and blobs is roughly an
order of magnitude, so this is not a nicety.

**P2P is disabled**, so replicas can only follow the safe head, which lags by
the batcher's cadence. Any promise of low-latency read replicas needs either p2p
or an internal unsafe-head feed.

**Proof construction walks the chain.** `leavesThrough` is linear in chain
length. Fine while outputs are rare; not fine on a chain that has been running
for a year and whose users are proving withdrawals. It wants an index from
output index to block.

---

## 10. Build order

**Phase 0 — Sepolia, single tenant, operated by us.** The chain spec, the
snapshot registry and intake checks, and the deployment pipeline generalized off
anvil: standard intent, confirmation-depth anchoring, automated guest bootstrap,
a durable deployment record. Success is launching two different customers'
snapshots on Sepolia by hand, from spec files, and having a third party
re-derive both with the verifier image.

**Phase 1 — Sepolia, self-serve.** The control plane, tenancy, the RPC gateway,
observability and the nondeterminism watchdog, checkpoint shipping for replicas.
Success is a customer launching without us in the room.

**Phase 2 — mainnet prerequisites.** The fee market and metering; blob DA; the
air gap and guardian; key custody and role separation across Safes; `eth_getLogs`
and an explorer worth pointing at. Nothing here is optional and the fee market
is the long pole.

**Phase 3 — mainnet**, under a published trust model, with the settlement track
(roadmap steps 4–6) running in parallel. Proofs gate the *contents* of the
disclosure, not the launch — which is the same posture every OP Stack chain took
on its way to Stage 1, stated in the open rather than implied.
