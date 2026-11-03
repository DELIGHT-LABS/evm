package types

import (
	"encoding/hex"
	"errors"
)

const defaultTxRejectErrorCode = -32000

type rpcError interface {
	ErrorCode() int
}

type rpcDataError interface {
	ErrorData() interface{}
}

// TxRejectError represents a transaction failure for JSON-RPC, exposing Solidity
// ABI data while preserving the original error for diagnostics and identity
// checks. ResolveTxRejectError supplies shared error mapping; this type owns the
// RPC representation and cause preservation.
type TxRejectError struct {
	cause error
	data  []byte
}

// NewTxRejectError returns an RPC-compatible transaction rejection
// error. The input data is copied and is expected to contain ABI-encoded revert
// data, including its four-byte selector.
func NewTxRejectError(cause error, data []byte) *TxRejectError {
	return &TxRejectError{
		cause: cause,
		data:  append([]byte(nil), data...),
	}
}

func (e *TxRejectError) Error() string {
	if e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *TxRejectError) Unwrap() error {
	return e.cause
}

func (e *TxRejectError) ErrorCode() int {
	var rpcErr rpcError
	if errors.As(e.cause, &rpcErr) {
		return rpcErr.ErrorCode()
	}
	return defaultTxRejectErrorCode
}

func (e *TxRejectError) ErrorData() interface{} {
	var dataErr rpcDataError
	if errors.As(e.cause, &dataErr) {
		return dataErr.ErrorData()
	}
	return "0x" + hex.EncodeToString(e.data)
}

// RevertData returns a copy of the ABI-encoded revert data supplied to the
// constructor.
func (e *TxRejectError) RevertData() []byte {
	return append([]byte(nil), e.data...)
}
