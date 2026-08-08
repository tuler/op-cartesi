#!/usr/bin/env bun
// Deposits ERC-20 tokens through OPERC20Portal.
//
//   bun devnet/deposit-erc20.ts [amount] [token]
//
// Deploys a test token on first use (via forge) and records it in
// devnet/token.env. A recorded token that no longer has code — anvil forgets
// every deployment on restart — is replaced by a fresh deploy.
//
// The portal escrows the tokens in the application contract — the same
// contract a voucher later runs from — and hands the guest Cartesi's own
// packed deposit payload, carried as the data of an OptimismPortal deposit.
// Nothing here touches L1StandardBridge: that bridge escrows in itself and
// releases only against an OptimismPortal withdrawal proof, which this chain
// cannot produce.

import { spawnSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { type Address, erc20Abi, getAddress, parseAbi } from "viem";
import { config, DEVNET_DIR, l1Public, l1Wallet, usage } from "./lib/env.ts";

const portalAbi = parseAbi([
    "function depositERC20Tokens(address token, address appContract, uint256 value, bytes execLayerData)",
]);

const [amountArg, tokenArg] = process.argv.slice(2);
if (amountArg === "-h" || amountArg === "--help") usage("deposit-erc20.ts [amount] [token]");
const amount = BigInt(amountArg ?? "1000000000000000000");

const portal = config.erc20Portal;
const executor = config.outputExecutor;
if (!portal || !executor) {
    console.error("run ./devnet/deploy-outputs.ts first");
    process.exit(1);
}

const l1 = l1Public();
const wallet = l1Wallet(config.depositorKey);
const depositor = wallet.account.address;

// A transaction to an address without code succeeds as a no-op, so a bad
// token address would sail through approve and the portal deposit and only
// surface at the first read ("returned no data"). Check for code up front.
const deployed = async (address: Address) => (await l1.getCode({ address })) !== undefined;

// The same goes for the outputs suite itself: addresses recorded by an
// earlier deploy-outputs.ts run outlive the anvil they were deployed to.
if (!(await deployed(portal)) || !(await deployed(executor))) {
    console.error(
        `the outputs suite (portal ${portal}, executor ${executor}) has no code on this L1 — rerun ./devnet/deploy-outputs.ts`,
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
        { cwd: join(dirname(DEVNET_DIR), "contracts"), encoding: "utf8" },
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
        `# Written by devnet/deposit-erc20.ts.\nTEST_TOKEN_ADDRESS=${token}\n`,
    );
    console.error(`  token ${token}, supply held by ${depositor}`);
}

console.error(`depositing ${amount} of ${token} through the portal at ${portal}`);
const approval = await wallet.writeContract({
    address: token,
    abi: erc20Abi,
    functionName: "approve",
    args: [portal, amount],
});
const approvalReceipt = await l1.waitForTransactionReceipt({ hash: approval });
if (approvalReceipt.status !== "success") {
    console.error(`approve reverted in ${approval}`);
    process.exit(1);
}
const deposit = await wallet.writeContract({
    address: portal,
    abi: portalAbi,
    functionName: "depositERC20Tokens",
    args: [token, executor, amount, "0x"],
});
const depositReceipt = await l1.waitForTransactionReceipt({ hash: deposit });
if (depositReceipt.status !== "success") {
    console.error(
        `depositERC20Tokens reverted in ${deposit} — does ${depositor} hold ${amount} of ${token}?`,
    );
    process.exit(1);
}

const escrowed = await l1.readContract({
    address: token,
    abi: erc20Abi,
    functionName: "balanceOf",
    args: [executor],
});
console.error(`  escrowed in the application: ${escrowed}`);
console.error("");
console.error(`the guest credits ${depositor} once the chain derives the deposit; check with:`);
console.error(`  bun devnet/balance.ts ${depositor} ${token}`);
