// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {IOptimismPortal} from "../interfaces/IOptimismPortal.sol";

/// @title OPPortal
/// @notice The base Cartesi portals sit on when the chain is an OP Stack L2.
///
/// Cartesi's own `Portal` holds an `IInputBox` and calls `addInput`. This holds
/// an `IOptimismPortal` and calls `depositTransaction`, which is the same idea
/// against the transport this chain actually has. Everything else about a
/// portal — pulling the asset into the application, encoding the payload — is
/// unchanged, so the subclasses below are Cartesi's, one line different.
///
/// The guest authenticates a portal by its address: a deposit made from a
/// contract arrives on L2 with `from` set to the *aliased* sender, so the
/// envelope's `msgSender` is `alias(portal)`. That is the same role msgSender
/// plays in Cartesi Rollups, where a portal deposit carries the portal's own
/// address.
abstract contract OPPortal {
    IOptimismPortal internal immutable OPTIMISM_PORTAL;

    /// @notice Extra L2 gas requested beyond the portal's floor. The chain
    /// meters in machine cycles and ignores this, but L1 still records it.
    uint64 internal constant GAS_MARGIN = 100_000;

    constructor(IOptimismPortal optimismPortal) {
        OPTIMISM_PORTAL = optimismPortal;
    }

    /// @notice The OptimismPortal deposits are routed through.
    function getOptimismPortal() public view returns (IOptimismPortal) {
        return OPTIMISM_PORTAL;
    }

    /// @dev Hands one input to the chain, addressed to an application.
    function _addInput(address appContract, bytes memory payload) internal {
        uint64 gasLimit = OPTIMISM_PORTAL.minimumGasLimit(uint64(payload.length)) + GAS_MARGIN;
        OPTIMISM_PORTAL.depositTransaction(appContract, 0, gasLimit, false, payload);
    }
}
