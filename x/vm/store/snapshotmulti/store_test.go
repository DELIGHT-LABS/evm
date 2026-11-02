package snapshotmulti_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/evm/x/vm/store/snapshotmulti"

	"cosmossdk.io/store/cachekv"
	"cosmossdk.io/store/dbadapter"
	storetypes "cosmossdk.io/store/types"
)

func setupStore() (*snapshotmulti.Store, *storetypes.KVStoreKey) {
	key := storetypes.NewKVStoreKey("store")
	kv := cachekv.NewStore(dbadapter.Store{DB: dbm.NewMemDB()})
	stores := map[*storetypes.KVStoreKey]storetypes.CacheWrap{key: kv}
	return snapshotmulti.NewStoreWithKVStores(stores), key
}

func TestEmptyNestedCacheWriteRevert(t *testing.T) {
	key := storetypes.NewKVStoreKey("store")
	parent := cachekv.NewStore(dbadapter.Store{DB: dbm.NewMemDB()})
	initial := cachekv.NewStore(parent)
	store := snapshotmulti.NewStoreWithKVStores(map[*storetypes.KVStoreKey]storetypes.CacheWrap{key: initial})

	frame := store.Snapshot()
	store.GetKVStore(key).Set([]byte("escrow"), []byte("40"))
	store.GetKVStore(key).Set([]byte("commitment"), []byte("1"))
	child := store.CacheMultiStore()
	child.Write()
	require.NotPanics(t, func() { store.RevertToSnapshot(frame) })

	for _, name := range []string{"escrow", "commitment"} {
		require.Nil(t, parent.Get([]byte(name)), "parent retained %s", name)
		require.Nil(t, store.GetKVStore(key).Get([]byte(name)), "current store retained %s", name)
	}
}

func TestCacheBranchAcrossSnapshots(t *testing.T) {
	for _, depth := range []int{1, 2, 3} {
		for _, revertInner := range []bool{false, true} {
			for _, revertOuter := range []bool{false, true} {
				t.Run(fmt.Sprintf("depth=%d/inner_revert=%t/outer_revert=%t", depth, revertInner, revertOuter), func(t *testing.T) {
					store, key, parent := setupCountingStore("shared", "deleted", "readOnly")
					frame := store.Snapshot()
					child := store.CacheMultiStore()
					handles := []storetypes.CacheMultiStore{child}
					for i := 1; i < depth; i++ {
						child = child.CacheMultiStore()
						handles = append(handles, child)
					}
					child.GetKVStore(key).Set([]byte("shared"), []byte("outer"))
					child.GetKVStore(key).Delete([]byte("deleted"))
					child.GetKVStore(key).Set([]byte("callback"), []byte("outer"))
					require.Equal(t, []byte("before"), child.GetKVStore(key).Get([]byte("readOnly")))

					inner := store.Snapshot()
					store.GetKVStore(key).Set([]byte("shared"), []byte("inner"))
					store.GetKVStore(key).Set([]byte("readOnly"), []byte("inner"))
					store.GetKVStore(key).Set([]byte("inner"), []byte("1"))
					// A newer layer may already cache both existing and missing keys.
					require.Nil(t, store.GetKVStore(key).Get([]byte("callback")))
					require.Equal(t, []byte("before"), store.GetKVStore(key).Get([]byte("deleted")))
					if revertInner {
						store.RevertToSnapshot(inner)
					}

					require.NotPanics(t, func() {
						for i := len(handles) - 1; i >= 0; i-- {
							handles[i].Write()
						}
					}, "surviving branches must remain writable after reentry")
					require.Equal(t, []byte("outer"), store.GetKVStore(key).Get([]byte("shared")))
					require.Nil(t, store.GetKVStore(key).Get([]byte("deleted")))
					require.Equal(t, []byte("outer"), store.GetKVStore(key).Get([]byte("callback")))
					wantReadOnly := []byte("inner")
					if revertInner {
						wantReadOnly = []byte("before")
						require.Nil(t, store.GetKVStore(key).Get([]byte("inner")))
					}
					require.Equal(t, wantReadOnly, store.GetKVStore(key).Get([]byte("readOnly")),
						"read-only cache entries must not overwrite reentrant writes")
					require.Empty(t, parent.writes, "branch writes must not reach the transaction store")
					require.Panics(t, child.Write, "a branch must still be single-use")

					if revertOuter {
						store.RevertToSnapshot(frame)
					}
					store.Write()
					if revertOuter {
						require.Empty(t, parent.writes)
						for _, name := range []string{"shared", "deleted", "readOnly"} {
							require.Equal(t, []byte("before"), parent.Get([]byte(name)))
						}
						require.Nil(t, parent.Get([]byte("callback")))
						require.Nil(t, parent.Get([]byte("inner")))
					} else {
						require.Equal(t, []byte("outer"), parent.Get([]byte("shared")))
						require.Nil(t, parent.Get([]byte("deleted")))
						require.Equal(t, []byte("outer"), parent.Get([]byte("callback")))
						require.Equal(t, wantReadOnly, parent.Get([]byte("readOnly")))
						for name, count := range parent.writes {
							require.Equal(t, 1, count, name)
						}
					}
				})
			}
		}
	}
}

