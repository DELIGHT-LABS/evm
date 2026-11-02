package snapshotkv_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/store/snapshotkv"

	storetypes "cosmossdk.io/store/types"
)

func TestCacheIteratorMergesCurrentParentAndWrites(t *testing.T) {
	parent := newCacheParent()
	for _, key := range []string{"a", "b", "d"} {
		parent.Set([]byte(key), []byte("40"))
	}
	cache := snapshotkv.NewCache(parent)
	require.Equal(t, []byte("40"), cache.Get([]byte("d")))
	cache.Set([]byte("a"), []byte("30"))
	cache.Delete([]byte("b"))
	cache.Set([]byte("c"), []byte("50"))
	parent.Set([]byte("d"), []byte("20"))
	parent.Set([]byte("e"), []byte("60"))

	for _, tc := range []struct {
		name    string
		start   []byte
		end     []byte
		reverse bool
		want    []string
	}{
		{name: "forward", want: []string{"a=30", "c=50", "d=20", "e=60"}},
		{name: "reverse", reverse: true, want: []string{"e=60", "d=20", "c=50", "a=30"}},
		{name: "range", start: []byte("b"), end: []byte("e"), want: []string{"c=50", "d=20"}},
		{name: "reverse_range", start: []byte("b"), end: []byte("e"), reverse: true, want: []string{"d=20", "c=50"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var iter storetypes.Iterator
			if tc.reverse {
				iter = cache.ReverseIterator(tc.start, tc.end)
			} else {
				iter = cache.Iterator(tc.start, tc.end)
			}
			var got []string
			for ; iter.Valid(); iter.Next() {
				got = append(got, fmt.Sprintf("%s=%s", iter.Key(), iter.Value()))
			}
			require.NoError(t, iter.Close())
			require.Equal(t, tc.want, got)
		})
	}
	require.Equal(t, []byte("40"), parent.Get([]byte("a")))
	require.Equal(t, []byte("40"), parent.Get([]byte("b")))
	require.Nil(t, parent.Get([]byte("c")), "iteration must not publish the merge view")
}

func TestCacheIteratorPreservesViewAcrossLocalWrites(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
			cache := snapshotkv.NewCache(newCacheParent())
			if wrapped {
				cache = cache.CacheWrap().(storetypes.CacheKVStore)
			}
			cache.Set([]byte("a"), []byte("1"))
			cache.Set([]byte("b"), []byte("2"))
			iter := cache.Iterator(nil, nil)

			cache.Delete([]byte("a"))
			cache.Set([]byte("b"), []byte("3"))
			cache.Set([]byte("c"), []byte("4"))

			var got []string
			for ; iter.Valid(); iter.Next() {
				got = append(got, fmt.Sprintf("%s=%s", iter.Key(), iter.Value()))
			}
			require.NoError(t, iter.Close())
			require.Equal(t, []string{"a=1", "b=2"}, got)
			require.Nil(t, cache.Get([]byte("a")))
			require.Equal(t, []byte("3"), cache.Get([]byte("b")))
			iter = cache.Iterator(nil, nil)
			got = nil
			for ; iter.Valid(); iter.Next() {
				got = append(got, fmt.Sprintf("%s=%s", iter.Key(), iter.Value()))
			}
			require.NoError(t, iter.Close())
			require.Equal(t, []string{"b=3", "c=4"}, got)
		})
	}
}

func TestCacheIteratorFollowsResolvedParent(t *testing.T) {
	first, second := newCacheParent(), newCacheParent()
	first.Set([]byte("a"), []byte("first"))
	second.Set([]byte("b"), []byte("second"))
	current := storetypes.KVStore(first)
	cache := snapshotkv.NewCacheWithParent(func() storetypes.KVStore { return current })
	for _, parent := range []storetypes.KVStore{first, second} {
		current = parent
		iter := cache.Iterator(nil, nil)
		require.True(t, iter.Valid())
		key := iter.Key()
		value := iter.Value()
		require.Equal(t, cache.Get(key), value)
		if parent == first {
			require.Equal(t, []byte("a"), key)
		} else {
			require.Equal(t, []byte("b"), key)
		}
		iter.Next()
		require.False(t, iter.Valid())
		require.NoError(t, iter.Close())
	}
}

func TestCacheIteratorDropsPublishedDelta(t *testing.T) {
	parent := newCacheParent()
	cache := snapshotkv.NewCache(parent)
	cache.Set([]byte("key"), []byte("40"))
	iter := cache.Iterator(nil, nil)
	require.NoError(t, iter.Close())
	cache.Set([]byte("key"), []byte("30"))
	cache.Write()
	parent.Set([]byte("key"), []byte("20"))

	iter = cache.Iterator(nil, nil)
	require.True(t, iter.Valid())
	require.Equal(t, []byte("20"), iter.Value())
	require.NoError(t, iter.Close())
}

func TestCacheTraceDefersWritesUntilPublication(t *testing.T) {
	parent := newCacheParent()
	parent.Set([]byte("key"), []byte("40"))
	cache := snapshotkv.NewCache(parent)
	var trace bytes.Buffer
	wrapped := cache.CacheWrapWithTrace(&trace, storetypes.TraceContext{"owner": "test"}).(storetypes.CacheKVStore)
	require.Equal(t, []byte("40"), wrapped.Get([]byte("key")))
	wrapped.Set([]byte("key"), []byte("20"))
	beforeWrite := trace.Len()
	require.Equal(t, []byte("20"), wrapped.Get([]byte("key")))
	require.Equal(t, beforeWrite, trace.Len(), "private reads and writes must not touch the traced parent")

	wrapped.Write()

	decoder := json.NewDecoder(&trace)
	var operations []string
	for decoder.More() {
		var operation struct {
			Operation string            `json:"operation"`
			Metadata  map[string]string `json:"metadata"`
		}
		require.NoError(t, decoder.Decode(&operation))
		require.Equal(t, "test", operation.Metadata["owner"])
		operations = append(operations, operation.Operation)
	}
	require.Equal(t, []string{"read", "write"}, operations)
	require.Equal(t, []byte("20"), cache.Get([]byte("key")))
	require.Equal(t, []byte("40"), parent.Get([]byte("key")))
}

type cacheWriteRecorder struct {
	storetypes.CacheKVStore
	keys    []string
	flushes int
}

func (s *cacheWriteRecorder) Set(key, value []byte) {
	s.keys = append(s.keys, string(key))
	s.CacheKVStore.Set(key, value)
}

func (s *cacheWriteRecorder) Delete(key []byte) {
	s.keys = append(s.keys, string(key))
	s.CacheKVStore.Delete(key)
}

func (s *cacheWriteRecorder) Write() {
	s.flushes++
	s.CacheKVStore.Write()
}

func TestCacheWriteSortsChangesWithoutFlushingParent(t *testing.T) {
	parent := &cacheWriteRecorder{CacheKVStore: newCacheParent()}
	cache := snapshotkv.NewCache(parent)
	cache.Set([]byte("z"), []byte("old"))
	cache.Set([]byte("z"), []byte("new"))
	cache.Delete([]byte("a"))
	cache.Set([]byte("m"), []byte("middle"))
	require.Nil(t, cache.Get([]byte("read_only")))

	cache.Write()
	cache.Write()

	require.Equal(t, []string{"a", "m", "z"}, parent.keys)
	require.Zero(t, parent.flushes)
	require.Equal(t, []byte("new"), parent.Get([]byte("z")))
}
