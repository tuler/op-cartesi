// The journaled ledger: the accounts drive plus the in-RAM state that rides
// with it, every mutation undoable back to a mark.
//
// The journal is what makes EVM-COMPAT §5's REVERT outcome possible: the
// router rolls a handler's ledger effects back without a machine rollback,
// then still commits the nonce bump and the fee. Journaling happens at the
// byte layer — a Store wrapper records each write's before-image — so a
// rollback restores the drive bit-exactly with no knowledge of drive
// semantics, and the registry's append-only rule is no special case. In-RAM
// state (token totals, metadata, portals, the fee) journals through the same
// entry list as restore closures.

import {
    AccountsDriveError,
    Drive,
    FileStore,
    MemStore,
    ProfileSparse,
    format as formatDrive,
    open as openDrive,
    type Store,
} from "./accounts/index.ts";
import { addrKey, l2TokenAddress, type Address } from "./addresses.ts";
import { getAddress, toBytes } from "viem";

/** A debit that the balance cannot cover. Distinct from AccountsDriveError:
 * this is application arithmetic, not a drive-format refusal. */
export class InsufficientFunds extends Error {
    constructor(what: string) {
        super(`insufficient ${what}`);
        this.name = "InsufficientFunds";
    }
}

type Restore = () => Promise<void> | void;

export class Journal {
    entries: Restore[] = [];

    note(r: Restore): void {
        this.entries.push(r);
    }

    mark(): number {
        return this.entries.length;
    }

    /** Undoes entries back to (and excluding) `mark`, newest first. */
    async rollbackTo(mark: number): Promise<void> {
        while (this.entries.length > mark) {
            await this.entries.pop()!();
        }
    }

    async rollbackAll(): Promise<void> {
        await this.rollbackTo(0);
    }

    commit(): void {
        this.entries.length = 0;
    }
}

/** JournaledStore records a before-image per write. Reads pass through. */
class JournaledStore implements Store {
    constructor(
        private inner: Store,
        private journal: Journal,
    ) {}

    readAt(off: number, len: number): Promise<Uint8Array> {
        return this.inner.readAt(off, len);
    }

    async writeAt(off: number, bytes: Uint8Array): Promise<void> {
        const before = await this.inner.readAt(off, bytes.length);
        this.journal.note(() => this.inner.writeAt(off, before));
        await this.inner.writeAt(off, bytes);
    }
}

export interface TokenMetadata {
    name: string;
    symbol: string;
    decimals: number;
}

export interface ResolvedFacade {
    id: number;
    l1Token: Address;
    width: number;
}

/** The devnet drive geometry, matching what the Lua app formatted: 1 MiB,
 * profile 2 (sparse), 4096-slot tables, in-header registry of 8. Consensus
 * once the snapshot is stored (ACCOUNTS.md §5.1). */
export const DEVNET_DRIVE_CONFIG = {
    driveLength: 1048576,
    profile: ProfileSparse,
    capacity: 4096,
    registryOffset: 0x100,
    registryCapacity: 8,
    sparseOffset: 266240,
    sparseCapacity: 4096,
};

export class Ledger {
    readonly journal = new Journal();
    drive!: Drive;
    /** The backing store beneath the journal (FileStore in the guest, for
     * sync; MemStore in tests). */
    backing!: Store;

    /** Per-token circulating total (credits minus debits): serves
     * totalSupply(). In RAM — only eth_call reads it, so it stays out of the
     * drive spec (EVM-COMPAT §9). */
    private totals = new Map<number, bigint>();
    private metadata = new Map<number, TokenMetadata>();
    /** Aliased portal address → kind (0 ether, 1 erc20). */
    private portals = new Map<Address, number>();
    /** Derived façade address → token id. */
    private facades = new Map<Address, number>();
    /** The flat per-transaction fee (ACCOUNTS.md §5.7); owner-settable,
     * zero at genesis for the same devnet reason the Lua app documents. */
    fee = 0n;

    static async create(backing: Store): Promise<Ledger> {
        const l = new Ledger();
        l.backing = backing;
        const store = new JournaledStore(backing, l.journal);
        const magic = await backing.readAt(0, 8);
        const formatted = new TextDecoder().decode(magic) === "ctsiacct";
        l.drive = formatted
            ? await openDrive(store)
            : await formatDrive(store, DEVNET_DRIVE_CONFIG);
        // A freshly formatted drive was journaled; the format is genesis
        // state, not an undoable input effect.
        l.journal.commit();
        l.rebuildFacades();
        return l;
    }

    static async openDevice(path: string): Promise<Ledger> {
        return Ledger.create(await FileStore.open(path));
    }

    static async inMemory(): Promise<Ledger> {
        return Ledger.create(new MemStore(DEVNET_DRIVE_CONFIG.driveLength));
    }

    /** Re-derives caches that shadow drive bytes, after a journal rollback
     * may have undone a registration. RAM state journals itself; only the
     * Drive's registry cache and the façade map read from bytes. */
    async afterRollback(): Promise<void> {
        await this.drive.reloadRegistry();
        this.rebuildFacades();
    }

    private rebuildFacades(): void {
        this.facades.clear();
        this.drive.tokens().forEach((t, id) => {
            const l1 = getAddress(`0x${Buffer.from(t.address).toString("hex")}`) as Address;
            this.facades.set(addrKey(l2TokenAddress(l1)), id);
        });
    }

