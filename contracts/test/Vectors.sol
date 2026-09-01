// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Vm} from "forge-std/Vm.sol";

/// @title Vectors
/// @notice Reads the shared conformance vectors (`../conformance`, generated
/// by `go test ./host/go/chain -run TestConformance -update`) so the Solidity suite
/// judges the node's bytes from the same file the node and the guest do.
///
/// Before this existed, each root, hash and proof node below was a `bytes32`
/// or a `hex"…"` literal pasted into a test from a Go run. That pins two
/// implementations to each other and hides which one is the reference; a
/// third implementation has nowhere to look. Now all three read one file.
///
/// Cases are addressed by array index, because `vm.parseJson` has no way to
/// select by field value. Every accessor therefore takes the id it expects
/// and `caseAt` asserts it, so reordering the file fails loudly instead of
/// silently testing a different case.
library Vectors {
    Vm private constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));

    string internal constant OUTPUTS_TREE = "../conformance/commitments/outputs-tree.json";
    string internal constant PASSER_TRIE = "../conformance/commitments/passer-trie.json";
    string internal constant WITHDRAWALS = "../conformance/encodings/withdrawal.json";

    /// @notice The JSON path of a case, after checking it is the one wanted.
    /// @param file One of the constants above.
    /// @param index The case's position in the file's `cases` array.
    /// @param id The `id` that position must hold.
    /// @return json The file's contents, and `path` its case's prefix.
    function caseAt(string memory file, uint256 index, string memory id)
        internal
        view
        returns (string memory json, string memory path)
    {
        json = vm.readFile(file);
        path = string.concat(".cases[", vm.toString(index), "]");
        string memory found = vm.parseJsonString(json, string.concat(path, ".id"));
        require(
            keccak256(bytes(found)) == keccak256(bytes(id)),
            string.concat(
                "vector case ", vm.toString(index), " of ", file,
                " is '", found, "', not '", id, "': the file was reordered"
            )
        );
    }

    // ------------------------------------------------------------ outputs tree

    /// @notice The raw outputs of an outputs-tree case, in emission order.
    function outputs(uint256 index, string memory id) internal view returns (bytes[] memory) {
        (string memory json, string memory path) = caseAt(OUTPUTS_TREE, index, id);
        return vm.parseJsonBytesArray(json, string.concat(path, ".outputs"));
    }

    /// @notice The tree root once every output of the case has been appended.
    function finalRoot(uint256 index, string memory id) internal view returns (bytes32) {
        (string memory json, string memory path) = caseAt(OUTPUTS_TREE, index, id);
        bytes32[] memory roots = vm.parseJsonBytes32Array(json, string.concat(path, ".rootAfter"));
        return roots[roots.length - 1];
    }

    /// @notice The co-path the node serves for one output, leaf level first.
    function outputSiblings(uint256 index, string memory id, uint256 outputIndex)
        internal
        view
        returns (bytes32[] memory)
    {
        (string memory json, string memory path) = caseAt(OUTPUTS_TREE, index, id);
        string memory proof = string.concat(path, ".proofs[", vm.toString(outputIndex), "]");
        require(
            vm.parseJsonUint(json, string.concat(proof, ".outputIndex")) == outputIndex,
            "vector proofs are not in output-index order"
        );
        return vm.parseJsonBytes32Array(json, string.concat(proof, ".outputHashesSiblings"));
    }

    // ----------------------------------------------------------- passer trie

    /// @notice The withdrawal trie root after the case's last block — what the
    /// header publishes as `withdrawalsRoot`.
    function trieRoot(uint256 index, string memory id) internal view returns (bytes32) {
        (string memory json, string memory path) = caseAt(PASSER_TRIE, index, id);
        return vm.parseJsonBytes32(json, string.concat(path, ".root"));
    }

    /// @notice One storage proof against that root: the slot, the RLP-decoded
    /// value, and the trie nodes root first.
    function storageProof(uint256 index, string memory id, uint256 proofIndex)
        internal
        view
        returns (bytes32 slot, bytes memory value, bytes[] memory nodes)
    {
        (string memory json, string memory path) = caseAt(PASSER_TRIE, index, id);
        string memory proof = string.concat(path, ".proofs[", vm.toString(proofIndex), "]");
        slot = vm.parseJsonBytes32(json, string.concat(proof, ".slot"));
        value = vm.parseJsonBytes(json, string.concat(proof, ".value"));
        nodes = vm.parseJsonBytesArray(json, string.concat(proof, ".nodes"));
    }

    /// @notice The reserved slot the outputs root lives at.
    function outputsRootSlot() internal view returns (bytes32) {
        return vm.parseJsonBytes32(vm.readFile(PASSER_TRIE), ".outputsRootSlot");
    }

    /// @notice The outputs root a case's last block committed — the value the
    /// reserved slot holds under that block's `withdrawalsRoot`.
    function committedOutputsRoot(uint256 index, string memory id)
        internal
        view
        returns (bytes32)
    {
        (string memory json, string memory path) = caseAt(PASSER_TRIE, index, id);
        return vm.parseJsonBytes32(json, string.concat(path, ".outputsRoot"));
    }

    // ------------------------------------------------------------ withdrawals

    /// @notice A withdrawal as the guest emits it, plus what the portal keys by.
    struct Withdrawal {
        uint256 nonce;
        address sender;
        address target;
        uint256 value;
        uint256 gasLimit;
        bytes data;
        bytes32 hash;
        bytes32 slot;
    }

    function withdrawal(uint256 index, string memory id)
        internal
        view
        returns (Withdrawal memory w)
    {
        (string memory json, string memory path) = caseAt(WITHDRAWALS, index, id);
        string memory f = string.concat(path, ".withdrawal");
        // Integers too large for a JSON number are carried as decimal strings:
        // a versioned nonce sets bit 240.
        w.nonce = vm.parseUint(vm.parseJsonString(json, string.concat(f, ".nonce")));
        w.sender = vm.parseJsonAddress(json, string.concat(f, ".sender"));
        w.target = vm.parseJsonAddress(json, string.concat(f, ".target"));
        w.value = vm.parseUint(vm.parseJsonString(json, string.concat(f, ".value")));
        w.gasLimit = vm.parseUint(vm.parseJsonString(json, string.concat(f, ".gasLimit")));
        w.data = vm.parseJsonBytes(json, string.concat(f, ".data"));
        w.hash = vm.parseJsonBytes32(json, string.concat(path, ".hash"));
        w.slot = vm.parseJsonBytes32(json, string.concat(path, ".slot"));
    }
}
