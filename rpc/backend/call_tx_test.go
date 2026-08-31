package backend

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	cmtrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"

	"github.com/cosmos/evm/rpc/backend/mocks"
	rpctypes "github.com/cosmos/evm/rpc/types"
	"github.com/cosmos/evm/testutil/constants"
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

func TestSetTxDefaultsPreservesTypedTransactionFieldsForEstimateGas(t *testing.T) {
	configurator := evmtypes.NewEVMConfigurator()
	configurator.ResetTestConfig()
	require.NoError(t, evmtypes.SetChainConfig(evmtypes.DefaultChainConfig(constants.ExampleChainID.EVMChainID)))
	t.Cleanup(configurator.ResetTestConfig)

	from := common.HexToAddress("0x1234")
	to := common.HexToAddress("0x5678")
	nonce := hexutil.Uint64(1)
	maxFeePerGas := hexutil.Big(*big.NewInt(2))
	maxPriorityFeePerGas := hexutil.Big(*big.NewInt(1))
	blobFeeCap := hexutil.Big(*big.NewInt(3))
	blobHashes := []common.Hash{common.HexToHash("0xabcd")}
	authorizationList := []ethtypes.SetCodeAuthorization{{
		Address: common.HexToAddress("0x9abc"),
		Nonce:   2,
	}}

	testCases := []struct {
		name       string
		args       evmtypes.TransactionArgs
		wantTxType uint8
		assertArgs func(*testing.T, evmtypes.TransactionArgs)
	}{
		{
			name: "set code transaction",
			args: evmtypes.TransactionArgs{
				AuthorizationList: authorizationList,
			},
			wantTxType: ethtypes.SetCodeTxType,
			assertArgs: func(t *testing.T, args evmtypes.TransactionArgs) {
				t.Helper()
				require.Equal(t, authorizationList, args.AuthorizationList)
			},
		},
		{
			name: "blob transaction",
			args: evmtypes.TransactionArgs{
				BlobFeeCap: &blobFeeCap,
				BlobHashes: blobHashes,
			},
			wantTxType: ethtypes.BlobTxType,
			assertArgs: func(t *testing.T, args evmtypes.TransactionArgs) {
				t.Helper()
				require.Equal(t, &blobFeeCap, args.BlobFeeCap)
				require.Equal(t, blobHashes, args.BlobHashes)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			const estimatedGas = uint64(75_000)

			var estimateArgs evmtypes.TransactionArgs
			backend := setupSetTxDefaultsTestBackend(t, estimatedGas, &estimateArgs)

			tc.args.From = &from
			tc.args.To = &to
			tc.args.Nonce = &nonce
			tc.args.MaxFeePerGas = &maxFeePerGas
			tc.args.MaxPriorityFeePerGas = &maxPriorityFeePerGas

			result, err := backend.SetTxDefaults(context.Background(), tc.args)
			require.NoError(t, err)

			tc.assertArgs(t, estimateArgs)
			require.Equal(t, tc.wantTxType, estimateArgs.TxType(ethtypes.LegacyTxType))
			require.NotNil(t, result.Gas)
			require.Equal(t, estimatedGas, uint64(*result.Gas))
			require.Equal(t, tc.wantTxType, result.ToTransaction(ethtypes.LegacyTxType).Type())
		})
	}
}

func setupSetTxDefaultsTestBackend(
	t *testing.T,
	estimatedGas uint64,
	estimateArgs *evmtypes.TransactionArgs,
) *Backend {
	t.Helper()

	backend := setupMockBackend(t)
	queryClient := mocks.NewEVMQueryClient(t)
	backend.QueryClient.QueryClient = queryClient

	queryClient.On("Params", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			header := args.Get(2).(grpc.HeaderCallOption)
			*header.HeaderAddr = metadata.Pairs(grpctypes.GRPCBlockHeightHeader, "1")
		}).
		Return(&evmtypes.QueryParamsResponse{Params: evmtypes.DefaultParams()}, nil).
		Maybe()
	queryClient.On("ValidatorAccount", mock.Anything, mock.Anything).
		Return(nil, errors.New("validator account unavailable")).
		Maybe()
	queryClient.On("BaseFee", mock.Anything, mock.Anything).
		Return(&evmtypes.QueryBaseFeeResponse{}, nil).
		Maybe()
	queryClient.On("EstimateGas", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*evmtypes.EthCallRequest)
			require.NoError(t, json.Unmarshal(req.Args, estimateArgs))
		}).
		Return(&evmtypes.EstimateGasResponse{Gas: estimatedGas}, nil).
		Once()

	header := tmtypes.Header{
		Height:  1,
		Time:    time.Now(),
		ChainID: constants.ExampleChainID.ChainID,
	}
	block := &tmtypes.Block{Header: header}
	client := backend.ClientCtx.Client.(*mocks.Client)
	client.On("Block", mock.Anything, mock.Anything).
		Return(&cmtrpctypes.ResultBlock{Block: block}, nil).
		Maybe()
	client.On("BlockResults", mock.Anything, mock.Anything).
		Return(&cmtrpctypes.ResultBlockResults{Height: 1}, nil).
		Maybe()
	client.On("Header", mock.Anything, mock.Anything).
		Return(&cmtrpctypes.ResultHeader{Header: &header}, nil).
		Maybe()
	client.On("ConsensusParams", mock.Anything, mock.Anything).
		Return(nil, errors.New("consensus params unavailable")).
		Maybe()

	return backend
}