    // ------------------------------------------------------------- accounts

    async account(addr: Address): Promise<{ nonce: bigint; balance: bigint }> {
        const a = await this.drive.getAccount(toBytes(addr));
        return a ? { nonce: a.nonce, balance: a.balance } : { nonce: 0n, balance: 0n };
    }

    async creditEther(addr: Address, amount: bigint): Promise<void> {
        if (amount === 0n) return;
        const a = await this.account(addr);
        await this.drive.setAccount(toBytes(addr), a.nonce, a.balance + amount);
    }

    async debitEther(addr: Address, amount: bigint): Promise<void> {
        if (amount === 0n) return;
        const a = await this.account(addr);
        if (a.balance < amount) throw new InsufficientFunds("ether balance");
        await this.drive.setAccount(toBytes(addr), a.nonce, a.balance - amount);
    }

    /** The one place nonces move: commit of an accepted (or reverted)
     * transaction, together with the fee (EVM-COMPAT §5). The fee is a
     * parameter, not read here: a transaction is charged under the schedule
     * it entered under — a setFee transaction must not be billed by the fee
     * it itself sets. */
    async bumpNonceAndCharge(addr: Address, fee: bigint): Promise<void> {
        const a = await this.account(addr);
        if (a.balance < fee) throw new InsufficientFunds("fee");
        await this.drive.setAccount(toBytes(addr), a.nonce + 1n, a.balance - fee);
    }

    // --------------------------------------------------------------- tokens

    /** Registers a token (width 32: arbitrary devnet ERC-20s get the full
     * uint256, as the Lua app chose), with optional metadata. Throws
     * AccountsDriveError duplicateToken / registryFull. */
    async registerToken(l1Token: Address, metadata?: TokenMetadata): Promise<{ id: number; l2Token: Address }> {
        const id = await this.drive.registerToken(toBytes(l1Token), 32);
        const l2Token = l2TokenAddress(l1Token);
        this.facades.set(addrKey(l2Token), id);
        this.journal.note(() => {
            this.facades.delete(addrKey(l2Token));
        });
        if (metadata) this.setMetadata(id, metadata);
        return { id, l2Token };
    }

    tokenByL2Address(addr: Address): ResolvedFacade | null {
        const id = this.facades.get(addrKey(addr));
        if (id === undefined) return null;
        const t = this.drive.tokens()[id];
        if (!t) return null;
        return {
            id,
            l1Token: getAddress(`0x${Buffer.from(t.address).toString("hex")}`),
            width: t.width,
        };
    }

    tokenByL1Address(l1Token: Address): ResolvedFacade | null {
        const r = this.drive.tokenByAddress(toBytes(l1Token));
        return r ? { id: r.id, l1Token, width: r.width } : null;
    }

    async tokenBalance(addr: Address, id: number): Promise<bigint> {
        return this.drive.getTokenBalance(toBytes(addr), id);
    }

    async creditToken(addr: Address, id: number, amount: bigint): Promise<void> {
        if (amount === 0n) return;
        const b = await this.tokenBalance(addr, id);
        await this.drive.setTokenBalance(toBytes(addr), id, b + amount);
        this.addTotal(id, amount);
    }

    async debitToken(addr: Address, id: number, amount: bigint): Promise<void> {
        if (amount === 0n) return;
        const b = await this.tokenBalance(addr, id);
        if (b < amount) throw new InsufficientFunds("token balance");
        await this.drive.setTokenBalance(toBytes(addr), id, b - amount);
        this.addTotal(id, -amount);
    }

    private addTotal(id: number, delta: bigint): void {
        const before = this.totals.get(id) ?? 0n;
        this.journal.note(() => {
            this.totals.set(id, before);
        });
        this.totals.set(id, before + delta);
    }

    totalSupply(id: number): bigint {
        return this.totals.get(id) ?? 0n;
    }

    setMetadata(id: number, m: TokenMetadata): void {
        const before = this.metadata.get(id);
        this.journal.note(() => {
            if (before) this.metadata.set(id, before);
            else this.metadata.delete(id);
        });
        this.metadata.set(id, m);
    }

    /** Loud placeholders until the owner sets truth (EVM-COMPAT §9): a
     * wrong-decimals default misprices real balances in every wallet. */
    metadataOf(id: number): TokenMetadata {
        return this.metadata.get(id) ?? { name: `UNREGISTERED-${id}`, symbol: `UNREG-${id}`, decimals: 0 };
    }

    // -------------------------------------------------------------- config

    registerPortal(aliasedPortal: Address, kind: number): void {
        const key = addrKey(aliasedPortal);
        const before = this.portals.get(key);
        this.journal.note(() => {
            if (before === undefined) this.portals.delete(key);
            else this.portals.set(key, before);
        });
        this.portals.set(key, kind);
    }

    portalKind(aliasedSender: Address): number | undefined {
        return this.portals.get(addrKey(aliasedSender));
    }

    setFee(fee: bigint): void {
        const before = this.fee;
        this.journal.note(() => {
            this.fee = before;
        });
        this.fee = fee;
    }

    /** Flushes drive bytes into machine state before a yield (spec §10).
     * A MemStore backing has no sync — tests read the bytes directly. */
    async sync(): Promise<void> {
        if (this.backing instanceof FileStore) await this.backing.sync();
    }
}

export { AccountsDriveError };
