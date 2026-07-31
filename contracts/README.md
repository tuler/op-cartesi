# contracts

The L1 side of this chain: what makes a Cartesi output executable against an OP
proposal, and what lets a Cartesi-style deposit reach the guest through
`OptimismPortal`.

A [Foundry](https://getfoundry.sh) project. Dependencies come from
[soldeer](https://soldeer.xyz) — the Cartesi Rollups contracts and OpenZeppelin
are pulled at pinned versions rather than copied in, so the proof libraries that
verify a voucher here are the ones a real Cartesi chain runs:

```sh
forge soldeer install   # populates dependencies/
forge test
```

`dependencies/` is generated and gitignored. `remappings.txt` is checked in and
`soldeer.remappings_generate` is off, so the import paths are a file you can
read rather than something regenerated behind you.

## What is here

| | |
|---|---|
| `src/OPOutputsMerkleRootValidator.sol` | Opens an OP proposal's root claim and reports the Cartesi outputs root inside it. Implements Cartesi's `IOutputsMerkleRootValidator`, which is the one question an `Application` asks before executing an output. |
| `src/OutputExecutor.sol` | A reduced stand-in for Cartesi's `Application`: proves an output against an accepted root with Cartesi's own `LibOutputValidityProof` and runs it. Also the contract that holds the bridged assets. |
| `src/portals/OPERC20Portal.sol` | Cartesi's `ERC20Portal` with `inputBox.addInput` replaced by `OptimismPortal.depositTransaction`. |
| `src/portals/OPEtherPortal.sol` | The same for ether, escrowing in the application rather than OP's lockbox. |
| `src/interfaces/` | The two OP surfaces used: `IOptimismPortal` (the input transport) and `IDisputeGame`/`IDisputeGameFactory` (the proposals). Interfaces rather than a dependency on the monorepo, which is not consumable as a library. |

Nothing here forks or modifies an OP contract. The validator reads
`DisputeGameFactory` through its public interface, and the portals call
`OptimismPortal` the way any depositing contract would.

## Deploying

`../devnet/deploy-outputs.sh` runs `script/DeployOutputs.s.sol` against the
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
