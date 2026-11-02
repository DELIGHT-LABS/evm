package snapshotmulti

import (
	"fmt"
	"io"
	"sort"

	"github.com/cosmos/evm/x/vm/store/snapshotkv"
	"github.com/cosmos/evm/x/vm/store/types"
	vmtypes "github.com/cosmos/evm/x/vm/types"

	storetypes "cosmossdk.io/store/types"
)

type Store struct {
	stores    map[storetypes.StoreKey]types.SnapshotKVStore
	storeKeys []*storetypes.KVStoreKey // ordered keys
	layers    []*cacheBoundary

	// A branch stays valid while its creation boundary and every ancestor
	// survive. New snapshots do not invalidate an existing branch.
	parent         *Store
	parentBoundary *cacheBoundary
}

type cacheBoundary struct {
	active bool
}

var _ types.SnapshotMultiStore = (*Store)(nil)

// NewStore creates a new Store objectwith CacheMultiStore and KVStoreKeys
func NewStore(cms storetypes.CacheMultiStore, keys map[string]*storetypes.KVStoreKey) *Store {
	s := &Store{
		stores:    make(map[storetypes.StoreKey]types.SnapshotKVStore),
		storeKeys: vmtypes.SortedKVStoreKeys(keys),
	}

	for _, key := range s.storeKeys {
		store := cms.GetKVStore(key).(storetypes.CacheKVStore)
		s.stores[key] = snapshotkv.NewStore(store)
	}

	return s
}

// NewStore creates a new Store object with KVStores
func NewStoreWithKVStores(stores map[*storetypes.KVStoreKey]storetypes.CacheWrap) *Store {
	s := &Store{
		stores: make(map[storetypes.StoreKey]types.SnapshotKVStore),
	}

	for key, store := range stores {
		s.stores[key] = snapshotkv.NewStore(store.(storetypes.CacheKVStore))
		s.storeKeys = append(s.storeKeys, key)
	}

	sort.Slice(s.storeKeys, func(i, j int) bool {
		return s.storeKeys[i].Name() < s.storeKeys[j].Name()
	})

	return s
}

// Snapshot pushes a new cached context to the stack,
// and returns the index of it.
func (s *Store) Snapshot() int {
	s.assertValid()
	for _, key := range s.storeKeys {
		s.stores[key].Snapshot()
	}
	s.layers = append(s.layers, &cacheBoundary{active: true})

	return len(s.layers) - 1
}

// SnapshotGuard validates the lifetime of a KV handle created in the current
// layer. New snapshots leave it valid; reverting its creation layer expires it.
// The handle may follow newer current layers, but never a replacement for a
// reverted boundary with a reused numeric snapshot index.
func (s *Store) SnapshotGuard() func() {
	s.assertValid()
	var boundary *cacheBoundary
	if len(s.layers) != 0 {
		boundary = s.layers[len(s.layers)-1]
	}
	return func() {
		s.assertValid()
		if boundary != nil && !boundary.active {
			panic("snapshotmulti: stale snapshot KV handle")
		}
	}
}

// RevertToSnapshot pops all the cached stores
// whose index is greator than or equal to target.
// The target should be snapshot index returned by `Snapshot`.
// This function panics if the index is out of bounds.
func (s *Store) RevertToSnapshot(target int) {
	s.assertValid()
	if target < 0 || target >= len(s.layers) {
		panic(fmt.Errorf("snapshot index %d out of bound [%d..%d)", target, 0, len(s.layers)))
	}
	for _, key := range s.storeKeys {
		s.stores[key].RevertToSnapshot(target)
	}
	for _, layer := range s.layers[target:] {
		layer.active = false
	}
	s.layers = s.layers[:target]
}

// GetStoreType returns the type of the store.
func (s *Store) GetStoreType() storetypes.StoreType {
	return storetypes.StoreTypeMulti
}

// Implements CacheWrapper.
func (s *Store) CacheWrap() storetypes.CacheWrap {
	return s.CacheMultiStore().(storetypes.CacheWrap)
}

