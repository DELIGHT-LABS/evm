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

type nilGasUnwrapper struct{}

func (nilGasUnwrapper) Error() string        { return "gas error with nil cause" }
func (nilGasUnwrapper) Unwrap() error        { return nil }
func (nilGasUnwrapper) Is(target error) bool { return target == core.ErrIntrinsicGas }

type nilNonGasUnwrapper struct{}

func (nilNonGasUnwrapper) Error() string { return "storage error with nil cause" }
func (nilNonGasUnwrapper) Unwrap() error { return nil }

func TestPredecessorReplayError(t *testing.T) {
	storageErr := errors.New("storage failure")
	for _, err := range []error{core.ErrIntrinsicGas, core.ErrFloorDataGas, core.ErrGasUintOverflow} {
		require.NoError(t, predecessorReplayError(nil, err))
		require.NoError(t, predecessorReplayError(nil, fmt.Errorf("wrapped: %w", err)))
		joined := errors.Join(err, storageErr)
		require.Same(t, joined, predecessorReplayError(nil, joined))
		wrapped := fmt.Errorf("wrapped: %w", joined)
		require.Same(t, wrapped, predecessorReplayError(nil, wrapped))
	}
	require.Same(t, storageErr, predecessorReplayError(nil, storageErr))

	t.Run("nil unwrap preserves gas terminal classification", func(t *testing.T) {
		err := nilGasUnwrapper{}
		require.ErrorIs(t, err, core.ErrIntrinsicGas)
		require.NoError(t, predecessorReplayError(nil, err))
		require.NoError(t, predecessorReplayError(nil, fmt.Errorf("wrapped: %w", err)))
	})

	t.Run("nil unwrap rejects nongas terminal", func(t *testing.T) {
		err := &nilNonGasUnwrapper{}
		require.Same(t, err, predecessorReplayError(nil, err))
		wrapped := fmt.Errorf("wrapped: %w", err)
		require.Same(t, wrapped, predecessorReplayError(nil, wrapped))
	})

	gasErr := fmt.Errorf("wrapped: %w", core.ErrIntrinsicGas)
	cause := errors.New("latched failure")
	wrappedStorage := fmt.Errorf("wrapped: %w", storageErr)
	wrappedJoin := fmt.Errorf("wrapped: %w", errors.Join(core.ErrIntrinsicGas, storageErr))
	for _, tc := range []struct {
		name      string
		candidate *txCandidateResult
		replayErr error
		want      error
	}{
		{"nil replay error", nil, nil, nil},
		{"nil replay error ignores metadata", &txCandidateResult{cause: cause, hookFailed: true}, nil, nil},
		{"gas without metadata", &txCandidateResult{}, gasErr, nil},
		{"gas with cause", &txCandidateResult{cause: cause}, gasErr, cause},
		{"gas with hook failure", &txCandidateResult{hookFailed: true}, gasErr, gasErr},
		{"gas with cause and hook failure", &txCandidateResult{cause: cause, hookFailed: true}, gasErr, cause},
		{"nongas precedes metadata", &txCandidateResult{cause: cause, hookFailed: true}, wrappedStorage, wrappedStorage},
		{"joined error precedes metadata", &txCandidateResult{cause: cause, hookFailed: true}, wrappedJoin, wrappedJoin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := predecessorReplayError(tc.candidate, tc.replayErr)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.Same(t, tc.want, err)
			}
		})
	}
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
