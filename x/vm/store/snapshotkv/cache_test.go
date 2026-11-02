package snapshotkv_test

import (
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/evm/x/vm/store/snapshotkv"

	"cosmossdk.io/store/cachekv"
	"cosmossdk.io/store/dbadapter"
	storetypes "cosmossdk.io/store/types"
)

func newCacheParent() *cachekv.Store {
	return cachekv.NewStore(dbadapter.Store{DB: dbm.NewMemDB()})
}

func TestCacheReadsParentAfterRead(t *testing.T) {
	for _, mode := range []string{"direct", "wrap", "nested", "trace"} {
		t.Run(mode, func(t *testing.T) {
			// Given existing and absent keys already observed through the cache.
			parent := newCacheParent()
			parent.Set([]byte("changed"), []byte("40"))
			parent.Set([]byte("deleted"), []byte("40"))
			cache := snapshotkv.NewCache(parent)
			switch mode {
			case "wrap":
				cache = cache.CacheWrap().(storetypes.CacheKVStore)
			case "nested":
				cache = cache.CacheWrap().CacheWrap().(storetypes.CacheKVStore)
			case "trace":
				cache = cache.CacheWrapWithTrace(io.Discard, nil).(storetypes.CacheKVStore)
			}
			require.Equal(t, []byte("40"), cache.Get([]byte("changed")))
			require.True(t, cache.Has([]byte("deleted")))
			require.False(t, cache.Has([]byte("inserted")))

			// When the parent changes after those reads.
			parent.Set([]byte("changed"), []byte("20"))
			parent.Delete([]byte("deleted"))
			parent.Set([]byte("inserted"), []byte("20"))

			// Then no clean value or absence masks the parent.
			require.Equal(t, []byte("20"), cache.Get([]byte("changed")))
			require.False(t, cache.Has([]byte("deleted")))
			require.Equal(t, []byte("20"), cache.Get([]byte("inserted")))
		})
	}
}

func TestCachePreservesExplicitWriteIntent(t *testing.T) {
	parent := newCacheParent()
	parent.Set([]byte("same"), []byte("40"))
	cache := snapshotkv.NewCache(parent)
	cache.Set([]byte("same"), []byte("40"))
	cache.Delete([]byte("missing"))

	parent.Set([]byte("same"), []byte("20"))
	parent.Set([]byte("missing"), []byte("20"))

	require.Equal(t, []byte("40"), cache.Get([]byte("same")))
	require.False(t, cache.Has([]byte("missing")))
	require.Equal(t, []byte("20"), parent.Get([]byte("same")))
	cache.Write()
	require.Equal(t, []byte("40"), parent.Get([]byte("same")))
	require.False(t, parent.Has([]byte("missing")))
	parent.Set([]byte("same"), []byte("10"))
	require.Equal(t, []byte("10"), cache.Get([]byte("same")), "Write must clear the local delta")
}

func TestCacheResolvesParentAtWrite(t *testing.T) {
	first, second := newCacheParent(), newCacheParent()
	first.Set([]byte("key"), []byte("40"))
	second.Set([]byte("key"), []byte("20"))
	current := storetypes.KVStore(first)
	cache := snapshotkv.NewCacheWithParent(func() storetypes.KVStore { return current })
	require.Equal(t, []byte("40"), cache.Get([]byte("key")))
	cache.Set([]byte("private"), []byte("value"))

	current = second

	require.Equal(t, []byte("20"), cache.Get([]byte("key")))
	cache.Write()
	require.Nil(t, first.Get([]byte("private")))
	require.Equal(t, []byte("value"), second.Get([]byte("private")))
}

func TestCacheRejectsExpiredWrite(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprintf("dirty=%t", dirty), func(t *testing.T) {
			parent := newCacheParent()
			open := true
			cache := snapshotkv.NewCacheWithParent(func() storetypes.KVStore {
				if !open {
					panic("closed parent")
				}
				return parent
			})
			open = false
			if dirty {
				cache.Set([]byte("private"), []byte("value"))
			}

			require.Panics(t, cache.Write)
			require.Nil(t, parent.Get([]byte("private")))
		})
	}
}

func TestCacheWrapExpiresAfterParentWrite(t *testing.T) {
	for _, mode := range []string{"constructor", "wrap", "trace"} {
		t.Run(mode, func(t *testing.T) {
			base := newCacheParent()
			parent := snapshotkv.NewCache(base)
			child := parent.CacheWrap()
			switch mode {
			case "constructor":
				child = snapshotkv.NewCache(parent)
			case "trace":
				child = parent.CacheWrapWithTrace(io.Discard, nil)
			}
			parent.Write()
			child.(storetypes.KVStore).Set([]byte("stale"), []byte("value"))

			require.Panics(t, child.Write)
			require.Nil(t, base.Get([]byte("stale")))
		})
	}
}

func TestCacheWrapIteratorsFollowParent(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%t", nested), func(t *testing.T) {
			parent := newCacheParent()
			parent.Set([]byte("changed"), []byte("40"))
			parent.Set([]byte("deleted"), []byte("40"))
			parent.Set([]byte("local"), []byte("40"))
			cache := snapshotkv.NewCache(parent).CacheWrap().(storetypes.CacheKVStore)
			if nested {
				cache = cache.CacheWrap().(storetypes.CacheKVStore)
			}
			require.Equal(t, []byte("40"), cache.Get([]byte("changed")))
			require.True(t, cache.Has([]byte("deleted")))
			require.False(t, cache.Has([]byte("inserted")))
			cache.Set([]byte("local"), []byte("40"))
			cache.Delete([]byte("missing"))

			for _, changed := range []bool{false, true} {
				want := []string{"changed=40", "deleted=40", "local=40"}
				reversed := []string{"local=40", "deleted=40", "changed=40"}
				if changed {
					parent.Set([]byte("changed"), []byte("20"))
					parent.Delete([]byte("deleted"))
					parent.Set([]byte("inserted"), []byte("20"))
					parent.Set([]byte("local"), []byte("20"))
					parent.Set([]byte("missing"), []byte("20"))
					want = []string{"changed=20", "inserted=20", "local=40"}
					reversed = []string{"local=40", "inserted=20", "changed=20"}
				}
				for _, reverse := range []bool{false, true} {
					var iter storetypes.Iterator
					expected := want
					if reverse {
						iter = cache.ReverseIterator(nil, nil)
						expected = reversed
					} else {
						iter = cache.Iterator(nil, nil)
					}
					var got []string
					for ; iter.Valid(); iter.Next() {
						got = append(got, fmt.Sprintf("%s=%s", iter.Key(), iter.Value()))
					}
					require.NoError(t, iter.Close())
					require.Equal(t, expected, got)
				}
			}
			require.Equal(t, []byte("20"), cache.Get([]byte("changed")))
			require.False(t, cache.Has([]byte("deleted")))
			require.Equal(t, []byte("20"), cache.Get([]byte("inserted")))
			require.Equal(t, []byte("40"), cache.Get([]byte("local")))
			require.False(t, cache.Has([]byte("missing")))
			require.Equal(t, []byte("20"), parent.Get([]byte("local")))
		})
	}
}
