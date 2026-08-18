#!/usr/bin/env bun
// Deposits ERC-20 tokens through the standard OP bridge (DESIGN §6).
//
//   bun scripts/deposit-erc20.ts [amount] [token]
//
// Deploys a test token on first use (via forge) and records it in
// devnet/token.env. A recorded token that no longer has code — anvil forgets
// every deployment on restart — is replaced by a fresh deploy.
//
// L1StandardBridge escrows the tokens in itself and sends
// finalizeBridgeERC20 through the messengers; the guest's L2StandardBridge
// predeploy credits the ledger, naming the L2 token by its derived façade
// address — the pair the bridge's own accounting later releases a
// withdrawal against. The Cartesi-portal path this script used to take is
// gone from the repo; this devnet registers the messenger, and the guest
// keeps the standard-bridge escrow exclusive with any portal escrow.

import { spawnSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { l2TokenAddress } from "@op-cartesi/evm";
import { config, paths, usage } from "devnet/env";
import { l1Public, l1Wallet } from "devnet/wallet";
import { type Address, erc20Abi, getAddress, parseAbi } from "viem";

const bridgeAbi = parseAbi([
    "function depositERC20(address l1Token, address l2Token, uint256 amount, uint32 minGasLimit, bytes extraData)",
]);

const [amountArg, tokenArg] = process.argv.slice(2);
if (amountArg === "-h" || amountArg === "--help") usage("deposit-erc20.ts [amount] [token]");
const amount = BigInt(amountArg ?? "1000000000000000000");

const bridge = config.l1StandardBridge;
if (!bridge) {
    console.error("run ./devnet/deploy-l1.ts first");
    process.exit(1);
}

const l1 = l1Public();
const wallet = l1Wallet(config.depositorKey);
const depositor = wallet.account.address;

// A transaction to an address without code succeeds as a no-op, so a bad
// token address would sail through approve and the portal deposit and only
// surface at the first read ("returned no data"). Check for code up front.
const deployed = async (address: Address) => (await l1.getCode({ address })) !== undefined;

// The same goes for the bridge itself: addresses recorded by an earlier
// deploy-l1.ts run outlive the anvil they were deployed to.
if (!(await deployed(bridge))) {
    console.error(
        `L1StandardBridge (${bridge}) has no code on this L1 — rerun ./devnet/deploy-l1.ts`,
    );
    process.exit(1);
}

let token: Address;
if (tokenArg) {
    token = getAddress(tokenArg);
    if (!(await deployed(token))) {
        console.error(`no contract code at ${token} on L1 — is the token deployed there?`);
        process.exit(1);
    }
} else if (config.testToken && (await deployed(config.testToken))) {
    token = config.testToken;
} else {
    if (config.testToken) {
        // token.env outlives anvil, which forgets every deployment on restart.
        console.error(
            `token.env's ${config.testToken} has no code on this L1 — deploying a fresh test token`,
        );
    } else {
        console.error("deploying a test token");
    }
    const out = spawnSync(
        "forge",
        [
            "script",
            "script/DeployTestToken.s.sol:DeployTestToken",
            "--rpc-url",
            config.l1Rpc,
            "--private-key",
            config.depositorKey,
            "--broadcast",
        ],
        { cwd: join(paths.repo, "contracts"), encoding: "utf8" },
    );
    const text = `${out.stdout ?? ""}${out.stderr ?? ""}`;
    const match = text.match(/TEST_TOKEN_ADDRESS=(0x[0-9a-fA-F]{40})/);
    if (!match) {
        console.error(text.split("\n").slice(-20).join("\n"));
        process.exit(1);
    }
    token = getAddress(match[1]!);
    if (!(await deployed(token))) {
        console.error(`forge reported ${token} but the L1 has no code there:`);
        console.error(text.split("\n").slice(-20).join("\n"));
        process.exit(1);
    }
    writeFileSync(
        config.tokenEnvFile,
        `# Written by scripts/deposit-erc20.ts.\nTEST_TOKEN_ADDRESS=${token}\n`,
    );
    console.error(`  token ${token}, supply held by ${depositor}`);
}

console.error(`depositing ${amount} of ${token} through L1StandardBridge at ${bridge}`);
const approval = await wallet.writeContract({
    address: token,
    abi: erc20Abi,
    functionName: "approve",
    args: [bridge, amount],
});
const approvalReceipt = await l1.waitForTransactionReceipt({ hash: approval });
if (approvalReceipt.status !== "success") {
    console.error(`approve reverted in ${approval}`);
    process.exit(1);
}
// The l2Token is the guest's derived façade for this L1 token — the only
// pair the guest credits, because it is the only pair the bridge's escrow
// accounting can later release.
const deposit = await wallet.writeContract({
    address: bridge,
    abi: bridgeAbi,
    functionName: "depositERC20",
    args: [token, l2TokenAddress(token), amount, 400_000, "0x"],
});
const depositReceipt = await l1.waitForTransactionReceipt({ hash: deposit });
if (depositReceipt.status !== "success") {
    console.error(
        `depositERC20 reverted in ${deposit} — does ${depositor} hold ${amount} of ${token}?`,
    );
    process.exit(1);
}

const escrowed = await l1.readContract({
    address: token,
    abi: erc20Abi,
    functionName: "balanceOf",
    args: [bridge],
});
console.error(`  escrowed in L1StandardBridge: ${escrowed}`);
console.error("");
console.error(`the guest credits ${depositor} once the chain derives the deposit; check with:`);
console.error(`  bun scripts/balance.ts ${depositor} ${token}`);
