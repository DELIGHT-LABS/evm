package tracer

import (
	"context"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

type testTracer struct {
	hooks *tracing.Hooks
}

func (t *testTracer) Hooks() *tracing.Hooks {
	return t.hooks
}

func newTestTracer(hooks *tracing.Hooks) *testTracer {
	return &testTracer{hooks: hooks}
}

func TestManagerDispatchesScopesAndUninstalls(t *testing.T) {
	var calls []string
	base := &tracing.Hooks{
		OnEnter: func(_ int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
			calls = append(calls, "base")
		},
	}
	manager := New(base)
	dispatcher := manager.Hooks()

	uninstallGlobal := manager.InstallGlobal(newTestTracer(&tracing.Hooks{
		OnEnter: func(_ int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
			calls = append(calls, "global")
		},
	}))
	uninstallSession := manager.InstallSession(newTestTracer(&tracing.Hooks{
		OnEnter: func(_ int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
			calls = append(calls, "session")
		},
	}))

	require.Same(t, dispatcher, manager.Hooks())
	dispatcher.OnEnter(0, 0, common.Address{}, common.Address{}, nil, 0, nil)
	require.Equal(t, []string{"base", "global", "session"}, calls)

	calls = nil
	uninstallSession()
	uninstallSession()
	dispatcher.OnEnter(0, 0, common.Address{}, common.Address{}, nil, 0, nil)
	require.Equal(t, []string{"base", "global"}, calls)

	calls = nil
	uninstallGlobal()
	dispatcher.OnEnter(0, 0, common.Address{}, common.Address{}, nil, 0, nil)
	require.Equal(t, []string{"base"}, calls)
}

func TestManagerUpdatesOnlySubscribedCallbacks(t *testing.T) {
	manager := New(&tracing.Hooks{
		OnTxStart: func(_ *tracing.VMContext, _ *ethtypes.Transaction, _ common.Address) {},
	})
	dispatcher := manager.Hooks()
	require.NotNil(t, dispatcher.OnTxStart)
	require.Nil(t, dispatcher.OnOpcode)

	opcodes := 0
	uninstall := manager.InstallSession(newTestTracer(&tracing.Hooks{
		OnOpcode: func(_ uint64, _ byte, _, _ uint64, _ tracing.OpContext, _ []byte, _ int, _ error) {
			opcodes++
		},
	}))
	require.Same(t, dispatcher, manager.Hooks())
	require.NotNil(t, dispatcher.OnOpcode)
	dispatcher.OnOpcode(0, 0, 0, 0, nil, nil, 0, nil)
	require.Equal(t, 1, opcodes)

	uninstall()
	require.Same(t, dispatcher, manager.Hooks())
	require.Nil(t, dispatcher.OnOpcode)
}

func TestManagerNestedHooksOmitTransactionBoundary(t *testing.T) {
	txStarts := 0
	txEnds := 0
	enters := 0
	manager := New(&tracing.Hooks{
		OnTxStart: func(_ *tracing.VMContext, _ *ethtypes.Transaction, _ common.Address) {
			txStarts++
		},
		OnTxEnd: func(_ *ethtypes.Receipt, _ error) {
			txEnds++
		},
	})
	nested := manager.NestedHooks()

	require.NotNil(t, manager.Hooks().OnTxStart)
	require.NotNil(t, manager.Hooks().OnTxEnd)
	require.Nil(t, nested.OnTxStart)
	require.Nil(t, nested.OnTxEnd)
	require.Nil(t, nested.OnEnter)

	manager.InstallSession(newTestTracer(&tracing.Hooks{
		OnEnter: func(_ int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
			enters++
		},
	}))
	require.Same(t, nested, manager.NestedHooks())
	require.NotNil(t, nested.OnEnter)

	manager.Hooks().OnTxStart(nil, nil, common.Address{})
	manager.Hooks().OnTxEnd(nil, nil)
	nested.OnEnter(0, 0, common.Address{}, common.Address{}, nil, 0, nil)
	require.Equal(t, 1, txStarts)
	require.Equal(t, 1, txEnds)
	require.Equal(t, 1, enters)
}

func TestManagerPreservesEveryHookField(t *testing.T) {
	hooksType := reflect.TypeOf(tracing.Hooks{})
	hooksValue := reflect.New(hooksType).Elem()
	for i := 0; i < hooksType.NumField(); i++ {
		fieldType := hooksType.Field(i).Type
		hooksValue.Field(i).Set(reflect.MakeFunc(fieldType, func(_ []reflect.Value) []reflect.Value {
			return nil
		}))
	}

	base := hooksValue.Addr().Interface().(*tracing.Hooks)
	dispatcherValue := reflect.ValueOf(New(base).Hooks()).Elem()
	for i := 0; i < hooksType.NumField(); i++ {
		require.Falsef(t, dispatcherValue.Field(i).IsNil(), "hook %s was not composed", hooksType.Field(i).Name)
	}
}

func TestManagerUsesDispatchSnapshot(t *testing.T) {
	manager := New(nil)
	var calls []string
	var uninstallSecond func()

	manager.InstallSession(newTestTracer(&tracing.Hooks{
		OnExit: func(_ int, _ []byte, _ uint64, _ error, _ bool) {
			calls = append(calls, "first")
			uninstallSecond()
		},
	}))
	uninstallSecond = manager.InstallSession(newTestTracer(&tracing.Hooks{
		OnExit: func(_ int, _ []byte, _ uint64, _ error, _ bool) {
			calls = append(calls, "second")
		},
	}))

	manager.Hooks().OnExit(0, nil, 0, nil, false)
	require.Equal(t, []string{"first", "second"}, calls)

	calls = nil
	manager.Hooks().OnExit(0, nil, 0, nil, false)
	require.Equal(t, []string{"first"}, calls)
}

func TestManagerContextRoundTrip(t *testing.T) {
	ctx := sdk.Context{}.WithContext(context.Background())
	_, ok := FromContext(ctx)
	require.False(t, ok)

	manager := New(nil)
	ctx = WithManager(ctx, manager)
	actual, ok := FromContext(ctx)
	require.True(t, ok)
	require.Same(t, manager, actual)
}
