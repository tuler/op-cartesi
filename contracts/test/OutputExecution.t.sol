// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Test} from "forge-std/Test.sol";

import {OutputValidityProof} from "cartesi-rollups-contracts/src/common/OutputValidityProof.sol";
import {Outputs} from "cartesi-rollups-contracts/src/common/Outputs.sol";

import {OutputTree} from "./OutputTree.sol";

import {
    OPOutputsMerkleRootValidator, OutputRootProof
} from "../src/OPOutputsMerkleRootValidator.sol";
import {IDisputeGame, IDisputeGameFactory} from "../src/interfaces/IDisputeGame.sol";
import {OutputExecutor} from "../src/OutputExecutor.sol";

/// @notice A dispute game with settable fields, standing in for OP's.
contract MockGame {
    bytes32 public rootClaim;
    uint256 public l2BlockNumber;
    uint8 public status;
    uint64 public createdAt;

    constructor(bytes32 claim, uint256 block_, uint8 status_) {
        rootClaim = claim;
        l2BlockNumber = block_;
        status = status_;
        createdAt = uint64(block.timestamp);
    }
}

contract MockFactory {
    uint32 public immutable gameType;
    address[] public games;

    constructor(uint32 gameType_) {
        gameType = gameType_;
    }

    function add(address game) external returns (uint256) {
        games.push(game);
        return games.length - 1;
    }

    function gameAtIndex(uint256 i) external view returns (uint32, uint64, IDisputeGame) {
        return (gameType, 0, IDisputeGame(games[i]));
    }

    function gameCount() external view returns (uint256) {
        return games.length;
    }
}

contract Recipient {
    uint256 public received;

    receive() external payable {
        received += msg.value;
    }
}

