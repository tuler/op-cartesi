# @cartesi/evm-compat

The wire-level half of [docs/EVM-COMPAT.md](../../docs/EVM-COMPAT.md),
shared by the guest runtime ([`@cartesi/routed-guest`](../../routed-guest))
and host tooling — pure viem, no drives, no machine:

- **addresses** — the 0xC751 system namespace, token-façade derivation
  (`l2TokenAddress`), L1→L2 aliasing.
- **tx** — input parsing (legacy, EIP-2930, EIP-1559, OP deposits) and
  sender recovery with the EIP-2 low-s rule.
- **evmcall** — `EvmCall`/`EvmSimulate` encode/decode and `Error(string)`
  revert encoding.
- **events** — `EvmLog` notices; `Transfer` helpers.
- **types** — contexts, outcomes, emissions, report tags.
- **abis** — the built-in contract ABIs (bridge, config, registry, ERC-20
  façade, `L1Block`), for anything that speaks to a routed guest.
