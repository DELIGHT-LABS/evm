package keeper

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/statedb"
	vmtracer "github.com/cosmos/evm/x/vm/tracer"
	vmsessiontracer "github.com/cosmos/evm/x/vm/tracer/session"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

type testExecutionTracer struct {
	hooks *tracing.Hooks
}

func (t *testExecutionTracer) Hooks() *tracing.Hooks {
	return t.hooks
}

func TestPrepareTracingInstallsSessionTracerWithoutApplicationFactories(t *testing.T) {
	k := Keeper{}
	ctx := sdk.Context{}.WithContext(context.Background())

	actualCtx, hooks := k.prepareTracing(ctx, core.Message{}, statedb.NewEmptyTxConfig(), false)
	require.NotNil(t, hooks)
	_, ok := vmtracer.FromContext(actualCtx)
	require.True(t, ok)

	calls := 0
	cleanup, ok := vmsessiontracer.Install(actualCtx, vmsessiontracer.New(&tracing.Hooks{
		OnEnter: func(_ int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
			calls++
		},
	}))
	require.True(t, ok)
	t.Cleanup(cleanup)

	hooks.OnEnter(0, 0, common.Address{}, common.Address{}, nil, 0, nil)
	require.Equal(t, 1, calls)
}

func TestPrepareTracingInstallsApplicationFactories(t *testing.T) {
	k := Keeper{}
	ctx := sdk.Context{}.WithContext(context.Background())
	txConfig := statedb.NewTxConfig(common.HexToHash("0x1"), 7)
	calls := 0

	k.SetGlobalTracerFactories(func(factoryCtx sdk.Context, execution vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		manager, ok := vmtracer.FromContext(factoryCtx)
		require.True(t, ok)
		require.NotNil(t, manager)
		require.Equal(t, txConfig.TxHash, execution.TxHash)
		require.Equal(t, uint64(txConfig.TxIndex), execution.TxIndex)
		require.True(t, execution.Commit)
		return factoryCtx, &testExecutionTracer{hooks: &tracing.Hooks{
			OnEnter: func(_ int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
				calls++
			},
		}}
	})

	actualCtx, hooks := k.prepareTracing(ctx, core.Message{}, txConfig, true)
	require.NotNil(t, hooks)
	_, ok := vmtracer.FromContext(actualCtx)
	require.True(t, ok)

	hooks.OnEnter(0, 0, common.Address{}, common.Address{}, nil, 0, nil)
	require.Equal(t, 1, calls)
}
