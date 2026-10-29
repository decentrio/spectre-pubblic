package client

import (
	"context"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
)

type Client interface {
	Status(context.Context) (*ResultStatus, error)
	BlockResults(context.Context, *int64) (*ResultBlockResults, error)
}

type ResultStatus = coretypes.ResultStatus

type ResultBlockResults struct {
	Height                int64                     `json:"height"`
	TxsResults            []*abci.ExecTxResult      `json:"txs_results"`
	EndBlockEvents        []abci.Event              `json:"end_block_events"`
	FinalizeBlockEvents   []abci.Event              `json:"finalize_block_events"`
	ValidatorUpdates      []abci.ValidatorUpdate    `json:"validator_updates"`
	ConsensusParamUpdates *cmtproto.ConsensusParams `json:"consensus_param_updates"`
	AppHash               []byte                    `json:"app_hash"`
}

type Event = abci.Event
type EventAttribute = abci.EventAttribute
