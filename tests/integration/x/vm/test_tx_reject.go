package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/params"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtbytes "github.com/cometbft/cometbft/libs/bytes"
	cmtrpcclient "github.com/cometbft/cometbft/rpc/client"
	cmtrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"

	evmapi "github.com/cosmos/evm/api/cosmos/evm/vm/v1"
	precompilecommon "github.com/cosmos/evm/precompiles/common"
	rpcbackend "github.com/cosmos/evm/rpc/backend"
	backendmocks "github.com/cosmos/evm/rpc/backend/mocks"
	ethapi "github.com/cosmos/evm/rpc/namespaces/ethereum/eth"
	rpctypes "github.com/cosmos/evm/rpc/types"
	"github.com/cosmos/evm/server/config"
	evmtypes "github.com/cosmos/evm/x/vm/types"

	"cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

func (s *KeeperTestSuite) TestTxRejectQueryErrorTransport() {
	client, closeClient := s.txRejectGRPCClient()
	defer closeClient()

	wrongChainID := new(big.Int).Add(s.Network.GetEIP155ChainID(), big.NewInt(1))
	chainIDArgs := evmtypes.TransactionArgs{
		To:      txRejectPtr(common.Address{}),
		ChainID: (*hexutil.Big)(wrongChainID),
	}
	chainIDRequest := txRejectCallRequest(s.T(), chainIDArgs)

	s.Run("estimate gas transports a chain ID rejection", func() {
		ctx := context.Background()
		_, err := client.EstimateGas(ctx, chainIDRequest)
		s.requireQueryRejection(ctx, err, codes.InvalidArgument, precompilecommon.SolidityErrChainIdMismatch, true)
	})

	s.Run("eth call transports a chain ID rejection", func() {
		ctx := context.Background()
		_, err := client.EthCall(ctx, chainIDRequest)
		s.requireQueryRejection(ctx, err, codes.InvalidArgument, precompilecommon.SolidityErrChainIdMismatch, true)
	})

	s.Run("estimate gas transports insufficient funds", func() {
		unfunded := s.Keyring.GetAddr(s.Keyring.AddKey())
		feeCap := hexutil.Big(*big.NewInt(1))
		value := hexutil.Big(*big.NewInt(1))
		args := evmtypes.TransactionArgs{
			From:         &unfunded,
			To:           txRejectPtr(common.Address{}),
			Value:        &value,
			MaxFeePerGas: &feeCap,
		}

		ctx := context.Background()
		_, err := client.EstimateGas(ctx, txRejectCallRequest(s.T(), args))
		s.requireQueryRejection(ctx, err, codes.Unknown, precompilecommon.SolidityErrSDKInsufficientFunds, true)
	})

	s.Run("successful estimate remains unchanged", func() {
		args := evmtypes.TransactionArgs{To: txRejectPtr(common.Address{})}
		response, err := client.EstimateGas(
			context.Background(),
			txRejectCallRequest(s.T(), args),
		)
		s.Require().NoError(err)
		s.Require().Equal(params.TxGas, response.Gas)
	})
}

func (s *KeeperTestSuite) TestTxRejectABCIQueryTransport() {
	queryClient, responses, _ := s.txRejectABCIQueryClient()
	wantChainID := new(big.Int).Add(s.Network.GetEIP155ChainID(), big.NewInt(1))
	request := txRejectCallRequest(s.T(), evmtypes.TransactionArgs{
		To:      txRejectPtr(common.Address{}),
		ChainID: (*hexutil.Big)(wantChainID),
	})

	_, plainErr := queryClient.EstimateGas(context.Background(), request)
	s.Require().Error(plainErr)
	s.Require().Equal(codes.InvalidArgument, status.Code(plainErr))
	s.Require().Empty(status.Convert(plainErr).Details())
	s.Require().Len(*responses, 1)
	plainABCI := (*responses)[0]

	plainDecoded := rpctypes.FromQueryError(context.Background(), plainErr)
	s.Require().Same(plainErr, plainDecoded)

	ctx := rpctypes.WithQueryErrorCapture(context.Background())
	_, optedErr := queryClient.EstimateGas(ctx, request)
	s.Require().Error(optedErr)
	s.Require().Equal(plainErr.Error(), optedErr.Error())
	s.Require().Equal(status.Code(plainErr), status.Code(optedErr))
	s.Require().Empty(status.Convert(optedErr).Details())
	s.Require().Len(*responses, 2)
	optedABCI := (*responses)[1]
	s.Require().Equal(plainABCI.Code, optedABCI.Code)
	s.Require().Equal(plainABCI.Codespace, optedABCI.Codespace)
	s.Require().Equal(plainABCI.Log, optedABCI.Log)
	s.requireQueryRejection(ctx, optedErr, codes.InvalidArgument, precompilecommon.SolidityErrChainIdMismatch, false)

	decoded := rpctypes.FromQueryError(ctx, optedErr)
	var chainIDError *evmtypes.ChainIDMismatchError
	s.Require().True(errors.As(decoded, &chainIDError))
	s.Require().Equal(s.Network.GetEIP155ChainID(), chainIDError.Expected)
	s.Require().Equal(wantChainID, chainIDError.Actual)
}

