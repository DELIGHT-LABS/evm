package types

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	errorsmod "cosmossdk.io/errors"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

type syntheticRPCError struct {
	message string
	code    int
	data    interface{}
}

func (e *syntheticRPCError) Error() string          { return e.message }
func (e *syntheticRPCError) ErrorCode() int         { return e.code }
func (e *syntheticRPCError) ErrorData() interface{} { return e.data }

func TestTxRejectError(t *testing.T) {
	t.Run("plain cause exposes ABI data and default RPC code", func(t *testing.T) {
		cause := errors.New("transaction rejected")
		input := []byte{0xde, 0xad, 0xbe, 0xef}
		err := NewTxRejectError(cause, input)

		require.Equal(t, cause.Error(), err.Error())
		require.ErrorIs(t, err, cause)
		require.Equal(t, -32000, err.ErrorCode())
		require.Equal(t, "0xdeadbeef", err.ErrorData())
		require.Equal(t, input, err.RevertData())
	})

	t.Run("wrapped RPC code and data take priority", func(t *testing.T) {
		cause := &syntheticRPCError{
			message: "execution reverted",
			code:    3,
			data:    "0x01020304",
		}
		err := NewTxRejectError(fmt.Errorf("outer: %w", cause), []byte{0xde, 0xad, 0xbe, 0xef})

		require.Equal(t, "outer: execution reverted", err.Error())
		require.Equal(t, 3, err.ErrorCode())
		require.Equal(t, "0x01020304", err.ErrorData())

		var rpcErr *syntheticRPCError
		require.ErrorAs(t, err, &rpcErr)
		require.Same(t, cause, rpcErr)
		require.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, err.RevertData())
	})

	t.Run("Cosmos error identity and ABCI key are preserved", func(t *testing.T) {
		cause := errorsmod.Wrap(sdkerrors.ErrUnauthorized, "ante rejected transaction")
		err := NewTxRejectError(cause, []byte{0xca, 0xfe})

		require.ErrorIs(t, err, sdkerrors.ErrUnauthorized)
		var cosmosErr *errorsmod.Error
		require.ErrorAs(t, err, &cosmosErr)
		require.Equal(t, sdkerrors.ErrUnauthorized.Codespace(), cosmosErr.Codespace())
		require.Equal(t, sdkerrors.ErrUnauthorized.ABCICode(), cosmosErr.ABCICode())
	})
}

func TestTxRejectErrorCopiesRevertData(t *testing.T) {
	input := []byte{0x01, 0x02, 0x03, 0x04}
	err := NewTxRejectError(errors.New("transaction rejected"), input)

	input[0] = 0xff
	require.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, err.RevertData())

	output := err.RevertData()
	output[1] = 0xff
	require.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, err.RevertData())
}