// CacheWrapWithTrace implements the CacheWrapper interface.
//
// NOTE: CacheWrapWithTrace is a method that enables a Store to satisfy the CacheWrapper interface.
// Although it accepts an io.Writer and tracingContext as inputs, these are not used in the implementation.
// Instead, it simply adds an additional cache layer on top of the existing KVStores.
// As a result, while the return value differs, the behavior is effectively the same as the Snapshot() method.
func (s *Store) CacheWrapWithTrace(_ io.Writer, _ storetypes.TraceContext) storetypes.CacheWrap {
	return s.CacheWrap()
}

// CacheMultiStore reserves a snapshot boundary and returns an independent SDK
// cache branch. Its writes stay private until Write merges them into the
// parent's current layer; discarding the branch does not change parent values.
// Branches are single-use and expire when their creation boundary is reverted
// or their parent is committed. Nested branches are written from the inside out.
func (s *Store) CacheMultiStore() storetypes.CacheMultiStore {
	s.Snapshot()
	child := &Store{
		stores:         make(map[storetypes.StoreKey]types.SnapshotKVStore, len(s.storeKeys)),
		storeKeys:      s.storeKeys,
		parent:         s,
		parentBoundary: s.layers[len(s.layers)-1],
	}
	for _, key := range s.storeKeys {
		child.stores[key] = snapshotkv.NewStore(snapshotkv.NewCacheWithParent(func() storetypes.KVStore {
			// Retained KV caches can write without entering this multistore.
			child.assertValid()
			return s.GetKVStore(key)
		}))
	}
	return child
}

// CacheMultiStoreWithVersion load stores at a snapshot version.
//
// NOTE: CacheMultiStoreWithVersion is no-op function.
func (s *Store) CacheMultiStoreWithVersion(_ int64) (storetypes.CacheMultiStore, error) {
	return s, nil
}

// GetStore returns an underlying Store by key.
func (s *Store) GetStore(key storetypes.StoreKey) storetypes.Store {
	s.assertValid()
	store := s.stores[key]
	if key == nil || store == nil {
		panic(fmt.Sprintf("kv store with key %v has not been registered in stores", key))
	}
	return store.CurrentStore()
}

// GetKVStore returns an underlying KVStore by key.
func (s *Store) GetKVStore(key storetypes.StoreKey) storetypes.KVStore {
	s.assertValid()
	store := s.stores[key]
	if key == nil || store == nil {
		panic(fmt.Sprintf("kv store with key %v has not been registered in stores", key))
	}
	return store.CurrentStore()
}

// TracingEnabled returns if tracing is enabled for the MultiStore.
func (s *Store) TracingEnabled() bool {
	return false
}

// SetTracer sets the tracer for the MultiStore that the underlying
// stores will utilize to trace operations. A MultiStore is returned.
//
// NOTE: SetTracer no-op function.
func (s *Store) SetTracer(_ io.Writer) storetypes.MultiStore {
	return s
}

// SetTracingContext updates the tracing context for the MultiStore by merging
// the given context with the existing context by key. Any existing keys will
// be overwritten. It is implied that the caller should update the context when
// necessary between tracing operations. It returns a modified MultiStore.
//
// NOTE: SetTracingContext no-op function
func (s *Store) SetTracingContext(_ storetypes.TraceContext) storetypes.MultiStore {
	return s
}

// LatestVersion returns the branch version of the store
func (s *Store) LatestVersion() int64 {
	return int64(len(s.layers))
}

// Write commits this store's caches only. For an SDK branch, its initial stores
// are private caches that merge into their immediate parent's current layer,
// not the outer transaction store. Root writes are final and clear the stack.
func (s *Store) Write() {
	// Validate every ancestor before writing any substore. Removed boundaries
	// and repeated handles must not silently write into another revision.
	s.assertValid()
	for _, key := range s.storeKeys {
		s.stores[key].Commit()
	}
	for _, layer := range s.layers {
		layer.active = false
	}
	s.layers = nil
	s.parentBoundary = nil
}

func (s *Store) assertValid() {
	if s.parent == nil {
		return
	}
	s.parent.assertValid()
	if s.parentBoundary == nil || !s.parentBoundary.active {
		panic("snapshotmulti: stale or already written cache branch")
	}
}
