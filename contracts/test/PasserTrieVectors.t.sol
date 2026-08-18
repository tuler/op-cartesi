// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Test} from "forge-std/Test.sol";

import {SecureMerkleTrie} from "../src/vendor/optimism/SecureMerkleTrie.sol";
import {RLPReader} from "../src/vendor/optimism/RLPReader.sol";
import {PasserTrieFixture} from "./PasserTrie.sol";

/// @notice Pins the node's Go withdrawal trie (chain/passertrie.go) to the
/// Solidity verifier OptimismPortal uses. Every constant below was produced
/// by the Go side for a trie holding two withdrawals and the outputs-root
/// slot; if either implementation drifts, these proofs stop verifying here
/// — before they stop verifying on L1.
///
/// The mirrored Go test is chain/passertrie_test.go's
/// TestPasserTrieMatchesSolidityVectors, which asserts the same root.
contract PasserTrieVectorsTest is Test {
    bytes32 internal constant OUTPUTS_ROOT_SLOT = keccak256("op-cartesi.outputsMerkleRoot");

    // A cumulative outputs tree holding keccak256("cartesi output 0") and
    // keccak256("cartesi output 1").
    bytes32 constant OUTPUTS_ROOT = 0xc6848d313a7dbcdb409c9606318673fde2df2896460c76716c63ba695a0466e5;

    // Withdrawal 1: nonce v1|0, 0x1111… -> 0x2222…, 1e15 wei, gas 100000.
    bytes32 constant W1_HASH = 0x27f31ff201b73d4a552b801c296a72166443942679761eb8f895fc846e2edb39;
    // Withdrawal 2: nonce v1|(1<<32), 0x3333… -> 0x4444…, 2e15 wei, data 0xbeef.
    bytes32 constant W2_HASH = 0xb3eb3decba3830b9d78bed4d85fb5a8d4c1ac75032d150fad77b3a8c068c6760;

    // The trie root the Go side committed for the three slots above.
    bytes32 constant WITHDRAWALS_ROOT = 0xb9e3308bcb994a5eb2144c3841889a2eab0c855b9b3251b41c67b856cff0f398;

    function outputsProof() internal pure returns (bytes[] memory proof) {
        proof = new bytes[](2);
        proof[0] =
            hex"f871a0a1dd8b8e825f70c1d35603753b60890c2a63648d1dc97c3bc1e14ae5930056f780a08dbdb6f3be331c3ced19b0a2baf59d1baffb600ee552c90cacbe9132e36490528080808080808080808080a055629c587fe7c44a7b2d7b4ef327c31b9fddddc94e9a3cf208d815ac957f9cc88080";
        proof[1] =
            hex"f843a031162d2015e595ff56791af1c06d5810127772b016e35987e7b4ce78a875e407a1a0c6848d313a7dbcdb409c9606318673fde2df2896460c76716c63ba695a0466e5";
    }

    function w1Proof() internal pure returns (bytes[] memory proof) {
        proof = new bytes[](2);
        proof[0] =
            hex"f871a0a1dd8b8e825f70c1d35603753b60890c2a63648d1dc97c3bc1e14ae5930056f780a08dbdb6f3be331c3ced19b0a2baf59d1baffb600ee552c90cacbe9132e36490528080808080808080808080a055629c587fe7c44a7b2d7b4ef327c31b9fddddc94e9a3cf208d815ac957f9cc88080";
        proof[1] = hex"e2a031f7ec4d5c9c587a91dc980620e3089ad157af828577d83e11486bc470a43dfc01";
    }

    /// The sentMessages slot proves with value 0x01 — byte for byte the check
    /// OptimismPortal.proveWithdrawalTransaction performs.
    function testWithdrawalSlotProvesLikeThePortal() public pure {
        bytes32 slot = keccak256(abi.encode(W1_HASH, uint256(0)));
        bool ok = SecureMerkleTrie.verifyInclusionProof(
            abi.encode(slot), hex"01", w1Proof(), WITHDRAWALS_ROOT
        );
        assertTrue(ok, "the Go trie's proof failed the portal's verifier");
    }

    /// The outputs root opens from the same root, through the same verifier.
    function testOutputsRootOpensFromTheSameRoot() public pure {
        bytes memory value =
            SecureMerkleTrie.get(abi.encode(OUTPUTS_ROOT_SLOT), outputsProof(), WITHDRAWALS_ROOT);
        bytes memory decoded = RLPReader.readBytes(RLPReader.toRLPItem(value));
        assertEq(decoded.length, 32, "outputs root value length");
        assertEq(bytes32(decoded), OUTPUTS_ROOT, "outputs root mismatch");
    }

    /// A hash the guest never emitted does not prove. Reusing a real
    /// withdrawal's proof for a forged key makes the verifier revert — the
    /// path does not lead where the key demands — which is just as much a
    /// failure to prove as returning false.
    function testForgedWithdrawalDoesNotProve() public {
        bytes32 forged = keccak256("never sent");
        bytes32 slot = keccak256(abi.encode(forged, uint256(0)));
        vm.expectRevert();
        this.verifyExternal(abi.encode(slot), hex"01", w1Proof(), WITHDRAWALS_ROOT);
    }

    /// @dev expectRevert needs a call frame; internal library calls have none.
    function verifyExternal(bytes memory key, bytes memory value, bytes[] memory proof, bytes32 root)
        external
        pure
        returns (bool)
    {
        return SecureMerkleTrie.verifyInclusionProof(key, value, proof, root);
    }

    /// The Solidity single-slot fixture agrees with the Go trie: a trie
    /// holding only the outputs-root slot reaches the same root both sides.
    function testFixtureMatchesGoForSingleSlotTrie() public pure {
        (bytes32 root, bytes[] memory proof) = PasserTrieFixture.outputsOnlyTrie(OUTPUTS_ROOT);
        bytes memory value = SecureMerkleTrie.get(abi.encode(OUTPUTS_ROOT_SLOT), proof, root);
        bytes memory decoded = RLPReader.readBytes(RLPReader.toRLPItem(value));
        assertEq(bytes32(decoded), OUTPUTS_ROOT);
        // Pinned from Go: NewPasserTrie().SetOutputsRoot(OUTPUTS_ROOT).Root().
        assertEq(
            root,
            0xeb1f9992f27f25df88c87c5bce969cb7ac49298870c512cf55481d1c2219b9ea,
            "fixture root diverges from the Go trie"
        );
    }
}
