// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Test} from "forge-std/Test.sol";

import {LibOutputValidityProof} from "cartesi-rollups-contracts/src/library/LibOutputValidityProof.sol";
import {OutputValidityProof} from "cartesi-rollups-contracts/src/common/OutputValidityProof.sol";

import {OutputTree} from "./OutputTree.sol";

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
/// The node builds the tree in Go (`chain/outputtree.go`) and the proof in
/// `chain/outputproof.go`; the chain that has to accept the result is this one.
/// Nothing checks that agreement automatically — the two implementations share
/// no code — so it is pinned here by a root computed by the Go side, over
/// outputs `TestOutputTreeMatchesSolidity` in `chain/outputproof_test.go` uses
/// verbatim. Change either implementation and one of the two tests fails.
contract OutputTreeTest is Test {
    using OutputTree for bytes32[];

    /// @dev The root `chain/outputtree.go` computes over OUTPUTS below.
    bytes32 constant GO_ROOT =
        0x49afe364b0c4fe906304bbfc9d6273a94df905335403d8e65eeda219ea7fb717;

    ProofChecker checker;
    bytes[] outputs;
    bytes32[] leaves;

    function setUp() public {
        checker = new ProofChecker();
        outputs.push("output zero");
        outputs.push("output one");
        outputs.push("output two");
        outputs.push("output three");
        outputs.push("output four");
        for (uint256 i; i < outputs.length; ++i) {
            leaves.push(keccak256(outputs[i]));
        }
    }

    function testRootMatchesTheGoImplementation() public view {
        assertEq(leaves.root(), GO_ROOT);
    }

    /// @notice Every proof reproduces the root through Cartesi's own verifier.
    ///
    /// This is the property the withdrawal path rests on, and it is checked
    /// against the released library rather than a reimplementation of it: the
    /// contract on L1 will run exactly this code.
    function testEveryProofVerifiesUnderCartesi() public view {
        for (uint256 i; i < outputs.length; ++i) {
            OutputValidityProof memory proof = OutputValidityProof({
                outputIndex: uint64(i),
                outputHashesSiblings: leaves.siblings(i)
            });
            assertTrue(checker.siblingsValid(proof));
            assertEq(checker.rootOf(outputs[i], proof), GO_ROOT);
        }
    }

    /// @notice A leaf the machine never emitted does not reach the root.
    function testForgedLeafDoesNotReproduceTheRoot() public view {
        OutputValidityProof memory proof =
            OutputValidityProof({outputIndex: 2, outputHashesSiblings: leaves.siblings(2)});
        assertTrue(checker.rootOf("an output the machine never emitted", proof) != GO_ROOT);
    }

    /// @notice A proof of the right shape but the wrong position fails too.
    function testProofAtTheWrongIndexIsRejected() public view {
        OutputValidityProof memory proof =
            OutputValidityProof({outputIndex: 3, outputHashesSiblings: leaves.siblings(2)});
        assertTrue(checker.rootOf(outputs[3], proof) != GO_ROOT);
    }
}
