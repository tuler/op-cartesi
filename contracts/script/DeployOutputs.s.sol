// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Script, console} from "forge-std/Script.sol";

import {IDisputeGameFactory} from "../src/interfaces/IDisputeGame.sol";
import {OPOutputsMerkleRootValidator} from "../src/OPOutputsMerkleRootValidator.sol";
import {OutputExecutor} from "../src/OutputExecutor.sol";

/// @notice Deploys the Cartesi output-execution pair — the validator that
/// opens OP proposals and the executor that runs app-specific outputs
/// against it — and prints the addresses for the devnet scripts to pick up.
/// Bridging deploys nothing here: ether and ERC-20 go through the stock OP
/// contracts (DESIGN §5–§6).
///
/// The validator and the executor point at each other, so the executor's
/// address is computed from the deployer's next nonce and baked into the
/// validator's constructor rather than adding a setter.
contract DeployOutputs is Script {
    function run() external {
        address factory = vm.envAddress("DISPUTE_GAME_FACTORY_ADDRESS");
        uint32 gameType = uint32(vm.envUint("DISPUTE_GAME_TYPE"));
        uint256 maturityDelay = vm.envOr("OUTPUT_MATURITY_DELAY", uint256(0));
        bool requireResolved = vm.envOr("OUTPUT_REQUIRE_RESOLVED", false);

        vm.startBroadcast();
        address deployer = msg.sender;
        address predictedExecutor = vm.computeCreateAddress(deployer, vm.getNonce(deployer) + 1);

        OPOutputsMerkleRootValidator validator = new OPOutputsMerkleRootValidator(
            IDisputeGameFactory(factory), gameType, predictedExecutor, maturityDelay, requireResolved
        );
        OutputExecutor executor = new OutputExecutor(validator);
        require(address(executor) == predictedExecutor, "executor address prediction failed");
        vm.stopBroadcast();

        console.log("OUTPUTS_VALIDATOR_ADDRESS=%s", address(validator));
        console.log("OUTPUT_EXECUTOR_ADDRESS=%s", address(executor));
    }
}
