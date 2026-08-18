// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

/// @notice Test fixture: builds the withdrawal trie the chain commits when it
/// holds exactly one storage slot — the reserved outputs-root slot. A
/// one-entry secure trie is a single leaf node, so the whole trie is
/// constructible in Solidity: the proof is that one node and the root is its
/// hash. This is what lets the execution tests run against honestly-verified
/// storage proofs without hardcoding them.
///
/// Multi-entry tries (blocks with withdrawals) are beyond what a test should
/// reimplement; those are pinned by vectors generated from the node's Go trie
/// in PasserTrieVectors.t.sol.
library PasserTrieFixture {
    bytes32 internal constant OUTPUTS_ROOT_SLOT = keccak256("op-cartesi.outputsMerkleRoot");

    /// @notice The trie holding only outputsRoot at the reserved slot.
    /// @return root The trie root — what the header's withdrawalsRoot would be.
    /// @return proof The storage proof of the slot against that root.
    function outputsOnlyTrie(bytes32 outputsRoot)
        internal
        pure
        returns (bytes32 root, bytes[] memory proof)
    {
        bytes memory leaf = leafNode(
            keccak256(abi.encode(OUTPUTS_ROOT_SLOT)), rlpBytes(trim(outputsRoot))
        );
        root = keccak256(leaf);
        proof = new bytes[](1);
        proof[0] = leaf;
    }

    /// @dev RLP leaf node: list of the hex-prefix path (even-length leaf,
    /// prefix 0x20) and the RLP-encoded storage value.
    function leafNode(bytes32 hashedKey, bytes memory value) internal pure returns (bytes memory) {
        bytes memory path = abi.encodePacked(bytes1(0x20), hashedKey); // 33 bytes
        bytes memory items = abi.encodePacked(rlpBytes(path), rlpBytes(value));
        if (items.length <= 55) {
            return abi.encodePacked(bytes1(uint8(0xc0 + items.length)), items);
        }
        require(items.length <= 255, "fixture: node too long");
        return abi.encodePacked(bytes1(0xf8), bytes1(uint8(items.length)), items);
    }

    /// @dev RLP byte-string encoding for the short strings a leaf holds.
    function rlpBytes(bytes memory data) internal pure returns (bytes memory) {
        if (data.length == 1 && uint8(data[0]) < 0x80) return data;
        require(data.length <= 55, "fixture: string too long");
        return abi.encodePacked(bytes1(uint8(0x80 + data.length)), data);
    }

    /// @dev Storage values are stored with leading zeros trimmed, the way
    /// geth writes account storage.
    function trim(bytes32 value) internal pure returns (bytes memory) {
        uint256 start;
        while (start < 32 && value[start] == 0) start++;
        bytes memory out = new bytes(32 - start);
        for (uint256 i; i < out.length; ++i) {
            out[i] = value[start + i];
        }
        return out;
    }
}
