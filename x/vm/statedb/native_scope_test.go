package statedb_test

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/statedb"
	"github.com/cosmos/evm/x/vm/types"

	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/testutil"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

func newNativeScopeDB(t *testing.T) (*statedb.StateDB, sdk.Context, *atomicTestKeeper) {
	t.Helper()
	key := storetypes.NewKVStoreKey("native_scope")
	ctx := testutil.DefaultContext(key, storetypes.NewTransientStoreKey("native_scope_t")).
		WithEventManager(sdk.NewEventManager())
	keeper := &atomicTestKeeper{key: key}
	return statedb.New(ctx, keeper, emptyTxConfig), ctx, keeper
}

func TestNativeScopeWriteLeavesIncomingSDKCachePrivate(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	child, write := action.CacheContext()
	native, scope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer scope.Rollback()

	native.KVStore(keeper.key).Set([]byte("private"), []byte("value"))
	native.EventManager().EmitEvent(sdk.NewEvent("private"))
	require.Nil(t, child.KVStore(keeper.key).Get([]byte("private")))
	require.Empty(t, child.EventManager().Events())
	require.Nil(t, action.KVStore(keeper.key).Get([]byte("private")))
	require.PanicsWithValue(t, types.ErrNativeScopeOrder, func() {
		native.MultiStore().(storetypes.CacheMultiStore).Write()
	})
	require.PanicsWithValue(t, types.ErrNativeScopeOrder, func() {
		native.MultiStore().GetKVStore(keeper.key).(storetypes.CacheKVStore).Write()
	})
	require.NoError(t, scope.Write())
	require.Equal(t, []byte("value"), child.KVStore(keeper.key).Get([]byte("private")))
	require.Equal(t, sdk.Events{sdk.NewEvent("private")}, child.EventManager().Events())
	require.Nil(t, action.KVStore(keeper.key).Get([]byte("private")))
	require.Empty(t, action.EventManager().Events())
	require.Nil(t, ctx.KVStore(keeper.key).Get([]byte("private")))

	write()
	require.Equal(t, []byte("value"), action.KVStore(keeper.key).Get([]byte("private")))
	end()
	require.NoError(t, db.Commit())
	require.Equal(t, []byte("value"), ctx.KVStore(keeper.key).Get([]byte("private")))
	require.Equal(t, sdk.Events{sdk.NewEvent("private")}, ctx.EventManager().Events())
}

func TestNativeScopeRollbackPreservesEarlierSibling(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	sibling, siblingWrite := action.CacheContext()
	first, firstScope, err := db.NewNativeScope(sibling)
	require.NoError(t, err)
	defer firstScope.Rollback()
	first.KVStore(keeper.key).Set([]byte("shared"), []byte("sibling"))
	first.EventManager().EmitEvent(sdk.NewEvent("sibling"))
	kept := common.HexToAddress("0x10")
	db.AddBalance(kept, uint256.NewInt(3), tracing.BalanceChangeUnspecified)
	require.NoError(t, firstScope.Write())
	siblingWrite()

	incoming, _ := action.CacheContext()
	outer, outerScope, err := db.NewNativeScope(incoming)
	require.NoError(t, err)
	defer outerScope.Rollback()
	child, childWrite := outer.CacheContext()
	nested, nestedScope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer nestedScope.Rollback()
	nested.KVStore(keeper.key).Set([]byte("shared"), []byte("nested"))
	nested.KVStore(keeper.key).Set([]byte("child"), []byte("discarded"))
	nested.EventManager().EmitEvent(sdk.NewEvent("nested"))
	lost := common.HexToAddress("0x20")
	db.AddBalance(lost, uint256.NewInt(9), tracing.BalanceChangeUnspecified)
	db.AddLog(&ethtypes.Log{Address: lost})
	db.AddRefund(17)
	require.NoError(t, nestedScope.Write())
	childWrite()
	require.Equal(t, []byte("nested"), outer.KVStore(keeper.key).Get([]byte("shared")))
	outerScope.Rollback()

	require.Equal(t, uint256.NewInt(3), db.GetBalance(kept))
	require.Zero(t, db.GetBalance(lost).Sign())
	require.Empty(t, db.Logs())
	require.Zero(t, db.GetRefund())
	active, err := db.GetCacheContext()
	require.NoError(t, err)
	require.Equal(t, action.MultiStore(), active.MultiStore())
	end()
	require.NoError(t, db.Commit())
	require.Equal(t, []byte("sibling"), ctx.KVStore(keeper.key).Get([]byte("shared")))
	require.Nil(t, ctx.KVStore(keeper.key).Get([]byte("child")))
	require.Equal(t, sdk.Events{sdk.NewEvent("sibling")}, ctx.EventManager().Events())
	require.Equal(t, uint256.NewInt(3), keeper.GetAccount(ctx, kept).Balance)
	require.Nil(t, keeper.GetAccount(ctx, lost))
}