func TestCacheBranchReadsCurrentParent(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			store, key := setupStore()
			store.GetKVStore(key).Set([]byte("a"), []byte("before"))
			child := store.CacheMultiStore()
			kv := child.GetKVStore(key)
			kv.Set([]byte("b"), []byte("branch"))
			store.Snapshot()
			store.GetKVStore(key).Set([]byte("a"), []byte("current"))
			store.GetKVStore(key).Set([]byte("c"), []byte("current"))
			require.Equal(t, []byte("current"), kv.Get([]byte("a")))
			require.True(t, kv.Has([]byte("c")))

			var iter storetypes.Iterator
			want := []string{"a=current", "b=branch", "c=current"}
			if reverse {
				iter = kv.ReverseIterator(nil, nil)
				want = []string{"c=current", "b=branch", "a=current"}
			} else {
				iter = kv.Iterator(nil, nil)
			}
			var got []string
			for ; iter.Valid(); iter.Next() {
				got = append(got, fmt.Sprintf("%s=%s", iter.Key(), iter.Value()))
			}
			require.NoError(t, iter.Close())
			require.Equal(t, want, got)
		})
	}
}

func TestCacheBranchWriteRevertsAtCurrentLayer(t *testing.T) {
	store, key := setupStore()
	store.GetKVStore(key).Set([]byte("shared"), []byte("before"))
	child := store.CacheMultiStore()
	child.GetKVStore(key).Set([]byte("shared"), []byte("branch"))
	child.GetKVStore(key).Set([]byte("callback"), []byte("branch"))
	inner := store.Snapshot()
	child.Write()
	require.Equal(t, []byte("branch"), store.GetKVStore(key).Get([]byte("shared")))
	store.RevertToSnapshot(inner)
	require.Equal(t, []byte("before"), store.GetKVStore(key).Get([]byte("shared")))
	require.Nil(t, store.GetKVStore(key).Get([]byte("callback")),
		"a late merge must not write below the current revert boundary")
	require.Panics(t, child.Write)
}

func TestCacheBranchRawKVWriteAfterExpiry(t *testing.T) {
	for _, written := range []bool{false, true} {
		t.Run(fmt.Sprintf("written=%t", written), func(t *testing.T) {
			store, key := setupStore()
			child := store.CacheMultiStore()
			kv := child.GetKVStore(key).(storetypes.CacheKVStore)
			if written {
				child.Write()
			} else {
				store.RevertToSnapshot(0)
			}
			kv.Set([]byte("stale"), []byte("1"))
			require.Panics(t, kv.Write, "a retained KV cache must not bypass branch expiry")
			require.Nil(t, store.GetKVStore(key).Get([]byte("stale")))
		})
	}
}

