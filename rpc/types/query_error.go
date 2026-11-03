package types

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/core/vm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	evmapi "github.com/cosmos/evm/api/cosmos/evm/vm/v1"
	common "github.com/cosmos/evm/precompiles/common"
)

type queryErrorStatus struct {
	cause     error
	status    *status.Status
	errorText string
}

type queryErrorCaptureKey struct{}

type queryErrorCapture struct {
	mu   sync.Mutex
	data *capturedQueryData
}

type capturedQueryData struct {
	cosmosKey  *common.CosmosErrorKey
	source     error
	rpcCode    int
	rpcData    interface{}
	revertData []byte
}

func (e *queryErrorStatus) Error() string {
	return e.errorText
}

func (e *queryErrorStatus) Unwrap() error {
	return e.cause
}

func (e *queryErrorStatus) GRPCStatus() *status.Status {
	return e.status
}

// queryRPCError keeps the received transport error and, for local ABCI queries,
// the original in-process source. Only the transport message is displayed.
type queryRPCError struct {
	cause  error
	source error
	code   int
	data   interface{}
}

type queryCosmosRPCError struct {
	*queryRPCError
	codespace string
	abciCode  uint32
}

func (e *queryCosmosRPCError) Codespace() string {
	return e.codespace
}

func (e *queryCosmosRPCError) ABCICode() uint32 {
	return e.abciCode
}

func (e *queryRPCError) Error() string {
	return e.cause.Error()
}

func (e *queryRPCError) Unwrap() []error {
	if e.source == nil {
		return []error{e.cause}
	}
	return []error{e.cause, e.source}
}

func (e *queryRPCError) ErrorCode() int {
	return e.code
}

func (e *queryRPCError) ErrorData() interface{} {
	return e.data
}

// WithQueryErrorCapture creates a request-local capture for in-process ABCI queries.
// gRPC responses carry error details independently of this local capture.
func WithQueryErrorCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, queryErrorCaptureKey{}, new(queryErrorCapture))
}

// QueryError captures ABI error data without changing an in-process query
// result. Across gRPC, it adds the data as a status detail while retaining the
// existing status code, message, and details.
//
// Keeper query handlers call this while the original error identity is still
// available. It uses ResolveTxRejectError for shared error conversion; backend
// callers restore the transported representation with FromQueryError.
func QueryError(ctx context.Context, original error) error {
	if original == nil || errors.Is(original, vm.ErrOutOfGas) {
		return original
	}
	capture := queryErrorCaptureFrom(ctx)
	hasTransport := grpc.ServerTransportStreamFromContext(ctx) != nil
	// ABCI routes share the keeper handler but must retain the original error:
	// wrapping an ordinary error in gRPC status changes SDK ABCI classification.
	if capture == nil && !hasTransport {
		return original
	}

	resolved, ok := ResolveTxRejectError(original).(*TxRejectError)
	if !ok {
		return original
	}
	if !hasTransport {
		capture.store(original, resolved)
		return original
	}

	base := status.Convert(original)
	if hasQueryErrorDetail(base) {
		return original
	}

	rpcData, err := json.Marshal(resolved.ErrorData())
	if err != nil {
		return original
	}
	detail := &evmapi.QueryErrorDetail{
		RpcCode:     int64(resolved.ErrorCode()),
		RevertData:  resolved.RevertData(),
		RpcDataJson: rpcData,
	}
	if key, found := queryCosmosErrorKey(resolved); found {
		detail.CosmosError = &evmapi.QueryErrorDetail_CosmosError{
			Codespace: key.Codespace,
			Code:      key.Code,
		}
	}

	enriched, err := base.WithDetails(detail)
	if err != nil {
		return original
	}
	return &queryErrorStatus{cause: original, status: enriched, errorText: original.Error()}
}