func TestNativeScopeWrittenDescendantsRevertWithOuterEVMFrame(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	frame := db.Snapshot()
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	incoming, write := action.CacheContext()
	outer, outerScope, err := db.NewNativeScope(incoming)
	require.NoError(t, err)
	defer outerScope.Rollback()
	child, childWrite := outer.CacheContext()
	native, scope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer scope.Rollback()
	addr := common.HexToAddress("0x30")
	native.KVStore(keeper.key).Set([]byte("child"), []byte("value"))
	native.EventManager().EmitEvent(sdk.NewEvent("child"))
	db.AddBalance(addr, uint256.NewInt(7), tracing.BalanceChangeUnspecified)
	db.SetNonce(addr, 4, tracing.NonceChangeUnspecified)
	db.AddLog(&ethtypes.Log{Address: addr})
	db.AddRefund(21)
	require.NoError(t, scope.Write())
	childWrite()
	require.NoError(t, outerScope.Write())
	write()
	end()

	require.NotPanics(t, func() { db.RevertToSnapshot(frame) })
	require.Zero(t, db.GetBalance(addr).Sign())
	require.Zero(t, db.GetNonce(addr))
	require.Empty(t, db.Logs())
	require.Zero(t, db.GetRefund())
	require.Nil(t, action.KVStore(keeper.key).Get([]byte("child")))
	require.Empty(t, action.EventManager().Events())
	require.NoError(t, db.Commit())
	require.Nil(t, ctx.KVStore(keeper.key).Get([]byte("child")))
	require.Nil(t, keeper.GetAccount(ctx, addr))
	require.Empty(t, ctx.EventManager().Events())
}

func TestNativeScopeSurvivesInnerEVMRevert(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	incoming, write := action.CacheContext()
	native, scope, err := db.NewNativeScope(incoming)
	require.NoError(t, err)
	defer scope.Rollback()
	native.KVStore(keeper.key).Set([]byte("before"), []byte("kept"))
	native.EventManager().EmitEvent(sdk.NewEvent("before"))
	frame := db.Snapshot()
	checkpoint := db.MultiStoreSnapshot()
	require.NoError(t, db.AddPrecompileFn(checkpoint))
	native.KVStore(keeper.key).Set([]byte("callback"), []byte("lost"))
	native.EventManager().EmitEvent(sdk.NewEvent("callback"))
	addr := common.HexToAddress("0x40")
	db.AddBalance(addr, uint256.NewInt(8), tracing.BalanceChangeUnspecified)
	db.AddRefund(12)
	db.AddLog(&ethtypes.Log{Address: addr})
	native.GasMeter().ConsumeGas(5, "callback")
	gas := native.GasMeter().GasConsumed()
	require.NotPanics(t, func() { db.RevertToSnapshot(frame) })
	require.Equal(t, gas, native.GasMeter().GasConsumed())
	require.Nil(t, native.KVStore(keeper.key).Get([]byte("callback")))
	require.Equal(t, []byte("kept"), native.KVStore(keeper.key).Get([]byte("before")))
	require.Equal(t, sdk.Events{sdk.NewEvent("before")}, native.EventManager().Events())
	require.Zero(t, db.GetBalance(addr).Sign())
	require.Empty(t, db.Logs())
	require.Zero(t, db.GetRefund())
	native.KVStore(keeper.key).Set([]byte("after"), []byte("kept"))
	native.EventManager().EmitEvent(sdk.NewEvent("after"))
	require.NoError(t, scope.Write())
	write()
	end()
	require.NoError(t, db.Commit())
	require.Equal(t, []byte("kept"), ctx.KVStore(keeper.key).Get([]byte("after")))
	require.Nil(t, ctx.KVStore(keeper.key).Get([]byte("callback")))
	require.Equal(t, sdk.Events{sdk.NewEvent("before"), sdk.NewEvent("after")}, ctx.EventManager().Events())
}