func TestSnapshotMultiIndexing(t *testing.T) {
	snapshotStore, _ := setupStore()

	idx0 := snapshotStore.Snapshot()
	require.Equal(t, 0, idx0)

	idx1 := snapshotStore.Snapshot()
	require.Equal(t, 1, idx1)

	idx2 := snapshotStore.Snapshot()
	require.Equal(t, 2, idx2)
}

func TestSnapshotMultiRevertAndWrite(t *testing.T) {
	snapshotStore, key := setupStore()
	kv := snapshotStore.GetKVStore(key)
	kv.Set([]byte("a"), []byte("1"))

	idx0 := snapshotStore.Snapshot()
	snapshotStore.GetKVStore(key).Set([]byte("b"), []byte("2"))

	idx1 := snapshotStore.Snapshot()
	snapshotStore.GetKVStore(key).Set([]byte("c"), []byte("3"))

	snapshotStore.RevertToSnapshot(idx1)
	require.Nil(t, snapshotStore.GetKVStore(key).Get([]byte("c")))
	require.Equal(t, []byte("2"), snapshotStore.GetKVStore(key).Get([]byte("b")))

	snapshotStore.RevertToSnapshot(idx0)
	require.Nil(t, snapshotStore.GetKVStore(key).Get([]byte("b")))
	require.Equal(t, []byte("1"), snapshotStore.GetKVStore(key).Get([]byte("a")))

	snapshotStore.Snapshot()
	snapshotStore.GetKVStore(key).Set([]byte("d"), []byte("4"))
	snapshotStore.Write()

	require.Equal(t, []byte("4"), kv.Get([]byte("d")))
	idx := snapshotStore.Snapshot()
	require.Equal(t, 0, idx)
}

func TestSnapshotMultiRevertOverwriteSameKey(t *testing.T) {
	// Setup a fresh SnapshotMultiStore and a key
	snapshotStore, key := setupStore()
	kv := snapshotStore.GetKVStore(key)

	// Initial write under key "a"
	kv.Set([]byte("a"), []byte("1"))

	// Overwrite "a" with "2"
	idx0 := snapshotStore.Snapshot()
	snapshotStore.GetKVStore(key).Set([]byte("a"), []byte("2"))

	// Overwrite "a" with "3"
	idx1 := snapshotStore.Snapshot()
	snapshotStore.GetKVStore(key).Set([]byte("a"), []byte("3"))

	// Revert to idx1: expect value "2"
	snapshotStore.RevertToSnapshot(idx1)
	require.Equal(t, []byte("2"), snapshotStore.GetKVStore(key).Get([]byte("a")))

	// Revert to idx0: expect value "1"
	snapshotStore.RevertToSnapshot(idx0)
	require.Equal(t, []byte("1"), snapshotStore.GetKVStore(key).Get([]byte("a")))

	// Overwrite "a" with "4"
	idx2 := snapshotStore.Snapshot()
	snapshotStore.GetKVStore(key).Set([]byte("a"), []byte("4"))
	snapshotStore.Write()

	// After write, the base store should have "4"
	require.Equal(t, []byte("4"), kv.Get([]byte("a")))

	// Write clears the snapshot stack, so reverting to idx2 should panic
	expectedErr := fmt.Sprintf("snapshot index %d out of bound [%d..%d)", idx2, 0, 0)
	require.PanicsWithErrorf(t, expectedErr, func() {
		snapshotStore.RevertToSnapshot(idx2)
	}, "RevertToSnapshot should panic when idx out of bounds")
}

func TestSnapshotMultiInvalidIndex(t *testing.T) {
	snapshotStore, _ := setupStore()
	snapshotStore.Snapshot()

	require.PanicsWithError(t, "snapshot index 1 out of bound [0..1)", func() {
		snapshotStore.RevertToSnapshot(1)
	})

	require.PanicsWithError(t, "snapshot index -1 out of bound [0..1)", func() {
		snapshotStore.RevertToSnapshot(-1)
	})
}

