package server

import (
	"context"
	"errors"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/bytes"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"

	evmapi "github.com/cosmos/evm/api/cosmos/evm/vm/v1"
)

var errNilABCIQueryResponse = errors.New("ABCI query returned a nil response")

type abciQuerier interface {
	Query(context.Context, *abci.RequestQuery) (*abci.ResponseQuery, error)
}

type queryClient struct {
	rpcclient.Client
	querier abciQuerier
}

var _ rpcclient.Client = (*queryClient)(nil)

func newQueryClient(client rpcclient.Client, querier abciQuerier) rpcclient.Client {
	return &queryClient{
		Client:  client,
		querier: querier,
	}
}

func (c *queryClient) ABCIQueryWithOptions(
	ctx context.Context,
	path string,
	data bytes.HexBytes,
	opts rpcclient.ABCIQueryOptions,
) (*coretypes.ResultABCIQuery, error) {
	if path != evmapi.Query_EthCall_FullMethodName && path != evmapi.Query_EstimateGas_FullMethodName {
		return c.Client.ABCIQueryWithOptions(ctx, path, data, opts)
	}

	response, err := c.querier.Query(ctx, &abci.RequestQuery{
		Path:   path,
		Data:   data,
		Height: opts.Height,
		Prove:  opts.Prove,
	})
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errNilABCIQueryResponse
	}

	return &coretypes.ResultABCIQuery{Response: *response}, nil
}
