// Package drive implements the Cartesi Accounts Drive Format, version 1
// (docs/ACCOUNTS-DRIVE-SPEC.md): a fixed-layout accounts database inside a
// Cartesi Machine flash drive, written by the guest as part of the state
// transition and readable — and provable — from outside without executing
// the machine.
//
// The same code serves both sides of the drive. A guest opens it over a
// FileStore on the raw device; a host opens it over a ReadMemoryStore against
// a parked machine, or a MemStore holding a copied image. All layout,
// hashing and probing logic is shared, which is what keeps two independent
// parties byte-for-byte agreed about what the drive means.
package drive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
)

// Profiles (spec §5).
const (
	ProfileSingleAsset = 0
	ProfileWide        = 1
	ProfileSparse      = 2
)

// Magic is the header's first 8 bytes, ASCII "ctsiacct".
var Magic = [8]byte{'c', 't', 's', 'i', 'a', 'c', 'c', 't'}

// Version is the format version this package implements.
const Version = 1

const headerSize = 4096

// Header field offsets (spec §5).
const (
	offMagic           = 0x00
	offVersion         = 0x08
	offFlags           = 0x0c
	offCapacity        = 0x10
	offTableOffset     = 0x18
	offLoadLimit       = 0x20
	offLiveCount       = 0x28
	offSeed            = 0x30
	offProfile         = 0x50
	offSlotSize        = 0x54
	offRegistryOffset  = 0x58
	offRegistryCap     = 0x60
	offTokenCount      = 0x68
	offSparseOffset    = 0x70
	offSparseCapacity  = 0x78
	offSparseLoadLimit = 0x80
	offSparseLiveCount = 0x88
	offSparseSlotSize  = 0x90
	minRegistryOffset  = 0x100
)

// Errors a caller is expected to branch on. In a guest, ErrTableFull,
// ErrRegistryFull and ErrOverflow are the conditions the spec requires to be
// answered by rejecting the input.
var (
	ErrNotFormatted   = errors.New("not an accounts drive (bad magic)")
	ErrVersion        = errors.New("unsupported accounts drive version")
	ErrTableFull      = errors.New("table at load limit")
	ErrRegistryFull   = errors.New("token registry full")
	ErrOverflow       = errors.New("balance exceeds declared width")
	ErrZeroAddress    = errors.New("zero address is invalid")
	ErrNonceProtected = errors.New("record has nonzero nonce (replay protection); pass force to delete")
	ErrProfile        = errors.New("operation not available in this profile")
	ErrUnknownToken   = errors.New("token not registered")
	ErrDuplicateToken = errors.New("token already registered")
	ErrBadWidth       = errors.New("balance width must be 8, 16 or 32")
)

// Config is the geometry of a drive to be formatted. Every field is a
// deployment constant (spec §5); Format validates the lot.
type Config struct {
	DriveLength uint64
	Capacity    uint64
	LoadLimit   uint64 // 0 → ⅞ of Capacity
	Seed        [32]byte
	Profile     uint32
	SlotSize    uint32 // 0 → 64; must be 64 unless Profile == ProfileWide
	TableOffset uint64 // 0 → 4096

	RegistryOffset   uint64 // 0 → none (allowed only in profile 0)
	RegistryCapacity uint64

	SparseOffset    uint64
	SparseCapacity  uint64
	SparseLoadLimit uint64 // 0 → ⅞ of SparseCapacity
	SparseSlotSize  uint32 // 0 → 64
}

type region struct {
	name     string
	off, len uint64
}

func (c *Config) defaults() {
	if c.SlotSize == 0 {
		c.SlotSize = 64
	}
	if c.TableOffset == 0 {
		c.TableOffset = headerSize
	}
	if c.LoadLimit == 0 && c.Capacity > 0 {
		c.LoadLimit = c.Capacity - c.Capacity/8
	}
	if c.Profile == ProfileSparse {
		if c.SparseSlotSize == 0 {
			c.SparseSlotSize = 64
		}
		if c.SparseLoadLimit == 0 && c.SparseCapacity > 0 {
			c.SparseLoadLimit = c.SparseCapacity - c.SparseCapacity/8
		}
	}
}

