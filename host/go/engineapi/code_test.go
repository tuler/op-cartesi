package engineapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/tuler/op-cartesi/host/go/chain"
	"github.com/tuler/op-cartesi/host/go/machine"
	"github.com/tuler/op-cartesi/host/go/mempool"
	abidrive "github.com/tuler/op-cartesi/lib/abi-drive/go"
	drive "github.com/tuler/op-cartesi/lib/accounts-drive/go"
)

const abiDriveBase = uint64(0x90000000000000)

// eth_getCode serves the routed-address markers: a wallet's "is this a
// contract?" check answers yes for everything the guest routes and "0x" for
// EOAs (EVM-COMPAT §10a).
func TestGetCodeServesRoutedMarkers(t *testing.T) {
	config := common.HexToAddress("0xc751000000000000000000000000000000000002")
	counter := common.HexToAddress("0xc0de000000000000000000000000000000000001")
	l1Token := common.HexToAddress("0x00000000000000000000000000000000000070ce")

	abiMem := drive.NewMemStore(262144)
	ad, err := abidrive.Format(abiMem, abidrive.Config{DriveLength: 262144, Capacity: 64, HeapOffset: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.Register(config, abidrive.KindSystem, []byte(`[{"name":"setFee"}]`)); err != nil {
		t.Fatal(err)
	}
	if err := ad.Register(counter, abidrive.KindApp, []byte(`[{"name":"increment"}]`)); err != nil {
		t.Fatal(err)
	}
	accountsMem := drive.NewMemStore(1 << 20)
	d, err := drive.Format(accountsMem, drive.Config{
		DriveLength:      1 << 20,
		Profile:          drive.ProfileSparse,
		Capacity:         4096,
		RegistryOffset:   0x100,
		RegistryCapacity: 8,
		SparseOffset:     266240,
		SparseCapacity:   4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RegisterToken(l1Token, 32); err != nil {
		t.Fatal(err)
	}

	m := machine.NewMock([]byte("code-rpc-test"))
	m.SetAccountsDrive(driveBase, accountsMem.B)
	m.SetAbiDrive(abiDriveBase, abiMem.B)
	pool := mempool.New(64)
	c, err := chain.New(context.Background(), chain.Config{ChainID: 901, GenesisTimestamp: 1000}, m, pool)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(c, pool, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := &rpcClient{t: t, url: srv.URL}

	cases := []struct {
		name string
		addr common.Address
		want string
	}{
		{"system built-in", config, "0xefc75100"},
		{"application", counter, "0xefc75101"},
		{"token façade", chain.L2TokenAddress(l1Token), "0xefc75102"},
		{"EOA", common.HexToAddress("0xa11ce"), "0x"},
	}
	for _, tc := range cases {
		var code hexutil.Bytes
		client.call("eth_getCode", &code, tc.addr, "latest")
		if code.String() != tc.want {
			t.Fatalf("%s: eth_getCode = %s, want %s", tc.name, code, tc.want)
		}
	}

	// cartesi_getContracts serves the same surface with the ABIs embedded as
	// JSON, and names the L1 token behind each façade.
	var listed Contracts
	client.call("cartesi_getContracts", &listed)
	if len(listed.Contracts) != 3 {
		t.Fatalf("contracts %d, want 3: %+v", len(listed.Contracts), listed)
	}
	byAddr := map[common.Address]ContractEntry{}
	for _, e := range listed.Contracts {
		byAddr[e.Address] = e
	}
	if e := byAddr[config]; e.Kind != "system" || string(e.Abi) != `[{"name":"setFee"}]` {
		t.Fatalf("config entry: %+v", e)
	}
	if e := byAddr[counter]; e.Kind != "app" || string(e.Abi) != `[{"name":"increment"}]` {
		t.Fatalf("counter entry: %+v", e)
	}
	facade := byAddr[chain.L2TokenAddress(l1Token)]
	if facade.Kind != "token" || facade.Abi != nil || facade.L1Token == nil || *facade.L1Token != l1Token {
		t.Fatalf("façade entry: %+v", facade)
	}
}

// A mock-machine dev node without drives keeps answering "0x" everywhere
// rather than erroring — the same posture as eth_getBalance's zeros.
func TestGetCodeWithoutDrivesAnswersEmpty(t *testing.T) {
	client, _ := newPublicServer(t)
	var code hexutil.Bytes
	client.call("eth_getCode", &code, common.HexToAddress("0xc0de"), "latest")
	if code.String() != "0x" {
		t.Fatalf("eth_getCode = %s, want 0x", code)
	}
}
