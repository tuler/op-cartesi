# Vendored Optimism libraries

`MerkleTrie.sol`, `SecureMerkleTrie.sol`, `RLPReader.sol`, `RLPErrors.sol`
and `Bytes.sol` are copied verbatim from the OP Stack's `contracts-bedrock`
(`src/libraries/`, MIT license), with only the import paths rewritten to be
relative. They are here so `OPOutputsMerkleRootValidator` verifies the
outputs-root storage slot with the *same* trie verifier
`OptimismPortal.proveWithdrawalTransaction` uses — pinning the two consumers
of the withdrawal trie to one implementation.

Source: https://github.com/ethereum-optimism/optimism/tree/develop/packages/contracts-bedrock/src/libraries