// A mapped ordinary Go error must remain an ordinary error on the ABCI path.
// Turning it into gRPC Unknown would change the SDK's ABCI error classification.
func (s *KeeperTestSuite) TestTxRejectABCIQueryErrorClassification() {
	queryClient, responses, _ := s.txRejectABCIQueryClient()
	unfunded := s.Keyring.GetAddr(s.Keyring.AddKey())
	request := txRejectCallRequest(s.T(), evmtypes.TransactionArgs{
		From:         &unfunded,
		To:           txRejectPtr(common.Address{}),
		Value:        (*hexutil.Big)(big.NewInt(1)),
		MaxFeePerGas: (*hexutil.Big)(big.NewInt(1)),
	})

	_, plainErr := queryClient.EstimateGas(context.Background(), request)
	s.Require().Error(plainErr)
	s.Require().Len(*responses, 1)
	baseline := (*responses)[0]
	s.Require().Equal(sdkerrors.ErrInvalidRequest.ABCICode(), baseline.Code)
	s.Require().Equal(sdkerrors.ErrInvalidRequest.Codespace(), baseline.Codespace)
	s.Require().Empty(status.Convert(plainErr).Details())

	ctx := rpctypes.WithQueryErrorCapture(context.Background())
	_, capturedErr := queryClient.EstimateGas(ctx, request)
	s.Require().EqualError(capturedErr, plainErr.Error())
	s.Require().Len(*responses, 2)
	s.Require().Equal(baseline, (*responses)[1])
	s.requireQueryRejection(ctx, capturedErr, status.Code(plainErr), precompilecommon.SolidityErrSDKInsufficientFunds, false)
}

func (s *KeeperTestSuite) TestTxRejectABCIQueryHTTPRoutes() {
	backend := s.txRejectABCIBackend()
	server := gethrpc.NewServer()
	s.T().Cleanup(server.Stop)
	s.Require().NoError(server.RegisterName("eth", ethapi.NewPublicAPI(log.NewNopLogger(), backend)))

	wrongChainID := new(big.Int).Add(s.Network.GetEIP155ChainID(), big.NewInt(1))
	args := evmtypes.TransactionArgs{
		To:      txRejectPtr(common.Address{}),
		ChainID: (*hexutil.Big)(wrongChainID),
	}
	definition := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrChainIdMismatch]
	encodedArgs, err := definition.Inputs.Pack(s.Network.GetEIP155ChainID(), wrongChainID)
	s.Require().NoError(err)
	wantData := hexutil.Encode(append(definition.ID[:4], encodedArgs...))

	for _, tc := range []struct {
		name   string
		method string
	}{
		{name: "estimate gas", method: "eth_estimateGas"},
		{name: "call", method: "eth_call"},
	} {
		s.Run(tc.name, func() {
			response := txRejectHTTPResponse(s.T(), server, tc.method, []interface{}{args, "0x1"})
			s.Require().NotNil(response.Error)
			s.Require().Equal(-32000, response.Error.Code)
			s.Require().Equal(wantData, response.Error.Data)
		})
	}
}

func (s *KeeperTestSuite) txRejectGRPCClient() (evmtypes.QueryClient, func()) {
	s.T().Helper()

	listener := bufconn.Listen(1024 * 1024)
	protoCodec := codec.NewProtoCodec(s.Network.GetEncodingConfig().InterfaceRegistry).GRPCCodec()
	server := grpc.NewServer(
		grpc.ForceServerCodec(protoCodec),
		grpc.UnaryInterceptor(func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			sdkCtx := s.Network.GetContext().WithContext(ctx)
			return handler(sdk.WrapSDKContext(sdkCtx), req)
		}),
	)
	evmtypes.RegisterQueryServer(server, s.Network.App.GetEVMKeeper())

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	conn, err := grpc.NewClient(
		"passthrough:///tx-reject",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(protoCodec)),
	)
	s.Require().NoError(err)

	cleanup := func() {
		s.Require().NoError(conn.Close())
		server.Stop()
		s.Require().NoError(<-serveErr)
	}
	return evmtypes.NewQueryClient(conn), cleanup
}

