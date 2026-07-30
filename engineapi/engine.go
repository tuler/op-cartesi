// Package engineapi exposes the chain over the two RPC surfaces op-node and
// friends expect from an execution engine: the authenticated Engine API
// (engine_* namespace) and a minimal eth_* subset.
package engineapi

import (
	"context"
	"errors"

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

// NewPayloadV3 mirrors geth's signature, including the pointer beacon root:
// V3 requires it, so a payload without one is rejected rather than silently
// treated as the zero hash (which would yield a different block hash).
func (e *EngineAPI) NewPayloadV3(ctx context.Context, data engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash) (engine.PayloadStatusV1, error) {
	if beaconRoot == nil {
		return engine.PayloadStatusV1{}, engine.InvalidParams.With(errors.New("nil parentBeaconBlockRoot post-cancun"))
	}
	if len(versionedHashes) != 0 {
		return engine.PayloadStatusV1{}, engine.InvalidParams.With(errors.New("no blob transactions on the L2 chain"))
	}
	return e.chain.ImportPayload(ctx, &data, beaconRoot)
}
