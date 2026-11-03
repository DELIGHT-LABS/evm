package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cmtrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"

	precompilecommon "github.com/cosmos/evm/precompiles/common"
	"github.com/cosmos/evm/rpc/backend/mocks"
	rpctypes "github.com/cosmos/evm/rpc/types"
	"github.com/cosmos/evm/testutil/constants"
	evmtypes "github.com/cosmos/evm/x/vm/types"

	errorsmod "cosmossdk.io/errors"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"
)

type txRejectSimulationAPI struct {
	backend *Backend
}

func (api *txRejectSimulationAPI) EstimateGas(
	ctx context.Context,
	args evmtypes.TransactionArgs,
	blockNrOrHash *rpctypes.BlockNumberOrHash,
	overrides *json.RawMessage,
) (hexutil.Uint64, error) {
	return api.backend.EstimateGas(ctx, args, blockNrOrHash, overrides)
}

func (api *txRejectSimulationAPI) Call(
	ctx context.Context,
	args evmtypes.TransactionArgs,
	blockNrOrHash rpctypes.BlockNumberOrHash,
	overrides *json.RawMessage,
) (hexutil.Bytes, error) {
	blockNumber, err := api.backend.BlockNumberFromComet(ctx, blockNrOrHash)
	if err != nil {
		return nil, err
	}
	res, err := api.backend.DoCall(ctx, args, blockNumber, overrides)
	if err != nil {
		return nil, err
	}
	return res.Ret, nil
}

type simulationRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

type simulationRPCResponse struct {
	Result json.RawMessage     `json:"result"`
	Error  *simulationRPCError `json:"error"`
}

func callSimulationRPC(t *testing.T, backend *Backend, method string) simulationRPCResponse {
	t.Helper()
	server := rpc.NewServer()
	t.Cleanup(server.Stop)
	require.NoError(t, server.RegisterName("eth", &txRejectSimulationAPI{backend: backend}))

	request, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  []interface{}{map[string]interface{}{}, "0x1"},
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", bytes.NewReader(request))
	req.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, req)
	require.Equal(t, 200, recorder.Code)
	var response simulationRPCResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}

func setupTxRejectSimulationBackend(t *testing.T) (*Backend, *mocks.EVMQueryClient) {
	t.Helper()
	backend := setupMockBackend(t)
	client := backend.ClientCtx.Client.(*mocks.Client)
	client.On("Header", mock.Anything, mock.Anything).Return(&cmtrpctypes.ResultHeader{
		Header: &tmtypes.Header{Height: 1},
	}, nil).Maybe()
	queryClient := backend.QueryClient.QueryClient.(*mocks.EVMQueryClient)
	return backend, queryClient
}

