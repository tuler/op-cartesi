# contracts

The L1 side of this chain: what makes a Cartesi output executable against an OP
proposal, and what lets a Cartesi-style deposit reach the guest through
`OptimismPortal`. Ether withdrawals no longer need a contract here at all —
they finalize through stock `OptimismPortal.proveWithdrawalTransaction`
against the withdrawal trie the header commits (DESIGN §5); what remains is
the voucher path for everything the portal cannot execute.

`src/vendor/optimism/` carries the OP Stack's `SecureMerkleTrie` and its
supporting libraries, copied verbatim (MIT) with only import paths rewritten:
`OPOutputsMerkleRootValidator` opens the Cartesi outputs root out of the
withdrawal trie with the *same* verifier the portal runs, and
`test/PasserTrieVectors.t.sol` pins the node's Go trie to it with the shared
vectors in [`../conformance`](../conformance/README.md), which the node
generates and its own suite replays — so the roots and proofs this suite
judges are the bytes the node actually serves, not a copy of them.

A [Foundry](https://getfoundry.sh) project. Dependencies come from
[soldeer](https://soldeer.xyz) — the Cartesi Rollups contracts and OpenZeppelin
are pulled at pinned versions rather than copied in, so the proof libraries that
verify a voucher here are the ones a real Cartesi chain runs:

```sh
forge soldeer install   # populates dependencies/
forge test
```

The suite reads [`../conformance`](../conformance/README.md) — hence the
read-only `fs_permissions` entry in `foundry.toml`. Regenerating those files is
a Go command (`go test ./host/go/chain -run TestConformance -update`); this side only
consumes them.

`dependencies/` is generated and gitignored. `remappings.txt` is checked in and
`soldeer.remappings_generate` is off, so the import paths are a file you can
read rather than something regenerated behind you.

Cartesi Rollups is pinned to **3.0.0-alpha.6**. Two things follow from that:

- `cartesi-machine-solidity-step` is declared here as well, from git. It is a
  transitive dependency — `CanonicalMachine` takes its constants from the
  emulator's own Solidity port — and soldeer does not resolve a dependency's
  dependencies.
- 3.0 removed the on-chain tree *builder* (`LibMerkle32`) and kept only the
  verifier (`LibOutputValidityProof` over `LibBinaryMerkleTree`), which is the
  right split: a contract never builds the outputs tree, it only checks a proof
  against a root. The builder now lives in the node (`host/go/chain/outputtree.go`),
  with a copy in `test/OutputTree.sol` for the tests. Since the two share no
  code, `test/OutputTree.t.sol` checks both against
  `../conformance/commitments/outputs-tree.json` — the same outputs, the same
  root, and the proofs the node serves run through Cartesi's own verifier. A
  disagreement would otherwise surface as a withdrawal that cannot be
  executed.

The verification itself is unchanged between 2.2 and 3.0: same
`OutputValidityProof` struct, same height-63 zero-padded keccak tree, same
`merkleRootAfterReplacement` semantics. What 3.0 adds to the surface this repo
implements is `getLastFinalizedMachineMerkleRoot` on
`IOutputsMerkleRootValidator`, which costs nothing to answer here — OP's output
root commits to the machine root in the same preimage as the outputs root,
because on this chain the L2 state root *is* the machine root.

## What is here

| | |
|---|---|
| `src/OPOutputsMerkleRootValidator.sol` | Opens an OP proposal's root claim and reports the Cartesi outputs root inside it. Implements Cartesi's `IOutputsMerkleRootValidator`, which is the one question an `Application` asks before executing an output. |
| `src/OutputExecutor.sol` | A reduced stand-in for Cartesi's `Application`: proves an output against an accepted root with Cartesi's own `LibOutputValidityProof` and runs it. Also the contract that holds the bridged assets. |
| `src/interfaces/` | The one OP surface used: `IDisputeGame`/`IDisputeGameFactory` (the proposals). An interface rather than a dependency on the monorepo, which is not consumable as a library. |

Nothing here forks or modifies an OP contract, and nothing here bridges:
ether and ERC-20 go through the stock `OptimismPortal` and
`L1StandardBridge` (DESIGN §5–§6). The Cartesi-style portals this
directory used to carry were removed once the standard paths worked — the
guest still speaks their deposit protocol for anyone who deploys their own.

## Deploying

`../devnet/deploy-outputs.ts` runs `script/DeployOutputs.s.sol` against the
devnet's L1, records the addresses in `devnet/outputs-addresses.env`, funds the
executor, and registers both portals with the guest.

The validator and the executor point at each other, so the executor's address is
predicted from the deployer's next nonce and passed to the validator's
constructor — one deployment, no setter, and no window where the validator will
answer for the wrong application.

## The trust assumption, stated once

`OPOutputsMerkleRootValidator` takes `maturityDelay` and `requireDefenderWins`
as constructor arguments. The devnet sets them to zero and false, which is
correct for a chain whose proposals are made by a permissioned proposer into
game type 1 and never disputed — there is no fault proof VM that can execute a
Cartesi Machine. A chain with a real proof system sets both, and nothing else
about these contracts changes.
