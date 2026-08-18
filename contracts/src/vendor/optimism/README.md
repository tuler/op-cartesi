# Vendored Optimism libraries

`MerkleTrie.sol`, `SecureMerkleTrie.sol`, `RLPReader.sol`, `RLPErrors.sol`,
`Bytes.sol`, `Encoding.sol`, `Hashing.sol`, `Types.sol` and `RLPWriter.sol`
are copied verbatim from the OP Stack's `contracts-bedrock`
(`src/libraries/`, MIT license), with only the import paths rewritten to be
relative. The trie libraries are here so `OPOutputsMerkleRootValidator`
verifies the outputs-root storage slot with the *same* trie verifier
`OptimismPortal.proveWithdrawalTransaction` uses; the encoding/hashing
libraries are here so the guest's cross-domain message encodings
(`@op-cartesi/evm` crossdomain.ts) are pinned against the *same* code
`L1CrossDomainMessenger` hashes and decodes with — see
`test/CrossDomainVectors.t.sol`. In both cases the point is one
implementation for the two consumers, not a reimplementation to drift.

Source: https://github.com/ethereum-optimism/optimism/tree/develop/packages/contracts-bedrock/src/libraries
