// Assemble the runtime rootfs the Dockerfile's final stage copies: the esbuild
// bundle plus @deroll/cmio's native addon and its runtime dependencies, which
// esbuild cannot inline. The packages are located through module resolution —
// from this app for the runtime, from the runtime for the addon, from the
// addon for its own dependencies — because their on-disk location depends on
// bun's install linker (isolated puts them in node_modules/.bun with symlinks
// only in the package that declares them; hoisted puts them at the workspace
// root). The chain follows the dependency graph: @deroll/cmio is the
// runtime's dependency, not this app's, so it is resolved from there.
import { cpSync, mkdirSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const out = process.argv[2] ?? "rootfs";

const fromApp = createRequire(join(here, "package.json"));
const guest = dirname(fromApp.resolve("@op-cartesi/app/package.json"));
const fromGuest = createRequire(join(guest, "package.json"));
const cmio = dirname(fromGuest.resolve("@deroll/cmio/package.json"));
const fromCmio = createRequire(join(cmio, "package.json"));

mkdirSync(join(out, "node_modules"), { recursive: true });
cpSync(join(here, "dist", "index.js"), join(out, "index.js"));
for (const [name, dir] of [
    ["@deroll/cmio", cmio],
    ["node-gyp-build", dirname(fromCmio.resolve("node-gyp-build/package.json"))],
    ["node-addon-api", dirname(fromCmio.resolve("node-addon-api/package.json"))],
]) {
    cpSync(dir, join(out, "node_modules", name), {
        recursive: true,
        dereference: true,
    });
}