func TestNativeScopeQueryRollbackEvictsLazyAccountReads(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	addr := common.HexToAddress("0x50")
	require.NoError(t, keeper.SetAccount(ctx, addr, statedb.Account{Balance: uint256.NewInt(2)}))
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	child, _ := action.CacheContext()
	native, scope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer scope.Rollback()
	require.NoError(t, keeper.SetAccount(native, addr, statedb.Account{Balance: uint256.NewInt(11)}))
	require.Equal(t, uint256.NewInt(11), db.GetBalance(addr))
	require.True(t, db.Exist(addr))
	native.EventManager().EmitEvent(sdk.NewEvent("query"))
	native.GasMeter().ConsumeGas(3, "query")
	gas := native.GasMeter().GasConsumed()
	scope.Rollback()
	require.Equal(t, gas, action.GasMeter().GasConsumed())
	require.Equal(t, uint256.NewInt(2), db.GetBalance(addr), "rollback must evict the read-only cached account")
	require.Empty(t, action.EventManager().Events())
	end()
	require.NoError(t, db.Commit())
	require.Equal(t, uint256.NewInt(2), keeper.GetAccount(ctx, addr).Balance)
	require.Empty(t, ctx.EventManager().Events())
}

func TestNativeScopeRejectsInvalidLifecycle(t *testing.T) {
	t.Run("out of order write does not merge", func(t *testing.T) {
		db, _, keeper := newNativeScopeDB(t)
		action, end, err := db.BeginNativeAction()
		require.NoError(t, err)
		defer end()
		incoming, _ := action.CacheContext()
		outer, parent, err := db.NewNativeScope(incoming)
		require.NoError(t, err)
		defer parent.Rollback()
		child, _ := outer.CacheContext()
		native, nested, err := db.NewNativeScope(child)
		require.NoError(t, err)
		defer nested.Rollback()
		native.KVStore(keeper.key).Set([]byte("nested"), []byte("private"))
		require.Error(t, parent.Write())
		require.Nil(t, incoming.KVStore(keeper.key).Get([]byte("nested")))
		require.Nil(t, action.KVStore(keeper.key).Get([]byte("nested")))
		nested.Rollback()
		parent.Rollback()
	})

	for _, writeScope := range []bool{false, true} {
		name := "rollback"
		if writeScope {
			name = "write"
		}
		t.Run("closed "+name, func(t *testing.T) {
			db, ctx, keeper := newNativeScopeDB(t)
			action, end, err := db.BeginNativeAction()
			require.NoError(t, err)
			incoming, write := action.CacheContext()
			native, scope, err := db.NewNativeScope(incoming)
			require.NoError(t, err)
			defer scope.Rollback()
			stale := native.KVStore(keeper.key)
			stale.Set([]byte("value"), []byte("original"))
			interior := db.Snapshot()
			if writeScope {
				require.NoError(t, scope.Write())
			} else {
				scope.Rollback()
			}
			require.Error(t, scope.Write())
			require.Panics(t, func() { db.RevertToSnapshot(interior) })
			// Store APIs cannot return an error; stale writes fail closed.
			require.Panics(t, func() {
				stale.Set([]byte("value"), []byte("stale"))
			})
			write()
			end()
			require.NoError(t, db.Commit())
			if writeScope {
				require.Equal(t, []byte("original"), ctx.KVStore(keeper.key).Get([]byte("value")))
			} else {
				require.Nil(t, ctx.KVStore(keeper.key).Get([]byte("value")))
			}
		})
	}
}

