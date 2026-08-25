// @op-cartesi/app: the application runtime of docs/EVM-COMPAT.md.
//
// Boot an Application, declare its contracts (address + ABI + callbacks), and
// run — the library owns admission, dispatch, the outcome model, the journal,
// the built-ins, and the drives. Application state is plain RAM the
// application owns outright.

export type { ApplicationOptions } from "./application.ts";
export { Application } from "./application.ts";
export type {
    BlockEnv,
    ContractSpec,
    TransactionCallbacks,
    TransactionEnv,
    ViewCallbacks,
    ViewEnv,
} from "./contract.ts";
export { contractHandler, Fail, Revert } from "./contract.ts";
export type { ResolvedFacade, TokenMetadata } from "./ledger.ts";
export {
    AccountsDriveError,
    DEVNET_DRIVE_CONFIG,
    InsufficientFunds,
    Journal,
    Ledger,
} from "./ledger.ts";
export type { AdvanceResult, InspectResult, RouterConfig } from "./router.ts";
export { Router } from "./router.ts";
export type { Handler, TickOutcome } from "./types.ts";
