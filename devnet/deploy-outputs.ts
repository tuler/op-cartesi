#!/usr/bin/env bun
// Deploys the contracts that make app-specific Cartesi outputs executable on
// L1 against OP proposals, and registers the standard messenger pair with
// the guest. Bridging itself deploys nothing: ether and ERC-20 go through
// the stock OP contracts (DESIGN §7f–§7g).
//
// See contracts/ for the Foundry project; this only supplies the addresses the
// devnet already knows and records what came back.

import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { type Address, encodeFunctionData, type Hex, parseAbi } from "viem";
import { privateKeyToAccount } from "viem/accounts";
import { addresses, chainParams, config, paths } from "./lib/env.ts";
import { die, must, run, say } from "./lib/proc.ts";
import { asKey, l1Public, l1Wallet } from "./lib/wallet.ts";

// How long a proposal must stand before its outputs may be executed, and
// whether its game must have resolved. Zero and false are the permissioned
// posture this chain already runs under — nothing can dispute a proposal,
// because no fault proof VM can execute a Cartesi Machine. A chain with a real
// proof system sets both.
const maturityDelay = process.env.OUTPUT_MATURITY_DELAY ?? "0";
const requireResolved = process.env.OUTPUT_REQUIRE_RESOLVED ?? "false";
const executorFunding = BigInt(process.env.EXECUTOR_FUNDING ?? "10000000000000000000");
// OptimismPortal's floor is 21000 + 16 per byte, and the registration calldata
// (registerMessenger's selector plus two ABI words) is 68 bytes. The chain
// meters in machine cycles and ignores this, but L1 still checks it.
const registerGasLimit = BigInt(process.env.REGISTER_GAS_LIMIT ?? "100000");

const portalAbi = parseAbi([
    "function depositTransaction(address to, uint256 value, uint64 gasLimit, bool isCreation, bytes data)",
]);
const registerAbi = parseAbi(["function registerMessenger(address l1Messenger, address l1Bridge)"]);

export async function deployOutputs(): Promise<void> {
    // Read fresh rather than from the import-time snapshot: this runs after a
    // deploy that may have happened while the process was already waiting.
    const a = addresses();
    const optimismPortal = chainParams().depositContractAddress;
    if (!a.disputeGameFactory) {
        die("no DisputeGameFactory; run the devnet with WITH_CONTRACTS=1 first");
    }
    if (Bun.which("forge") === null) {
        die("forge is not on PATH; install foundry (https://getfoundry.sh)");
    }

    const contracts = join(paths.repo, "contracts");
    if (!existsSync(join(contracts, "dependencies"))) {
        await must(["forge", "soldeer", "install"], { cwd: contracts });
    }

    const { stdout } = await run(
        [
            "forge",
            "script",
            "script/DeployOutputs.s.sol:DeployOutputs",
            "--rpc-url",
            config.l1Rpc,
            "--private-key",
            a.deployerKey,
            "--broadcast",
        ],
        {
            cwd: contracts,
            capture: true,
            env: {
                DISPUTE_GAME_FACTORY_ADDRESS: a.disputeGameFactory,
                DEPOSIT_CONTRACT_ADDRESS: optimismPortal,
                DISPUTE_GAME_TYPE: a.disputeGameType,
                OUTPUT_MATURITY_DELAY: maturityDelay,
                OUTPUT_REQUIRE_RESOLVED: requireResolved,
            },
        },
    );
    // forge's own output is the record of what was deployed; it is echoed
    // whether or not the parse below finds anything, since a failure here is
    // read by looking at it.
    console.log(stdout);

    const found = [
        ...stdout.matchAll(/(OUTPUTS_VALIDATOR|OUTPUT_EXECUTOR)_ADDRESS=(0x[0-9a-fA-F]{40})/g),
    ];
    const deployed = new Map(found.map((m) => [m[1]!, m[2]! as Address]));
    const executor = deployed.get("OUTPUT_EXECUTOR");
    if (!executor) {
        die("the deploy script did not report the addresses; see its output above");
    }

    writeFileSync(
        paths.outputsAddresses,
        [
            "# Written by devnet/deploy-outputs.ts. Read back by devnet/lib/env.ts.",
            ...found.map((m) => `${m[1]}_ADDRESS=${m[2]}`),
            "",
        ].join("\n"),
    );

    const wallet = l1Wallet(a.deployerKey);
    const l1 = l1Public();

    // The executor pays app-specific vouchers from its own balance, the way
    // a Cartesi Application holds the assets it can be told to move. Ether
    // and ERC-20 bridging no longer touch it — the portal and the standard
    // bridge pay from their own escrows — so this funding only backs
    // demonstrations of executeOutput against hand-made vouchers.
    const funding = await wallet.sendTransaction({ to: executor, value: executorFunding });
    await l1.waitForTransactionReceipt({ hash: funding });

    // Tell the guest which contracts to trust across the bridge: an owner
    // call to the routed guest's config contract (EVM-COMPAT §6),
    // registerMessenger(l1Messenger, l1Bridge), carried as an L1 deposit
    // addressed to the config address.
    //
    // The guest cannot have these addresses baked into its genesis state:
    // they do not exist until L1 is deployed, and L1 cannot be deployed
    // after a genesis that names them. So they arrive as an input like any
    // other — one the config contract takes only from the owner address
    // baked into the snapshot. An EOA calling the portal directly is not
    // aliased, so the deposit reaches the guest with `from` set to the owner
    // itself, which is what it checks. Once registered, the guest relays
    // deposits from the aliased L1 messenger through its own
    // L2CrossDomainMessenger and L2StandardBridge predeploys.
    const signer = privateKeyToAccount(asKey(a.deployerKey)).address;
    if (signer.toLowerCase() !== a.guestOwner.toLowerCase()) {
        die(
            `the deploy key is not the guest's owner (${a.guestOwner}); the guest will ignore this registration`,
        );
    }
    const registerConfig = async (data: Hex) => {
        const hash = await wallet.writeContract({
            address: optimismPortal,
            abi: portalAbi,
            functionName: "depositTransaction",
            args: [a.guestConfigAddress, 0n, registerGasLimit, false, data],
        });
        await l1.waitForTransactionReceipt({ hash });
    };
    if (!a.l1CrossDomainMessenger || !a.l1StandardBridge) {
        die("deploy-l1.ts did not record the messenger/bridge addresses; rerun it");
    }
    await registerConfig(
        encodeFunctionData({
            abi: registerAbi,
            functionName: "registerMessenger",
            args: [a.l1CrossDomainMessenger, a.l1StandardBridge],
        }),
    );

    say(`wrote ${paths.outputsAddresses}`);
    for (const [name, address] of deployed) console.log(`  ${name}_ADDRESS=${address}`);
    console.log("  registered the standard messenger with the guest");
}

if (import.meta.main) await deployOutputs();
