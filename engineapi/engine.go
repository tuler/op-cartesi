// Package engineapi exposes the chain over the two RPC surfaces op-node and
// friends expect from an execution engine: the authenticated Engine API
// (engine_* namespace) and a minimal eth_* subset.
package engineapi

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

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
	if err := checkNewPayloadParams(versionedHashes, beaconRoot); err != nil {
		return engine.PayloadStatusV1{}, err
	}
	return e.chain.ImportPayload(ctx, &data, beaconRoot)
}

// NewPayloadV4 is the Isthmus-and-later form. It differs from V3 only by the
// execution-requests argument, which is always empty here: this chain has no
// EL-triggered requests, and op-node reconstructs the header with an empty
// requests hash to match.
func (e *EngineAPI) NewPayloadV4(ctx context.Context, data engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash, executionRequests []hexutil.Bytes) (engine.PayloadStatusV1, error) {
	if err := checkNewPayloadParams(versionedHashes, beaconRoot); err != nil {
		return engine.PayloadStatusV1{}, err
	}
	if len(executionRequests) != 0 {
		return engine.PayloadStatusV1{}, engine.InvalidParams.With(errors.New("execution requests are not produced by this chain"))
	}
	return e.chain.ImportPayload(ctx, &data, beaconRoot)
}

// GetPayloadV4 serves the Isthmus-and-later form. The envelope shape is
// unchanged; the withdrawals root travels inside the payload.
func (e *EngineAPI) GetPayloadV4(ctx context.Context, id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	return e.GetPayloadV3(ctx, id)
}

func checkNewPayloadParams(versionedHashes []common.Hash, beaconRoot *common.Hash) error {
	if beaconRoot == nil {
		return engine.InvalidParams.With(errors.New("nil parentBeaconBlockRoot post-cancun"))
	}
	if len(versionedHashes) != 0 {
		return engine.InvalidParams.With(errors.New("no blob transactions on the L2 chain"))
	}
	return nil
}
