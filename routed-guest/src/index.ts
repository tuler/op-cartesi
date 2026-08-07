// @cartesi/routed-guest: the guest runtime of docs/EVM-COMPAT.md.
//
// An application boots a Guest, declares its contracts (address + ABI +
// callbacks), and runs — the library owns admission, dispatch, the
// three-outcome model, the journal, the built-ins, and the drives.

export { Revert, contractHandler } from "./contract.ts";
export type { ContractSpec, TransactionCallbacks, TransactionEnv, ViewCallbacks, ViewEnv } from "./contract.ts";
export { Guest } from "./guest.ts";
export type { GuestOptions } from "./guest.ts";
export {
    AccountsDriveError,
    DEVNET_DRIVE_CONFIG,
    InsufficientFunds,
    Journal,
    JournaledStore,
    Ledger,
} from "./ledger.ts";
export type { ResolvedFacade, TokenMetadata } from "./ledger.ts";
export { Router } from "./router.ts";
export type { AdvanceResult, InspectResult, RouterConfig } from "./router.ts";
export type { Handler } from "./types.ts";
