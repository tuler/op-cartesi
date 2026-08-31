# integration

Compatibility tests that drive an op-cartesi engine with **op-node's own wire
types** rather than with hand-written JSON: op-node serializes the payload
attributes, deserializes the execution payloads, and independently recomputes
every block hash with `ExecutionPayloadEnvelope.CheckBlockHash`. A deliberate
one-field mutation to header construction is caught here, so the check has
teeth.

A separate Go module, so the engine itself depends only on op-geth.

## Which engine it drives

The suite talks to the engine over authenticated HTTP and nothing else, so
that is a choice made at startup:

```sh
go test ./...                     # an engine wired up in this process

OP_CARTESI_TEST_ENGINE_URL=http://127.0.0.1:8551 \
OP_CARTESI_TEST_ETH_URL=http://127.0.0.1:8545 \
OP_CARTESI_TEST_JWT=./jwt.hex \
  go test ./...                   # one already listening
```

| Variable | |
|---|---|
| `OP_CARTESI_TEST_ENGINE_URL` | the authenticated Engine API endpoint. Setting it switches the whole run. |
| `OP_CARTESI_TEST_ETH_URL` | the public `eth_`/`cartesi_` endpoint. Defaults to the engine URL, which also serves them. |
| `OP_CARTESI_TEST_JWT` | the hex secret, or a file holding one — the same file op-node is given. Empty means unauthenticated. |

The second mode is what makes this a conformance suite rather than a Go test.
[ENGINE-RPC-SPEC](../docs/ENGINE-RPC-SPEC.md) is a wire contract; an engine in
any language that serves it should be able to face this, and pointing the
suite at one is a prerequisite for finding out.

Nothing here holds a handle on the engine's internals. What the tests need to
know — the head, the outputs commitment, whether a transaction is still
queued — they ask for over the wire, which is both a truer test of the served
surface and what lets the same assertions run against an engine this process
did not build.

## What a remote engine cannot be asked

Three tests need more than a wire connection, and skip with a reason when
pointed at one:

| Test | Needs |
|---|---|
| `TestStateRootAdvancesDeterministically` | to run the same input sequence twice from the same state — an engine it can rewind |
| `TestVerifierRebuildsBlockFromPayload` | two independent engines. The devnet checks this against two real nodes anyway (`compose.yaml`'s `verifier-engine`), which is the stronger version |
| `TestIsthmusOutputRootThroughOpNode` | a machine whose emissions it chooses, to make the outputs commitment move by a known amount |

Everything else runs either way. A remote engine is shared by the whole run,
so each test builds on the head it finds rather than on genesis; it does not
need to be freshly started, and the suite will not rewind it.