func (c *Config) validate() error {
	if c.DriveLength == 0 || c.DriveLength&(c.DriveLength-1) != 0 {
		return fmt.Errorf("drive length %d is not a power of two", c.DriveLength)
	}
	if c.Profile > ProfileSparse {
		return fmt.Errorf("unknown profile %d", c.Profile)
	}
	ss := uint64(c.SlotSize)
	switch {
	case c.Profile == ProfileWide:
		if ss < 128 || ss&(ss-1) != 0 {
			return fmt.Errorf("wide slot size %d: want a power of two ≥ 128", ss)
		}
	case ss != 64:
		return fmt.Errorf("slot size %d: must be 64 outside the wide profile", ss)
	}
	if c.Capacity < 2 {
		return fmt.Errorf("capacity %d < 2", c.Capacity)
	}
	if c.LoadLimit < 1 || c.LoadLimit >= c.Capacity {
		return fmt.Errorf("load limit %d outside [1, capacity-1]", c.LoadLimit)
	}
	if c.TableOffset < headerSize || c.TableOffset%ss != 0 {
		return fmt.Errorf("table offset %d: want ≥ %d and a multiple of the slot size", c.TableOffset, headerSize)
	}
	regions := []region{
		{"header", 0, headerSize},
		{"accounts table", c.TableOffset, c.Capacity * ss},
	}
	if c.Profile != ProfileSingleAsset && c.RegistryOffset == 0 {
		return fmt.Errorf("profile %d requires a token registry", c.Profile)
	}
	if c.RegistryOffset != 0 {
		if c.RegistryCapacity == 0 || c.RegistryCapacity > 65536 {
			return fmt.Errorf("registry capacity %d outside [1, 65536]", c.RegistryCapacity)
		}
		if c.RegistryOffset%32 != 0 {
			return fmt.Errorf("registry offset %d not a multiple of 32", c.RegistryOffset)
		}
		end := c.RegistryOffset + 32*c.RegistryCapacity
		if c.RegistryOffset < headerSize { // in-header placement (spec §5)
			if c.RegistryOffset < minRegistryOffset || end > headerSize {
				return fmt.Errorf("in-header registry [%#x,%#x) outside [%#x,%#x)", c.RegistryOffset, end, minRegistryOffset, headerSize)
			}
		} else {
			regions = append(regions, region{"registry", c.RegistryOffset, 32 * c.RegistryCapacity})
		}
	}
	if c.Profile == ProfileSparse {
		sss := uint64(c.SparseSlotSize)
		if sss != 64 && sss != 32 {
			return fmt.Errorf("sparse slot size %d: want 64 or 32", sss)
		}
		if c.SparseCapacity < 2 {
			return fmt.Errorf("sparse capacity %d < 2", c.SparseCapacity)
		}
		if c.SparseLoadLimit < 1 || c.SparseLoadLimit >= c.SparseCapacity {
			return fmt.Errorf("sparse load limit %d outside [1, capacity-1]", c.SparseLoadLimit)
		}
		if c.SparseOffset < headerSize || c.SparseOffset%sss != 0 {
			return fmt.Errorf("sparse offset %d: want ≥ %d and a multiple of the slot size", c.SparseOffset, headerSize)
		}
		regions = append(regions, region{"sparse table", c.SparseOffset, c.SparseCapacity * sss})
	}
	for _, r := range regions {
		if r.off+r.len > c.DriveLength || r.off+r.len < r.off {
			return fmt.Errorf("%s [%d,%d) outside drive of %d bytes", r.name, r.off, r.off+r.len, c.DriveLength)
		}
	}
	for i, a := range regions {
		for _, b := range regions[i+1:] {
			if a.off < b.off+b.len && b.off < a.off+a.len {
				return fmt.Errorf("%s overlaps %s", a.name, b.name)
			}
		}
	}
	return nil
}

