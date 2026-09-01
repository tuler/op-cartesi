// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Test} from "forge-std/Test.sol";

import {LibOutputValidityProof} from "cartesi-rollups-contracts/src/library/LibOutputValidityProof.sol";
import {OutputValidityProof} from "cartesi-rollups-contracts/src/common/OutputValidityProof.sol";

import {OutputTree} from "./OutputTree.sol";
import {Vectors} from "./Vectors.sol";

/// @notice Exposes the proof library's internal, calldata-taking functions.
contract ProofChecker {
    using LibOutputValidityProof for OutputValidityProof;

    function rootOf(bytes calldata output, OutputValidityProof calldata proof)
        external
        pure
        returns (bytes32)
    {
        return proof.computeOutputsMerkleRoot(keccak256(output));
    }

    function siblingsValid(OutputValidityProof calldata proof) external pure returns (bool) {
        return proof.isSiblingsArrayLengthValid();
    }
}

/// @notice Pins the node's outputs tree to Cartesi's on-chain verifier.
///
/// The node builds the tree in `host/go/chain/outputtree.go` and the proof in
/// `host/go/chain/outputproof.go`; the chain that has to accept the result is this
/// one. The two implementations share no code, so the agreement is checked
/// against the shared vectors
/// (`conformance/commitments/outputs-tree.json`, BLOCKS-SPEC §10): the outputs
/// and the root come from the file, and so do the **proofs the node actually
/// serves** — which is the stronger check, because a proof this suite built
/// itself would only prove the two builders agree.
///
/// `OutputTree.sol` stays as an independent Solidity builder, checked against
/// the same file: three implementations of one tree, one set of expectations.
contract OutputTreeTest is Test {
    using OutputTree for bytes32[];

    /// @dev The case whose five outputs this suite has always used.
    uint256 constant CASE = 2;
    string constant CASE_ID = "solidity-five";

    ProofChecker checker;
    bytes[] outputs;
    bytes32[] leaves;
    bytes32 root;

    function setUp() public {
        checker = new ProofChecker();
        outputs = Vectors.outputs(CASE, CASE_ID);
        root = Vectors.finalRoot(CASE, CASE_ID);
        for (uint256 i; i < outputs.length; ++i) {
            leaves.push(keccak256(outputs[i]));
        }
    }

    /// @notice The Solidity builder reaches the root the node committed.
    function testRootMatchesTheNode() public view {
        assertEq(leaves.root(), root);
    }

    /// @notice Every proof the node serves reproduces the root through
    /// Cartesi's own verifier — the code that will run on L1, not a
    /// reimplementation of it.
    function testNodeProofsVerifyUnderCartesi() public view {
        for (uint256 i; i < outputs.length; ++i) {
            OutputValidityProof memory proof = OutputValidityProof({
                outputIndex: uint64(i),
                outputHashesSiblings: Vectors.outputSiblings(CASE, CASE_ID, i)
            });
            assertTrue(checker.siblingsValid(proof));
            assertEq(checker.rootOf(outputs[i], proof), root);
        }
    }

    /// @notice The Solidity builder's own co-paths agree with the node's, leaf
    /// for leaf. If they diverge, one of the two builders is wrong and the
    /// test above would not say which.
    function testSiblingsMatchTheNode() public view {
        for (uint256 i; i < outputs.length; ++i) {
            assertEq(
                keccak256(abi.encode(leaves.siblings(i))),
                keccak256(abi.encode(Vectors.outputSiblings(CASE, CASE_ID, i))),
                "co-path differs from the one the node serves"
            );
        }
    }

    /// @notice A leaf the machine never emitted does not reach the root.
    function testForgedLeafDoesNotReproduceTheRoot() public view {
        OutputValidityProof memory proof = OutputValidityProof({
            outputIndex: 2,
            outputHashesSiblings: Vectors.outputSiblings(CASE, CASE_ID, 2)
        });
        assertTrue(checker.rootOf("an output the machine never emitted", proof) != root);
    }

    /// @notice A proof of the right shape but the wrong position fails too.
    function testProofAtTheWrongIndexIsRejected() public view {
        OutputValidityProof memory proof = OutputValidityProof({
            outputIndex: 3,
            outputHashesSiblings: Vectors.outputSiblings(CASE, CASE_ID, 2)
        });
        assertTrue(checker.rootOf(outputs[3], proof) != root);
    }
}
