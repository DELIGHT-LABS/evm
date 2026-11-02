package statedb

import (
	"github.com/cosmos/evm/x/vm/store/snapshotkv"
	"github.com/cosmos/evm/x/vm/store/snapshotmulti"
	"github.com/cosmos/evm/x/vm/types"

	storetypes "cosmossdk.io/store/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

type nativeScopeState uint8

const (
	nativeScopeOpen nativeScopeState = iota
	nativeScopePublished
	nativeScopeDiscarded
)

// NativeScope isolates shared EVM work above an existing SDK cache. Write
// merges only into that cache; the SDK caller still decides whether to write or
// discard it. Scopes, contexts, and interior revisions must not escape their
// synchronous enclosing native action.
type NativeScope struct {
	db              *StateDB
	parent          *NativeScope
	ctx             sdk.Context
	destination     sdk.Context
	store           *snapshotmulti.Store
	cover           *nativeCheckpoint
	token           *nativeContextToken
	state           nativeScopeState
	revision        int
	revisionIndex   int
	actionDepth     int
	processedEvents int
}

// NewNativeScope creates and activates a private, snapshot-capable native view
// above ctx. ctx must inherit coverage from BeginNativeAction or a still-open
// NativeScope. The enclosing checkpoint must predate any opaque SDK cache
// carrying ctx. Pass the returned context downstream and close the scope with
// Write or Rollback, inside out.
func (s *StateDB) NewNativeScope(ctx sdk.Context) (sdk.Context, *NativeScope, error) {
	token, ok := ctx.Value(nativeContextKey{}).(*nativeContextToken)
	if !ok || !token.open || token.db != s || !token.checkpoint.active ||
		token.checkpoint.scope != s.nativeScope {
		return sdk.Context{}, nil, types.ErrNativeScopeContext
	}

	stores := make(map[*storetypes.KVStoreKey]storetypes.CacheWrap)
	for _, key := range s.keeper.KVStoreKeys() {
		// Own fresh caches, rather than borrowing ctx's caches: publishing this
		// overlay must not accidentally call the incoming SDK cache's Write.
		stores[key] = snapshotkv.NewCache(ctx.MultiStore().GetKVStore(key))
	}
	scope := &NativeScope{
		db:            s,
		parent:        s.nativeScope,
		destination:   ctx,
		store:         snapshotmulti.NewStoreWithKVStores(stores),
		cover:         token.checkpoint,
		revisionIndex: len(s.validRevisions),
		actionDepth:   s.nativeActionDepth,
	}
	scope.revision = s.Snapshot()
	scope.ctx = ctx.WithMultiStore(&nativeScopeStore{
		nativeCacheStore: scope.store,
		scope:            scope,
		root:             true,
	}).WithEventManager(sdk.NewEventManager())
	s.nativeScope = scope
	checkpoint := s.appendNativeCheckpoint(scope.store.Snapshot())
	scope.token = &nativeContextToken{db: s, checkpoint: checkpoint, open: true}
	scope.ctx = scope.ctx.WithValue(nativeContextKey{}, scope.token)
	return scope.ctx, scope, nil
}

func (scope *NativeScope) checkOpen() error {
	if scope.state != nativeScopeOpen || scope.db.nativeScope != scope ||
		scope.db.nativeActionDepth != scope.actionDepth {
		return types.ErrNativeScopeOrder
	}
	return nil
}

// Write merges private native state and events into the incoming SDK cache and
// closes the scope. It does not call the incoming cache's Write. EVM journal
// entries remain available for enclosing rollback, while this overlay's native
// undo transfers to its enclosing checkpoint. Balance reconciliation and EVM
// flushes must be done by the caller before Write; it performs neither implicitly.
func (scope *NativeScope) Write() error {
	if err := scope.checkOpen(); err != nil {
		return err
	}
	if !scope.cover.active {
		return types.ErrNativeScopeCoverage
	}
	scope.store.Write()
	scope.destination.EventManager().EmitEvents(scope.ctx.EventManager().Events())
	scope.state = nativeScopePublished
	scope.db.validRevisions = scope.db.validRevisions[:scope.revisionIndex]
	scope.finish()
	return nil
}

// Rollback restores shared EVM state, discards private native state/events, and
// closes the scope. It is safe to defer: after Write or a prior Rollback it does
// nothing. Rolling back an open scope out of order is a programmer error.
// Consumed gas and the precompile call counter are deliberately not restored.
func (scope *NativeScope) Rollback() {
	if scope.state != nativeScopeOpen {
		return
	}
	if err := scope.checkOpen(); err != nil {
		panic(err)
	}
	scope.db.RevertToSnapshot(scope.revision)
	scope.state = nativeScopeDiscarded
	scope.finish()
}

func (scope *NativeScope) finish() {
	scope.token.open = false
	scope.db.nativeScope = scope.parent
}
