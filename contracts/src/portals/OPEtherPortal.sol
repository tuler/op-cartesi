// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {InputEncoding} from "cartesi-rollups-contracts/src/common/InputEncoding.sol";

import {IOptimismPortal} from "../interfaces/IOptimismPortal.sol";
import {OPPortal} from "./OPPortal.sol";

/// @title OPEtherPortal
/// @notice Cartesi's EtherPortal against OptimismPortal.
///
/// Unlike the ERC-20 case, ETH has somewhere else it could go: OP's own
/// `OptimismPortal.depositTransaction` mints ETH on L2 directly, which is the
/// path scripts/deposit.ts already uses. The difference is custody. That path
/// leaves the ETH in OP's lockbox, where a Cartesi voucher cannot reach it;
/// this one puts it in the application contract, where a voucher can. A chain
/// using vouchers for withdrawals wants this one, so that both directions
/// agree about who holds the money.
contract OPEtherPortal is OPPortal {
    error EtherTransferFailed();

    constructor(IOptimismPortal optimismPortal) OPPortal(optimismPortal) {}

    /// @notice Send ether to the application and tell the guest.
    function depositEther(address appContract, bytes calldata execLayerData) external payable {
        (bool success,) = appContract.call{value: msg.value}("");
        if (!success) revert EtherTransferFailed();
        _addInput(
            appContract, InputEncoding.encodeEtherDeposit(msg.sender, msg.value, execLayerData)
        );
    }
}
