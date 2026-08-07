# @cartesi/routed-guest

The guest runtime of [docs/EVM-COMPAT.md](../docs/EVM-COMPAT.md): the router
with its admission rules and outcome model, the journaled ledger over the
accounts drive, the built-in handlers (ERC-20 façades, bridge, config,
registry, `L1Block`, portal receiver), and the **application API** — a
contract is an address, an ABI, and callbacks:

```ts
import { Fail, Guest, Revert } from "@cartesi/routed-guest";
import { parseAbi } from "viem";

const guest = await Guest.boot({ chainId: 901n, owner });

let count = 0n; // application state is a variable — see below

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
        count: () => count, // plain values; the library ABI-encodes
    },
});

await guest.run();
```

## State, and the two ways to fail

An application's state is **plain RAM that it owns**. It is machine state,
covered by the state root and as deterministic as anything else in the
machine, but the journal does not reach it: on REVERT the ledger rolls
back and your variables do not. (REJECT and `eth_call` need no help — the
sequencer runs each input, and every query, on a machine fork it throws
away.)

That gives one rule, and it is the whole programming model:

> A callback that has written nothing of its own throws **`Revert`**. One
> that has already written throws **`Fail`**.

`Revert` rolls the ledger back and drops the outputs. `Fail` keeps the
ledger and the outputs and reports the error alongside them, which is the
honest outcome once your own state has moved. Both charge the sender's
nonce and fee, and **neither can reject the input** — nothing an
application throws rolls the machine back, drive refusals included, so a
buggy contract costs its sender a transaction instead of giving them a
free retry.

Order a callback so every fallible step comes before its first write and
you stay in `Revert` territory, which is the safer half. That is easy here
in a way it is not in the EVM: one input is one handler is one function
body, with no cross-contract call that can revert underneath you.

Dispatch is ABI-driven: calldata decodes against the registered ABI and the
function's `stateMutability` decides which side may run it. Every registered
contract — built-ins included — is recorded in the **ABI drive**
([docs/ABI-DRIVE-SPEC.md](../docs/ABI-DRIVE-SPEC.md)) at boot, before the
first yield, so a stored snapshot describes its own interface surface: the
accounts drive names the tokens, the ABI drive names the contracts.

Registration is a boot-time act (between `boot()` and `run()`): the manifest
is fixed at snapshot build — there is no dynamic deploy.