// QueryErrorWithStatus returns the ordinary gRPC status for the requested code
// and cause message. For an in-process query with a local capture, it retains
// the typed cause before status conversion; across gRPC, it adds error details.
func QueryErrorWithStatus(ctx context.Context, code codes.Code, cause error) error {
	if cause == nil {
		return nil
	}
	grpcStatus := status.New(code, cause.Error())
	if grpc.ServerTransportStreamFromContext(ctx) == nil {
		if capture := queryErrorCaptureFrom(ctx); capture != nil && !errors.Is(cause, vm.ErrOutOfGas) {
			if resolved, ok := ResolveTxRejectError(cause).(*TxRejectError); ok {
				capture.store(cause, resolved)
			}
		}
		return grpcStatus.Err()
	}
	original := &queryErrorStatus{
		cause:     cause,
		status:    grpcStatus,
		errorText: grpcStatus.Err().Error(),
	}
	return QueryError(ctx, original)
}

// FromQueryError restores captured in-process ABI data or decodes it from a
// gRPC status detail into the transaction rejection error used by JSON-RPC.
// The returned error preserves both the received transport error and, for a
// local capture, its original typed source. Unknown or malformed data leaves
// the transport error unchanged.
func FromQueryError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if capture := queryErrorCaptureFrom(ctx); capture != nil {
		if captured, ok := capture.load(); ok {
			cause := &queryRPCError{
				cause:  err,
				source: captured.source,
				code:   captured.rpcCode,
				data:   captured.rpcData,
			}
			if key := captured.cosmosKey; key != nil {
				return NewTxRejectError(&queryCosmosRPCError{
					queryRPCError: cause,
					codespace:     key.Codespace,
					abciCode:      key.Code,
				}, captured.revertData)
			}
			return NewTxRejectError(cause, captured.revertData)
		}
	}
	grpcStatus, ok := status.FromError(err)
	if !ok {
		return err
	}
	for _, raw := range grpcStatus.Details() {
		detail, ok := raw.(*evmapi.QueryErrorDetail)
		if !ok {
			continue
		}
		code := int(detail.RpcCode)
		if int64(code) != detail.RpcCode {
			continue
		}
		var data interface{}
		if decodeErr := json.Unmarshal(detail.RpcDataJson, &data); decodeErr != nil {
			continue
		}
		cause := &queryRPCError{cause: err, code: code, data: data}
		if key := detail.CosmosError; key != nil {
			return NewTxRejectError(&queryCosmosRPCError{
				queryRPCError: cause,
				codespace:     key.Codespace,
				abciCode:      key.Code,
			}, detail.RevertData)
		}
		return NewTxRejectError(cause, detail.RevertData)
	}
	return err
}

// queryCosmosErrorKey uses the shared classifier for legacy Cause chains and
// retains standard multi-error traversal for joined errors.
func queryCosmosErrorKey(err error) (common.CosmosErrorKey, bool) {
	if key, found := common.ExtractCosmosErrorKey(err); found {
		return key, true
	}
	var cosmosError interface {
		Codespace() string
		ABCICode() uint32
	}
	if errors.As(err, &cosmosError) {
		return common.CosmosErrorKey{Codespace: cosmosError.Codespace(), Code: cosmosError.ABCICode()}, true
	}
	return common.CosmosErrorKey{}, false
}

func queryErrorCaptureFrom(ctx context.Context) *queryErrorCapture {
	capture, _ := ctx.Value(queryErrorCaptureKey{}).(*queryErrorCapture)
	return capture
}

func (c *queryErrorCapture) store(source error, resolved *TxRejectError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data != nil {
		return
	}
	c.data = &capturedQueryData{
		source:     source,
		rpcCode:    resolved.ErrorCode(),
		rpcData:    resolved.ErrorData(),
		revertData: resolved.RevertData(),
	}
	if key, found := queryCosmosErrorKey(resolved); found {
		c.data.cosmosKey = &key
	}
}

func (c *queryErrorCapture) load() (capturedQueryData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		return capturedQueryData{}, false
	}
	// Captured bytes are immutable; FromQueryError copies them into TxRejectError.
	return *c.data, true
}

func hasQueryErrorDetail(grpcStatus *status.Status) bool {
	for _, raw := range grpcStatus.Details() {
		if _, ok := raw.(*evmapi.QueryErrorDetail); ok {
			return true
		}
	}
	return false
}
