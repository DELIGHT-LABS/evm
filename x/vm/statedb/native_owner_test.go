package statedb

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/store/snapshotmulti"
	"github.com/cosmos/evm/x/vm/types"

	"cosmossdk.io/store/cachekv"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/testutil"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestPrecompileRollbackUsesCheckpointOwner(t *testing.T) {
	// Given a native checkpoint in a private SDK child, followed by a return
	// to another context. Undo must use the owner captured at checkpoint time.
	key := storetypes.NewKVStoreKey("native")
	ctx := testutil.DefaultContext(key, storetypes.NewTransientStoreKey("transient"))
	parent, _ := ctx.CacheContext()
	parentStore := snapshotmulti.NewStore(parent.MultiStore().(storetypes.CacheMultiStore),
		map[string]*storetypes.KVStoreKey{key.Name(): key})
	parent = parent.WithMultiStore(parentStore)
	parentStore.Snapshot()
	parent.KVStore(key).Set([]byte("parent"), []byte("kept"))
	parent.EventManager().EmitEvent(sdk.NewEvent("parent"))

	child, _ := parent.CacheContext()
	owner := snapshotmulti.NewStoreWithKVStores(map[*storetypes.KVStoreKey]storetypes.CacheWrap{
		key: cachekv.NewStore(child.MultiStore().GetKVStore(key)),
	})
	child = child.WithMultiStore(owner)
	db := &StateDB{
		ctx:         ctx,
		cacheCtx:    child,
		snapshotter: owner,
		writeCache:  func() {},
		journal:     newJournal(),
	}
	frame := db.Snapshot()
	require.NoError(t, db.AddPrecompileFn(db.MultiStoreSnapshot()))
	child.KVStore(key).Set([]byte("callback"), []byte("discarded"))
	child.EventManager().EmitEvent(sdk.NewEvent("callback"))

	db.cacheCtx = parent
	db.snapshotter = parentStore

	// When the EVM frame is reverted after the active context changed.
	db.RevertToSnapshot(frame)

	// Then only the checkpoint owner's native changes and events disappear.
	require.Nil(t, child.KVStore(key).Get([]byte("callback")))
	require.Empty(t, child.EventManager().Events())
	require.Equal(t, []byte("kept"), parent.KVStore(key).Get([]byte("parent")))
	require.Equal(t, sdk.Events{sdk.NewEvent("parent")}, parent.EventManager().Events())
}

func TestNativeRollbackChecksCoverageBeforeUndoingMemory(t *testing.T) {
	// Given an invalid retained revision inside an already published scope.
	address := common.HexToAddress("0x91")
	db := &StateDB{journal: newJournal(), stateObjects: make(map[common.Address]*stateObject)}
	db.stateObjects[address] = newObject(db, address, Account{Balance: uint256.NewInt(9)})
	root := &nativeCheckpoint{active: true, journalIndex: 0}
	published := &NativeScope{state: nativeScopePublished, cover: root}
	child := &nativeCheckpoint{active: true, journalIndex: 1, scope: published}
	db.journal.append(precompileCallChange{checkpoint: root})
	db.journal.append(precompileCallChange{checkpoint: child})
	db.journal.append(balanceChange{account: &address, prev: uint256.NewInt(1)})
	db.validRevisions = []revision{{id: 0, journalIndex: 1}}

	// When its rollback cannot reach the checkpoint covering the native merge.
	require.PanicsWithValue(t, types.ErrNativeScopeCoverage, func() { db.RevertToSnapshot(0) })

	// Then no later memory entry was undone before detecting the bad coverage.
	require.Equal(t, uint256.NewInt(9), db.stateObjects[address].Balance())
	require.Len(t, db.journal.entries, 3)
	require.True(t, child.active)
}
