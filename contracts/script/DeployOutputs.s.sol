// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Script, console} from "forge-std/Script.sol";

import {IDisputeGameFactory} from "../src/interfaces/IDisputeGame.sol";
import {IOptimismPortal} from "../src/interfaces/IOptimismPortal.sol";
import {OPOutputsMerkleRootValidator} from "../src/OPOutputsMerkleRootValidator.sol";
import {OutputExecutor} from "../src/OutputExecutor.sol";
import {OPERC20Portal} from "../src/portals/OPERC20Portal.sol";
import {OPEtherPortal} from "../src/portals/OPEtherPortal.sol";

/// @notice Deploys everything the withdrawal path needs, in one transaction
/// batch, and prints the addresses for the devnet scripts to pick up.
///
/// The validator and the executor point at each other, so the executor's
/// address is computed from the deployer's next nonce and baked into the
/// validator's constructor rather than adding a setter.
contract DeployOutputs is Script {
    function run() external {
        address factory = vm.envAddress("DISPUTE_GAME_FACTORY_ADDRESS");
        address optimismPortal = vm.envAddress("DEPOSIT_CONTRACT_ADDRESS");
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

        OPERC20Portal erc20Portal = new OPERC20Portal(IOptimismPortal(optimismPortal));
        OPEtherPortal etherPortal = new OPEtherPortal(IOptimismPortal(optimismPortal));
        vm.stopBroadcast();

        console.log("OUTPUTS_VALIDATOR_ADDRESS=%s", address(validator));
        console.log("OUTPUT_EXECUTOR_ADDRESS=%s", address(executor));
        console.log("ERC20_PORTAL_ADDRESS=%s", address(erc20Portal));
        console.log("ETHER_PORTAL_ADDRESS=%s", address(etherPortal));
    }
}
