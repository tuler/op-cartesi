# @cartesi/routed-guest

The guest runtime of [docs/EVM-COMPAT.md](../docs/EVM-COMPAT.md): the router
with its admission rules and three-outcome model, the journaled ledger over
the accounts drive, the built-in handlers (ERC-20 façades, bridge, config,
registry, `L1Block`, portal receiver), and the **application API** — a
contract is an address, an ABI, and callbacks:

```ts
import { Guest, Revert } from "@cartesi/routed-guest";
import { parseAbi } from "viem";

const guest = await Guest.boot({ chainId: 901n, owner });

const state = guest.store(8); // journaled: rolls back with REVERT/REJECT

await guest.contract({
    address: "0xc0de000000000000000000000000000000000001",
    abi: parseAbi([
        "function transfer(address to, uint256 value)",
        "function count() view returns (uint256)",
    ]),
    transactions: {
        // ABI parameters are the callback's own, fully typed; the
        // environment rides last (declare fewer parameters to ignore it).
        transfer: async (to, value, { tx, ledger, out }) => {
            /* throw new Revert("reason") to revert with data; any other
               exception reverts with its message — the EVM's own rule */
        },
    },
    views: {
        count: async () => 0n, // plain values; the library ABI-encodes
    },
});

await guest.run();
```

Dispatch is ABI-driven: calldata decodes against the registered ABI and the
function's `stateMutability` decides which side may run it. Every registered
contract — built-ins included — is recorded in the **ABI drive**
([docs/ABI-DRIVE-SPEC.md](../docs/ABI-DRIVE-SPEC.md)) at boot, before the
first yield, so a stored snapshot describes its own interface surface: the
accounts drive names the tokens, the ABI drive names the contracts.

Registration is a boot-time act (between `boot()` and `run()`): the manifest
is fixed at snapshot build — there is no dynamic deploy.
