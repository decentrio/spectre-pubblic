package http

import (
	"context"
	"net/http"

	rpcclient "github.com/tendermint/tendermint/rpc/client"

	cmthttp "github.com/cometbft/cometbft/rpc/client/http"
)

type HTTP struct {
	client *cmthttp.HTTP
}

var _ rpcclient.Client = (*HTTP)(nil)

func NewWithClient(remote, wsEndpoint string, client *http.Client) (*HTTP, error) {
	cmtClient, err := cmthttp.NewWithClient(remote, wsEndpoint, client)
	if err != nil {
		return nil, err
	}
	return &HTTP{client: cmtClient}, nil
}

func (h *HTTP) Status(ctx context.Context) (*rpcclient.ResultStatus, error) {
	return h.client.Status(ctx)
}

func (h *HTTP) BlockResults(ctx context.Context, height *int64) (*rpcclient.ResultBlockResults, error) {
	results, err := h.client.BlockResults(ctx, height)
	if err != nil || results == nil {
		return nil, err
	}

	return &rpcclient.ResultBlockResults{
		Height:                results.Height,
		TxsResults:            results.TxsResults,
		EndBlockEvents:        results.FinalizeBlockEvents,
		FinalizeBlockEvents:   results.FinalizeBlockEvents,
		ValidatorUpdates:      results.ValidatorUpdates,
		ConsensusParamUpdates: results.ConsensusParamUpdates,
		AppHash:               results.AppHash,
	}, nil
}
