package statedb

import (
	"io"

	"github.com/cosmos/evm/x/vm/store/snapshotkv"
	"github.com/cosmos/evm/x/vm/store/snapshotmulti"
	"github.com/cosmos/evm/x/vm/types"

	"cosmossdk.io/store/tracekv"
	storetypes "cosmossdk.io/store/types"
)

// Keep the lifetime check on returned KV handles as well as on the multistore:
// ordinary SDK caches can retain a KV parent and later write without calling
// the scope's multistore again.
type nativeCacheStore = storetypes.CacheMultiStore

type nativeScopeStore struct {
	nativeCacheStore
	scope *NativeScope
	root  bool
}

func (s *nativeScopeStore) assertOpen() {
	if s.scope != nil && s.scope.state != nativeScopeOpen {
		panic(types.ErrNativeScopeOrder)
	}
}

func (s *nativeScopeStore) GetKVStore(key storetypes.StoreKey) storetypes.KVStore {
	s.assertOpen()
	return nativeScopeKV{
		store: s.nativeCacheStore.(*snapshotmulti.Store),
		key:   key,
		scope: s.scope,
		root:  s.root,
		guard: s.nativeCacheStore.(*snapshotmulti.Store).SnapshotGuard(),
	}
}

func (s *nativeScopeStore) GetStore(key storetypes.StoreKey) storetypes.Store {
	return s.GetKVStore(key)
}

func (s *nativeScopeStore) CacheMultiStore() storetypes.CacheMultiStore {
	s.assertOpen()
	return &nativeScopeStore{nativeCacheStore: s.nativeCacheStore.CacheMultiStore(), scope: s.scope}
}

func (s *nativeScopeStore) CacheMultiStoreWithVersion(version int64) (storetypes.CacheMultiStore, error) {
	s.assertOpen()
	child, err := s.nativeCacheStore.CacheMultiStoreWithVersion(version)
	if err != nil {
		return nil, err
	}
	return &nativeScopeStore{
		nativeCacheStore: child,
		scope:            s.scope,
		root:             s.root && child == s.nativeCacheStore,
	}, nil
}

func (s *nativeScopeStore) CacheWrap() storetypes.CacheWrap {
	return s.CacheMultiStore().(storetypes.CacheWrap)
}

func (s *nativeScopeStore) CacheWrapWithTrace(io.Writer, storetypes.TraceContext) storetypes.CacheWrap {
	return s.CacheWrap()
}

func (s *nativeScopeStore) Write() {
	s.assertOpen()
	if s.root {
		panic(types.ErrNativeScopeOrder)
	}
	s.nativeCacheStore.Write()
}

type nativeScopeKV struct {
	store *snapshotmulti.Store
	key   storetypes.StoreKey
	scope *NativeScope
	root  bool
	guard func()
}

func (s nativeScopeKV) assertOpen() {
	if s.scope != nil && s.scope.state != nativeScopeOpen {
		panic(types.ErrNativeScopeOrder)
	}
	s.guard()
}

// Resolve the layer on every access. A handle (or an SDK cache wrapping it)
// retained across a new native checkpoint must not write into the old layer.
func (s nativeScopeKV) current() storetypes.CacheKVStore {
	s.assertOpen()
	return s.store.GetKVStore(s.key).(storetypes.CacheKVStore)
}

func (s nativeScopeKV) GetStoreType() storetypes.StoreType {
	return s.current().GetStoreType()
}

func (s nativeScopeKV) Get(key []byte) []byte {
	return s.current().Get(key)
}

func (s nativeScopeKV) Has(key []byte) bool {
	return s.current().Has(key)
}

func (s nativeScopeKV) Set(key, value []byte) {
	s.current().Set(key, value)
}

func (s nativeScopeKV) Delete(key []byte) {
	s.current().Delete(key)
}

func (s nativeScopeKV) Write() {
	s.assertOpen()
	if s.root {
		panic(types.ErrNativeScopeOrder)
	}
	s.current().Write()
}

func (s nativeScopeKV) Iterator(start, end []byte) storetypes.Iterator {
	return s.current().Iterator(start, end)
}

func (s nativeScopeKV) ReverseIterator(start, end []byte) storetypes.Iterator {
	return s.current().ReverseIterator(start, end)
}

func (s nativeScopeKV) CacheWrap() storetypes.CacheWrap {
	s.assertOpen()
	s.guard = s.store.SnapshotGuard()
	return snapshotkv.NewCacheWithParent(func() storetypes.KVStore {
		return s.current()
	})
}

func (s nativeScopeKV) CacheWrapWithTrace(w io.Writer, tc storetypes.TraceContext) storetypes.CacheWrap {
	s.assertOpen()
	s.guard = s.store.SnapshotGuard()
	return snapshotkv.NewCacheWithParent(func() storetypes.KVStore {
		return tracekv.NewStore(s.current(), w, tc)
	})
}
