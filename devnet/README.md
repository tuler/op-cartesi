# Devnet

Running op-cartesi as a real OP Stack L2 needs four pieces:

1. **An L1 chain** — any post-Dencun Ethereum devnet (`anvil`, `geth --dev`, or kurtosis).
2. **L1 contracts** — `OptimismPortal`, `SystemConfig`, `DisputeGameFactory`/`L2OutputOracle`, and the batch inbox address. These are the stock OP Stack contracts, deployed with [`op-deployer`](https://docs.optimism.io/builders/chain-operators/tools/op-deployer). op-cartesi does not deploy or modify them.
3. **op-cartesi** — the execution engine, serving the Engine API.
4. **op-node** — in sequencer mode, pointed at the L1 and at op-cartesi.

Steps 1 and 2 are ordinary OP Stack chain-bringup and are not automated here: `op-deployer` needs a funded L1 deployer key and produces the addresses that go into `rollup.json`. What *is* automated is step 3 and the configuration that ties it to step 4, which is where op-cartesi differs from an op-geth chain.

## Quick start: the whole stack on anvil

`start-devnet.sh` brings up anvil as L1, a Cartesi Machine, op-cartesi as the
execution engine, and op-node sequencing on top:

```sh
./devnet/build-snapshot.sh     # once, needs the guest images (see that script)
./devnet/start-devnet.sh
```

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

No L1 contracts are deployed. `op-proposer` therefore has nothing to propose
to, and withdrawals have nothing to prove against; both need the OP contract
suite from `op-deployer`.

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

Since there is no `OptimismPortal` here, `deposit.sh` installs a minimal
emitter at the rollup config's deposit contract address with `anvil_setCode`.
Derivation reads the `TransactionDeposited` log rather than the contract that
produced it, so as far as the chain is concerned the two are the same.

Two operational notes the script encodes, both easy to trip over by hand:

- A machine server holds exactly one machine, and `machine.load` refuses to
  replace it. Config generation and the node each need their own server.
- Linux boots inside the machine before the engine answers, which takes tens of
  seconds. op-node must not be started before the engine logs
  `chain initialized`.

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
