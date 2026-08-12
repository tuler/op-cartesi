# Scripts

Everything you do *to* a devnet, as opposed to bringing one up. Putting the
stack on its feet is [`devnet/`](../devnet/README.md); this is the other half —
move ether and tokens across the bridge, ask the guest what it holds and what
it routes, send a raw L2 transaction — plus the one script that builds the
guest they all address.

```sh
bun install                    # once, at the repo root (bun workspace)
./scripts/build-snapshot.ts    # once — `cartesi build` of demo/
./devnet/start-devnet.ts       # in another terminal
```

| | |
|---|---|
| `deposit.ts <recipient> [wei]` | An L1 deposit through `OptimismPortal`, followed to the L2 transaction op-node derives from it |
| `withdraw.ts <recipient> <wei>` | Asks the guest to withdraw, then proves and executes the voucher on L1 |
| `deposit-erc20.ts [amount] [token]` | The same for ERC-20, through the Cartesi-style portal. Deploys a test token on first use |
| `withdraw-erc20.ts <recipient> <amount> [l1-token]` | The ERC-20 return trip |
| `execute-voucher.ts <L2 tx hash>` | Proves and executes the voucher a transaction emitted, on its own |
| `balance.ts <address> [l1-token]` | What the guest's ledger says — the accounts drive, or an ERC-20 façade |
| `contracts.ts` | `cartesi_getContracts`: every address the guest routes, with its ABI |
| `send-l2-tx.ts <to> [calldata] [value-wei]` | One signed L2 transaction, for payloads nothing else covers |
| `build-snapshot.ts` | `cartesi build` of [`demo/`](../demo/README.md) — the stored machine that is the chain's genesis state |
| `lib/voucher.ts` | The second half of every withdrawal: wait for a proposal, open its root claim, prove the output against it |
| `lib/l2.ts` | Sending an L2 transaction the way a wallet would — viem fills the nonce and the fees, and signs EIP-1559 |

Every file is directly executable (`./scripts/deposit.ts …`) and works from
any working directory.

## What these depend on

The devnet publishes the configuration and the clients; the scripts import
them and nothing goes back the other way:

```ts
import { config, paths, usage } from "devnet/env";      // ports, keys, addresses
import { l1Public, l1Wallet, l2Public } from "devnet/wallet";  // pre-extended viem clients
import { die, must } from "devnet/proc";                // spawning and failing
```

That direction is the point. The ports the devnet binds and the addresses its
deploys wrote are the devnet's own facts — recorded in `devnet/l1-addresses.env`,
`devnet/outputs-addresses.env` and friends by the panes that produced them,
and re-read on every call rather than cached, so a script started before a
deploy still sees what the deploy wrote.

Overrides go in a `.env` at the repo root (`SENDER_KEY=…`, `L1_RPC=…`) — see
[the devnet README](../devnet/README.md) for the precedence rules. Point
`L2_RPC` at the verifier to run any of the read-only scripts against it
instead of the sequencer.

## Where the mechanics are written down

The devnet README carries the explanations these scripts are the front end of:
[deposits](../devnet/README.md#deposits), [withdrawals](../devnet/README.md#withdrawals),
[tokens](../devnet/README.md#tokens), and why the ERC-20 path avoids
`L1StandardBridge`. The wire vocabulary the guest and these scripts share is
[docs/EVM-COMPAT.md](../docs/EVM-COMPAT.md); the ledger they read is
[docs/ACCOUNTS.md](../docs/ACCOUNTS.md).
