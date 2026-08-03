import { build, type BuildOptions } from "esbuild";

const options: BuildOptions = {
    entryPoints: ["src/index.ts"],
    bundle: true,
    outfile: "dist/index.js",
    platform: "node",
    target: "node22",
    // @deroll/cmio is a native addon (.node): it cannot be inlined into
    // the bundle and must be required at runtime from node_modules. The
    // Dockerfile copies it (and its node-gyp-build loader) next to the bundle.
    external: ["@deroll/cmio"],
};

await build(options);
