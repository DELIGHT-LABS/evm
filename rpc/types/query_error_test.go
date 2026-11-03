package types

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	evmapi "github.com/cosmos/evm/api/cosmos/evm/vm/v1"
	common "github.com/cosmos/evm/precompiles/common"
	evmtypes "github.com/cosmos/evm/x/vm/types"

	errorsmod "cosmossdk.io/errors"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

// legacyQueryCause models wrappers supported by the Cosmos classifier that
// expose Cause without implementing the standard Go Unwrap method.
type legacyQueryCause struct{ error }

func (e legacyQueryCause) Cause() error { return e.error }

func TestQueryErrorPreservesCosmosKey(t *testing.T) {
	for _, source := range []error{
		fmt.Errorf("bank send: %w", sdkerrors.ErrInsufficientFunds),
		legacyQueryCause{sdkerrors.ErrInsufficientFunds},
		errors.Join(core.ErrInsufficientFunds, sdkerrors.ErrInsufficientFunds),
	} {
		for _, remote := range []bool{false, true} {
			t.Run(fmt.Sprintf("%T/remote=%v", source, remote), func(t *testing.T) {
				ctx := WithQueryErrorCapture(context.Background())
				serverCtx := ctx
				if remote {
					serverCtx = queryGRPCContext()
				}
				encoded := QueryErrorWithStatus(serverCtx, codes.InvalidArgument, source)
				transport := roundTripQueryStatus(t, encoded)
				decoded := FromQueryError(ctx, transport)

				wantKey := common.NewCosmosErrorKey(sdkerrors.ErrInsufficientFunds)
				key, found := common.ExtractCosmosErrorKey(decoded)
				require.True(t, found)
				require.Equal(t, wantKey, key)
				require.EqualError(t, decoded, transport.Error())
				require.ErrorIs(t, decoded, transport)
				if !remote {
					require.ErrorIs(t, decoded, source)
				}
				var rejection *TxRejectError
				require.ErrorAs(t, decoded, &rejection)
				require.Equal(t, defaultTxRejectErrorCode, rejection.ErrorCode())
				definition := common.SharedErrorABI.Errors[common.SolidityErrSDKInsufficientFunds]
				require.Equal(t, definition.ID[:4], rejection.RevertData())
			})
		}
	}
}

func TestLocalQueryErrorCapture(t *testing.T) {
	ctx := WithQueryErrorCapture(context.Background())
	source := errorsmod.Wrap(sdkerrors.ErrUnauthorized, "ante rejected transaction")

	require.Same(t, source, QueryError(ctx, source))

	transport := status.Error(codes.Unknown, "old ABCI response text")
	decoded := FromQueryError(ctx, transport)
	require.Equal(t, transport.Error(), decoded.Error())
	require.ErrorIs(t, decoded, transport)
	require.ErrorIs(t, decoded, source)
	require.ErrorIs(t, decoded, sdkerrors.ErrUnauthorized)

	var cosmosError *errorsmod.Error
	require.ErrorAs(t, decoded, &cosmosError)
	require.Equal(t, sdkerrors.ErrUnauthorized.Codespace(), cosmosError.Codespace())
	require.Equal(t, sdkerrors.ErrUnauthorized.ABCICode(), cosmosError.ABCICode())

	var rejection *TxRejectError
	require.ErrorAs(t, decoded, &rejection)
	require.Equal(t, defaultTxRejectErrorCode, rejection.ErrorCode())
	require.NotEqual(t, "0x", rejection.ErrorData())
	require.NotEmpty(t, rejection.RevertData())
	key, found := common.ExtractCosmosErrorKey(decoded)
	require.True(t, found)
	require.Equal(t, common.NewCosmosErrorKey(sdkerrors.ErrUnauthorized), key)
}

