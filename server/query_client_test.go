package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/bytes"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"

	evmapi "github.com/cosmos/evm/api/cosmos/evm/vm/v1"
)

type queryFunc func(context.Context, *abci.RequestQuery) (*abci.ResponseQuery, error)

func (f queryFunc) Query(ctx context.Context, req *abci.RequestQuery) (*abci.ResponseQuery, error) {
	return f(ctx, req)
}

type delegatingClient struct {
	rpcclient.Client
	query func(context.Context, string, bytes.HexBytes, rpcclient.ABCIQueryOptions) (*coretypes.ResultABCIQuery, error)
}

func (c *delegatingClient) ABCIQueryWithOptions(
	ctx context.Context,
	path string,
	data bytes.HexBytes,
	opts rpcclient.ABCIQueryOptions,
) (*coretypes.ResultABCIQuery, error) {
	return c.query(ctx, path, data, opts)
}

func TestQueryClientPreservesContextForEVMQueries(t *testing.T) {
	type contextKey struct{}

	marker := &struct{}{}
	data := bytes.HexBytes{0x01, 0x02, 0x03}
	opts := rpcclient.ABCIQueryOptions{Height: 42, Prove: true}
	response := &abci.ResponseQuery{
		Code:   7,
		Log:    "query log",
		Value:  []byte{0x04, 0x05},
		Height: 41,
	}

	for _, path := range []string{evmapi.Query_EthCall_FullMethodName, evmapi.Query_EstimateGas_FullMethodName} {
		t.Run(path, func(t *testing.T) {
			querier := queryFunc(func(ctx context.Context, req *abci.RequestQuery) (*abci.ResponseQuery, error) {
				require.Same(t, marker, ctx.Value(contextKey{}))
				require.Equal(t, path, req.Path)
				require.Equal(t, []byte(data), req.Data)
				require.Equal(t, opts.Height, req.Height)
				require.Equal(t, opts.Prove, req.Prove)
				return response, nil
			})

			client := newQueryClient(&delegatingClient{
				query: func(context.Context, string, bytes.HexBytes, rpcclient.ABCIQueryOptions) (*coretypes.ResultABCIQuery, error) {
					t.Fatal("EVM query delegated to the local RPC client")
					return nil, nil
				},
			}, querier)

			result, err := client.ABCIQueryWithOptions(
				context.WithValue(context.Background(), contextKey{}, marker),
				path,
				data,
				opts,
			)
			require.NoError(t, err)
			require.Equal(t, *response, result.Response)
		})
	}
}

func TestQueryClientPreservesQueryError(t *testing.T) {
	expectedErr := errors.New("query failed")
	client := newQueryClient(&delegatingClient{}, queryFunc(
		func(context.Context, *abci.RequestQuery) (*abci.ResponseQuery, error) {
			return nil, expectedErr
		},
	))

	result, err := client.ABCIQueryWithOptions(
		context.Background(),
		evmapi.Query_EthCall_FullMethodName,
		nil,
		rpcclient.DefaultABCIQueryOptions,
	)
	require.Nil(t, result)
	require.Equal(t, expectedErr, err)
}

func TestQueryClientPropagatesRequestContextCancellation(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	deadlineCtx, deadlineCancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer deadlineCancel()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{name: "canceled", ctx: canceledCtx, err: context.Canceled},
		{name: "deadline exceeded", ctx: deadlineCtx, err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newQueryClient(&delegatingClient{}, queryFunc(
				func(ctx context.Context, _ *abci.RequestQuery) (*abci.ResponseQuery, error) {
					require.ErrorIs(t, ctx.Err(), tc.err)
					return nil, ctx.Err()
				},
			))

			result, err := client.ABCIQueryWithOptions(
				tc.ctx,
				evmapi.Query_EthCall_FullMethodName,
				nil,
				rpcclient.DefaultABCIQueryOptions,
			)
			require.Nil(t, result)
			require.Equal(t, tc.err, err)
		})
	}
}

func TestQueryClientRejectsNilQueryResponse(t *testing.T) {
	client := newQueryClient(&delegatingClient{}, queryFunc(
		func(context.Context, *abci.RequestQuery) (*abci.ResponseQuery, error) {
			return nil, nil
		},
	))

	result, err := client.ABCIQueryWithOptions(
		context.Background(),
		evmapi.Query_EstimateGas_FullMethodName,
		nil,
		rpcclient.DefaultABCIQueryOptions,
	)
	require.ErrorIs(t, err, errNilABCIQueryResponse)
	require.Nil(t, result)
}

func TestQueryClientDelegatesUnrelatedQueries(t *testing.T) {
	type contextKey struct{}

	expectedResult := &coretypes.ResultABCIQuery{Response: abci.ResponseQuery{Code: 9}}
	expectedErr := errors.New("delegated error")
	path := "/cosmos.bank.v1beta1.Query/Balance"
	data := bytes.HexBytes{0x06, 0x07}
	opts := rpcclient.ABCIQueryOptions{Height: 99, Prove: true}
	ctx := context.WithValue(context.Background(), contextKey{}, "delegation marker")
	delegated := false

	client := newQueryClient(&delegatingClient{
		query: func(gotCtx context.Context, gotPath string, gotData bytes.HexBytes, gotOpts rpcclient.ABCIQueryOptions) (*coretypes.ResultABCIQuery, error) {
			delegated = true
			require.Same(t, ctx, gotCtx)
			require.Equal(t, path, gotPath)
			require.Equal(t, data, gotData)
			require.Equal(t, opts, gotOpts)
			return expectedResult, expectedErr
		},
	}, queryFunc(func(context.Context, *abci.RequestQuery) (*abci.ResponseQuery, error) {
		t.Fatal("unrelated query sent directly to the proxy app")
		return nil, nil
	}))

	result, err := client.ABCIQueryWithOptions(ctx, path, data, opts)
	require.True(t, delegated)
	require.Same(t, expectedResult, result)
	require.Equal(t, expectedErr, err)
}
