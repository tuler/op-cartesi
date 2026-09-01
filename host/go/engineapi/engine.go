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

	"github.com/tuler/op-cartesi/host/go/chain"
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

// GetPayloadV4 serves the payload built for a forkchoice update. Only the V4
// form is served: op-node selects the payload-method version by fork, and this
// chain is Isthmus from genesis, so it never calls V3.
func (e *EngineAPI) GetPayloadV4(_ context.Context, id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	envelope, ok := e.chain.Payload(id)
	if !ok {
		return nil, engine.UnknownPayload
	}
	return envelope, nil
}

// NewPayloadV4 is the Isthmus form, and the only one served. The beacon root
// is a pointer, mirroring geth: it is required, so a payload without one is
// rejected rather than silently treated as the zero hash, which would yield a
// different block hash. The execution-requests argument is always empty — this
// chain has no EL-triggered requests, and op-node reconstructs the header with
// an empty requests hash to match.
func (e *EngineAPI) NewPayloadV4(ctx context.Context, data engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash, executionRequests []hexutil.Bytes) (engine.PayloadStatusV1, error) {
	if err := checkNewPayloadParams(versionedHashes, beaconRoot); err != nil {
		return engine.PayloadStatusV1{}, err
	}
	if len(executionRequests) != 0 {
		return engine.PayloadStatusV1{}, engine.InvalidParams.With(errors.New("execution requests are not produced by this chain"))
	}
	return e.chain.ImportPayload(ctx, &data, beaconRoot)
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