func TestSnapshotMultiGetStore(t *testing.T) {
	snapshotStore, key := setupStore()

	s := snapshotStore.GetStore(key)
	require.NotNil(t, s)
	require.Equal(t, snapshotStore.GetKVStore(key), s)

	badKey := storetypes.NewKVStoreKey("bad")
	require.Panics(t, func() { snapshotStore.GetStore(badKey) })
	require.Panics(t, func() { snapshotStore.GetKVStore(badKey) })
}

func TestSnapshotMultiCacheWrap(t *testing.T) {
	snapshotStore, _ := setupStore()

	wrap := snapshotStore.CacheWrap()
	require.NotSame(t, snapshotStore, wrap)

	idx := snapshotStore.Snapshot()
	require.Equal(t, 1, idx)
}

func TestSnapshotMultiCacheWrapWithTrace(t *testing.T) {
	snapshotStore, _ := setupStore()

	// NOTES: CacheWrapWithTrace of snapshotmulti.Store is same with regualr CacheWrap,
	// and arguments are not actually used.
	wrap := snapshotStore.CacheWrapWithTrace(nil, nil)
	require.NotSame(t, snapshotStore, wrap)

	idx := snapshotStore.Snapshot()
	require.Equal(t, 1, idx)
}

func TestSnapshotMultiCacheMultiStore(t *testing.T) {
	snapshotStore, _ := setupStore()

	m := snapshotStore.CacheMultiStore()
	require.NotSame(t, snapshotStore, m)

	idx := snapshotStore.Snapshot()
	require.Equal(t, 1, idx)
}

func TestSnapshotMultiCacheMultiStoreWithVersion(t *testing.T) {
	snapshotStore, _ := setupStore()

	m, _ := snapshotStore.CacheMultiStoreWithVersion(1)
	require.Equal(t, snapshotStore, m)
}

func TestSnapshotMultiMetadata(t *testing.T) {
	snapshotStore, _ := setupStore()

	require.Equal(t, storetypes.StoreTypeMulti, snapshotStore.GetStoreType())
	require.False(t, snapshotStore.TracingEnabled())
	require.Equal(t, snapshotStore, snapshotStore.SetTracer(nil))
	require.Equal(t, snapshotStore, snapshotStore.SetTracingContext(nil))
}

func TestSnapshotMultiLatestVersion(t *testing.T) {
	snapshotStore, _ := setupStore()

	initialVersion := int64(0)
	ver0 := snapshotStore.LatestVersion()
	require.Equal(t, ver0, initialVersion)

	idx0 := snapshotStore.Snapshot()
	ver1 := snapshotStore.LatestVersion()
	require.Equal(t, ver1, int64(idx0+1))

	idx1 := snapshotStore.Snapshot()
	ver2 := snapshotStore.LatestVersion()
	require.Equal(t, ver2, int64(idx1+1))
}

type countingStore struct {
	storetypes.KVStore
	writes map[string]int
}

func (s countingStore) Set(key, value []byte) {
	s.writes[string(key)]++
	s.KVStore.Set(key, value)
}

func (s countingStore) Delete(key []byte) {
	s.writes[string(key)]++
	s.KVStore.Delete(key)
}

func setupCountingStore(initialKeys ...string) (*snapshotmulti.Store, *storetypes.KVStoreKey, countingStore) {
	key := storetypes.NewKVStoreKey("store")
	parent := countingStore{KVStore: cachekv.NewStore(dbadapter.Store{DB: dbm.NewMemDB()}), writes: make(map[string]int)}
	for _, name := range initialKeys {
		parent.KVStore.Set([]byte(name), []byte("before"))
	}
	return snapshotmulti.NewStoreWithKVStores(map[*storetypes.KVStoreKey]storetypes.CacheWrap{key: cachekv.NewStore(parent)}), key, parent
}

