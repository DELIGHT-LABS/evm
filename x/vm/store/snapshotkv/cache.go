package snapshotkv

import (
	"io"
	"sort"
	"sync"

	"cosmossdk.io/store/cachekv"
	"cosmossdk.io/store/tracekv"
	storetypes "cosmossdk.io/store/types"
)

// cacheStore keeps only explicit Set/Delete operations. Untouched keys are read
// from the parent on every access, including keys previously observed as absent.
type cacheStore struct {
	mu         sync.Mutex
	parent     func() storetypes.KVStore
	writes     map[string][]byte // A present nil value is a deletion.
	generation uint64
	fixed      bool
	iteration  *cachekv.Store
}

var _ storetypes.CacheKVStore = (*cacheStore)(nil)

// NewCache creates a private write-back cache above a fixed parent KV view.
// Write merges only this cache's changes; it never calls parent.Write.
func NewCache(parent storetypes.KVStore) storetypes.CacheKVStore {
	s := &cacheStore{
		parent: func() storetypes.KVStore { return parent },
		writes: make(map[string][]byte),
		fixed:  true,
	}
	if cache, ok := parent.(*cacheStore); ok {
		s.parent = cache.childParent()
	}
	return s
}

// NewCacheWithParent resolves the parent on fallback reads, iteration and Write.
// The resolver must validate its owner's lifetime and return the immediate
// parent's current layer, not this cache or a global active native scope.
// Local Set/Delete remain private; Write resolves the parent even when empty.
func NewCacheWithParent(parent func() storetypes.KVStore) storetypes.CacheKVStore {
	return &cacheStore{parent: parent, writes: make(map[string][]byte)}
}

func (s *cacheStore) GetStoreType() storetypes.StoreType {
	return s.parent().GetStoreType()
}

func (s *cacheStore) Get(key []byte) []byte {
	storetypes.AssertValidKey(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if value, ok := s.writes[string(key)]; ok {
		return value
	}
	return s.parent().Get(key)
}

func (s *cacheStore) Has(key []byte) bool {
	return s.Get(key) != nil
}

func (s *cacheStore) Set(key, value []byte) {
	storetypes.AssertValidKey(key)
	storetypes.AssertValidValue(value)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes[string(key)] = value
	if s.iteration != nil {
		s.iteration.Set(key, value)
	}
}

func (s *cacheStore) Delete(key []byte) {
	storetypes.AssertValidKey(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes[string(key)] = nil
	if s.iteration != nil {
		s.iteration.Delete(key)
	}
}

func (s *cacheStore) Write() {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent := s.parent()
	keys := make([]string, 0, len(s.writes))
	for key := range s.writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if value := s.writes[key]; value != nil {
			parent.Set([]byte(key), value)
		} else {
			parent.Delete([]byte(key))
		}
	}
	clear(s.writes)
	s.iteration = nil
	s.generation++
}

func (s *cacheStore) Iterator(start, end []byte) storetypes.Iterator {
	return s.iterator(start, end, false)
}

func (s *cacheStore) ReverseIterator(start, end []byte) storetypes.Iterator {
	return s.iterator(start, end, true)
}

func (s *cacheStore) iterator(start, end []byte, reverse bool) storetypes.Iterator {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent := s.parent()
	// The SDK merge view receives writes only: never Get/Has or Write.
	// A fixed parent permits reusing its sorted delta, but a live resolver may
	// select another layer, so its merge view must be rebuilt on every call.
	view := s.iteration
	if view == nil {
		view = cachekv.NewStore(parent)
		for key, value := range s.writes {
			if value != nil {
				view.Set([]byte(key), value)
			} else {
				view.Delete([]byte(key))
			}
		}
		if s.fixed {
			s.iteration = view
		}
	}
	if reverse {
		return view.ReverseIterator(start, end)
	}
	return view.Iterator(start, end)
}

func (s *cacheStore) CacheWrap() storetypes.CacheWrap {
	return NewCache(s)
}

func (s *cacheStore) CacheWrapWithTrace(w io.Writer, tc storetypes.TraceContext) storetypes.CacheWrap {
	parent := s.childParent()
	return NewCacheWithParent(func() storetypes.KVStore {
		return tracekv.NewStore(parent(), w, tc)
	})
}

func (s *cacheStore) childParent() func() storetypes.KVStore {
	s.mu.Lock()
	generation := s.generation
	s.mu.Unlock()
	return func() storetypes.KVStore {
		s.mu.Lock()
		defer s.mu.Unlock()
		if generation != s.generation {
			panic("snapshotkv: stale cache wrap")
		}
		s.parent()
		return s
	}
}