// Token is one registry entry (spec §8).
type Token struct {
	Address [20]byte
	Width   int // balance width in bytes: 8, 16 or 32
}

// Drive is an open accounts drive. Geometry is cached at Open (it is
// immutable by spec); the registry is cached too and refreshed on writes
// through this instance — a host watching a drive someone else appends to
// should call ReloadRegistry when the token count moves.
type Drive struct {
	store  Store
	cfg    Config
	tokens []Token
	colOff []uint32 // wide profile: column byte offset per token id
}

// Format writes a v1 header (and nothing else) describing cfg onto a store
// that must be all-zero, then opens it. It is how a snapshot build formats
// the drive image, and how tests build drives from nothing.
func Format(s Store, cfg Config) (*Drive, error) {
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	h := make([]byte, headerSize)
	copy(h[offMagic:], Magic[:])
	binary.LittleEndian.PutUint32(h[offVersion:], Version)
	binary.LittleEndian.PutUint64(h[offCapacity:], cfg.Capacity)
	binary.LittleEndian.PutUint64(h[offTableOffset:], cfg.TableOffset)
	binary.LittleEndian.PutUint64(h[offLoadLimit:], cfg.LoadLimit)
	copy(h[offSeed:], cfg.Seed[:])
	binary.LittleEndian.PutUint32(h[offProfile:], cfg.Profile)
	binary.LittleEndian.PutUint32(h[offSlotSize:], cfg.SlotSize)
	binary.LittleEndian.PutUint64(h[offRegistryOffset:], cfg.RegistryOffset)
	binary.LittleEndian.PutUint64(h[offRegistryCap:], cfg.RegistryCapacity)
	if cfg.Profile == ProfileSparse {
		binary.LittleEndian.PutUint64(h[offSparseOffset:], cfg.SparseOffset)
		binary.LittleEndian.PutUint64(h[offSparseCapacity:], cfg.SparseCapacity)
		binary.LittleEndian.PutUint64(h[offSparseLoadLimit:], cfg.SparseLoadLimit)
		binary.LittleEndian.PutUint32(h[offSparseSlotSize:], cfg.SparseSlotSize)
	}
	if err := s.WriteAt(0, h); err != nil {
		return nil, err
	}
	return Open(s)
}

// Open reads and validates the header, caches the geometry and the registry.
func Open(s Store) (*Drive, error) {
	h := make([]byte, headerSize)
	if err := s.ReadAt(0, h); err != nil {
		return nil, err
	}
	if !bytes.Equal(h[offMagic:offMagic+8], Magic[:]) {
		return nil, ErrNotFormatted
	}
	if v := binary.LittleEndian.Uint32(h[offVersion:]); v != Version {
		return nil, fmt.Errorf("%w: %d", ErrVersion, v)
	}
	if f := binary.LittleEndian.Uint32(h[offFlags:]); f != 0 {
		return nil, fmt.Errorf("%w: nonzero flags %#x", ErrVersion, f)
	}
	d := &Drive{store: s}
	c := &d.cfg
	c.Capacity = binary.LittleEndian.Uint64(h[offCapacity:])
	c.TableOffset = binary.LittleEndian.Uint64(h[offTableOffset:])
	c.LoadLimit = binary.LittleEndian.Uint64(h[offLoadLimit:])
	copy(c.Seed[:], h[offSeed:offSeed+32])
	c.Profile = binary.LittleEndian.Uint32(h[offProfile:])
	c.SlotSize = binary.LittleEndian.Uint32(h[offSlotSize:])
	if c.SlotSize == 0 {
		c.SlotSize = 64
	}
	c.RegistryOffset = binary.LittleEndian.Uint64(h[offRegistryOffset:])
	c.RegistryCapacity = binary.LittleEndian.Uint64(h[offRegistryCap:])
	c.SparseOffset = binary.LittleEndian.Uint64(h[offSparseOffset:])
	c.SparseCapacity = binary.LittleEndian.Uint64(h[offSparseCapacity:])
	c.SparseLoadLimit = binary.LittleEndian.Uint64(h[offSparseLoadLimit:])
	c.SparseSlotSize = binary.LittleEndian.Uint32(h[offSparseSlotSize:])
	if err := d.ReloadRegistry(); err != nil {
		return nil, err
	}
	return d, nil
}

