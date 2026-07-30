// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {LibOutputValidityProof} from "../cartesi/library/LibOutputValidityProof.sol";
import {OutputValidityProof} from "../cartesi/common/OutputValidityProof.sol";
import {Outputs} from "../cartesi/common/Outputs.sol";
import {IOutputsMerkleRootValidator} from "../cartesi/consensus/IOutputsMerkleRootValidator.sol";

/// @title OutputExecutor
/// @notice Executes Cartesi outputs against an OP proposal.
///
/// This is a reduced stand-in for Cartesi's `Application`, and the reduction is
/// deliberate about what it keeps. Validation is Cartesi's own code, unmodified
/// — `LibOutputValidityProof` over `LibMerkle32` — and the validator it asks is
/// the `IOutputsMerkleRootValidator` interface a real `Application` asks. What
/// is left out is ownership, upgrades, ERC-721/1155 receivers and the
/// `DelegateCallVoucher` variant, none of which bear on whether an output the
/// machine emitted can be proven and run on L1.
///
/// A production chain should deploy the real `Application` and point it at the
/// same validator; nothing here is a substitute for that. This exists so the
/// path can be exercised end to end without pulling the whole contract suite
/// in, and so a proof produced by this node is checked against Cartesi's real
/// verifier rather than a reimplementation of it.
contract OutputExecutor {
    using LibOutputValidityProof for OutputValidityProof;

    IOutputsMerkleRootValidator public immutable validator;

    /// @notice Outputs are single-use, keyed by their chain-wide index.
    mapping(uint256 => bool) public executed;

    event OutputExecuted(uint256 indexed outputIndex, bytes32 outputsMerkleRoot);

    error OutputNotReproducible(bytes32 computedRoot);
    error OutputsMerkleRootNotValid(bytes32 outputsMerkleRoot);
    error OutputAlreadyExecuted(uint256 outputIndex);
    error SiblingsLengthInvalid(uint256 got);
    error UnsupportedOutput(bytes4 selector);
    error VoucherFailed(bytes returnData);

    constructor(IOutputsMerkleRootValidator validator_) {
        validator = validator_;
    }

    receive() external payable {}

    /// @notice Prove an output belongs to an accepted outputs root and run it.
    ///
    /// The output bytes are exactly what the machine emitted; the leaf is their
    /// keccak, and the proof lifts it to a root the validator must already have
    /// accepted from a proposal.
    function executeOutput(bytes calldata output, OutputValidityProof calldata proof) external {
        bytes32 outputsMerkleRoot = validateOutput(output, proof);

        if (executed[proof.outputIndex]) revert OutputAlreadyExecuted(proof.outputIndex);
        executed[proof.outputIndex] = true;

        bytes4 selector = bytes4(output[:4]);
        bytes calldata arguments = output[4:];
        if (selector == Outputs.Voucher.selector) {
            (address destination, uint256 value, bytes memory payload) =
                abi.decode(arguments, (address, uint256, bytes));
            (bool ok, bytes memory returnData) = destination.call{value: value}(payload);
            if (!ok) revert VoucherFailed(returnData);
        } else if (selector == Outputs.Notice.selector) {
            // A notice carries no call. Proving it is the whole point of it.
        } else {
            revert UnsupportedOutput(selector);
        }
        emit OutputExecuted(proof.outputIndex, outputsMerkleRoot);
    }

    /// @notice Check an output without running it, returning the outputs root
    /// the proof reproduces.
    function validateOutput(bytes calldata output, OutputValidityProof calldata proof)
        public
        view
        returns (bytes32)
    {
        if (!proof.isSiblingsArrayLengthValid()) {
            revert SiblingsLengthInvalid(proof.outputHashesSiblings.length);
        }
        bytes32 outputsMerkleRoot = proof.computeOutputsMerkleRoot(keccak256(output));
        if (!validator.isOutputsMerkleRootValid(address(this), outputsMerkleRoot)) {
            revert OutputsMerkleRootNotValid(outputsMerkleRoot);
        }
        return outputsMerkleRoot;
    }
}
