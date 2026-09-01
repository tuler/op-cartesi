// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Test} from "forge-std/Test.sol";

import {SecureMerkleTrie} from "../src/vendor/optimism/SecureMerkleTrie.sol";
import {RLPReader} from "../src/vendor/optimism/RLPReader.sol";
import {PasserTrieFixture} from "./PasserTrie.sol";
import {Vectors} from "./Vectors.sol";

/// @notice Pins the node's withdrawal trie (`host/go/chain/passertrie.go`) to the
/// Solidity verifier `OptimismPortal` uses.
///
/// Everything below — the roots, the withdrawal hashes, the proof nodes —
/// comes from `conformance/commitments/passer-trie.json` and
/// `conformance/encodings/withdrawal.json` (BLOCKS-SPEC §11), the same files
/// the node's own suite replays. The node generates them and checks every
/// proof with geth's verifier before writing; this checks the same bytes with
/// the verifier that will actually run on L1. If either implementation drifts,
/// these proofs stop verifying here — before they stop verifying on L1.
contract PasserTrieVectorsTest is Test {
    /// @dev The two-withdrawal trie, and the withdrawals it holds.
    uint256 constant TRIE = 2;
    string constant TRIE_ID = "solidity-two-withdrawals";
    uint256 constant SINGLE = 1;
    string constant SINGLE_ID = "solidity-single-slot";

    /// @dev Proof order within the trie case, as the node emits it.
    uint256 constant P_OUTPUTS_ROOT = 0;
    uint256 constant P_W1 = 1;
    uint256 constant P_FORGED = 3;

    bytes32 withdrawalsRoot;
    bytes32 outputsRootSlot;

    function setUp() public {
        withdrawalsRoot = Vectors.trieRoot(TRIE, TRIE_ID);
        outputsRootSlot = Vectors.outputsRootSlot();
    }

    /// @notice The reserved slot is the hash of its name, not a small integer,
    /// so colliding with a sentMessages slot would take a keccak collision.
    function testReservedSlotIsTheHashedName() public view {
        assertEq(outputsRootSlot, keccak256("op-cartesi.outputsMerkleRoot"));
    }

    /// @notice The sentMessages slot proves with value 0x01 — byte for byte
    /// the check `OptimismPortal.proveWithdrawalTransaction` performs.
    function testWithdrawalSlotProvesLikeThePortal() public view {
        Vectors.Withdrawal memory w = Vectors.withdrawal(1, "portal-1");
        (bytes32 slot, bytes memory value, bytes[] memory proof) =
            Vectors.storageProof(TRIE, TRIE_ID, P_W1);

        // The slot the node proved is the one the portal will ask for.
        assertEq(slot, keccak256(abi.encode(w.hash, uint256(0))), "slot");
        assertEq(slot, w.slot, "the withdrawal vector's own slot");
        // A single 0x01 byte is its own RLP encoding, so the decoded value the
        // vector carries is what the portal passes to the verifier verbatim.
        assertEq(value, hex"01", "stored value");

        assertTrue(
            SecureMerkleTrie.verifyInclusionProof(
                abi.encode(slot), value, proof, withdrawalsRoot
            ),
            "the node's proof failed the portal's verifier"
        );
    }

    /// @notice The outputs root opens from the same root, through the same
    /// verifier — which is how one header field serves both the portal's
    /// withdrawal proofs and Cartesi's output proofs.
    function testOutputsRootOpensFromTheSameRoot() public view {
        (bytes32 slot, bytes memory expected, bytes[] memory proof) =
            Vectors.storageProof(TRIE, TRIE_ID, P_OUTPUTS_ROOT);
        assertEq(slot, outputsRootSlot, "the proof is not of the reserved slot");

        bytes memory stored = SecureMerkleTrie.get(abi.encode(slot), proof, withdrawalsRoot);
        bytes memory decoded = RLPReader.readBytes(RLPReader.toRLPItem(stored));
        assertEq(decoded.length, 32, "outputs root length");
        assertEq(decoded, expected, "outputs root differs from the vector");
        assertEq(
            bytes32(decoded),
            Vectors.committedOutputsRoot(TRIE, TRIE_ID),
            "the slot does not hold the root the block committed"
        );
    }

    /// @notice A hash the guest never emitted does not prove. The node serves
    /// a real exclusion proof for it, and reusing it as an inclusion proof
    /// makes the verifier revert — the path does not lead where the key
    /// demands — which is just as much a failure to prove as returning false.
    function testForgedWithdrawalDoesNotProve() public {
        (bytes32 slot, bytes memory value, bytes[] memory proof) =
            Vectors.storageProof(TRIE, TRIE_ID, P_FORGED);
        assertEq(value.length, 0, "the vector's forged slot is not empty");

        vm.expectRevert();
        this.verifyExternal(abi.encode(slot), hex"01", proof, withdrawalsRoot);
    }

    /// @dev expectRevert needs a call frame; internal library calls have none.
    function verifyExternal(bytes memory key, bytes memory value, bytes[] memory proof, bytes32 root)
        external
        pure
        returns (bool)
    {
        return SecureMerkleTrie.verifyInclusionProof(key, value, proof, root);
    }

    /// @notice The Solidity single-slot fixture agrees with the node: a trie
    /// holding only the reserved slot reaches the same root both sides. That
    /// is what lets the execution tests build honestly-verified storage
    /// proofs instead of hardcoding them.
    function testFixtureMatchesTheNodeForASingleSlotTrie() public view {
        bytes32 outputsRoot = Vectors.committedOutputsRoot(SINGLE, SINGLE_ID);
        (bytes32 root, bytes[] memory proof) = PasserTrieFixture.outputsOnlyTrie(outputsRoot);

        assertEq(root, Vectors.trieRoot(SINGLE, SINGLE_ID), "fixture root diverges from the node");

        bytes memory stored = SecureMerkleTrie.get(abi.encode(outputsRootSlot), proof, root);
        assertEq(bytes32(RLPReader.readBytes(RLPReader.toRLPItem(stored))), outputsRoot);
    }
}
