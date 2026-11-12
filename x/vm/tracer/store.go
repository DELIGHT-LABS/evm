package tracer

import storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

// traceMultiStore observes writes without changing cache creation or publication.
type traceMultiStore struct {
	storetypes.MultiStore
	observe func(storetypes.StoreKey, []byte)
}

type traceCacheMultiStore struct {
	*traceMultiStore
	write func()
}

func (s *traceCacheMultiStore) Write() { s.write() }

// ObserveStoreWrites reports keys before Set or Delete without changing cache
// creation or publication.
func ObserveStoreWrites(ms storetypes.MultiStore, observe func(storetypes.StoreKey, []byte)) storetypes.MultiStore {
	store := &traceMultiStore{MultiStore: ms, observe: observe}
	if cache, ok := ms.(storetypes.CacheMultiStore); ok {
		return &traceCacheMultiStore{traceMultiStore: store, write: cache.Write}
	}
	return store
}

func (s *traceMultiStore) CacheMultiStore() storetypes.CacheMultiStore {
	return ObserveStoreWrites(s.MultiStore.CacheMultiStore(), s.observe).(storetypes.CacheMultiStore)
}

func (s *traceMultiStore) CacheMultiStoreWithVersion(version int64) (storetypes.CacheMultiStore, error) {
	cache, err := s.MultiStore.CacheMultiStoreWithVersion(version)
	if err != nil {
		return nil, err
	}
	return ObserveStoreWrites(cache, s.observe).(storetypes.CacheMultiStore), nil
}

func (s *traceMultiStore) CacheWrap() storetypes.CacheWrap { return s.CacheMultiStore() }

func (s *traceMultiStore) GetStore(key storetypes.StoreKey) storetypes.Store {
	store := s.MultiStore.GetStore(key)
	if kv, ok := store.(storetypes.KVStore); ok {
		observed := &traceKVStore{KVStore: kv, key: key, observe: s.observe}
		if cache, ok := kv.(storetypes.CacheKVStore); ok {
			return &traceCacheKVStore{traceKVStore: observed, write: cache.Write}
		}
		return observed
	}
	return store
}

func (s *traceMultiStore) GetKVStore(key storetypes.StoreKey) storetypes.KVStore {
	return s.GetStore(key).(storetypes.KVStore)
}

type traceKVStore struct {
	storetypes.KVStore
	key     storetypes.StoreKey
	observe func(storetypes.StoreKey, []byte)
}

func (s *traceKVStore) Set(key, value []byte) {
	s.observe(s.key, key)
	s.KVStore.Set(key, value)
}

func (s *traceKVStore) Delete(key []byte) {
	s.observe(s.key, key)
	s.KVStore.Delete(key)
}

func (s *traceKVStore) CacheWrap() storetypes.CacheWrap {
	cache := s.KVStore.CacheWrap()
	return &traceCacheKVStore{
		traceKVStore: &traceKVStore{KVStore: cache.(storetypes.KVStore), key: s.key, observe: s.observe},
		write:        cache.Write,
	}
}

type traceCacheKVStore struct {
	*traceKVStore
	write func()
}

func (s *traceCacheKVStore) Write() { s.write() }