func TestLocalQueryErrorWithStatusCapturesBeforeConversion(t *testing.T) {
	ctx := WithQueryErrorCapture(context.Background())
	source := errorsmod.Wrap(sdkerrors.ErrInsufficientFunds, "cannot pay gas")

	serverError := QueryErrorWithStatus(ctx, codes.InvalidArgument, source)
	require.Equal(t, status.Error(codes.InvalidArgument, source.Error()), serverError)
	require.False(t, errors.Is(serverError, source))

	// The outer keeper boundary must return the status unchanged and must not
	// replace the typed error captured before status conversion.
	require.Same(t, serverError, QueryError(ctx, serverError))
	transport := status.Error(codes.InvalidArgument, source.Error())
	decoded := FromQueryError(ctx, transport)
	require.Equal(t, transport.Error(), decoded.Error())
	require.ErrorIs(t, decoded, transport)
	require.ErrorIs(t, decoded, source)
	require.ErrorIs(t, decoded, sdkerrors.ErrInsufficientFunds)
}

func TestLocalQueryErrorCaptureIgnoresNilResultAndIsRequestScoped(t *testing.T) {
	first := WithQueryErrorCapture(context.Background())
	firstSource := errors.New("first rejection")
	wrapped := NewTxRejectError(firstSource, []byte{1, 2, 3, 4})
	require.Same(t, wrapped, QueryError(first, wrapped))
	require.NoError(t, FromQueryError(first, nil))

	second := WithQueryErrorCapture(first)
	transport := status.Error(codes.Unknown, "second request failure")
	require.Same(t, transport, FromQueryError(second, transport))
}

func TestLocalQueryErrorCaptureOwnsRevertData(t *testing.T) {
	ctx := WithQueryErrorCapture(context.Background())
	source := common.NewRevertWithSolidityError(common.SharedErrorABI, common.SolidityErrSDKInsufficientFunds)
	var carrier common.RevertDataCarrier
	require.ErrorAs(t, source, &carrier)
	want := append([]byte(nil), carrier.RevertData()...)
	require.Same(t, source, QueryError(ctx, source))

	// A caller-owned carrier may expose mutable bytes. Capturing must detach them.
	carrier.RevertData()[0] ^= 0xff
	transport := status.Error(codes.Unknown, "captured rejection")
	first := FromQueryError(ctx, transport).(*TxRejectError)
	require.Equal(t, want, first.RevertData())
	first.RevertData()[0] ^= 0xff
	second := FromQueryError(ctx, transport).(*TxRejectError)
	require.NotSame(t, first, second)
	require.Equal(t, want, second.RevertData())
	_, classified := common.ExtractCosmosErrorKey(second)
	require.False(t, classified)
}

func TestLocalQueryErrorCaptureConcurrentRequests(t *testing.T) {
	const requests = 16
	sources := make([]error, requests)
	for i := range sources {
		sources[i] = fmt.Errorf("source %d", i)
	}

	var wait sync.WaitGroup
	for i := range sources {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			ctx := WithQueryErrorCapture(context.Background())
			source := sources[index]
			payload := []byte(fmt.Sprintf("%04d", index))
			wrapped := NewTxRejectError(source, payload)
			require.Same(t, wrapped, QueryError(ctx, wrapped))

			transport := status.Error(codes.Unknown, fmt.Sprintf("transport %d", index))
			decoded := FromQueryError(ctx, transport)
			require.ErrorIs(t, decoded, source)
			require.ErrorIs(t, decoded, transport)
			var rejection *TxRejectError
			require.ErrorAs(t, decoded, &rejection)
			require.Equal(t, payload, rejection.RevertData())
		}(i)
	}
	wait.Wait()
}

