package keeper

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/stretchr/testify/require"
)

func TestInvalidPredecessorError(t *testing.T) {
	for _, err := range []error{core.ErrIntrinsicGas, core.ErrFloorDataGas, core.ErrGasUintOverflow} {
		require.True(t, invalidPredecessorError(fmt.Errorf("wrapped: %w", err)))
		require.False(t, invalidPredecessorError(errors.Join(err, errors.New("storage failure"))))
	}
	require.False(t, invalidPredecessorError(errors.New("storage failure")))
}

func TestDeferredTraceForwardsChildExit(t *testing.T) {
	called := false
	failure := errors.New("child reverted")
	output := []byte{0x12}
	d := deferRootTrace(&tracing.Hooks{
		OnExit: func(depth int, data []byte, gas uint64, err error, reverted bool) {
			called = true
			require.Equal(t, 1, depth)
			require.Equal(t, output, data)
			require.Equal(t, uint64(10), gas)
			require.ErrorIs(t, err, failure)
			require.True(t, reverted)
		},
	})
	d.hooks.OnExit(1, output, 10, failure, true)
	require.True(t, called, "child exit must be forwarded immediately")
}

func TestDeferredTraceFinishesAfterFinalOutcome(t *testing.T) {
	var events []string
	var endReceipt *ethtypes.Receipt
	var rootOutput []byte
	var rootGas uint64
	var rootErr error
	var rootReverted bool
	d := deferRootTrace(&tracing.Hooks{
		OnExit: func(_ int, output []byte, gas uint64, err error, reverted bool) {
			events = append(events, "root exit")
			rootOutput, rootGas, rootErr, rootReverted = output, gas, err, reverted
		},
		OnTxEnd: func(receipt *ethtypes.Receipt, _ error) { events = append(events, "tx end"); endReceipt = receipt },
	})
	d.hooks.OnExit(0, nil, 10, nil, false)
	d.hooks.OnTxEnd(&ethtypes.Receipt{GasUsed: 10}, nil)
	events = append(events, "policy hook")
	final := &ethtypes.Receipt{GasUsed: 47, Status: ethtypes.ReceiptStatusFailed}
	d.finish(vm.ErrExecutionReverted, []byte{0xde, 0xad}, final, nil)
	require.Equal(t, []string{"policy hook", "root exit", "tx end"}, events)
	require.Equal(t, []byte{0xde, 0xad}, rootOutput)
	require.Equal(t, uint64(10), rootGas)
	require.ErrorIs(t, rootErr, vm.ErrExecutionReverted)
	require.True(t, rootReverted)
	require.Same(t, final, endReceipt)
}
