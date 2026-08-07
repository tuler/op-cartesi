package engineapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	abidrive "github.com/tuler/op-cartesi/abi-drive/go"
	drive "github.com/tuler/op-cartesi/accounts-drive/go"
	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
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