func TestTxRejectSimulationRoutes(t *testing.T) {
	type route struct {
		name       string
		method     string
		configure  func(*mocks.EVMQueryClient, *evmtypes.MsgEthereumTxResponse, *evmtypes.EstimateGasResponse, error)
		invoke     func(*Backend) (interface{}, error)
		wantResult string
	}
	blockNumber := rpctypes.BlockNumber(1)
	block := rpctypes.BlockNumberOrHash{BlockNumber: &blockNumber}
	routes := []route{
		{
			name:   "estimateGas",
			method: "eth_estimateGas",
			configure: func(client *mocks.EVMQueryClient, _ *evmtypes.MsgEthereumTxResponse, response *evmtypes.EstimateGasResponse, err error) {
				client.On("EstimateGas", mock.Anything, mock.Anything).Return(response, err).Maybe()
			},
			invoke: func(backend *Backend) (interface{}, error) {
				return backend.EstimateGas(context.Background(), evmtypes.TransactionArgs{}, &block, nil)
			},
			wantResult: `"0x5208"`,
		},
		{
			name:   "call",
			method: "eth_call",
			configure: func(client *mocks.EVMQueryClient, response *evmtypes.MsgEthereumTxResponse, _ *evmtypes.EstimateGasResponse, err error) {
				client.On("EthCall", mock.Anything, mock.Anything).Return(response, err).Maybe()
			},
			invoke: func(backend *Backend) (interface{}, error) {
				return backend.DoCall(context.Background(), evmtypes.TransactionArgs{}, blockNumber, nil)
			},
			wantResult: `"0x0102"`,
		},
	}

	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			t.Run("request-local captured query error", func(t *testing.T) {
				backend, queryClient := setupTxRejectSimulationBackend(t)
				var source error
				var transport error
				if route.name == "estimateGas" {
					source = fmt.Errorf("execution: %w", core.ErrInsufficientFundsForTransfer)
					transport = status.Error(codes.Internal, "legacy ABCI estimate failure")
					queryClient.On("EstimateGas", mock.Anything, mock.Anything).
						Run(func(args mock.Arguments) {
							require.Same(t, source, rpctypes.QueryError(args.Get(0).(context.Context), source))
						}).
						Return(nil, transport).
						Maybe()
				} else {
					source = errorsmod.Wrap(sdkerrors.ErrInsufficientFunds, "bank send")
					transport = status.Error(codes.Internal, "legacy ABCI call failure")
					queryClient.On("EthCall", mock.Anything, mock.Anything).
						Run(func(args mock.Arguments) {
							require.Same(t, source, rpctypes.QueryError(args.Get(0).(context.Context), source))
						}).
						Return(nil, transport).
						Maybe()
				}

				_, err := route.invoke(backend)
				require.EqualError(t, err, transport.Error())
				require.ErrorIs(t, err, source)
				require.ErrorIs(t, err, transport)
				var rejection *rpctypes.TxRejectError
				require.ErrorAs(t, err, &rejection)
				definition := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrSDKInsufficientFunds]
				require.Equal(t, definition.ID[:4], rejection.RevertData())

				response := callSimulationRPC(t, backend, route.method)
				require.NotNil(t, response.Error)
				require.Equal(t, -32000, response.Error.Code)
				require.Equal(t, transport.Error(), response.Error.Message)
				require.Equal(t, hexutil.Encode(definition.ID[:4]), response.Error.Data)
			})

			t.Run("mapped query error", func(t *testing.T) {
				backend, queryClient := setupTxRejectSimulationBackend(t)
				cause := fmt.Errorf("query failed: %w", core.ErrInsufficientFundsForTransfer)
				route.configure(queryClient, nil, nil, cause)

				_, err := route.invoke(backend)
				require.ErrorIs(t, err, core.ErrInsufficientFundsForTransfer)
				require.Same(t, cause, errors.Unwrap(err))
				var rejection *rpctypes.TxRejectError
				require.ErrorAs(t, err, &rejection)
				definition := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrSDKInsufficientFunds]
				require.Equal(t, definition.ID[:4], rejection.RevertData())

				response := callSimulationRPC(t, backend, route.method)
				require.NotNil(t, response.Error)
				require.Equal(t, -32000, response.Error.Code)
				require.Equal(t, cause.Error(), response.Error.Message)
				require.Equal(t, hexutil.Encode(definition.ID[:4]), response.Error.Data)
			})

			t.Run("existing execution revert", func(t *testing.T) {
				backend, queryClient := setupTxRejectSimulationBackend(t)
				data := []byte{0xde, 0xad, 0xbe, 0xef}
				route.configure(
					queryClient,
					&evmtypes.MsgEthereumTxResponse{VmError: vm.ErrExecutionReverted.Error(), Ret: data},
					&evmtypes.EstimateGasResponse{VmError: vm.ErrExecutionReverted.Error(), Ret: data},
					nil,
				)

				_, err := route.invoke(backend)
				var dataErr rpc.DataError
				var codeErr interface{ ErrorCode() int }
				require.ErrorAs(t, err, &dataErr)
				require.ErrorAs(t, err, &codeErr)
				require.Equal(t, 3, codeErr.ErrorCode())
				require.Equal(t, hexutil.Encode(data), dataErr.ErrorData())

				response := callSimulationRPC(t, backend, route.method)
				require.NotNil(t, response.Error)
				require.Equal(t, 3, response.Error.Code)
				require.Equal(t, hexutil.Encode(data), response.Error.Data)
			})

			t.Run("infrastructure error", func(t *testing.T) {
				backend, queryClient := setupTxRejectSimulationBackend(t)
				route.configure(queryClient, nil, nil, context.DeadlineExceeded)

				_, err := route.invoke(backend)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Equal(t, context.DeadlineExceeded, err)
				var rejection *rpctypes.TxRejectError
				require.False(t, errors.As(err, &rejection))
			})

			t.Run("success", func(t *testing.T) {
				backend, queryClient := setupTxRejectSimulationBackend(t)
				route.configure(
					queryClient,
					&evmtypes.MsgEthereumTxResponse{Ret: []byte{1, 2}},
					&evmtypes.EstimateGasResponse{Gas: 21_000},
					nil,
				)

				_, err := route.invoke(backend)
				require.NoError(t, err)

				response := callSimulationRPC(t, backend, route.method)
				require.Nil(t, response.Error)
				require.JSONEq(t, route.wantResult, string(response.Result))
			})
		})
	}
}

func TestResolveTxRejectErrorIdempotence(t *testing.T) {
	cause := fmt.Errorf("ante: %w", core.ErrNonceTooLow)
	definition := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrNonceTooLow]
	data := definition.ID[:4]
	direct := rpctypes.NewTxRejectError(cause, data)
	require.Same(t, direct, resolveTxRejectError(direct))

	wrapped := fmt.Errorf("outer: %w", direct)
	resolved := resolveTxRejectError(wrapped)
	require.ErrorIs(t, resolved, core.ErrNonceTooLow)
	require.Same(t, wrapped, errors.Unwrap(resolved))
	require.Equal(t, data, resolved.(precompilecommon.RevertDataCarrier).RevertData())
}