func TestNestedCacheWriteLifecycle(t *testing.T) {
	for _, depth := range []int{1, 2, 3} {
		for _, commit := range []bool{false, true} {
			t.Run(fmt.Sprintf("depth=%d/commit=%t", depth, commit), func(t *testing.T) {
				store, key, parent := setupCountingStore("shared", "deleted")

				frame := store.Snapshot()
				store.GetKVStore(key).Set([]byte("escrow"), []byte("40"))
				store.GetKVStore(key).Set([]byte("commitment"), []byte("1"))
				store.GetKVStore(key).Set([]byte("shared"), []byte("precompile"))

				var handles []storetypes.CacheMultiStore
				var current storetypes.MultiStore = store
				expected := map[string][]byte{"escrow": []byte("40"), "commitment": []byte("1")}
				for i := 0; i < depth; i++ {
					child := current.CacheMultiStore()
					name := fmt.Sprintf("callback%d", i)
					child.GetKVStore(key).Set([]byte(name), []byte("1"))
					child.GetKVStore(key).Set([]byte("shared"), []byte(name))
					expected[name] = []byte("1")
					handles = append(handles, child)
					current = child
				}
				current.GetKVStore(key).Delete([]byte("deleted"))
				for i := len(handles) - 1; i >= 0; i-- {
					handles[i].Write()
				}
				require.Equal(t, []byte(fmt.Sprintf("callback%d", depth-1)), store.GetKVStore(key).Get([]byte("shared")))
				require.Nil(t, store.GetKVStore(key).Get([]byte("deleted")))

				// Further parent writes must remain in the same revert boundary.
				store.GetKVStore(key).Set([]byte("after"), []byte("1"))
				store.GetKVStore(key).Set([]byte("shared"), []byte("after"))
				expected["after"] = []byte("1")
				expected["shared"] = []byte("after")
				require.Empty(t, parent.writes, "nested writes escaped into the parent")
				require.Equal(t, []byte("before"), parent.Get([]byte("shared")))

				if commit {
					store.Write()
					for name, value := range expected {
						require.Equal(t, value, parent.Get([]byte(name)), name)
						require.Equal(t, 1, parent.writes[name], name)
					}
					require.Nil(t, parent.Get([]byte("deleted")))
					require.Equal(t, 1, parent.writes["deleted"])
					store.Write()
					for name := range parent.writes {
						require.Equal(t, 1, parent.writes[name], "second commit rewrote %s", name)
					}
				} else {
					require.NotPanics(t, func() { store.RevertToSnapshot(frame) })
					for name := range expected {
						if name == "shared" {
							continue
						}
						require.Nil(t, parent.Get([]byte(name)), name)
						require.Nil(t, store.GetKVStore(key).Get([]byte(name)), name)
					}
					require.Equal(t, []byte("before"), store.GetKVStore(key).Get([]byte("shared")))
					require.Equal(t, []byte("before"), store.GetKVStore(key).Get([]byte("deleted")))
					store.Write()
					require.Empty(t, parent.writes)
				}
			})
		}
	}
}

func TestDiscardedCacheBranch(t *testing.T) {
	store, key := setupStore()
	frame := store.Snapshot()
	store.GetKVStore(key).Set([]byte("shared"), []byte("parent"))
	child := store.CacheMultiStore()
	child.GetKVStore(key).Set([]byte("shared"), []byte("discarded"))
	child.GetKVStore(key).Set([]byte("callback"), []byte("discarded"))
	require.Equal(t, []byte("parent"), store.GetKVStore(key).Get([]byte("shared")))
	require.Nil(t, store.GetKVStore(key).Get([]byte("callback")))
	store.Write()
	require.Equal(t, []byte("parent"), store.GetKVStore(key).Get([]byte("shared")))
	require.Nil(t, store.GetKVStore(key).Get([]byte("callback")))
	require.Panics(t, child.Write)
	require.Equal(t, 0, store.Snapshot())
	store.RevertToSnapshot(frame)
}