func TestNativeScopeRejectsUnownedContexts(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	raw, _ := ctx.CacheContext()
	_, scope, err := db.NewNativeScope(raw)
	require.Error(t, err)
	require.Nil(t, scope)
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	defer end()
	// Once a child is active, another scope cannot bypass it by reusing the
	// older ancestor context. Opaque SDK caches themselves remain SDK-owned.
	child, _ := action.CacheContext()
	_, active, err := db.NewNativeScope(child)
	require.NoError(t, err)
	_, scope, err = db.NewNativeScope(action)
	require.ErrorIs(t, err, types.ErrNativeScopeContext)
	require.Nil(t, scope)
	active.Rollback()
	other := statedb.New(ctx, keeper, emptyTxConfig)
	_, scope, err = other.NewNativeScope(action)
	require.Error(t, err)
	require.Nil(t, scope)
	require.Empty(t, ctx.EventManager().Events())
}

func TestNativeRetainedKVHandleFollowsCheckpoint(t *testing.T) {
	for _, inScope := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			name := "root"
			if inScope {
				name = "scope"
			}
			if wrapped {
				name += "/cache-wrap"
			}
			t.Run(name, func(t *testing.T) {
				// Given a handle retained before an inner native checkpoint.
				db, ctx, keeper := newNativeScopeDB(t)
				action, end, err := db.BeginNativeAction()
				require.NoError(t, err)
				native := action
				var scope *statedb.NativeScope
				var write func()
				if inScope {
					var incoming sdk.Context
					incoming, write = action.CacheContext()
					native, scope, err = db.NewNativeScope(incoming)
					require.NoError(t, err)
					defer scope.Rollback()
				}
				retained := native.MultiStore().GetKVStore(keeper.key)
				retained.Set([]byte("value"), []byte("before"))
				if wrapped {
					retained = retained.CacheWrap().(storetypes.KVStore)
				}
				frame := db.Snapshot()
				_, finish, err := db.BeginNativeAction()
				require.NoError(t, err)

				// When the callback writes through that handle and then reverts.
				retained.Set([]byte("value"), []byte("callback"))
				if wrapped {
					retained.(storetypes.CacheKVStore).Write()
				}
				finish()
				db.RevertToSnapshot(frame)

				// Then its write cannot escape below the newer checkpoint.
				require.Equal(t, []byte("before"), native.KVStore(keeper.key).Get([]byte("value")))
				if inScope {
					require.NoError(t, scope.Write())
					write()
				}
				end()
				require.NoError(t, db.Commit())
				require.Equal(t, []byte("before"), ctx.KVStore(keeper.key).Get([]byte("value")))
			})
		}
	}
}

func TestNativeKVHandleCreatedInRevertedCallbackExpires(t *testing.T) {
	db, _, keeper := newNativeScopeDB(t)
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	defer end()
	parent := action.MultiStore().GetKVStore(keeper.key)
	parent.Set([]byte("value"), []byte("before"))
	frame := db.Snapshot()
	_, finish, err := db.BeginNativeAction()
	require.NoError(t, err)
	callback := action.MultiStore().GetKVStore(keeper.key)
	wrapped := parent.CacheWrap().(storetypes.CacheKVStore)
	wrapped.Set([]byte("value"), []byte("failed"))
	finish()
	db.RevertToSnapshot(frame)
	_, finishReplacement, err := db.BeginNativeAction()
	require.NoError(t, err)
	defer finishReplacement()

	require.Panics(t, func() { callback.Set([]byte("value"), []byte("stale")) })
	require.Panics(t, wrapped.Write)
	require.Equal(t, []byte("before"), parent.Get([]byte("value")))
}
