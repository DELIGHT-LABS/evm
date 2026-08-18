package backend

import (
	"context"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	cmtrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"

	"github.com/cosmos/evm/rpc/backend/mocks"
	rpctypes "github.com/cosmos/evm/rpc/types"
	evmtypes "github.com/cosmos/evm/x/vm/types"
)

func TestEstimateGasAppliesEVMTimeout(t *testing.T) {
	backend := setupMockBackend(t)
	backend.Cfg.JSONRPC.EVMTimeout = 10 * time.Millisecond

	client := backend.ClientCtx.Client.(*mocks.Client)
	client.On("Header", mock.Anything, mock.Anything).Return(&cmtrpctypes.ResultHeader{
		Header: &tmtypes.Header{Height: 1},
	}, nil)

	queryClient := backend.QueryClient.QueryClient.(*mocks.EVMQueryClient)
	queryClient.On("EstimateGas", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, _ *evmtypes.EthCallRequest, _ ...grpc.CallOption) (*evmtypes.EstimateGasResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		nil,
	)

	blockNumber := rpctypes.BlockNumber(1)
	from := common.HexToAddress("0x1")
	_, err := backend.EstimateGas(context.Background(), evmtypes.TransactionArgs{
		From: &from,
	}, &rpctypes.BlockNumberOrHash{BlockNumber: &blockNumber}, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
