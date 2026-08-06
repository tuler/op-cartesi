#!/usr/bin/env bun
// Deposits ERC-20 tokens through OPERC20Portal.
//
//   bun devnet/deposit-erc20.ts [amount] [token]
//
// Deploys a test token on first use (via forge) and records it in
// devnet/token.env.
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
import { getAddress, parseAbi, type Address } from "viem";
import { config, DEVNET_DIR, l1Public, l1Wallet, usage } from "./lib/env.ts";

const erc20Abi = parseAbi([
    "function approve(address spender, uint256 value) returns (bool)",
    "function balanceOf(address owner) view returns (uint256)",
]);

const portalAbi = parseAbi([
    "function depositERC20Tokens(address token, address appContract, uint256 value, bytes execLayerData)",
]);

const [amountArg, tokenArg] = process.argv.slice(2);
if (amountArg === "-h" || amountArg === "--help") usage("deposit-erc20.ts [amount] [token]");
const amount = BigInt(amountArg ?? "1000000000000000000");

const portal = config.erc20Portal;
const executor = config.outputExecutor;
if (!portal || !executor) {
    console.error("run ./devnet/deploy-outputs.sh first");
    process.exit(1);
}

const l1 = l1Public();
const wallet = l1Wallet(config.depositorKey);
const depositor = wallet.account.address;

let token: Address;
if (tokenArg) {
    token = getAddress(tokenArg);
} else if (config.testToken) {
    token = config.testToken;
} else {
    console.error("deploying a test token");
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
await l1.waitForTransactionReceipt({ hash: approval });
const deposit = await wallet.writeContract({
    address: portal,
    abi: portalAbi,
    functionName: "depositERC20Tokens",
    args: [token, executor, amount, "0x"],
});
await l1.waitForTransactionReceipt({ hash: deposit });

const escrowed = await l1.readContract({ address: token, abi: erc20Abi, functionName: "balanceOf", args: [executor] });
console.error(`  escrowed in the application: ${escrowed}`);
console.error("");
console.error(`the guest credits ${depositor} once the chain derives the deposit; check with:`);
console.error(`  bun devnet/balance.ts ${depositor} ${token}`);