func TestRemoteQueryErrorDetails(t *testing.T) {
	serverCtx := queryGRPCContext()

	t.Run("local caller without capture remains unchanged", func(t *testing.T) {
		original := NewTxRejectError(errors.New("transaction rejected"), []byte{0xde, 0xad, 0xbe, 0xef})
		require.Same(t, original, QueryError(context.Background(), original))
		require.Same(t, original, FromQueryError(context.Background(), original))
	})

	t.Run("ABI and existing RPC object data survive detail encoding", func(t *testing.T) {
		cause := &syntheticRPCError{
			message: vm.ErrExecutionReverted.Error(),
			code:    3,
			data:    map[string]interface{}{"reason": "existing"},
		}
		original := NewTxRejectError(cause, []byte{0xca, 0xfe, 0xba, 0xbe})
		enriched := QueryError(serverCtx, original)
		transport := roundTripQueryStatus(t, enriched)
		details := status.Convert(transport).Details()
		require.Len(t, details, 1)
		detail, ok := details[0].(*evmapi.QueryErrorDetail)
		require.True(t, ok)
		require.Equal(t, int64(3), detail.RpcCode)
		require.JSONEq(t, `{"reason":"existing"}`, string(detail.RpcDataJson))
		require.Equal(t, []byte{0xca, 0xfe, 0xba, 0xbe}, detail.RevertData)
		require.Nil(t, detail.CosmosError)
		decoded := FromQueryError(context.Background(), transport)

		var rejection *TxRejectError
		require.ErrorAs(t, decoded, &rejection)
		require.Equal(t, 3, rejection.ErrorCode())
		require.Equal(t, map[string]interface{}{"reason": "existing"}, rejection.ErrorData())
		require.Equal(t, []byte{0xca, 0xfe, 0xba, 0xbe}, rejection.RevertData())
		require.ErrorIs(t, decoded, transport)
		require.False(t, errors.Is(decoded, cause))
	})

	t.Run("nil RPC data survives detail encoding", func(t *testing.T) {
		cause := &syntheticRPCError{message: vm.ErrExecutionReverted.Error(), code: 3, data: nil}
		enriched := QueryError(serverCtx, NewTxRejectError(cause, []byte{0xca, 0xfe}))
		decoded := FromQueryError(context.Background(), roundTripQueryStatus(t, enriched))

		var rejection *TxRejectError
		require.ErrorAs(t, decoded, &rejection)
		var dataError rpcDataError
		require.ErrorAs(t, decoded, &dataError)
		require.Nil(t, rejection.ErrorData())
		require.Equal(t, []byte{0xca, 0xfe}, rejection.RevertData())
	})

	t.Run("existing gRPC status and details are retained", func(t *testing.T) {
		base, err := status.New(codes.InvalidArgument, "ante rejected").WithDetails(
			&errdetails.RequestInfo{RequestId: "request-1"},
		)
		require.NoError(t, err)
		original := NewTxRejectError(base.Err(), []byte{1, 2, 3, 4})
		enriched := QueryError(serverCtx, original)
		transport := roundTripQueryStatus(t, enriched)
		grpcStatus := status.Convert(transport)
		require.Equal(t, status.Code(original), grpcStatus.Code())
		require.Equal(t, status.Convert(original).Message(), grpcStatus.Message())
		require.Len(t, grpcStatus.Details(), 2)
		requestInfo, ok := grpcStatus.Details()[0].(*errdetails.RequestInfo)
		require.True(t, ok)
		require.Equal(t, "request-1", requestInfo.RequestId)
	})

	t.Run("Cosmos classification survives detail encoding", func(t *testing.T) {
		cause := errorsmod.Wrap(sdkerrors.ErrUnauthorized, "ante rejected transaction")
		enriched := QueryError(serverCtx, NewTxRejectError(cause, []byte{2, 3, 4, 5}))
		decoded := FromQueryError(context.Background(), roundTripQueryStatus(t, enriched))

		var cosmosError interface {
			Codespace() string
			ABCICode() uint32
		}
		require.ErrorAs(t, decoded, &cosmosError)
		require.Equal(t, sdkerrors.ErrUnauthorized.Codespace(), cosmosError.Codespace())
		require.Equal(t, sdkerrors.ErrUnauthorized.ABCICode(), cosmosError.ABCICode())
	})

	t.Run("wrapped gRPC message is preserved", func(t *testing.T) {
		original := fmt.Errorf(
			"outer: %w",
			NewTxRejectError(status.Error(codes.InvalidArgument, "invalid transaction"), []byte{1, 2, 3, 4}),
		)
		baseline := status.Convert(original)
		enriched := QueryError(serverCtx, original)
		require.Equal(t, baseline.Code(), status.Code(enriched))
		require.Equal(t, baseline.Message(), status.Convert(enriched).Message())
		require.Equal(t, original.Error(), enriched.Error())
		require.ErrorIs(t, enriched, original)

		transport := roundTripQueryStatus(t, enriched)
		require.Equal(t, baseline.Message(), status.Convert(transport).Message())
		decoded := FromQueryError(context.Background(), transport)
		require.Equal(t, transport.Error(), decoded.Error())
		require.ErrorIs(t, decoded, transport)
	})

	t.Run("valid detail is not duplicated", func(t *testing.T) {
		original := NewTxRejectError(errors.New("transaction rejected"), []byte{1, 2, 3, 4})
		first := QueryError(serverCtx, original)
		require.Same(t, first, QueryError(serverCtx, first))
		require.Len(t, status.Convert(first).Details(), 1)
	})

	t.Run("malformed detail remains unchanged", func(t *testing.T) {
		malformed, err := status.New(codes.Internal, "malformed detail").WithDetails(&evmapi.QueryErrorDetail{
			RpcCode:     -32000,
			RpcDataJson: []byte(`{"reason":`),
			RevertData:  []byte{1, 2, 3, 4},
		})
		require.NoError(t, err)
		original := malformed.Err()
		require.Same(t, original, FromQueryError(context.Background(), original))
	})
}

