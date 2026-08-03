// The vendored accounts-drive library must stay byte-identical to the
// golden-tested original in accounts-drive/js/src. Vendoring exists only
// because the docker build context is this directory and the package is
// unpublished; this test is the drift guard. Runs in-repo (where the sibling
// path exists); skips in any context that ships ledger-app alone.

import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const upstreamDir = fileURLToPath(new URL("../../accounts-drive/js/src/", import.meta.url));
const vendoredDir = fileURLToPath(new URL("../src/accounts/", import.meta.url));

const FILES = ["index.ts", "drive.ts", "store.ts", "keccak.ts"];

describe.skipIf(!existsSync(upstreamDir))("vendored accounts-drive", () => {
    for (const file of FILES) {
        it(`${file} matches accounts-drive/js/src`, () => {
            expect(readFileSync(vendoredDir + file, "utf8")).toBe(readFileSync(upstreamDir + file, "utf8"));
        });
    }
});
