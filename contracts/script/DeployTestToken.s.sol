// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Script, console} from "forge-std/Script.sol";
import {TestERC20} from "../test/TestERC20.sol";

/// @notice A token to bridge on the devnet.
contract DeployTestToken is Script {
    function run() external {
        vm.startBroadcast();
        TestERC20 token = new TestERC20(vm.envOr("TOKEN_SUPPLY", uint256(1_000_000 ether)));
        vm.stopBroadcast();
        console.log("TEST_TOKEN_ADDRESS=%s", address(token));
    }
}