func TestQueryErrorDetailGogoCompatibility(t *testing.T) {
	// The SDK uses gogo messages; status.Details resolves the same wire type
	// through the modern protobuf registry used by the RPC transport adapter.
	detail := &evmtypes.QueryErrorDetail{
		RpcCode:     -32000,
		RevertData:  []byte{1, 2, 3, 4},
		RpcDataJson: []byte("null"),
		CosmosError: &evmtypes.QueryErrorDetail_CosmosError{Codespace: "sdk", Code: 5},
	}
	grpcStatus, err := status.New(codes.InvalidArgument, "rejected").WithDetails(detail)
	require.NoError(t, err)
	transport := roundTripQueryStatus(t, grpcStatus.Err())
	details := status.Convert(transport).Details()
	require.Len(t, details, 1)
	decodedDetail, ok := details[0].(*evmapi.QueryErrorDetail)
	require.True(t, ok)
	// Both generated APIs must also agree on the embedded message wire format.
	encoded, err := proto.Marshal(decodedDetail)
	require.NoError(t, err)
	var gogoDetail evmtypes.QueryErrorDetail
	require.NoError(t, gogoDetail.Unmarshal(encoded))
	require.Equal(t, detail, &gogoDetail)

	decoded := FromQueryError(context.Background(), transport)
	var rejection *TxRejectError
	require.ErrorAs(t, decoded, &rejection)
	require.Equal(t, -32000, rejection.ErrorCode())
	require.Nil(t, rejection.ErrorData())
	require.Equal(t, detail.RevertData, rejection.RevertData())
	key, found := common.ExtractCosmosErrorKey(decoded)
	require.True(t, found)
	require.Equal(t, common.NewCosmosErrorKey(sdkerrors.ErrInsufficientFunds), key)
}

