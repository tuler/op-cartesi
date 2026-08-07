# @cartesi/abis

Reference implementation of the ABI drive
([docs/ABI-DRIVE-SPEC.md](../../docs/ABI-DRIVE-SPEC.md)): a raw flash drive
recording which addresses a routed guest serves as contracts, and the
standard JSON ABI of each — written once at first boot, before the first
yield, so the record is genesis state under the genesis root.

With the accounts drive naming the tokens, the ABI drive naming the
contracts makes a stored machine self-describing: the chain, or anyone
holding the snapshot, learns the machine's interface surface by reading
drive bytes — no knowledge of the application's implementation required.

```ts
import { AbiDrive, KIND_APP } from "@cartesi/abis";

const drive = await AbiDrive.openOrFormat(store);
await drive.register(address, KIND_APP, JSON.stringify(abi));
const entries = await drive.entries(); // { address, kind, abi }[]
```

Works over any byte store (`@cartesi/accounts`' `Store`): a device file in
the guest, memory in tests, machine reads on the host.
