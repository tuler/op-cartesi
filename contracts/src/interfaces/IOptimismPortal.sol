// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

/// @notice The one method op-cartesi needs from OP's `OptimismPortal`.
///
/// It is the chain's input transport. op-node derives L2 blocks from
/// `TransactionDeposited` logs emitted by exactly one address — the
/// `deposit_contract_address` in rollup.json — so anything that wants to reach
/// the machine has to go through here. A contract that emitted its own event
/// would be invisible to a node deriving from L1 alone, which is the property
/// that makes this a rollup.
///
/// In Cartesi Rollups terms: this is the InputBox.
interface IOptimismPortal {
    /// @param _to The L2 address the deposit is addressed to. For a portal
    /// deposit this is the application contract, mirroring
    /// `InputBox.addInput(appContract, ...)`.
    /// @param _value Wei to transfer on L2. Zero for token deposits.
    /// @param _gasLimit L2 gas, which this chain does not meter but the portal
    /// still enforces a floor on, based on the payload length.
    /// @param _isCreation Always false here.
    /// @param _data The payload the guest receives.
    function depositTransaction(
        address _to,
        uint256 _value,
        uint64 _gasLimit,
        bool _isCreation,
        bytes memory _data
    ) external payable;

    /// @notice The floor `_gasLimit` must clear for a payload of this size.
    function minimumGasLimit(uint64 _byteCount) external pure returns (uint64);
}