func TestQueryErrorDetailPreservesWideRPCCode(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("RPC ErrorCode returns int; wide codes require a 64-bit platform")
	}
	serverCtx := queryGRPCContext()
	code := int64(1) << 40
	source := &syntheticRPCError{message: "custom RPC failure", code: int(code), data: nil}
	enriched := QueryError(serverCtx, NewTxRejectError(source, nil))
	decoded := FromQueryError(context.Background(), roundTripQueryStatus(t, enriched))
	var rejection *TxRejectError
	require.ErrorAs(t, decoded, &rejection)
	require.Equal(t, source.ErrorCode(), rejection.ErrorCode())
}

func TestQueryErrorWithStatusWithoutTransportReturnsPlainStatus(t *testing.T) {
	cause := errors.New("estimate rejected")
	err := QueryErrorWithStatus(context.Background(), codes.FailedPrecondition, cause)

	require.Equal(t, status.Error(codes.FailedPrecondition, cause.Error()), err)
	require.Empty(t, status.Convert(err).Details())
}

func TestQueryErrorLeavesOutOfGasUnchanged(t *testing.T) {
	localCtx := WithQueryErrorCapture(context.Background())
	remoteCtx := queryGRPCContext()
	cause := NewTxRejectError(vm.ErrOutOfGas, []byte{0xde, 0xad, 0xbe, 0xef})

	require.Same(t, cause, QueryError(localCtx, cause))
	require.Same(t, cause, QueryError(remoteCtx, cause))
	transport := status.Error(codes.ResourceExhausted, "out of gas")
	require.Same(t, transport, FromQueryError(localCtx, transport))
}

func TestQueryErrorTransportLeavesUnmappedErrorsUnchanged(t *testing.T) {
	ctx := queryGRPCContext()
	require.NoError(t, QueryError(ctx, nil))
	for _, source := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		sdkerrors.ErrIO,
		status.Error(codes.Unavailable, "query unavailable"),
		errors.New("unrecognized failure"),
	} {
		require.True(t, source == QueryError(ctx, source), "unexpected wrapper for %T", source)
	}
}

func roundTripQueryStatus(t *testing.T, err error) error {
	t.Helper()
	encoded, marshalErr := proto.Marshal(status.Convert(err).Proto())
	require.NoError(t, marshalErr)
	decoded := new(statuspb.Status)
	require.NoError(t, proto.Unmarshal(encoded, decoded))
	return status.FromProto(decoded).Err()
}

// queryGRPCStream models the server transport installed by gRPC before handlers
// run. Actual SDK query transport is also covered by the keeper bufconn tests.
type queryGRPCStream struct{}

func (queryGRPCStream) Method() string               { return evmapi.Query_EstimateGas_FullMethodName }
func (queryGRPCStream) SetHeader(metadata.MD) error  { return nil }
func (queryGRPCStream) SendHeader(metadata.MD) error { return nil }
func (queryGRPCStream) SetTrailer(metadata.MD) error { return nil }

func queryGRPCContext() context.Context {
	return grpc.NewContextWithServerTransportStream(context.Background(), queryGRPCStream{})
}

func TestQueryErrorGRPCTransportWithLocalCapture(t *testing.T) {
	ctx := WithQueryErrorCapture(queryGRPCContext())
	for _, convertStatus := range []bool{false, true} {
		t.Run(fmt.Sprintf("convertStatus=%v", convertStatus), func(t *testing.T) {
			source := sdkerrors.ErrInsufficientFunds
			var err error
			if convertStatus {
				err = QueryErrorWithStatus(ctx, codes.InvalidArgument, source)
			} else {
				err = QueryError(ctx, source)
			}
			transport := roundTripQueryStatus(t, err)
			// The client has no access to an interceptor's local capture.
			decoded := FromQueryError(context.Background(), transport)
			var rejection *TxRejectError
			require.ErrorAs(t, decoded, &rejection)
			definition := common.SharedErrorABI.Errors[common.SolidityErrSDKInsufficientFunds]
			require.Equal(t, definition.ID[:4], rejection.RevertData())
		})
	}
}
