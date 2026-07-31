// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

/// @notice The subset of OP's dispute game interface this contract reads.
interface IDisputeGame {
    function rootClaim() external pure returns (bytes32);
    function l2BlockNumber() external pure returns (uint256);
    function status() external view returns (uint8);
    function createdAt() external pure returns (uint64);
}

interface IDisputeGameFactory {
    function gameAtIndex(uint256 index)
        external
        view
        returns (uint32 gameType, uint64 timestamp, IDisputeGame proxy);
    function gameCount() external view returns (uint256);
}
