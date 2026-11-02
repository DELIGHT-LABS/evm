package statedb_test

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	storetypes "cosmossdk.io/store/types"
)

func TestNativeScopeReadsIncomingCacheAfterRead(t *testing.T) {
	for _, mode := range []string{"scope", "wrap", "nested_wrap", "trace"} {
		t.Run(mode, func(t *testing.T) {
			// Given a private scope and optional SDK KV caches above it.
			db, ctx, keeper := newNativeScopeDB(t)
			action, end, err := db.BeginNativeAction()
			require.NoError(t, err)
			incoming, write := action.CacheContext()
			k := []byte("balance")
			incoming.KVStore(keeper.key).Set(k, []byte("40"))
			native, scope, err := db.NewNativeScope(incoming)
			require.NoError(t, err)
			defer scope.Rollback()
			kv := native.MultiStore().GetKVStore(keeper.key)
			switch mode {
			case "wrap":
				kv = kv.CacheWrap().(storetypes.KVStore)
			case "nested_wrap":
				kv = kv.CacheWrap().(storetypes.KVStore).CacheWrap().(storetypes.KVStore)
			case "trace":
				kv = kv.CacheWrapWithTrace(io.Discard, nil).(storetypes.KVStore)
			}
			require.Equal(t, []byte("40"), kv.Get(k))

			// When the incoming SDK cache changes, untouched reads stay live.
			incoming.KVStore(keeper.key).Set(k, []byte("20"))

			// Then the private view observes it without publishing its caches.
			require.Equal(t, []byte("20"), kv.Get(k))
			require.True(t, kv.Has(k))
			require.Nil(t, action.KVStore(keeper.key).Get(k))
			scope.Rollback()
			write()
			end()
			require.NoError(t, db.Commit())
			require.Equal(t, []byte("20"), ctx.KVStore(keeper.key).Get(k))
		})
	}
}