contract OutputExecutionTest is Test {
    using OutputTree for bytes32[];

    uint32 constant GAME_TYPE = 1;
    uint8 constant IN_PROGRESS = 0;
    uint8 constant CHALLENGER_WINS = 1;
    uint8 constant DEFENDER_WINS = 2;

    MockFactory factory;
    OPOutputsMerkleRootValidator validator;
    OutputExecutor executor;

    bytes32[] leaves;

    function setUp() public {
        factory = new MockFactory(GAME_TYPE);
        address predicted = vm.computeCreateAddress(address(this), vm.getNonce(address(this)) + 1);
        validator = new OPOutputsMerkleRootValidator(
            IDisputeGameFactory(address(factory)), GAME_TYPE, predicted, 0, false
        );
        executor = new OutputExecutor(validator);
        assertEq(address(executor), predicted, "executor did not land where the validator expects");
        vm.deal(address(executor), 100 ether);
    }

    /// @dev Appends an output to the chain-wide tree and returns its index.
    function appendOutput(bytes memory output) internal returns (uint256) {
        leaves.push(keccak256(output));
        return leaves.length - 1;
    }

    function outputsRoot() internal view returns (bytes32) {
        return leaves.root();
    }

    function proofFor(uint256 index) internal view returns (OutputValidityProof memory) {
        return OutputValidityProof({
            outputIndex: uint64(index),
            outputHashesSiblings: leaves.siblings(index)
        });
    }

    /// @dev Builds the OP output root the way op-node does, and proposes it.
    function propose(uint256 l2Block, uint8 status) internal returns (uint256, OutputRootProof memory) {
        OutputRootProof memory proof = OutputRootProof({
            version: bytes32(0),
            stateRoot: keccak256(abi.encode("machine root", l2Block)),
            messagePasserStorageRoot: outputsRoot(),
            latestBlockhash: keccak256(abi.encode("block hash", l2Block))
        });
        bytes32 claim = keccak256(
            abi.encode(
                proof.version, proof.stateRoot, proof.messagePasserStorageRoot, proof.latestBlockhash
            )
        );
        MockGame game = new MockGame(claim, l2Block, status);
        return (factory.add(address(game)), proof);
    }

    function voucher(address destination, uint256 value) internal pure returns (bytes memory) {
        return abi.encodeCall(Outputs.Voucher, (destination, value, ""));
    }

    /// The path the whole design exists for: a voucher the machine emitted is
    /// proven against a proposal made by stock op-proposer, and executed.
    function testVoucherExecutesAgainstAProposal() public {
        Recipient alice = new Recipient();
        uint256 index = appendOutput(voucher(address(alice), 1 ether));
        (uint256 gameIndex, OutputRootProof memory proof) = propose(100, IN_PROGRESS);

        validator.accept(gameIndex, proof);
        executor.executeOutput(voucher(address(alice), 1 ether), proofFor(index));

        assertEq(alice.received(), 1 ether, "the voucher did not pay out");
        assertTrue(executor.executed(index), "the output was not marked executed");
    }

    /// A voucher is a single-use permission.
    function testVoucherCannotBeReplayed() public {
        Recipient alice = new Recipient();
        uint256 index = appendOutput(voucher(address(alice), 1 ether));
        (uint256 gameIndex, OutputRootProof memory proof) = propose(100, IN_PROGRESS);
        validator.accept(gameIndex, proof);

        executor.executeOutput(voucher(address(alice), 1 ether), proofFor(index));
        vm.expectRevert(abi.encodeWithSelector(OutputExecutor.OutputAlreadyExecuted.selector, index));
        executor.executeOutput(voucher(address(alice), 1 ether), proofFor(index));
    }

    /// An output the machine never emitted has no proof, so nothing it is
    /// paired with reproduces an accepted root.
    function testForgedOutputIsRejected() public {
        Recipient alice = new Recipient();
        uint256 index = appendOutput(voucher(address(alice), 1 ether));
        (uint256 gameIndex, OutputRootProof memory proof) = propose(100, IN_PROGRESS);
        validator.accept(gameIndex, proof);

        bytes memory forged = voucher(address(alice), 99 ether);
        vm.expectRevert();
        executor.executeOutput(forged, proofFor(index));
    }

    /// Nothing executes until a proposal has been opened on L1.
    function testNothingExecutesWithoutAnAcceptedProposal() public {
        Recipient alice = new Recipient();
        uint256 index = appendOutput(voucher(address(alice), 1 ether));
        propose(100, IN_PROGRESS);
        vm.expectRevert();
        executor.executeOutput(voucher(address(alice), 1 ether), proofFor(index));
    }

    /// The claim is what binds an outputs root to L1; a proof that does not
    /// hash to it proves nothing.
    function testAcceptRejectsAProofThatDoesNotOpenTheClaim() public {
        appendOutput(voucher(address(0xbeef), 1 ether));
        (uint256 gameIndex, OutputRootProof memory proof) = propose(100, IN_PROGRESS);
        proof.messagePasserStorageRoot = keccak256("a different outputs root");
        vm.expectRevert();
        validator.accept(gameIndex, proof);
    }

    /// A challenged game is never a source of truth, whatever the posture.
    function testAcceptRejectsAChallengedGame() public {
        appendOutput(voucher(address(0xbeef), 1 ether));
        (uint256 gameIndex, OutputRootProof memory proof) = propose(100, CHALLENGER_WINS);
        vm.expectRevert(OPOutputsMerkleRootValidator.GameChallenged.selector);
        validator.accept(gameIndex, proof);
    }

    /// The strict posture a chain with a real proof system sets.
    function testResolvedOnlyValidatorWaitsForTheDefenderToWin() public {
        OPOutputsMerkleRootValidator strict = new OPOutputsMerkleRootValidator(
            IDisputeGameFactory(address(factory)), GAME_TYPE, address(executor), 1 days, true
        );
        appendOutput(voucher(address(0xbeef), 1 ether));
        (uint256 unresolved, OutputRootProof memory proof) = propose(100, IN_PROGRESS);
        vm.expectRevert(
            abi.encodeWithSelector(OPOutputsMerkleRootValidator.GameNotResolved.selector, IN_PROGRESS)
        );
        strict.accept(unresolved, proof);

        (uint256 resolved, OutputRootProof memory proof2) = propose(101, DEFENDER_WINS);
        vm.expectRevert(); // still inside the maturity delay
        strict.accept(resolved, proof2);

        vm.warp(block.timestamp + 1 days);
        strict.accept(resolved, proof2);
        assertTrue(strict.isOutputsMerkleRootValid(address(executor), proof2.messagePasserStorageRoot));
    }

    /// The interface's other question, answered from the same preimage.
    function testMachineRootTracksTheHighestProposal() public {
        appendOutput(voucher(address(0xbeef), 1 ether));
        (uint256 first, OutputRootProof memory p1) = propose(100, IN_PROGRESS);
        validator.accept(first, p1);
        assertEq(validator.getLastFinalizedMachineMerkleRoot(address(executor)), p1.stateRoot);

        appendOutput(voucher(address(0xbeef), 2 ether));
        (uint256 second, OutputRootProof memory p2) = propose(200, IN_PROGRESS);
        validator.accept(second, p2);
        assertEq(validator.getLastFinalizedMachineMerkleRoot(address(executor)), p2.stateRoot);

        // An older proposal must not move it backwards.
        validator.accept(first, p1);
        assertEq(validator.getLastFinalizedMachineMerkleRoot(address(executor)), p2.stateRoot);
    }

    /// One validator answers for one application.
    function testValidatorAnswersOnlyForItsApplication() public {
        appendOutput(voucher(address(0xbeef), 1 ether));
        (uint256 gameIndex, OutputRootProof memory proof) = propose(100, IN_PROGRESS);
        validator.accept(gameIndex, proof);
        assertTrue(validator.isOutputsMerkleRootValid(address(executor), proof.messagePasserStorageRoot));
        assertFalse(validator.isOutputsMerkleRootValid(address(0xdead), proof.messagePasserStorageRoot));
    }

    /// The tree is cumulative, so an old output stays provable against a later
    /// proposal — which is what a user actually has to prove against.
    function testOldOutputProvesAgainstALaterProposal() public {
        Recipient alice = new Recipient();
        uint256 index = appendOutput(voucher(address(alice), 1 ether));
        propose(100, IN_PROGRESS); // never accepted

        for (uint256 i; i < 5; ++i) {
            appendOutput(voucher(address(0xbeef), i));
        }
        (uint256 later, OutputRootProof memory proof) = propose(200, IN_PROGRESS);
        validator.accept(later, proof);

        executor.executeOutput(voucher(address(alice), 1 ether), proofFor(index));
        assertEq(alice.received(), 1 ether);
    }
}