func setupTxRejectDefaultsBackend(t *testing.T) (*Backend, *admissionMempool) {
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

	pool := &admissionMempool{}
	backend.Mempool = pool
	return backend, pool
}

func TestTxRejectFeeCapValidationRoutes(t *testing.T) {
	configurator := evmtypes.NewEVMConfigurator()
	configurator.ResetTestConfig()
	require.NoError(t, evmtypes.SetChainConfig(evmtypes.DefaultChainConfig(constants.ExampleChainID.EVMChainID)))
	t.Cleanup(configurator.ResetTestConfig)

	feeCap := hexutil.Big(*big.NewInt(1))
	tipCap := hexutil.Big(*big.NewInt(2))
	gas := hexutil.Uint64(21_000)
	nonce := hexutil.Uint64(1)
	args := evmtypes.TransactionArgs{
		Gas:                  &gas,
		Nonce:                &nonce,
		MaxFeePerGas:         &feeCap,
		MaxPriorityFeePerGas: &tipCap,
	}
	wantMessage := core.ErrTipAboveFeeCap.Error()

	for _, tc := range []struct {
		name   string
		invoke func(*Backend) error
	}{
		{
			name: "set transaction defaults",
			invoke: func(backend *Backend) error {
				_, err := backend.SetTxDefaults(context.Background(), args)
				return err
			},
		},
		{
			name: "resend",
			invoke: func(backend *Backend) error {
				_, err := backend.Resend(context.Background(), args, nil, nil)
				return err
			},
		},
		{
			name: "create access list",
			invoke: func(backend *Backend) error {
				blockNumber := rpctypes.BlockNumber(1)
				_, err := backend.CreateAccessList(
					context.Background(),
					args,
					rpctypes.BlockNumberOrHash{BlockNumber: &blockNumber},
					nil,
				)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, pool := setupTxRejectDefaultsBackend(t)
			err := tc.invoke(backend)
			require.EqualError(t, err, wantMessage)
			require.ErrorIs(t, err, core.ErrTipAboveFeeCap)
			var rejection *rpctypes.TxRejectError
			require.ErrorAs(t, err, &rejection)
			definition := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrTipAboveFeeCap]
			require.Equal(t, definition.ID[:4], rejection.RevertData())
			require.Equal(t, -32000, rejection.ErrorCode())
			require.Equal(t, hexutil.Encode(definition.ID[:4]), rejection.ErrorData())
			require.Zero(t, pool.inserts)
		})
	}
}

func TestCreateAccessListPreservesDoCallRejection(t *testing.T) {
	configurator := evmtypes.NewEVMConfigurator()
	configurator.ResetTestConfig()
	require.NoError(t, evmtypes.SetChainConfig(evmtypes.DefaultChainConfig(constants.ExampleChainID.EVMChainID)))
	t.Cleanup(configurator.ResetTestConfig)

	backend, _ := setupTxRejectDefaultsBackend(t)
	queryClient := backend.QueryClient.QueryClient.(*mocks.EVMQueryClient)
	cause := fmt.Errorf("query failed: %w", core.ErrInsufficientFundsForTransfer)
	queryClient.On("EthCall", mock.Anything, mock.Anything).Return(nil, cause).Once()

	from := ethcommon.HexToAddress("0x1234")
	to := ethcommon.HexToAddress("0x5678")
	gas := hexutil.Uint64(21_000)
	nonce := hexutil.Uint64(1)
	feeCap := hexutil.Big(*big.NewInt(2))
	tipCap := hexutil.Big(*big.NewInt(1))
	blockNumber := rpctypes.BlockNumber(1)
	_, err := backend.CreateAccessList(context.Background(), evmtypes.TransactionArgs{
		From:                 &from,
		To:                   &to,
		Gas:                  &gas,
		Nonce:                &nonce,
		MaxFeePerGas:         &feeCap,
		MaxPriorityFeePerGas: &tipCap,
	}, rpctypes.BlockNumberOrHash{BlockNumber: &blockNumber}, nil)

	require.ErrorContains(t, err, "failed to apply transaction:")
	require.ErrorContains(t, err, "err: "+cause.Error())
	require.ErrorIs(t, err, core.ErrInsufficientFundsForTransfer)
	var rejection *rpctypes.TxRejectError
	require.ErrorAs(t, err, &rejection)
	definition := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrSDKInsufficientFunds]
	require.Equal(t, definition.ID[:4], rejection.RevertData())
}