// Config returns the drive's geometry.
func (d *Drive) Config() Config { return d.cfg }

// Tokens returns the cached registry, in id order.
func (d *Drive) Tokens() []Token { return d.tokens }

// ReloadRegistry re-reads the registry (and the wide column offsets).
func (d *Drive) ReloadRegistry() error {
	d.tokens = nil
	d.colOff = nil
	if d.cfg.RegistryOffset == 0 {
		return nil
	}
	n, err := d.counter(offTokenCount)
	if err != nil {
		return err
	}
	if n > d.cfg.RegistryCapacity {
		return fmt.Errorf("token count %d exceeds registry capacity %d", n, d.cfg.RegistryCapacity)
	}
	col := uint32(64)
	for i := uint64(0); i < n; i++ {
		e := make([]byte, 32)
		if err := d.store.ReadAt(d.cfg.RegistryOffset+32*i, e); err != nil {
			return err
		}
		t := Token{Width: int(e[20])}
		copy(t.Address[:], e[:20])
		if t.Width != 8 && t.Width != 16 && t.Width != 32 {
			return fmt.Errorf("registry entry %d: %w (%d)", i, ErrBadWidth, t.Width)
		}
		d.tokens = append(d.tokens, t)
		d.colOff = append(d.colOff, col)
		col += uint32(t.Width)
	}
	return nil
}