func (s *KeeperTestSuite) txRejectABCIQueryClient() (*rpctypes.QueryClient, *[]abci.ResponseQuery, *backendmocks.Client) {
	s.T().Helper()

	cometClient := backendmocks.NewClient(s.T())
	responses := make([]abci.ResponseQuery, 0, 2)
	cometClient.EXPECT().ABCIQueryWithOptions(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(ctx context.Context, path string, data cmtbytes.HexBytes, opts cmtrpcclient.ABCIQueryOptions) (*cmtrpctypes.ResultABCIQuery, error) {
			response, err := s.Network.App.GetBaseApp().Query(ctx, &abci.RequestQuery{
				Path:   path,
				Data:   data,
				Height: opts.Height,
				Prove:  opts.Prove,
			})
			if err != nil {
				return nil, err
			}
			responses = append(responses, *response)
			return &cmtrpctypes.ResultABCIQuery{Response: *response}, nil
		})

	encodingConfig := s.Network.GetEncodingConfig()
	clientCtx := client.Context{}.
		WithChainID(s.Network.GetChainID()).
		WithCodec(encodingConfig.Codec).
		WithInterfaceRegistry(encodingConfig.InterfaceRegistry).
		WithClient(cometClient)
	return rpctypes.NewQueryClient(clientCtx), &responses, cometClient
}

func (s *KeeperTestSuite) txRejectABCIBackend() *rpcbackend.Backend {
	s.T().Helper()

	queryClient, _, cometClient := s.txRejectABCIQueryClient()
	cometClient.On("Header", mock.Anything, mock.Anything).
		Return(&cmtrpctypes.ResultHeader{Header: &tmtypes.Header{
			Height:          s.Network.GetContext().BlockHeight(),
			ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
		}}, nil).
		Maybe()

	backend := &rpcbackend.Backend{
		RPCClient:   cometClient,
		QueryClient: queryClient,
		Logger:      log.NewNopLogger(),
		EvmChainID:  new(big.Int).Set(s.Network.GetEIP155ChainID()),
	}
	backend.Cfg.JSONRPC.GasCap = config.DefaultGasCap
	return backend
}

func (s *KeeperTestSuite) requireQueryRejection(
	ctx context.Context,
	err error,
	grpcCode codes.Code,
	solidityError string,
	wantStatusDetails bool,
) {
	s.T().Helper()
	s.Require().Error(err)
	s.Require().Equal(grpcCode, status.Code(err))
	if wantStatusDetails {
		details := status.Convert(err).Details()
		s.Require().Len(details, 1)
		detail, ok := details[0].(*evmapi.QueryErrorDetail)
		s.Require().True(ok)
		s.Require().Equal(int64(-32000), detail.RpcCode)
		s.Require().NotEmpty(detail.RevertData)
	} else {
		s.Require().Empty(status.Convert(err).Details())
	}

	decoded := rpctypes.FromQueryError(ctx, err)
	rejection, ok := decoded.(*rpctypes.TxRejectError)
	s.Require().True(ok)
	s.Require().Equal(-32000, rejection.ErrorCode())

	definition := precompilecommon.SharedErrorABI.Errors[solidityError]
	data := rejection.RevertData()
	s.Require().GreaterOrEqual(len(data), 4)
	s.Require().Equal(definition.ID[:4], data[:4])
	s.Require().Equal(hexutil.Encode(data), rejection.ErrorData())
}

func txRejectCallRequest(t *testing.T, args evmtypes.TransactionArgs) *evmtypes.EthCallRequest {
	t.Helper()
	encoded, err := json.Marshal(args)
	require.NoError(t, err)
	return &evmtypes.EthCallRequest{
		Args:   encoded,
		GasCap: config.DefaultGasCap,
	}
}

type txRejectHTTPResult struct {
	Error *struct {
		Code int         `json:"code"`
		Data interface{} `json:"data"`
	} `json:"error"`
}

func txRejectHTTPResponse(t *testing.T, server *gethrpc.Server, method string, params []interface{}) txRejectHTTPResult {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	require.NoError(t, err)

	request := httptest.NewRequest("POST", "/", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	require.Equal(t, 200, recorder.Code)

	var response txRejectHTTPResult
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}

func txRejectPtr[T any](value T) *T {
	return &value
}
