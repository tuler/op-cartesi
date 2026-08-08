# ledger-app: the devnet guest

The devnet's application, and deliberately small: it boots
[`@cartesi/routed-guest`](../routed-guest) with the devnet genesis
(`CHAIN_ID`, `OWNER` from the image environment) and registers the one
app-specific contract — a counter at `0xc0de…01`, the smallest honest
demonstration of the standard's fall-through: an ABI, callbacks, and state
that is simply a variable, because application RAM belongs to the
application (EVM-COMPAT §10a).

Everything standard lives in the workspace libraries:

- [`@cartesi/routed-guest`](../routed-guest) — the runtime: router,
  admission, the outcome model, the journaled ledger, the built-in
  handlers, and the application API (`guest.contract({ address, abi,
  transactions, views })`).
- [`@cartesi/evm-compat`](../evm-compat/js) — the wire-level vocabulary:
  addresses, transaction parsing, `EvmCall`/`EvmSimulate`, `EvmLog`, report
  tags, and the built-in ABIs. Host tooling (devnet scripts, tests) imports
  from here.
- [`@cartesi/abis`](../abi-drive/js) — the ABI drive
  ([docs/ABI-DRIVE-SPEC.md](../docs/ABI-DRIVE-SPEC.md)): the machine's own
  record of the contracts it serves. With the accounts drive
  ([docs/ACCOUNTS-DRIVE-SPEC.md](../docs/ACCOUNTS-DRIVE-SPEC.md)) naming the
  tokens, a stored snapshot describes its interface surface with no
  knowledge of the application.
- [`@cartesi/accounts`](../accounts-drive/js) — the accounts drive itself.

## Build and run

```sh
bun install        # once, at the repo root (bun workspace)
bun run typecheck
bun run build      # esbuild bundle (dist/index.js)

cartesi build      # @cartesi/cli 2.0 alpha → snapshot under .cartesi/image
```

The runtime's tests live with the runtime (`routed-guest`, `abi-drive/js`):
`bun run test` at the repo root runs them all.

`cartesi.toml` declares three drives: root, the accounts drive (raw, 1 MiB)
and the abi drive (raw, 256 KiB) — both unmounted, formatted by the guest at
first boot before the first yield, and handed to the app user with
`user = "dapp"` (cartesi-init runs the entrypoint unprivileged). It also
pins `machine.ram_image` to a machine-emulator-0.21.0-compatible kernel
installed on the host (the macOS homebrew path; adjust for your OS) until
the CLI's sdk ships one. Genesis parameters (`CHAIN_ID`, `OWNER`) are
Dockerfile `ENV`, which `cartesi build` passes into the machine; defaults
match `devnet/lib/env.ts`.

`@deroll/cmio` stays a direct dependency although only the library uses it:
it is the native addon `stage.mts` stages next to the bundle, and the
dependency keeps it resolvable from this package under bun's isolated
installs.

## Not here yet

The rest of EVM-COMPAT §11's shim half: `EvmLog` receipt decoding.
(Discovery is done — `eth_getCode` answers the `0xEFC751<kind>` markers so
wallets see routed addresses as contracts, and `cartesi_getContracts`
serves the full surface with ABIs; `bun devnet/contracts.ts` prints it.)
The CLI also auto-places flash drives — the shim discovers both drives by
label rather than assuming fixed starts.
