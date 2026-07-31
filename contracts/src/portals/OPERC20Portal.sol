// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {IERC20} from "@openzeppelin-contracts-5.2.0/token/ERC20/IERC20.sol";
import {InputEncoding} from "cartesi-rollups-contracts/src/common/InputEncoding.sol";

import {IOptimismPortal} from "../interfaces/IOptimismPortal.sol";
import {OPPortal} from "./OPPortal.sol";

/// @title OPERC20Portal
/// @notice Cartesi's ERC20Portal with its input transport swapped for
/// OptimismPortal.
///
/// Compare Cartesi's own:
///
///     token.transferFrom(msg.sender, appContract, value);
///     bytes memory payload = InputEncoding.encodeERC20Deposit(...);
///     getInputBox().addInput(appContract, payload);   // ← only this differs
///
/// The escrow is identical — tokens go to the application contract, which is
/// what lets a voucher move them back out later — and the payload is Cartesi's
/// own encoding, so a guest written for Cartesi Rollups parses it unchanged.
///
/// This deliberately does not use OP's `L1StandardBridge`. That bridge escrows
/// in itself and can only be released by `L1CrossDomainMessenger` after
/// `OptimismPortal.proveWithdrawalTransaction` — an MPT proof against a state
/// root that on this chain is a Cartesi hash tree, so nothing can satisfy it.
/// Tokens bridged through the standard bridge would be stuck. See DESIGN §7d.
contract OPERC20Portal is OPPortal {
    error ERC20TransferFailed();

    constructor(IOptimismPortal optimismPortal) OPPortal(optimismPortal) {}

    /// @notice Escrow tokens in the application and tell the guest.
    /// @param token The token being deposited.
    /// @param appContract The application that will hold it.
    /// @param value How much.
    /// @param execLayerData Arbitrary bytes passed through to the guest.
    function depositERC20Tokens(
        IERC20 token,
        address appContract,
        uint256 value,
        bytes calldata execLayerData
    ) external {
        if (!token.transferFrom(msg.sender, appContract, value)) {
            revert ERC20TransferFailed();
        }
        _addInput(
            appContract, InputEncoding.encodeERC20Deposit(token, msg.sender, value, execLayerData)
        );
    }
}
