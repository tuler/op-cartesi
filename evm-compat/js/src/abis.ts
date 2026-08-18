// The built-in contract ABIs of EVM-COMPAT §9 — the standard's interface
// surface, importable by host tooling without touching the guest runtime.
// The runtime's handlers implement exactly these.

import { parseAbi } from "viem";

/** The bridge at 0xC751…01: withdrawals as vouchers. */
export const bridgeAbi = parseAbi([
    "function withdrawEther(address to) payable",
    "function withdrawERC20(address token, address to, uint256 amount)",
]);

/** The owner config contract at 0xC751…02. */
export const configAbi = parseAbi([
    "function setFee(uint256 fee)",
    "function registerPortal(uint8 kind, address portal)",
    "function registerMessenger(address l1Messenger, address l1Bridge)",
    "function registerToken(address l1Token, uint8 width, string name, string symbol, uint8 decimals)",
    "function setTokenMetadata(address l1Token, string name, string symbol, uint8 decimals)",
    "function fee() view returns (uint256)",
    "function owner() view returns (address)",
]);

export const PORTAL_ETHER = 0;
export const PORTAL_ERC20 = 1;

/** The router registry at 0xC751…00: discovery views over the manifest. */
export const registryAbi = parseAbi([
    "function handlerAt(address target) view returns (bool routed, bool isPayable)",
    "function handlers() view returns (address[])",
    "function l2TokenOf(address l1Token) view returns (address)",
]);

/** The ERC-20 façade served at every registered token's derived address.
 * approve/allowance/transferFrom are deferred (EVM-COMPAT §9). */
export const erc20FacadeAbi = parseAbi([
    "function balanceOf(address owner) view returns (uint256)",
    "function totalSupply() view returns (uint256)",
    "function decimals() view returns (uint8)",
    "function name() view returns (string)",
    "function symbol() view returns (string)",
    "function transfer(address to, uint256 value) returns (bool)",
]);

/** The adopted OP predeploy: L2ToL1MessagePasser at 0x4200…0016 — the
 * function viem's initiateWithdrawal sends, so the stock OP withdrawal flow
 * (viem.sh/op-stack/guides/withdrawals) starts against this chain exactly as
 * it would against any other. */
export const messagePasserAbi = parseAbi([
    "function initiateWithdrawal(address target, uint256 gasLimit, bytes data) payable",
]);

/** The adopted OP predeploy: L1Block views at 0x4200…0015. */
export const l1BlockAbi = parseAbi([
    "function number() view returns (uint64)",
    "function timestamp() view returns (uint64)",
    "function basefee() view returns (uint256)",
    "function blobBaseFee() view returns (uint256)",
    "function hash() view returns (bytes32)",
    "function sequenceNumber() view returns (uint64)",
    "function batcherHash() view returns (bytes32)",
    "function baseFeeScalar() view returns (uint32)",
    "function blobBaseFeeScalar() view returns (uint32)",
]);
