# lib

What the host and the guest both need, in whatever language each side is
written in. Every library here is **pure** in its language — no cross-language
bindings — and usable on both sides of the machine boundary.

| | Languages | |
|---|---|---|
| [`accounts-drive/`](accounts-drive) | Go, Rust, C, Lua, JS, Python | the guest's account state, read by the host out of machine memory ([spec](../docs/ACCOUNTS-DRIVE-SPEC.md)) |
| [`abi-drive/`](abi-drive) | Go, JS | which addresses the guest routes, and what ABI each speaks ([spec](../docs/ABI-DRIVE-SPEC.md)) |
| [`evm-compat/`](evm-compat) | JS | the wire vocabulary of [EVM-COMPAT](../docs/EVM-COMPAT.md): addresses, transaction admission, `EvmCall`, `EvmLog`, report tags |

The pattern is the same in each: one directory per language, a shared
specification, and shared vectors both sides read rather than constants copied
between them —
[`accounts-drive/testdata/golden.json`](accounts-drive/testdata/golden.json)
for the drives, [`conformance/`](../conformance/README.md) for everything the
chain commits to. A seventh accounts-drive implementation is a directory and a
test that replays the golden file.