// counter reads one of the header's live counters.
func (d *Drive) counter(off uint64) (uint64, error) {
	b := make([]byte, 8)
	if err := d.store.ReadAt(off, b); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (d *Drive) setCounter(off, v uint64) error {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return d.store.WriteAt(off, b)
}

// LiveCount returns the accounts table's occupied-slot count.
func (d *Drive) LiveCount() (uint64, error) { return d.counter(offLiveCount) }

// SparseLiveCount returns the sparse table's occupied-slot count.
func (d *Drive) SparseLiveCount() (uint64, error) { return d.counter(offSparseLiveCount) }

// TokenCount returns the number of registered tokens.
func (d *Drive) TokenCount() (uint64, error) { return d.counter(offTokenCount) }

// ---------------------------------------------------------------- tables

// table describes one of the two hash tables so the probing code (spec §6)
// is written once. key() extracts the record's key; a slot is live iff the
// key's address part is nonzero.
type table struct {
	base     uint64
	capacity uint64
	slotSize uint64
	countOff uint64 // header offset of the live counter
	limit    uint64
	key      func(slot []byte) []byte
	liveOff  int // offset of the address within the slot, for the liveness test
}

func (d *Drive) accountsTable() table {
	return table{
		base: d.cfg.TableOffset, capacity: d.cfg.Capacity, slotSize: uint64(d.cfg.SlotSize),
		countOff: offLiveCount, limit: d.cfg.LoadLimit,
		key: func(s []byte) []byte { return s[0:20] }, liveOff: 0,
	}
}

func (d *Drive) sparseTable() table {
	return table{
		base: d.cfg.SparseOffset, capacity: d.cfg.SparseCapacity, slotSize: uint64(d.cfg.SparseSlotSize),
		countOff: offSparseLiveCount, limit: d.cfg.SparseLoadLimit,
		key: func(s []byte) []byte { return append(append([]byte{}, s[0:2]...), s[4:24]...) }, liveOff: 4,
	}
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// home implements spec §6.1.
func (d *Drive) home(key []byte, capacity uint64) uint64 {
	h := Keccak256(d.cfg.Seed[:], key)
	return binary.LittleEndian.Uint64(h[:8]) % capacity
}

func (d *Drive) readSlot(t table, i uint64) ([]byte, error) {
	s := make([]byte, t.slotSize)
	err := d.store.ReadAt(t.base+t.slotSize*i, s)
	return s, err
}

func (d *Drive) writeSlot(t table, i uint64, s []byte) error {
	return d.store.WriteAt(t.base+t.slotSize*i, s)
}

func (d *Drive) live(t table, slot []byte) bool { return !isZero(slot[t.liveOff : t.liveOff+20]) }

func (d *Drive) displacement(t table, slot []byte, at uint64) uint64 {
	return (at + t.capacity - d.home(t.key(slot), t.capacity)) % t.capacity
}

// lookup implements spec §6.2, returning the slot index when found.
func (d *Drive) lookup(t table, key []byte) (uint64, []byte, bool, error) {
	h := d.home(key, t.capacity)
	for dist := uint64(0); dist < t.capacity; dist++ {
		i := (h + dist) % t.capacity
		slot, err := d.readSlot(t, i)
		if err != nil {
			return 0, nil, false, err
		}
		if !d.live(t, slot) {
			return 0, nil, false, nil
		}
		if bytes.Equal(t.key(slot), key) {
			return i, slot, true, nil
		}
		if d.displacement(t, slot, i) < dist {
			return 0, nil, false, nil
		}
	}
	return 0, nil, false, nil // unreachable while load limit < capacity
}

// insert implements spec §6.3. The caller has checked the key is absent and
// the load limit has room; insert bumps the live counter itself.
func (d *Drive) insert(t table, rec []byte) error {
	carry := rec
	h := d.home(t.key(carry), t.capacity)
	dist := uint64(0)
	for {
		i := (h + dist) % t.capacity
		slot, err := d.readSlot(t, i)
		if err != nil {
			return err
		}
		if !d.live(t, slot) {
			if err := d.writeSlot(t, i, carry); err != nil {
				return err
			}
			n, err := d.counter(t.countOff)
			if err != nil {
				return err
			}
			return d.setCounter(t.countOff, n+1)
		}
		if disp := d.displacement(t, slot, i); disp < dist {
			if err := d.writeSlot(t, i, carry); err != nil {
				return err
			}
			carry = slot
			h = d.home(t.key(carry), t.capacity)
			dist = disp
		}
		dist++
	}
}

// remove implements spec §6.4 (backward shift) from a known slot index.
func (d *Drive) remove(t table, i uint64) error {
	for {
		j := (i + 1) % t.capacity
		next, err := d.readSlot(t, j)
		if err != nil {
			return err
		}
		if !d.live(t, next) || d.displacement(t, next, j) == 0 {
			if err := d.writeSlot(t, i, make([]byte, t.slotSize)); err != nil {
				return err
			}
			n, err := d.counter(t.countOff)
			if err != nil {
				return err
			}
			return d.setCounter(t.countOff, n-1)
		}
		if err := d.writeSlot(t, i, next); err != nil {
			return err
		}
		i = j
	}
}

// ---------------------------------------------------------------- accounts

// Account is the fixed 64-byte record of spec §7, decoded.
type Account struct {
	Address [20]byte
	Nonce   uint64
	Balance *big.Int
}

// GetAccount looks an account up; found is false when the address has no
// record (which is a proven statement, not a failure).
func (d *Drive) GetAccount(addr [20]byte) (a Account, found bool, err error) {
	if isZero(addr[:]) {
		return a, false, ErrZeroAddress
	}
	_, slot, ok, err := d.lookup(d.accountsTable(), addr[:])
	if err != nil || !ok {
		return a, false, err
	}
	a.Address = addr
	a.Nonce = binary.LittleEndian.Uint64(slot[24:32])
	a.Balance = new(big.Int).SetBytes(slot[32:64])
	return a, true, nil
}

// SetAccount writes an account's nonce and balance, inserting the record if
// the address has none. ErrTableFull means the input carrying this change
// must be rejected (spec §6.3).
func (d *Drive) SetAccount(addr [20]byte, nonce uint64, balance *big.Int) error {
	if isZero(addr[:]) {
		return ErrZeroAddress
	}
	rec := make([]byte, 64)
	copy(rec[0:20], addr[:])
	binary.LittleEndian.PutUint64(rec[24:32], nonce)
	if err := putBE(rec[32:64], balance); err != nil {
		return err
	}
	t := d.accountsTable()
	i, slot, ok, err := d.lookup(t, addr[:])
	if err != nil {
		return err
	}
	if ok {
		// Update in place, touching only the 64-byte record: in the wide
		// profile the rest of the slot is token columns.
		copy(slot[:64], rec)
		return d.writeSlot(t, i, slot)
	}
	n, err := d.counter(t.countOff)
	if err != nil {
		return err
	}
	if n >= t.limit {
		return ErrTableFull
	}
	full := make([]byte, t.slotSize)
	copy(full, rec)
	return d.insert(t, full)
}

// DeleteAccount removes an account's record. A record with a nonzero nonce
// is refused unless force is set: deleting it re-enables replay of that
// account's past transactions (spec §7). In the wide profile the record's
// token columns go with it. Returns false when there was no record.
func (d *Drive) DeleteAccount(addr [20]byte, force bool) (bool, error) {
	if isZero(addr[:]) {
		return false, ErrZeroAddress
	}
	t := d.accountsTable()
	i, slot, ok, err := d.lookup(t, addr[:])
	if err != nil || !ok {
		return false, err
	}
	if !force && !isZero(slot[24:32]) {
		return false, ErrNonceProtected
	}
	return true, d.remove(t, i)
}

// ---------------------------------------------------------------- registry

// TokenByAddress resolves a token address to its id.
func (d *Drive) TokenByAddress(addr [20]byte) (id uint16, tok Token, found bool) {
	for i, t := range d.tokens {
		if t.Address == addr {
			return uint16(i), t, true
		}
	}
	return 0, Token{}, false
}

// RegisterToken appends a token to the registry and returns its id. In the
// wide profile the new column must fit the slot (spec §7); everywhere the
// registry must have room (spec §8). Both failures mean rejecting the input.
func (d *Drive) RegisterToken(addr [20]byte, width int) (uint16, error) {
	if d.cfg.RegistryOffset == 0 {
		return 0, ErrProfile
	}
	if width != 8 && width != 16 && width != 32 {
		return 0, ErrBadWidth
	}
	if _, _, dup := d.TokenByAddress(addr); dup {
		return 0, ErrDuplicateToken
	}
	n := uint64(len(d.tokens))
	if n >= d.cfg.RegistryCapacity {
		return 0, ErrRegistryFull
	}
	if d.cfg.Profile == ProfileWide {
		sum := uint32(64)
		for _, t := range d.tokens {
			sum += uint32(t.Width)
		}
		if sum+uint32(width) > d.cfg.SlotSize {
			return 0, fmt.Errorf("%w: columns would need %d bytes in a %d-byte slot", ErrTableFull, sum+uint32(width), d.cfg.SlotSize)
		}
	}
	if d.cfg.Profile == ProfileSparse && d.cfg.SparseSlotSize == 32 && width != 8 {
		return 0, fmt.Errorf("%w: 32-byte sparse slots require width 8", ErrBadWidth)
	}
	e := make([]byte, 32)
	copy(e[:20], addr[:])
	e[20] = byte(width)
	if err := d.store.WriteAt(d.cfg.RegistryOffset+32*n, e); err != nil {
		return 0, err
	}
	if err := d.setCounter(offTokenCount, n+1); err != nil {
		return 0, err
	}
	col := uint32(64)
	if len(d.colOff) > 0 {
		col = d.colOff[len(d.colOff)-1] + uint32(d.tokens[len(d.tokens)-1].Width)
	}
	d.tokens = append(d.tokens, Token{Address: addr, Width: width})
	d.colOff = append(d.colOff, col)
	return uint16(n), nil
}

// ------------------------------------------------------------ token balances

// GetTokenBalance reads an account's balance of a registered token. An
// absent record (or column of an absent account) is zero, not an error.
func (d *Drive) GetTokenBalance(addr [20]byte, id uint16) (*big.Int, error) {
	if isZero(addr[:]) {
		return nil, ErrZeroAddress
	}
	if uint64(id) >= uint64(len(d.tokens)) {
		return nil, ErrUnknownToken
	}
	switch d.cfg.Profile {
	case ProfileWide:
		_, slot, ok, err := d.lookup(d.accountsTable(), addr[:])
		if err != nil || !ok {
			return new(big.Int), err
		}
		off := d.colOff[id]
		return new(big.Int).SetBytes(slot[off : off+uint32(d.tokens[id].Width)]), nil
	case ProfileSparse:
		_, slot, ok, err := d.lookup(d.sparseTable(), sparseKey(id, addr))
		if err != nil || !ok {
			return new(big.Int), err
		}
		if d.cfg.SparseSlotSize == 32 {
			return new(big.Int).SetBytes(slot[24:32]), nil
		}
		return new(big.Int).SetBytes(slot[32:64]), nil
	default:
		return nil, ErrProfile
	}
}

// SetTokenBalance writes an account's balance of a registered token.
// ErrOverflow (value exceeds the token's declared width) and ErrTableFull
// both mean rejecting the input. In the wide profile a missing account
// record is created with a zero nonce and native balance; in the sparse
// profile a balance reaching zero deletes its record (spec §9).
func (d *Drive) SetTokenBalance(addr [20]byte, id uint16, value *big.Int) error {
	if isZero(addr[:]) {
		return ErrZeroAddress
	}
	if uint64(id) >= uint64(len(d.tokens)) {
		return ErrUnknownToken
	}
	if value.Sign() < 0 || value.BitLen() > 8*d.tokens[id].Width {
		return ErrOverflow
	}
	switch d.cfg.Profile {
	case ProfileWide:
		t := d.accountsTable()
		i, slot, ok, err := d.lookup(t, addr[:])
		if err != nil {
			return err
		}
		if !ok {
			if value.Sign() == 0 {
				return nil
			}
			n, err := d.counter(t.countOff)
			if err != nil {
				return err
			}
			if n >= t.limit {
				return ErrTableFull
			}
			full := make([]byte, t.slotSize)
			copy(full[0:20], addr[:])
			off := d.colOff[id]
			value.FillBytes(full[off : off+uint32(d.tokens[id].Width)])
			return d.insert(t, full)
		}
		off := d.colOff[id]
		value.FillBytes(slot[off : off+uint32(d.tokens[id].Width)])
		return d.writeSlot(t, i, slot)
	case ProfileSparse:
		t := d.sparseTable()
		key := sparseKey(id, addr)
		i, slot, ok, err := d.lookup(t, key)
		if err != nil {
			return err
		}
		if value.Sign() == 0 {
			if !ok {
				return nil
			}
			return d.remove(t, i)
		}
		if ok {
			d.putSparseBalance(slot, value)
			return d.writeSlot(t, i, slot)
		}
		n, err := d.counter(t.countOff)
		if err != nil {
			return err
		}
		if n >= t.limit {
			return ErrTableFull
		}
		rec := make([]byte, t.slotSize)
		binary.LittleEndian.PutUint16(rec[0:2], id)
		copy(rec[4:24], addr[:])
		d.putSparseBalance(rec, value)
		return d.insert(t, rec)
	default:
		return ErrProfile
	}
}

func (d *Drive) putSparseBalance(slot []byte, v *big.Int) {
	if d.cfg.SparseSlotSize == 32 {
		v.FillBytes(slot[24:32])
	} else {
		v.FillBytes(slot[32:64])
	}
}

func sparseKey(id uint16, addr [20]byte) []byte {
	k := make([]byte, 22)
	binary.LittleEndian.PutUint16(k[0:2], id)
	copy(k[2:], addr[:])
	return k
}

// putBE writes a non-negative big.Int as big-endian into buf, or ErrOverflow.
func putBE(buf []byte, v *big.Int) error {
	if v == nil {
		return nil
	}
	if v.Sign() < 0 || v.BitLen() > 8*len(buf) {
		return ErrOverflow
	}
	v.FillBytes(buf)
	return nil
}
