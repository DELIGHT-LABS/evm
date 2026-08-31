package session

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	vmtracer "github.com/cosmos/evm/x/vm/tracer"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestInstall(t *testing.T) {
	ctx := sdk.Context{}.WithContext(context.Background())
	cleanup, ok := Install(ctx, New(&tracing.Hooks{}))
	require.False(t, ok)
	cleanup()

	manager := vmtracer.New(nil)
	ctx = vmtracer.WithManager(ctx, manager)
	calls := 0
	cleanup, ok = Install(ctx, New(&tracing.Hooks{
		OnEnter: func(_ int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
			calls++
		},
	}))
	require.True(t, ok)

	manager.Hooks().OnEnter(0, 0, common.Address{}, common.Address{}, nil, 0, nil)
	require.Equal(t, 1, calls)
	cleanup()
	require.Nil(t, manager.Hooks().OnEnter)
}

func TestWithDepthOffset(t *testing.T) {
	var enterDepth, exitDepth, opcodeDepth, faultDepth int
	txEnds := 0
	hooks := &tracing.Hooks{
		OnEnter: func(depth int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
			enterDepth = depth
		},
		OnExit: func(depth int, _ []byte, _ uint64, _ error, _ bool) {
			exitDepth = depth
		},
		OnOpcode: func(_ uint64, _ byte, _, _ uint64, _ tracing.OpContext, _ []byte, depth int, _ error) {
			opcodeDepth = depth
		},
		OnFault: func(_ uint64, _ byte, _, _ uint64, _ tracing.OpContext, depth int, _ error) {
			faultDepth = depth
		},
		OnTxEnd: func(_ *ethtypes.Receipt, _ error) {
			txEnds++
		},
	}
	offset := WithDepthOffset(hooks, 2).Hooks()

	offset.OnEnter(1, 0, common.Address{}, common.Address{}, nil, 0, nil)
	offset.OnExit(2, nil, 0, nil, false)
	offset.OnOpcode(0, 0, 0, 0, nil, nil, 3, nil)
	offset.OnFault(0, 0, 0, 0, nil, 4, nil)
	offset.OnTxEnd(nil, nil)

	require.Equal(t, 3, enterDepth)
	require.Equal(t, 4, exitDepth)
	require.Equal(t, 5, opcodeDepth)
	require.Equal(t, 6, faultDepth)
	require.Equal(t, 1, txEnds)
	require.Nil(t, WithDepthOffset(nil, 1).Hooks())
}
