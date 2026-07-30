package engineapi

import (
	"net/http"

	"github.com/ethereum/go-ethereum/rpc"

	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/mempool"
)

// NewHandler builds the HTTP handler for one RPC endpoint. withEngine selects
// the authenticated engine port (engine_* + eth_*, JWT-protected when a
// secret is set) versus the public port (eth_* only, no auth).
func NewHandler(c *chain.Chain, pool *mempool.Pool, withEngine bool, jwtSecret []byte) (http.Handler, error) {
	server := rpc.NewServer()
	if err := server.RegisterName("eth", NewEthAPI(c, pool)); err != nil {
		return nil, err
	}
	if withEngine {
		if err := server.RegisterName("engine", NewEngineAPI(c)); err != nil {
			return nil, err
		}
	}
	var handler http.Handler = server
	if withEngine && len(jwtSecret) > 0 {
		handler = JWTHandler(jwtSecret, handler)
	}
	return handler, nil
}
