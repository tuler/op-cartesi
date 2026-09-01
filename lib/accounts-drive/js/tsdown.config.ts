import { defineConfig } from "tsdown";

export default defineConfig({
    entry: ["src/index.ts"],
    format: ["esm", "cjs"],
    dts: true,
    platform: "node",
    target: "node18",
    // @noble/hashes (a package.json dependency) and node:* builtins are
    // external by default; nothing else is imported.
});
