// Package engineapi exposes the chain over the two RPC surfaces op-node and
// friends expect from an execution engine: the authenticated Engine API
// (engine_* namespace) and a minimal eth_* subset.
package engineapi

import (
	"context"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/chain"
)

type EngineAPI struct {
	chain *chain.Chain
}

func NewEngineAPI(c *chain.Chain) *EngineAPI {
	return &EngineAPI{chain: c}
}

func (e *EngineAPI) ForkchoiceUpdatedV3(ctx context.Context, fc engine.ForkchoiceStateV1, attrs *engine.PayloadAttributes) (*engine.ForkChoiceResponse, error) {
	return e.chain.ForkchoiceUpdated(ctx, fc, attrs)
}

func (e *EngineAPI) GetPayloadV3(_ context.Context, id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	envelope, ok := e.chain.Payload(id)
	if !ok {
		return nil, engine.UnknownPayload
	}
	return envelope, nil
}

func (e *EngineAPI) NewPayloadV3(ctx context.Context, data engine.ExecutableData, _ []common.Hash, beaconRoot common.Hash) (engine.PayloadStatusV1, error) {
	return e.chain.ImportPayload(ctx, &data, &beaconRoot)
}
