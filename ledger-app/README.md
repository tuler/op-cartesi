# ledger-app: the routed guest, prototyped

The guest half of **[docs/EVM-COMPAT.md](../docs/EVM-COMPAT.md)** in
TypeScript: a router that dispatches every transaction and every call on its
`to` address to native handlers, over [`@deroll/cmio`](https://github.com/tuler/deroll)
(the Node.js binding for libcmt) with viem doing the Ethereum heavy lifting —
transaction parsing (legacy, EIP-2930, EIP-1559, OP deposits), sender
recovery, ABI encode/decode.

What works, exercised by the vitest suite (`bun run test`, host-side, no
machine needed):

- **Enforcement in the router**: signature recovery, the EIP-2 low-s rule,
  the chain-id pin, nonce equality, fee-and-value cover — uniformly, before
  any dispatch. Typed transactions are first-class; `--legacy` is dead.
- **The three-outcome model**: ACCEPT / REVERT / REJECT, with a byte-level
  write journal over the accounts drive so a revert rolls back the handler
  and the value transfer while still consuming nonce and fee — failed
  transactions are not free. Charged under the fee schedule the transaction
  entered under.
- **The built-in family**: native ether transfers; the ERC-20 façade over
  the accounts drive at derived addresses (`balanceOf`, `transfer`,
  `totalSupply`, metadata) emitting real `Transfer` events; the bridge
  (`withdrawEther`/`withdrawERC20` → vouchers); the owner config contract
  (`setFee`, `registerPortal`, `registerToken`); the adopted `L1Block`
  predeploy fed by op-node's own attributes deposit; and the Cartesi-portal
  receiver at the application contract address.
- **Events as `EvmLog` notices**, provable in the outputs tree, decodable
  into standard receipt logs.
- **`EvmCall` / `EvmSimulate` over inspect**, with one-byte report framing
  (0x00 app, 0x01 return data, 0x02 revert data) so `eth_call` can return
  ABI words and surface `Error(string)` reverts.

The ledger is the **accounts drive** (docs/ACCOUNTS-DRIVE-SPEC.md), via the
TypeScript library at `accounts-drive/js` — a bun workspace dependency
(`@cartesi/accounts`), consumed as source. The snapshot build makes this
work by using the repo root as its docker context (`cartesi.toml` sets
`context = ".."`), so the workspace resolves inside the build too.

## On @deroll/cmio vs @deroll/app

This app deliberately sits on the low-level `Rollup` binding, not
`createApp`: the app layer's handler-chain model (every handler sniffs every
payload) is exactly the single-app pattern EVM-COMPAT replaces with address
routing, and its inspect handlers cannot reject — which is how `eth_call`
reverts travel. `@deroll/cmio` fits as-is: envelope decode, outputs with the
libcmt-maintained outputs-root accumulator, reports, and boolean
accept/reject for both request kinds. The router layer here is in effect the
seed of a `@deroll/op` package, if it grows up.

## Build and run

```sh
bun install        # once, at the repo root (bun workspace)
bun run test       # vitest, host-side, in-memory drive
bun run typecheck
bun run build      # esbuild bundle (dist/index.js)

cartesi build      # @cartesi/cli 2.0 alpha → snapshot under .cartesi/image
```

`cartesi.toml` declares the accounts drive (raw, 1 MiB, unmounted, formatted
by the guest at first boot, before the first yield). Genesis parameters
(`CHAIN_ID`, `OWNER`) are Dockerfile `ENV`, which `cartesi build` passes into
the machine; defaults match `devnet/env.sh`.

## Not here yet

The rest of EVM-COMPAT §11's shim half: `EvmLog` receipt decoding and
`eth_getCode` markers. (`eth_call` is done — the shim builds the `EvmCall`
envelope and maps the report framing to return data and code-3 revert
errors, so `readContract` works against the façades — and this app is the
devnet guest: `devnet/build-snapshot.sh` wraps `cartesi build` and the
chain boots from `.cartesi/image`.) The CLI also auto-places flash drives —
the shim must discover the accounts drive by label rather than assume the
spec's recommended 2^55 start.
