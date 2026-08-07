import { type BuildOptions, build } from "esbuild";

const options: BuildOptions = {
    entryPoints: ["src/index.ts"],
    bundle: true,
    outfile: "dist/index.js",
    platform: "node",
    target: "node22",
    // CJS, explicitly: the staged rootfs has no package.json next to the
    // bundle, so node parses index.js as CommonJS whatever this package's
    // "type" says.
    format: "cjs",
    // @deroll/cmio is a native addon (.node): it cannot be inlined into
    // the bundle and must be required at runtime from node_modules. The
    // Dockerfile copies it (and its node-gyp-build loader) next to the bundle.
    external: ["@deroll/cmio"],
};

await build(options);
