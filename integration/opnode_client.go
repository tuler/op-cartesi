package integration

import (
	"context"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// engineClient issues the same Engine API calls, with the same method names,
// argument order and types, as op-node's sources.EngineAPIClient at v1.19.2.
//
// op-node's own client is not imported directly because op-service/sources
// transitively pulls in op-core/superchain, whose //go:embed of a generated
// superchain-configs.zip is absent from the published module — that package
// cannot be built from the module cache. The types crossing the wire here
// (eth.PayloadAttributes, eth.ExecutionPayload, eth.ForkchoiceState, ...) are
// still op-node's genuine types from op-service/eth, so the serialization
// being exercised is the real thing. Swap this for sources.EngineAPIClient if
// the upstream module becomes consumable.
type engineClient struct {
	rpc *rpc.Client
}

func newEngineClient(c *rpc.Client) *engineClient {
	return &engineClient{rpc: c}
}

func (e *engineClient) ForkchoiceUpdate(ctx context.Context, fc *eth.ForkchoiceState, attrs *eth.PayloadAttributes) (*eth.ForkchoiceUpdatedResult, error) {
	var result eth.ForkchoiceUpdatedResult
	if err := e.rpc.CallContext(ctx, &result, "engine_forkchoiceUpdatedV3", fc, attrs); err != nil {
		return nil, err
	}
	return &result, nil
}

// The payload methods are V4 only, mirroring op-node's own version selection:
// Config.NewPayloadVersion and GetPayloadVersion switch to V4 at Isthmus, and
// this chain is Isthmus from genesis. Forkchoice stays V3, which is what
// op-node calls for every fork from Ecotone on.
func (e *engineClient) GetPayload(ctx context.Context, info eth.PayloadInfo) (*eth.ExecutionPayloadEnvelope, error) {
	var result eth.ExecutionPayloadEnvelope
	if err := e.rpc.CallContext(ctx, &result, "engine_getPayloadV4", info.ID); err != nil {
		return nil, err
	}
	return &result, nil
}

func (e *engineClient) NewPayload(ctx context.Context, payload *eth.ExecutionPayload, parentBeaconBlockRoot *common.Hash) (*eth.PayloadStatusV1, error) {
	var result eth.PayloadStatusV1
	err := e.rpc.CallContext(ctx, &result, "engine_newPayloadV4", payload, []common.Hash{}, parentBeaconBlockRoot, []hexutil.Bytes{})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
