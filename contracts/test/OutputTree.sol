// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {LibKeccak256} from "cartesi-rollups-contracts/src/library/LibKeccak256.sol";

/// @title OutputTree
/// @notice Builds the outputs Merkle tree, for tests.
///
/// The tree is fixed-height and zero-padded: leaves are `keccak256(output)`,
/// parents are `keccak256(left ‖ right)`, and every level beyond the real
/// leaves contributes that level's all-zero subtree root. That is what makes a
/// 63-level proof cheap for a tree holding five outputs.
///
/// This lives in the test tree because building is the off-chain side of the
/// protocol. Cartesi 2.x shipped a builder on chain in `LibMerkle32`; 3.0
/// dropped it and kept only `merkleRootAfterReplacement` — the verifier half —
/// which is the right split: a chain never builds the tree in a contract, it
/// only checks a proof against a root someone else built.
///
/// The node's own builder is `chain/outputtree.go`, and
/// `testMatchesTheGoImplementation` pins the two together.
library OutputTree {
    /// @notice CanonicalMachine.LOG2_MAX_OUTPUTS.
    uint256 internal constant HEIGHT = 63;

    /// @notice The root of an all-zero subtree at each level, level 0 first.
    function zeroHashes() internal pure returns (bytes32[] memory z) {
        z = new bytes32[](HEIGHT + 1);
        for (uint256 h = 1; h <= HEIGHT; ++h) {
            z[h] = LibKeccak256.hashPair(z[h - 1], z[h - 1]);
        }
    }

    /// @notice The tree root over `leaves`.
    function root(bytes32[] memory leaves) internal pure returns (bytes32) {
        bytes32[] memory z = zeroHashes();
        bytes32[] memory level = leaves;
        for (uint256 h = 0; h < HEIGHT; ++h) {
            level = parentLevel(level, z[h]);
        }
        return level.length == 0 ? z[HEIGHT] : level[0];
    }

    /// @notice The co-path for one leaf: the sibling at each level, level 0
    /// first, always HEIGHT long.
    function siblings(bytes32[] memory leaves, uint256 index)
        internal
        pure
        returns (bytes32[] memory sibs)
    {
        bytes32[] memory z = zeroHashes();
        sibs = new bytes32[](HEIGHT);
        bytes32[] memory level = leaves;
        uint256 cursor = index;
        for (uint256 h = 0; h < HEIGHT; ++h) {
            sibs[h] = at(level, cursor ^ 1, z[h]);
            level = parentLevel(level, z[h]);
            cursor >>= 1;
        }
    }

    /// @dev Pairs a level up, padding an odd tail with that level's default.
    function parentLevel(bytes32[] memory nodes, bytes32 defaultNode)
        private
        pure
        returns (bytes32[] memory level)
    {
        if (nodes.length == 0) return nodes;
        level = new bytes32[]((nodes.length + 1) / 2);
        for (uint256 i; i < level.length; ++i) {
            level[i] = LibKeccak256.hashPair(nodes[2 * i], at(nodes, 2 * i + 1, defaultNode));
        }
    }

    /// @dev The node, or the level's default beyond the end.
    function at(bytes32[] memory nodes, uint256 index, bytes32 defaultNode)
        private
        pure
        returns (bytes32)
    {
        return index < nodes.length ? nodes[index] : defaultNode;
    }
}
