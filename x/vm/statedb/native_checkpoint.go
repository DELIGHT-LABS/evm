package statedb

import (
	"fmt"

	storetypes "github.com/cosmos/evm/x/vm/store/types"
	"github.com/cosmos/evm/x/vm/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// A checkpoint retains its creation owner. A published scope transfers native
// undo to its covering checkpoint, but leaves ordinary EVM journal entries intact.
type nativeCheckpoint struct {
	store         storetypes.Snapshotter
	snapshot      int
	events        sdk.EventManagerI
	prevEvents    sdk.Events
	processed     *int
	prevProcessed int
	scope         *NativeScope
	journalIndex  int
	active        bool
}

type nativeContextKey struct{}

type nativeContextToken struct {
	db         *StateDB
	checkpoint *nativeCheckpoint
	open       bool
}

func (s *StateDB) nativeContext() sdk.Context {
	if s.nativeScope != nil {
		return s.nativeScope.ctx
	}
	if s.writeCache != nil {
		return s.cacheCtx
	}
	return s.ctx
}

func (s *StateDB) nativeSnapshotter() storetypes.Snapshotter {
	if s.nativeScope != nil {
		return s.nativeScope.store
	}
	return s.snapshotter
}

func (s *StateDB) nativeProcessedEvents() *int {
	if s.nativeScope != nil {
		return &s.nativeScope.processedEvents
	}
	return &s.processedEventsCount
}

func (s *StateDB) appendNativeCheckpoint(snapshot int) *nativeCheckpoint {
	ctx := s.nativeContext()
	processed := s.nativeProcessedEvents()
	checkpoint := &nativeCheckpoint{
		store:         s.nativeSnapshotter(),
		snapshot:      snapshot,
		events:        ctx.EventManager(),
		prevEvents:    append(sdk.Events(nil), ctx.EventManager().Events()...),
		processed:     processed,
		prevProcessed: *processed,
		scope:         s.nativeScope,
		journalIndex:  s.journal.length(),
		active:        true,
	}
	s.journal.append(precompileCallChange{checkpoint: checkpoint})
	return checkpoint
}

func (s *StateDB) addPrecompileCheckpoint(snapshot int) (*nativeCheckpoint, error) {
	if s.precompileCallsCounter >= types.MaxPrecompileCalls {
		return nil, fmt.Errorf("max calls to precompiles (%d) reached", types.MaxPrecompileCalls)
	}
	checkpoint := s.appendNativeCheckpoint(snapshot)
	s.precompileCallsCounter++
	return checkpoint, nil
}

// BeginNativeAction checkpoints the active native store before SDK child caches
// can be created. The returned context carries that coverage through CacheContext.
// end must run after all descendant scopes finish; it does not commit the store
// or release the checkpoint needed by an enclosing EVM frame.
func (s *StateDB) BeginNativeAction() (sdk.Context, func(), error) {
	ctx, err := s.GetCacheContext()
	if err != nil {
		return sdk.Context{}, nil, err
	}
	if s.precompileCallsCounter >= types.MaxPrecompileCalls {
		return sdk.Context{}, nil, fmt.Errorf("max calls to precompiles (%d) reached", types.MaxPrecompileCalls)
	}
	checkpoint, err := s.addPrecompileCheckpoint(s.MultiStoreSnapshot())
	if err != nil {
		return sdk.Context{}, nil, err
	}
	token := &nativeContextToken{db: s, checkpoint: checkpoint, open: true}
	s.nativeActionDepth++
	depth := s.nativeActionDepth
	end := func() {
		if !token.open || s.nativeActionDepth != depth || s.nativeScope != checkpoint.scope {
			panic(types.ErrNativeScopeOrder)
		}
		token.open = false
		s.nativeActionDepth--
	}
	return ctx.WithValue(nativeContextKey{}, token), end, nil
}

func (checkpoint *nativeCheckpoint) revert() {
	// validateNativeRollback already proved that a published owner's covering
	// checkpoint occurs in this same rollback. Never touch its closed store.
	if checkpoint.scope == nil || checkpoint.scope.state == nativeScopeOpen {
		checkpoint.store.RevertToSnapshot(checkpoint.snapshot)
		checkpoint.events.OverrideEvents(checkpoint.prevEvents)
		*checkpoint.processed = checkpoint.prevProcessed
	}
	checkpoint.active = false
}

func (s *StateDB) validateNativeRollback(revisionID, journalIndex int) {
	if s.nativeScope != nil && revisionID < s.nativeScope.revision {
		panic(types.ErrNativeScopeOrder)
	}
	// Check every entry before mutating memory, events, or stores. A missing
	// covering checkpoint must not leave a partially reverted StateDB.
	for _, entry := range s.journal.entries[journalIndex:] {
		change, ok := entry.(precompileCallChange)
		if !ok {
			continue
		}
		checkpoint := change.checkpoint
		for {
			if !checkpoint.active || checkpoint.journalIndex < journalIndex {
				panic(types.ErrNativeScopeCoverage)
			}
			if checkpoint.scope == nil || checkpoint.scope.state == nativeScopeOpen {
				break
			}
			if checkpoint.scope.state != nativeScopePublished {
				panic(types.ErrNativeScopeCoverage)
			}
			checkpoint = checkpoint.scope.cover
		}
	}
}
