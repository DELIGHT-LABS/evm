package statedb_test

import (
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/statedb"

	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/testutil"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestNestedCacheContextPrecompileJournal(t *testing.T) {
	for _, tc := range []struct {
		revert    bool
		openCache bool
	}{
		{},
		{revert: true},
		{openCache: true},
		{revert: true, openCache: true},
	} {
		t.Run(fmt.Sprintf("revert=%t/open_cache=%t", tc.revert, tc.openCache), func(t *testing.T) {
			key := storetypes.NewKVStoreKey("native")
			tkey := storetypes.NewTransientStoreKey("native_transient")
			ctx := testutil.DefaultContext(key, tkey).WithEventManager(sdk.NewEventManager())
			keeper := &atomicTestKeeper{key: key}
			db := statedb.New(ctx, keeper, emptyTxConfig)
			outer := common.HexToAddress("0x10")
			child := common.HexToAddress("0x20")
			db.AddBalance(outer, uint256.NewInt(1), tracing.BalanceChangeUnspecified)
			frame := db.Snapshot()
			db.AddBalance(child, uint256.NewInt(2), tracing.BalanceChangeUnspecified)

			cacheCtx, err := db.GetCacheContext()
			require.NoError(t, err)
			snapshot := db.MultiStoreSnapshot()
			require.NoError(t, db.AddPrecompileFn(snapshot))
			require.NoError(t, db.FlushToCacheCtx())
			// FlushToCacheCtx must expose dirty EVM state to native execution
			// without committing it to the outer Cosmos transaction.
			require.Equal(t, uint256.NewInt(2), keeper.GetAccount(cacheCtx, child).Balance)
			require.Nil(t, keeper.GetAccount(ctx, child))
			cacheCtx.KVStore(key).Set([]byte("escrow"), []byte("40"))
			cacheCtx.KVStore(key).Set([]byte("commitment"), []byte("1"))
			cacheCtx.EventManager().EmitEvent(sdk.NewEvent("native"))
			db.MarkEventProcessed(0)

			nestedCtx, write := cacheCtx.CacheContext()
			nestedCtx.KVStore(key).Set([]byte("callback"), []byte("1"))
			nestedCtx.EventManager().EmitEvent(sdk.NewEvent("callback"))
			if !tc.openCache {
				write()
				db.MarkEventProcessed(1)
			}

			// A subsequent native call adds another precompileCallChange.
			// Reverting the EVM frame must replay both entries in reverse.
			second := db.MultiStoreSnapshot()
			require.NoError(t, db.AddPrecompileFn(second))
			require.NoError(t, db.FlushToCacheCtx())
			cacheCtx.KVStore(key).Set([]byte("second"), []byte("1"))
			cacheCtx.EventManager().EmitEvent(sdk.NewEvent("second"))
			if tc.openCache {
				db.MarkEventProcessed(1)
				write()
			}
			db.MarkEventProcessed(2)

			for _, name := range []string{"escrow", "commitment", "callback", "second"} {
				require.Nil(t, ctx.KVStore(key).Get([]byte(name)), name)
			}
			require.Empty(t, ctx.EventManager().Events())
			if tc.revert {
				require.NotPanics(t, func() { db.RevertToSnapshot(frame) })
				require.Empty(t, cacheCtx.EventManager().Events())
				require.False(t, db.IsEventProcessed(0))
			}
			require.NoError(t, db.Commit())
			require.Equal(t, uint256.NewInt(1), keeper.GetAccount(ctx, outer).Balance)
			if tc.revert {
				require.Nil(t, keeper.GetAccount(ctx, child))
				require.Empty(t, ctx.EventManager().Events())
			} else {
				require.Equal(t, uint256.NewInt(2), keeper.GetAccount(ctx, child).Balance)
				require.Len(t, ctx.EventManager().Events(), 3)
				require.True(t, db.IsEventProcessed(2))
			}
			for _, name := range []string{"escrow", "commitment", "callback", "second"} {
				if tc.revert {
					require.Nil(t, ctx.KVStore(key).Get([]byte(name)), name)
					require.Nil(t, cacheCtx.KVStore(key).Get([]byte(name)), name)
				} else {
					require.NotNil(t, ctx.KVStore(key).Get([]byte(name)), name)
				}
			}
		})
	}
}
