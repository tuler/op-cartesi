// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.22;

import {Test} from "forge-std/Test.sol";

import {Encoding} from "../src/vendor/optimism/Encoding.sol";
import {Hashing} from "../src/vendor/optimism/Hashing.sol";
import {Types} from "../src/vendor/optimism/Types.sol";

/// @notice Pins the guest's cross-domain message encodings (@op-cartesi/evm
/// crossdomain.ts) to the OP Stack's own Encoding and Hashing libraries,
/// vendored verbatim. From the moment a withdrawal carries a relayMessage
/// payload these encodings are consensus the guest must reproduce exactly:
/// L1CrossDomainMessenger recomputes the v1 hash for replay protection, and
/// the withdrawal hash is what the portal proves. Every constant below was
/// produced by the TypeScript side for the fixed message below; a drift in
/// either implementation breaks this test before it breaks a withdrawal.
contract CrossDomainVectorsTest is Test {
    address constant L2_MESSENGER = 0x4200000000000000000000000000000000000007;
    address constant L2_BRIDGE = 0x4200000000000000000000000000000000000010;
    address constant L1_MESSENGER = 0x00000000000000000000000000000000000f00F7;
    address constant L1_BRIDGE = 0x00000000000000000000000000000000000F0010;

    // finalizeBridgeERC20(l1Token, l2Token, from, to, 300, "") — the inner
    // message the L2 bridge sends for a 300-unit withdrawal.
    bytes constant MESSAGE =
        hex"0166a07a00000000000000000000000059b670e9fa9d0a427751af201d676719a970857b000000000000000000000000e78a0f7e598cc8b0bb87894b0f60dd2a88d6a8ab000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb9226600000000000000000000000000000000000000000000000000000000000000bb000000000000000000000000000000000000000000000000000000000000012c00000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000000";

    // encodeRelayMessage of the message above: messenger nonce 0 version 1,
    // sender = L2 bridge, target = L1 bridge, value 0, minGasLimit 200000.
    bytes constant RELAY =
        hex"d764ad0b0001000000000000000000000000000000000000000000000000000000000000000000000000000000000000420000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000f001000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000030d4000000000000000000000000000000000000000000000000000000000000000c000000000000000000000000000000000000000000000000000000000000000e40166a07a00000000000000000000000059b670e9fa9d0a427751af201d676719a970857b000000000000000000000000e78a0f7e598cc8b0bb87894b0f60dd2a88d6a8ab000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb9226600000000000000000000000000000000000000000000000000000000000000bb000000000000000000000000000000000000000000000000000000000000012c00000000000000000000000000000000000000000000000000000000000000c0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000";

    bytes32 constant MSG_HASH = 0xc1809ec84428317a8537ac71ae4c83f6037ea1e7fe63911248b984dabd571e88;

    // The guest's baseGas(len(MESSAGE), 200000). Not recomputed on L1 —
    // only its place inside the withdrawal hash matters — but committed
    // here so a formula change is a visible decision.
    uint256 constant GAS_LIMIT = 516_982;

    // hashWithdrawal of the messenger-shaped withdrawal: passer nonce
    // v1 | (42 << 32), sender = L2 messenger, target = L1 messenger,
    // value 0, gasLimit above, data = RELAY.
    bytes32 constant WITHDRAWAL_HASH =
        0xbbbf25d8ab7f7259713453eddad8ecdb6317541a9e5b0b4f3152169d421253a8;

    function messageNonce() internal pure returns (uint256) {
        return Encoding.encodeVersionedNonce(0, 1);
    }

    /// The guest's relayMessage bytes are Encoding.encodeCrossDomainMessageV1.
    function testRelayMessageEncodingMatches() public pure {
        bytes memory encoded = Encoding.encodeCrossDomainMessageV1(
            messageNonce(), L2_BRIDGE, L1_BRIDGE, 0, 200_000, MESSAGE
        );
        assertEq(encoded, RELAY, "relayMessage encoding drifted");
    }

    /// The replay-protection hash both messengers key by.
    function testCrossDomainMessageHashMatches() public pure {
        bytes32 hash = Hashing.hashCrossDomainMessageV1(
            messageNonce(), L2_BRIDGE, L1_BRIDGE, 0, 200_000, MESSAGE
        );
        assertEq(hash, MSG_HASH, "hashCrossDomainMessageV1 drifted");
    }

    /// The withdrawal hash the portal proves for the wrapped message.
    function testMessengerWithdrawalHashMatches() public pure {
        bytes32 hash = Hashing.hashWithdrawal(
            Types.WithdrawalTransaction({
                nonce: Encoding.encodeVersionedNonce(uint240(42) << 32, 1),
                sender: L2_MESSENGER,
                target: L1_MESSENGER,
                value: 0,
                gasLimit: GAS_LIMIT,
                data: RELAY
            })
        );
        assertEq(hash, WITHDRAWAL_HASH, "the messenger-shaped withdrawal hash drifted");
    }
}
