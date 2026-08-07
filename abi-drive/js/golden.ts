// Writes ../go/testdata/golden-v1.bin with this (reference) writer, so the
// Go reader's golden test proves both languages read the same bytes.
// Regenerate with `bun abi-drive/js/golden.ts` from the repo root after any
// deliberate format change — never casually: the format is consensus.

import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { MemStore } from "@cartesi/accounts";
import { AbiDrive, KIND_APP, KIND_SYSTEM } from "./src/index.ts";

const mem = new MemStore(262144);
const drive = await AbiDrive.format(mem);
await drive.register(
    "0xC751000000000000000000000000000000000001",
    KIND_SYSTEM,
    JSON.stringify([
        {
            type: "function",
            name: "withdrawEther",
            stateMutability: "payable",
            inputs: [{ name: "to", type: "address" }],
            outputs: [],
        },
    ]),
);
await drive.register(
    "0xC0DE000000000000000000000000000000000001",
    KIND_APP,
    JSON.stringify([
        { type: "function", name: "increment", stateMutability: "nonpayable", inputs: [], outputs: [] },
        { type: "function", name: "count", stateMutability: "view", inputs: [], outputs: [{ name: "", type: "uint256" }] },
    ]),
);

const out = join(dirname(fileURLToPath(import.meta.url)), "..", "go", "testdata", "golden-v1.bin");
writeFileSync(out, mem.bytes);
console.log(`wrote ${mem.bytes.length} bytes to ${out}`);
