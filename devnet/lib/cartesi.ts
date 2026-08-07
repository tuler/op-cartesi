// The cartesi_ RPC namespace (engineapi/cartesi.go) as a viem client
// extension — the .extend(cartesiActions()) counterpart of viem's own
// walletActionsL1/publicActionsL1: typed methods over the schema, with the
// wire's hex quantities converted at the boundary so call sites speak
// bigint.

import {
    type Abi,
    type Account,
    type Address,
    type Chain,
    type Client,
    type Hex,
    numberToHex,
    type Transport,
} from "viem";

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

/** One routed address, as cartesi_getContracts reports it: the recorded
 * contracts carry their standard JSON ABI; token façades carry the L1 token
 * instead — their shared ABI is fixed by the standard (erc20FacadeAbi). */
export interface ContractEntry {
    address: Address;
    kind: "system" | "app" | "token";
    abi?: Abi;
    l1Token?: Address;
}

/** What cartesi_getContracts returns: the guest's interface surface as of a
 * block — the ABI drive's contracts plus the registry's token façades. */
export interface ContractsList {
    blockHash: Hex;
    blockNumber: Hex;
    contracts: ContractEntry[];
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
    {
        Method: "cartesi_getContracts";
        Parameters: [] | [Hex];
        ReturnType: ContractsList;
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
    /** The guest's interface surface as of a block (defaults to the head):
     * every routed address, with ABIs for the recorded contracts and the L1
     * token behind each façade. */
    getContracts(args?: { blockNumber?: bigint }): Promise<ContractsList>;
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
        getContracts: (args) =>
            client.request({
                method: "cartesi_getContracts",
                params: args?.blockNumber !== undefined ? [numberToHex(args.blockNumber)] : [],
            }),
    });
}
