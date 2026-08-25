# @op-cartesi/app

The application runtime of [docs/EVM-COMPAT.md](../docs/EVM-COMPAT.md): the
router with its admission rules and outcome model, the journaled ledger over
the accounts drive, the built-in handlers (ERC-20 façades, bridge, config,
registry, `L1Block`, portal receiver), and the **application API** — a
contract is an address, an ABI, and callbacks:

```ts
import { Application, Fail, Revert } from "@op-cartesi/app";
import { parseAbi } from "viem";

const app = await Application.boot({ chainId: 901n, owner });

let count = 0n; // application state is a variable — see below

await app.contract({
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

await app.run();
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

## The per-block tick

A contract may also declare `onBlock`, which runs **once at the head of every
block, whether or not the block carries any transactions** — the hook for a
game clock, an auction that expires, an interest accrual: logic that has to
advance on its own rather than waiting to be called.

```ts
await app.contract({
    address,
    abi,
    transactions: { ... },
    onBlock: async ({ block, l1, ledger, out }) => {
        // block: this L2 block's number, timestamp and prevRandao
        // l1: the L1 attributes this block just delivered
    },
});
```

Nothing in the chain had to change to make this possible, because the input
was already arriving. Every OP Stack block opens with op-node's L1-attributes
deposit addressed to `L1Block` (`0x4200…0015`), and the sequencer runs it
through the machine before any mempool transaction — so a block with zero user
transactions is still a block in which the guest runs. The tick is what the
router does once that deposit has landed and its attributes are stored.

What follows from that, and is worth knowing before you write one:

- **It is the *start* of the block, not the end.** The attributes deposit is
  transaction index 0, and no input follows the block's last transaction —
  the block simply seals. So a tick sees this block's header context with the
  previous block's transactions applied. For a game that is the natural order:
  advance the clock, then process this block's moves.
- **It fires once per block, on the chain's own deposit only.** The sender is
  the authority — op-node's canonical depositor — so a user transaction to
  `L1Block` carrying attributes-shaped calldata buys no extra tick.
- **Use `block.timestamp` for clocks**, not `l1.timestamp`: the first is this
  L2 block's, the second only moves once per L1 block.
- **Failure works exactly as it does in a transaction callback** — `Revert`
  before your first private write, `Fail` after — and nothing thrown from a
  tick can reject the input or disturb another contract's tick. Each tick gets
  its own journal mark and its own output buffer, and the block's L1
  attributes are stored before any tick runs, so an application bug can never
  shadow the chain's own L1 context.
- **Nobody pays for it, which is exactly why it must stay bounded.** A tick
  has no sender, so there is no fee and no nonce; its cycles are billed to the
  attributes deposit and count against the block gas limit that admits
  everyone else's transactions. And this input is mandatory in a way no other
  input is: the sequencer aborts the block build if a deposit fails *hard*, so
  a tick that loops forever or blows the per-input cycle limit does not cost a
  sender a transaction — it stalls block production. Exceptions are contained;
  runaway cost is not, and cannot be from inside the guest.
- **It is not an ABI entry.** A tick has no selector and no caller, so
  recording one would misdescribe the contract's interface to the ABI drive
  and to the tooling that reads it.

Dispatch is ABI-driven: calldata decodes against the registered ABI and the
function's `stateMutability` decides which side may run it. Every registered
contract — built-ins included — is recorded in the **ABI drive**
([docs/ABI-DRIVE-SPEC.md](../docs/ABI-DRIVE-SPEC.md)) at boot, before the
first yield, so a stored snapshot describes its own interface surface: the
accounts drive names the tokens, the ABI drive names the contracts.

Registration is a boot-time act (between `boot()` and `run()`): the manifest
is fixed at snapshot build — there is no dynamic deploy.