func TestCacheBranchLifetime(t *testing.T) {
	t.Run("siblings merge in write order", func(t *testing.T) {
		store, key := setupStore()
		first := store.CacheMultiStore()
		first.GetKVStore(key).Set([]byte("first"), []byte("1"))
		first.GetKVStore(key).Set([]byte("shared"), []byte("first"))
		second := store.CacheMultiStore()
		second.GetKVStore(key).Set([]byte("second"), []byte("1"))
		second.GetKVStore(key).Set([]byte("shared"), []byte("second"))
		first.Write()
		require.Equal(t, []byte("1"), store.GetKVStore(key).Get([]byte("first")))
		require.Equal(t, []byte("first"), store.GetKVStore(key).Get([]byte("shared")))
		second.Write()
		require.Equal(t, []byte("1"), store.GetKVStore(key).Get([]byte("second")))
		require.Equal(t, []byte("second"), store.GetKVStore(key).Get([]byte("shared")))
		require.Panics(t, first.Write)
		require.Panics(t, second.Write)
	})

	t.Run("revert and reuse same index", func(t *testing.T) {
		store, key := setupStore()
		frame := store.Snapshot()
		child := store.CacheMultiStore()
		child.GetKVStore(key).Set([]byte("stale"), []byte("1"))
		store.RevertToSnapshot(frame)
		require.Equal(t, frame, store.Snapshot())
		fresh := store.CacheMultiStore()
		fresh.GetKVStore(key).Set([]byte("fresh"), []byte("1"))
		require.Panics(t, child.Write)
		fresh.Write()
		require.Nil(t, store.GetKVStore(key).Get([]byte("stale")))
		require.Equal(t, []byte("1"), store.GetKVStore(key).Get([]byte("fresh")))
	})

	t.Run("child after parent write", func(t *testing.T) {
		store, key := setupStore()
		parent := store.CacheMultiStore()
		child := parent.CacheMultiStore()
		child.GetKVStore(key).Set([]byte("discarded"), []byte("1"))
		parent.Write()
		require.Panics(t, child.Write)
		require.Nil(t, store.GetKVStore(key).Get([]byte("discarded")))
	})

	t.Run("ancestor invalidation", func(t *testing.T) {
		store, key := setupStore()
		frame := store.Snapshot()
		parent := store.CacheMultiStore()
		child := parent.CacheMultiStore()
		child.GetKVStore(key).Set([]byte("stale"), []byte("1"))
		store.RevertToSnapshot(frame)
		require.Panics(t, child.Write)
		require.Panics(t, parent.Write)
		require.Nil(t, store.GetKVStore(key).Get([]byte("stale")))
	})
}

func TestNestedCacheSnapshotIndices(t *testing.T) {
	store, key := setupStore()
	frame := store.Snapshot()
	require.Equal(t, 0, frame)
	store.GetKVStore(key).Set([]byte("precompile"), []byte("1"))
	child := store.CacheMultiStore()
	require.Equal(t, int64(2), store.LatestVersion())
	child.GetKVStore(key).Set([]byte("callback"), []byte("1"))
	child.Write()
	require.Equal(t, 2, store.Snapshot())

	// The SDK cache boundary remains a real, revertible layer after Write.
	store.RevertToSnapshot(1)
	require.Equal(t, []byte("1"), store.GetKVStore(key).Get([]byte("precompile")))
	require.Nil(t, store.GetKVStore(key).Get([]byte("callback")))
	store.RevertToSnapshot(frame)
	require.Nil(t, store.GetKVStore(key).Get([]byte("precompile")))
}

func TestCacheBranchValidatesBeforeWritingAnyStore(t *testing.T) {
	stores := make(map[*storetypes.KVStoreKey]storetypes.CacheWrap)
	for _, name := range []string{"a", "b"} {
		stores[storetypes.NewKVStoreKey(name)] = cachekv.NewStore(dbadapter.Store{DB: dbm.NewMemDB()})
	}
	store := snapshotmulti.NewStoreWithKVStores(stores)
	child := store.CacheMultiStore()
	for key := range stores {
		child.GetKVStore(key).Set([]byte("stale"), []byte("1"))
	}
	store.RevertToSnapshot(0)
	store.Snapshot()
	require.Panics(t, child.Write)
	for key := range stores {
		require.Nil(t, store.GetKVStore(key).Get([]byte("stale")), key.Name())
	}
}
