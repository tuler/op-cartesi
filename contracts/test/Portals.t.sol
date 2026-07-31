// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Test} from "forge-std/Test.sol";
import {IERC20} from "@openzeppelin-contracts-5.2.0/token/ERC20/IERC20.sol";

import {IOptimismPortal} from "../src/interfaces/IOptimismPortal.sol";
import {OPERC20Portal} from "../src/portals/OPERC20Portal.sol";
import {OPEtherPortal} from "../src/portals/OPEtherPortal.sol";
import {TestERC20} from "./TestERC20.sol";

/// @notice Records what a portal asked OptimismPortal to deposit, which is the
/// only thing the guest will ever see.
contract MockOptimismPortal is IOptimismPortal {
    address public lastTo;
    uint256 public lastValue;
    uint64 public lastGasLimit;
    bytes public lastData;
    uint256 public calls;

    function depositTransaction(address to, uint256 value, uint64 gasLimit, bool isCreation, bytes memory data)
        external
        payable
        override
    {
        require(!isCreation, "portals never create contracts");
        require(gasLimit >= minimumGasLimit(uint64(data.length)), "below the portal's floor");
        lastTo = to;
        lastValue = value;
        lastGasLimit = gasLimit;
        lastData = data;
        calls++;
    }

    function minimumGasLimit(uint64 byteCount) public pure override returns (uint64) {
        return 21000 + 16 * byteCount;
    }
}

contract PortalsTest is Test {
    MockOptimismPortal portal;
    address app = address(0xA99);

    function setUp() public {
        portal = new MockOptimismPortal();
    }

    /// The escrow lands in the application, not in the portal — which is what
    /// makes a voucher able to move it back out later.
    function testERC20DepositEscrowsInTheApplication() public {
        TestERC20 token = new TestERC20(1000 ether);
        OPERC20Portal erc20 = new OPERC20Portal(portal);
        token.approve(address(erc20), 100 ether);

        erc20.depositERC20Tokens(IERC20(address(token)), app, 100 ether, hex"c0ffee");

        assertEq(token.balanceOf(app), 100 ether, "the application does not hold the tokens");
        assertEq(token.balanceOf(address(erc20)), 0, "the portal kept tokens");
        assertEq(portal.calls(), 1, "the guest was not told");
    }

    /// The payload is Cartesi's own packed encoding, so a guest written for
    /// Cartesi Rollups parses it unchanged: token ‖ sender ‖ value ‖ extra.
    function testERC20DepositPayloadIsCartesiEncoding() public {
        TestERC20 token = new TestERC20(1000 ether);
        OPERC20Portal erc20 = new OPERC20Portal(portal);
        token.approve(address(erc20), 5 ether);
        erc20.depositERC20Tokens(IERC20(address(token)), app, 5 ether, hex"beef");

        assertEq(portal.lastTo(), app, "the deposit is not addressed to the application");
        assertEq(portal.lastValue(), 0, "an ERC-20 deposit must move no ether");
        assertEq(
            portal.lastData(),
            abi.encodePacked(address(token), address(this), uint256(5 ether), hex"beef"),
            "payload is not the Cartesi ERC-20 deposit encoding"
        );
    }

    /// Ether goes to the application too, so both directions agree about
    /// custody — unlike OptimismPortal's own ether path, which keeps it.
    function testEtherDepositReachesTheApplication() public {
        OPEtherPortal ether_ = new OPEtherPortal(portal);
        ether_.depositEther{value: 3 ether}(app, hex"01");

        assertEq(app.balance, 3 ether, "the application did not receive the ether");
        assertEq(
            portal.lastData(),
            abi.encodePacked(address(this), uint256(3 ether), hex"01"),
            "payload is not the Cartesi ether deposit encoding"
        );
    }

    /// A failed transferFrom must take the whole deposit with it, or the guest
    /// would be told about tokens the application never received.
    function test_RevertWhen_TransferFails() public {
        TestERC20 token = new TestERC20(1 ether);
        OPERC20Portal erc20 = new OPERC20Portal(portal);
        token.approve(address(erc20), 100 ether); // approved, but not held
        vm.expectRevert();
        erc20.depositERC20Tokens(IERC20(address(token)), app, 100 ether, "");
        assertEq(portal.calls(), 0, "the guest was told about a deposit that did not happen");
    }
}
