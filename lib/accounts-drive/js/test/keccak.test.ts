// Pins the hash to the spec's Appendix B vectors (generated with
// go-ethereum's keccak), the well-known empty-string digest, and the five
// home slots at C=8.

import assert from "node:assert/strict";
import { test } from "node:test";
import { bytesToHex, home, sparseKey } from "../src/drive.ts";
import { keccak256 } from "../src/keccak.ts";

function repAddr(b: number): Uint8Array {
    return new Uint8Array(20).fill(b);
}

test("keccak vectors", () => {
    assert.equal(
        bytesToHex(keccak256(new Uint8Array(0))),
        "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
    );
    assert.equal(
        bytesToHex(keccak256()),
        "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
    );
    const seed = new Uint8Array(32);
    assert.equal(
        bytesToHex(keccak256(seed, repAddr(0xbb))),
        "693865af36f54054f08a9e83ec52e6df2001c39673abba1e01d450211dff1a92",
    );
    assert.equal(
        bytesToHex(keccak256(seed, sparseKey(3, repAddr(0xbb)))),
        "e5cad18719cbc3bfeb1ff8587f27c3cfd5f9da3867df2634691bd24e26d147d3",
    );
});

test("keccak handles rate-boundary and multi-block inputs", () => {
    // Same bytes, different chunking, must agree; >136-byte inputs exercise
    // multi-block absorption.
    const big = new Uint8Array(300).map((_, i) => i & 0xff);
    const whole = bytesToHex(keccak256(big));
    const split = bytesToHex(
        keccak256(big.subarray(0, 135), big.subarray(135, 137), big.subarray(137)),
    );
    assert.equal(split, whole);
    assert.equal(
        bytesToHex(keccak256(new Uint8Array(136))),
        // keccak256 of 136 zero bytes (exactly one rate block).
        bytesToHex(keccak256(new Uint8Array(100), new Uint8Array(36))),
    );
});

test("home slots (spec Appendix B, zero seed)", () => {
    const seed = new Uint8Array(32);
    const want: Record<number, number> = { 0xaa: 5, 0xbb: 1, 0xcc: 7, 0xdd: 1, 0xee: 1 };
    for (const [b, w] of Object.entries(want)) {
        assert.equal(
            home(seed, repAddr(Number(b)), 8),
            w,
            `home(${Number(b).toString(16)}..) mod 8`,
        );
    }
    assert.equal(home(seed, repAddr(0xbb), 2 ** 19), 342121, "home(bb..) mod 2^19");
    assert.equal(home(seed, sparseKey(3, repAddr(0xbb)), 2 ** 19), 117477, "sparse home mod 2^19");
});
