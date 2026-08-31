package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/rollup"
)

// genesisCommand boots the machine, derives the L2 genesis block from its root
// hash, and writes the rollup.json op-node needs. The same chain flags must be
// passed here and to `run`, or op-node will be configured for a genesis block
// the node does not serve.
func genesisCommand(args []string) error {
	fs := flag.NewFlagSet("genesis", flag.ExitOnError)
	var cf chainFlags
	cf.register(fs)
	var (
		out            = fs.String("out", "-", "output file for rollup.json, or - for stdout")
		l1ChainID      = fs.Uint64("l1.chain-id", 900, "L1 chain id")
		l1GenesisHash  = fs.String("l1.genesis-hash", "", "hash of the L1 block the rollup starts after (required)")
		l1GenesisNum   = fs.Uint64("l1.genesis-number", 0, "number of the L1 block the rollup starts after")
		blockTime      = fs.Uint64("block-time", 2, "seconds per L2 block")
		seqDrift       = fs.Uint64("max-sequencer-drift", 600, "max sequencer drift in seconds")
		seqWindow      = fs.Uint64("seq-window-size", 3600, "sequencing window size in L1 blocks")
		channelTimeout = fs.Uint64("channel-timeout", 300, "channel timeout in L1 blocks")
		batcher        = fs.String("batcher-address", "", "L1 address the batcher sends batches from (required)")
		batchInbox     = fs.String("batch-inbox-address", "", "L1 batch inbox address (required)")
		portal         = fs.String("deposit-contract-address", "", "L1 OptimismPortal address (required)")
		sysConfig      = fs.String("l1-system-config-address", "", "L1 SystemConfig address (required)")
		// These belong to the deployed SystemConfig, not to this chain. op-node
		// starts from the genesis system config in rollup.json and then applies
		// SystemConfig updates observed on L1, so a rollup.json that disagrees
		// with the contract begins on values L1 later contradicts.
		baseFeeScalar     = fs.Uint64("base-fee-scalar", rollup.DefaultBaseFeeScalar, "L1 base fee scalar from the deployed SystemConfig")
		blobBaseFeeScalar = fs.Uint64("blob-base-fee-scalar", rollup.DefaultBlobBaseFeeScalar, "L1 blob base fee scalar from the deployed SystemConfig")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	required := map[string]string{
		"-l1.genesis-hash":          *l1GenesisHash,
		"-batcher-address":          *batcher,
		"-batch-inbox-address":      *batchInbox,
		"-deposit-contract-address": *portal,
		"-l1-system-config-address": *sysConfig,
	}
	for flagName, value := range required {
		if value == "" {
			return fmt.Errorf("%s is required; it comes from the L1 contract deployment", flagName)
		}
	}

	ctx := context.Background()
	m, err := cf.openMachine(ctx)
	if err != nil {
		return err
	}
	defer m.Close(ctx)

	cfg, err := cf.chainConfig()
	if err != nil {
		return err
	}
	c, err := chain.New(ctx, cfg, m, nil)
	if err != nil {
		return err
	}
	genesis := c.HeadBlock()

	params := rollup.Params{
		L1ChainID: *l1ChainID,
		L2ChainID: cfg.ChainID,
		L1Genesis: rollup.BlockID{
			Hash:   common.HexToHash(*l1GenesisHash),
			Number: *l1GenesisNum,
		},
		BlockTime:              *blockTime,
		SequencerDrift:         *seqDrift,
		SeqWindowSize:          *seqWindow,
		ChannelTimeout:         *channelTimeout,
		BatcherAddr:            common.HexToAddress(*batcher),
		BatchInboxAddress:      common.HexToAddress(*batchInbox),
		DepositContractAddress: common.HexToAddress(*portal),
		L1SystemConfigAddress:  common.HexToAddress(*sysConfig),
		EIP1559Denominator:     uint32(cfg.EIP1559Denominator),
		EIP1559Elasticity:      uint32(cfg.EIP1559Elasticity),
		BaseFeeScalar:          uint32(*baseFeeScalar),
		BlobBaseFeeScalar:      uint32(*blobBaseFeeScalar),
	}
	rollupCfg, err := rollup.Build(rollup.L2Genesis{
		Hash:      genesis.Hash(),
		Timestamp: genesis.Time(),
		GasLimit:  genesis.Header.GasLimit,
	}, params)
	if err != nil {
		return err
	}

	w := os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	return rollupCfg.Encode(w)
}
