# Devnet

Running op-cartesi as a real OP Stack L2 needs four pieces:

1. **An L1 chain** — any post-Dencun Ethereum devnet (`anvil`, `geth --dev`, or kurtosis).
2. **L1 contracts** — `OptimismPortal`, `SystemConfig`, `DisputeGameFactory`/`L2OutputOracle`, and the batch inbox address. These are the stock OP Stack contracts, deployed with [`op-deployer`](https://docs.optimism.io/builders/chain-operators/tools/op-deployer). op-cartesi does not deploy or modify them.
3. **op-cartesi** — the execution engine, serving the Engine API.
4. **op-node** — in sequencer mode, pointed at the L1 and at op-cartesi.

Steps 1 and 2 are ordinary OP Stack chain-bringup and are not automated here: `op-deployer` needs a funded L1 deployer key and produces the addresses that go into `rollup.json`. What *is* automated is step 3 and the configuration that ties it to step 4, which is where op-cartesi differs from an op-geth chain.

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

op-cartesi implements the **V3** Engine API methods, which covers every fork from
Ecotone through Holocene. `generate-config.sh` therefore activates Regolith
through Holocene at genesis and leaves Isthmus and later unset.

Isthmus is not supported yet: it switches op-node to `engine_newPayloadV4` /
`engine_getPayloadV4` and adds the `withdrawalsRoot` header field. Setting
`isthmus_time` in `rollup.json` will make op-node call methods the shim does not
serve.

## Genesis hash consistency

The L2 genesis block hash is derived from the Cartesi Machine's root hash plus
the chain configuration. Two consequences:

- Changing the machine (a different guest program, a different snapshot) changes
  the genesis hash, and `rollup.json` must be regenerated.
- The chain flags passed to `genesis` and to `run` must be identical. If they
  differ, op-node will refuse to start, complaining that the engine's genesis
  block does not match the rollup config — which is the failure mode you want,
  rather than a chain that silently disagrees with its own configuration.
