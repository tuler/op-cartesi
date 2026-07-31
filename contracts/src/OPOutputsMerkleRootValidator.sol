// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {IOutputsMerkleRootValidator} from "cartesi-rollups-contracts/src/consensus/IOutputsMerkleRootValidator.sol";
import {IERC165} from "@openzeppelin-contracts-5.2.0/utils/introspection/IERC165.sol";

import {IDisputeGame, IDisputeGameFactory} from "./interfaces/IDisputeGame.sol";

/// @notice OP's OutputRootProof, the preimage of the root claim a proposal
/// records on L1. Verbatim from the OP Stack's Types.sol, because the hash has
/// to match what op-node computed.
struct OutputRootProof {
    bytes32 version;
    bytes32 stateRoot;
    bytes32 messagePasserStorageRoot;
    bytes32 latestBlockhash;
}

/// @title OPOutputsMerkleRootValidator
/// @notice Lets Cartesi's output execution verify against proposals the OP
/// Stack already makes.
///
/// This is the whole bridge between the two settlement models, and it is small
/// for a reason. A Cartesi `Application` asks one question before executing a
/// voucher — `isOutputsMerkleRootValid` — and an OP proposal already commits to
/// the answer: op-node builds its root claim as
///
///     keccak256(version ‖ stateRoot ‖ messagePasserStorageRoot ‖ blockHash)
///
/// and on this chain `messagePasserStorageRoot` is the header's
/// `withdrawalsRoot`, which is the Cartesi outputs Merkle root. So opening the
/// claim's preimage on chain — four words and one keccak — turns a proposal
/// made by stock `op-proposer` into an outputs root that stock
/// `Application.executeOutput` will accept.
///
/// Nothing here touches `OptimismPortal`. Its withdrawal path proves an MPT
/// storage slot against the L2 state root, which cannot work when that root is
/// a Cartesi hash tree; this sidesteps it rather than forking it, which is
/// DESIGN §4's option 2.
contract OPOutputsMerkleRootValidator is IOutputsMerkleRootValidator {
    /// @notice Game status values from OP's GameStatus enum.
    uint8 internal constant IN_PROGRESS = 0;
    uint8 internal constant CHALLENGER_WINS = 1;
    uint8 internal constant DEFENDER_WINS = 2;

    IDisputeGameFactory public immutable factory;
    /// @notice Only games of this type are read. A chain that later registers a
    /// Cartesi-native game type points a new validator at it.
    uint32 public immutable gameType;
    /// @notice The application whose outputs these are. One validator serves
    /// one application, which is what makes the appContract argument of
    /// isOutputsMerkleRootValid answerable.
    address public immutable appContract;
    /// @notice How long a proposal must have stood before its outputs count.
    uint256 public immutable maturityDelay;
    /// @notice Whether a game must have resolved in the defender's favour.
    ///
    /// False is the permissioned posture: proposals are made by an authorised
    /// proposer and nothing can dispute them, because no fault proof VM can
    /// execute a Cartesi Machine yet. It is the same trust assumption the chain
    /// already runs under, made explicit in a constructor argument rather than
    /// hidden. True is what a chain with a real proof system sets, and then
    /// maturityDelay should cover the dispute window.
    bool public immutable requireDefenderWins;

    /// @notice An outputs root, and the L2 block whose proposal established it.
    mapping(bytes32 => uint256) public acceptedAtL2Block;
    /// @notice The machine root of the highest proposal accepted so far, and
    /// that proposal's L2 block.
    ///
    /// This costs nothing to keep: OP's output root already commits to the
    /// machine's Merkle root in the same preimage as the outputs root, so
    /// opening a claim yields both of the fields Cartesi's interface asks for.
    bytes32 internal lastFinalizedMachineRoot;
    uint256 internal lastFinalizedL2Block;

    event OutputsMerkleRootAccepted(
        bytes32 indexed outputsMerkleRoot, uint256 indexed gameIndex, uint256 l2BlockNumber
    );

    error WrongGameType(uint32 got, uint32 want);
    error GameNotResolved(uint8 status);
    error GameChallenged();
    error GameTooYoung(uint64 createdAt, uint256 maturesAt);
    error ProofDoesNotMatchClaim(bytes32 computed, bytes32 claimed);

    constructor(
        IDisputeGameFactory factory_,
        uint32 gameType_,
        address appContract_,
        uint256 maturityDelay_,
        bool requireDefenderWins_
    ) {
        factory = factory_;
        gameType = gameType_;
        appContract = appContract_;
        maturityDelay = maturityDelay_;
        requireDefenderWins = requireDefenderWins_;
    }

    /// @notice Open a proposal's root claim and record the Cartesi outputs root
    /// it commits to.
    ///
    /// Permissionless by design: the proof is checked against a claim already
    /// on L1, so anyone submitting it can only make true statements. Whoever
    /// wants to execute an output calls this once for the proposal they intend
    /// to prove against.
    function accept(uint256 gameIndex, OutputRootProof calldata proof) external {
        (uint32 gt,, IDisputeGame game) = factory.gameAtIndex(gameIndex);
        if (gt != gameType) revert WrongGameType(gt, gameType);

        uint8 status = game.status();
        if (status == CHALLENGER_WINS) revert GameChallenged();
        if (requireDefenderWins && status != DEFENDER_WINS) revert GameNotResolved(status);

        uint64 createdAt = game.createdAt();
        if (block.timestamp < uint256(createdAt) + maturityDelay) {
            revert GameTooYoung(createdAt, uint256(createdAt) + maturityDelay);
        }

        bytes32 computed = keccak256(
            abi.encode(
                proof.version, proof.stateRoot, proof.messagePasserStorageRoot, proof.latestBlockhash
            )
        );
        bytes32 claimed = game.rootClaim();
        if (computed != claimed) revert ProofDoesNotMatchClaim(computed, claimed);

        // The outputs tree is cumulative over the chain, so a later proposal
        // establishes a superset of what an earlier one did. Keeping the
        // highest block an outputs root was seen at makes that visible without
        // changing the answer.
        uint256 l2Block = game.l2BlockNumber();
        bytes32 outputsMerkleRoot = proof.messagePasserStorageRoot;
        if (l2Block > acceptedAtL2Block[outputsMerkleRoot]) {
            acceptedAtL2Block[outputsMerkleRoot] = l2Block;
        }
        if (l2Block > lastFinalizedL2Block) {
            lastFinalizedL2Block = l2Block;
            lastFinalizedMachineRoot = proof.stateRoot;
        }
        emit OutputsMerkleRootAccepted(outputsMerkleRoot, gameIndex, l2Block);
    }

    /// @inheritdoc IOutputsMerkleRootValidator
    function isOutputsMerkleRootValid(address appContract_, bytes32 outputsMerkleRoot)
        external
        view
        override
        returns (bool)
    {
        return appContract_ == appContract && acceptedAtL2Block[outputsMerkleRoot] != 0;
    }

    /// @inheritdoc IOutputsMerkleRootValidator
    ///
    /// @dev The machine's Merkle root as of the highest accepted proposal.
    /// Answering it costs nothing here: the OP output root commits to the
    /// machine root in the same preimage as the outputs root, because on this
    /// chain the L2 state root simply *is* the machine root. A validator for an
    /// EVM L2 would have no such value to return.
    function getLastFinalizedMachineMerkleRoot(address appContract_)
        external
        view
        override
        returns (bytes32)
    {
        if (appContract_ != appContract) return bytes32(0);
        return lastFinalizedMachineRoot;
    }

    /// @inheritdoc IERC165
    function supportsInterface(bytes4 interfaceId) external pure override returns (bool) {
        return interfaceId == type(IOutputsMerkleRootValidator).interfaceId
            || interfaceId == type(IERC165).interfaceId;
    }
}
