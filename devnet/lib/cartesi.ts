// The cartesi_ RPC namespace (engineapi/cartesi.go) as a viem client
// extension — the .extend(cartesiActions()) counterpart of viem's own
// walletActionsL1/publicActionsL1: typed methods over the schema, with the
// wire's hex quantities converted at the boundary so call sites speak
// bigint.

import { numberToHex, type Account, type Chain, type Client, type Hex, type Transport } from "viem";

/** What cartesi_getTransactionEmissions returns: the transaction's provable
 * outputs with their chain-wide indices, and its diagnostic reports. */
export interface TransactionEmissions {
    outputs: { index: Hex; payload: Hex }[];
    reports: Hex[];
}

/** What cartesi_getOutputProof returns: the raw output and its sibling
 * hashes against the outputs root of the chosen block. */
export interface OutputProof {
    output: Hex;
    outputHashesSiblings: Hex[];
}

/** The raw schema, for createClient({ rpcSchema: rpcSchema<...>() }) — the
 * extension below is typed against it, so a client built without the schema
 * cannot be extended by accident. */
export type CartesiRpcSchema = [
    {
        Method: "cartesi_getTransactionEmissions";
        Parameters: [Hex];
        ReturnType: TransactionEmissions;
    },
    {
        Method: "cartesi_getOutputProof";
        Parameters: [Hex, Hex];
        ReturnType: OutputProof;
    },
];

// A type alias, deliberately not an interface: .extend()'s constraint wants
// an implicit string index signature, which TypeScript grants to object type
// aliases but never to interfaces — the same reason viem's own action
// bundles are aliases.
export type CartesiActions = {
    /** The emissions of one L2 transaction, by hash. */
    getTransactionEmissions(args: { hash: Hex }): Promise<TransactionEmissions>;
    /** A proof of the output at `index` against the outputs commitment of
     * `blockNumber` — the proposed block, not necessarily the emitting one:
     * the tree is cumulative, so a withdrawal stays provable against every
     * later proposal. */
    getOutputProof(args: { index: bigint; blockNumber: bigint }): Promise<OutputProof>;
};

// Shaped exactly like viem's own decorators: generic over transport, chain
// and account (with defaults), only the rpcSchema pinned — so a client
// built without the cartesi_ schema cannot be extended by accident.
export function cartesiActions() {
    return <
        transport extends Transport,
        chain extends Chain | undefined = Chain | undefined,
        account extends Account | undefined = Account | undefined,
    >(
        client: Client<transport, chain, account, CartesiRpcSchema>,
    ): CartesiActions => ({
        getTransactionEmissions: ({ hash }) =>
            client.request({ method: "cartesi_getTransactionEmissions", params: [hash] }),
        getOutputProof: ({ index, blockNumber }) =>
            client.request({
                method: "cartesi_getOutputProof",
                params: [numberToHex(index), numberToHex(blockNumber)],
            }),
    });
}
