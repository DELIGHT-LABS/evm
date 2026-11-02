package statedb_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/statedb"
	"github.com/cosmos/evm/x/vm/types"

	storetypes "cosmossdk.io/store/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

type lockedNativeKeeper struct {
	*atomicTestKeeper
	locked *big.Int
}

func TestNativeBalanceLazyReadPreservesEVMReadGasAccounting(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	address := common.HexToAddress("0x68")
	require.NoError(t, keeper.SetAccount(ctx, address, statedb.Account{Balance: uint256.NewInt(18)}))
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	defer end()
	child, _ := action.CacheContext()
	child = child.WithGasMeter(storetypes.NewGasMeter(1))
	native, scope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer scope.Rollback()

	// EVM account reads already have EVM opcode gas accounting. Selecting the
	// active native store must not also drain the enclosing SDK call's meter.
	require.NotPanics(t, func() {
		require.Equal(t, uint256.NewInt(18), db.GetBalance(address))
	})
	require.Zero(t, native.GasMeter().GasConsumed())
}

func TestNativeBalanceReconcileUsesKeeperBalanceReader(t *testing.T) {
	_, ctx, keeper := newNativeScopeDB(t)
	// Expose only the Keeper contract, rather than the concrete keeper's
	// additional methods.
	reader := struct{ statedb.Keeper }{Keeper: keeper}
	db := statedb.New(ctx, reader, emptyTxConfig)
	address := common.HexToAddress("0x69")
	require.NoError(t, keeper.SetAccount(ctx, address, statedb.Account{Balance: uint256.NewInt(10)}))
	require.Equal(t, uint256.NewInt(10), db.GetBalance(address))
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	defer end()
	require.NoError(t, keeper.SetAccount(action, address, statedb.Account{Balance: uint256.NewInt(25)}))

	require.NoError(t, db.ReconcileNativeBalances(action))
	require.Equal(t, uint256.NewInt(25), db.GetBalance(address))
}

func TestNativeBalanceOriginalReadCacheSurvivesRevert(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	address := common.HexToAddress("0x70")
	require.NoError(t, keeper.SetAccount(ctx, address, statedb.Account{Balance: uint256.NewInt(18)}))
	revision := db.Snapshot()
	before := ctx.GasMeter().GasConsumed()
	require.Equal(t, uint256.NewInt(18), db.GetBalance(address))
	require.Greater(t, ctx.GasMeter().GasConsumed(), before)
	db.RevertToSnapshot(revision)

	// Immutable original-context reads retain ordinary EVM memoization.
	// Only reads from a mutable native working view need cache undo.
	before = ctx.GasMeter().GasConsumed()
	require.Equal(t, uint256.NewInt(18), db.GetBalance(address))
	require.Equal(t, before, ctx.GasMeter().GasConsumed())
}

func (k *lockedNativeKeeper) GetAccount(ctx sdk.Context, address common.Address) *statedb.Account {
	account := k.atomicTestKeeper.GetAccount(ctx, address)
	if account == nil {
		return nil
	}
	spendable := new(big.Int).Sub(account.Balance.ToBig(), k.locked)
	return statedb.NewAccount(0, new(uint256.Int).SetBytes(spendable.Bytes()), new(big.Int).Set(k.locked), nil)
}

func (k *lockedNativeKeeper) SetAccount(ctx sdk.Context, address common.Address, account statedb.Account) error {
	locked := account.LockedBalanceSnapshot()
	if locked == nil {
		locked = k.locked
	}
	total := new(big.Int).Add(account.Balance.ToBig(), locked)
	return k.atomicTestKeeper.SetAccount(ctx, address, statedb.Account{Balance: new(uint256.Int).SetBytes(total.Bytes())})
}

func TestNativeBalanceReconcilePreservesSavedLockedOffset(t *testing.T) {
	// Given an EVM account loaded with a fixed locked offset of 20.
	_, ctx, base := newNativeScopeDB(t)
	keeper := &lockedNativeKeeper{atomicTestKeeper: base, locked: big.NewInt(20)}
	db := statedb.New(ctx, keeper, emptyTxConfig)
	address := common.HexToAddress("0x66")
	require.NoError(t, base.SetAccount(ctx, address, statedb.Account{Balance: uint256.NewInt(100)}))
	require.Equal(t, uint256.NewInt(80), db.GetBalance(address))
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	defer end()
	child, _ := action.CacheContext()
	native, scope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer scope.Rollback()

	// When native state has a different current lock, import its full total
	// using the original offset, not the current spendable balance.
	keeper.locked = big.NewInt(25)
	require.NoError(t, base.SetAccount(native, address, statedb.Account{Balance: uint256.NewInt(107)}))
	require.NoError(t, db.ReconcileNativeBalances(native))
	require.Equal(t, uint256.NewInt(87), db.GetBalance(address))
	db.AddBalance(address, uint256.NewInt(1), tracing.BalanceChangeUnspecified)
	require.NoError(t, db.FlushToCacheCtx())

	// Then export reconstructs 88 + 20, not 88 + the current lock of 25.
	require.Equal(t, uint256.NewInt(108), base.GetAccount(native, address).Balance)
	scope.Rollback()
	require.Equal(t, uint256.NewInt(80), db.GetBalance(address))
	require.Equal(t, uint256.NewInt(100), base.GetAccount(ctx, address).Balance)
}

func TestNativeBalanceRejectsTotalBelowSavedLockedOffset(t *testing.T) {
	_, ctx, base := newNativeScopeDB(t)
	keeper := &lockedNativeKeeper{atomicTestKeeper: base, locked: big.NewInt(20)}
	db := statedb.New(ctx, keeper, emptyTxConfig)
	address := common.HexToAddress("0x67")
	require.NoError(t, base.SetAccount(ctx, address, statedb.Account{Balance: uint256.NewInt(100)}))
	require.Equal(t, uint256.NewInt(80), db.GetBalance(address))
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	defer end()
	child, _ := action.CacheContext()
	native, scope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer scope.Rollback()
	keeper.locked = new(big.Int)
	require.NoError(t, base.SetAccount(native, address, statedb.Account{Balance: uint256.NewInt(10)}))

	require.ErrorIs(t, db.ReconcileNativeBalances(native), types.ErrNativeBalance)
	require.Equal(t, uint256.NewInt(80), db.GetBalance(address))
	scope.Rollback()
	require.Equal(t, uint256.NewInt(100), base.GetAccount(ctx, address).Balance)
}

func TestNativeBalancePendingWritesVisibleOnLazyRead(t *testing.T) {
	for _, inScope := range []bool{false, true} {
		name := "root cache"
		if inScope {
			name = "private scope"
		}
		t.Run(name, func(t *testing.T) {
			db, ctx, keeper := newNativeScopeDB(t)
			addr := common.HexToAddress("0x61")
			require.NoError(t, keeper.SetAccount(ctx, addr, statedb.Account{Balance: uint256.NewInt(10)}))
			action, end, err := db.BeginNativeAction()
			require.NoError(t, err)
			defer end()
			native := action
			if inScope {
				child, _ := action.CacheContext()
				var scope *statedb.NativeScope
				native, scope, err = db.NewNativeScope(child)
				require.NoError(t, err)
				defer scope.Rollback()
			}
			require.NoError(t, keeper.SetAccount(native, addr, statedb.Account{Balance: uint256.NewInt(25)}))
			require.Equal(t, uint256.NewInt(25), db.GetBalance(addr), "EVM reentry must read pending native state")
			require.Equal(t, uint256.NewInt(10), keeper.GetAccount(db.GetContext(), addr).Balance,
				"the committed baseline must stay separate from the effective native context")
		})
	}
}

func TestNativeBalanceReconcileIsAbsoluteAndSurvivesFlush(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	addr := common.HexToAddress("0x62")
	require.NoError(t, keeper.SetAccount(ctx, addr, statedb.Account{Balance: uint256.NewInt(10)}))
	db.AddBalance(addr, uint256.NewInt(2), tracing.BalanceChangeUnspecified)
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	child, write := action.CacheContext()
	native, scope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer scope.Rollback()
	require.NoError(t, db.FlushToCacheCtx())
	require.Equal(t, uint256.NewInt(12), keeper.GetAccount(native, addr).Balance)
	require.NoError(t, keeper.SetAccount(native, addr, statedb.Account{Balance: uint256.NewInt(31)}))

	require.NoError(t, db.ReconcileNativeBalances(native))
	require.Equal(t, uint256.NewInt(31), db.GetBalance(addr))
	require.NoError(t, db.ReconcileNativeBalances(native))
	require.Equal(t, uint256.NewInt(31), db.GetBalance(addr), "reconciliation imports an absolute total, not a delta")
	require.NoError(t, db.FlushToCacheCtx())
	require.Equal(t, uint256.NewInt(31), keeper.GetAccount(native, addr).Balance,
		"flush must not overwrite native changes with the old EVM balance")
	db.AddBalance(addr, uint256.NewInt(4), tracing.BalanceChangeUnspecified)
	require.NoError(t, db.FlushToCacheCtx())
	require.Equal(t, uint256.NewInt(35), keeper.GetAccount(native, addr).Balance)
	require.NoError(t, scope.Write())
	write()
	end()
	require.NoError(t, db.Commit())
	require.Equal(t, uint256.NewInt(35), keeper.GetAccount(ctx, addr).Balance)
}

func TestNativeBalancePureImportDoesNotExportAccount(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	addr := common.HexToAddress("0x63")
	require.NoError(t, keeper.SetAccount(ctx, addr, statedb.Account{Balance: uint256.NewInt(10)}))
	require.Equal(t, uint256.NewInt(10), db.GetBalance(addr))
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	child, write := action.CacheContext()
	native, scope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer scope.Rollback()
	require.NoError(t, keeper.SetAccount(native, addr, statedb.Account{Balance: uint256.NewInt(24)}))
	// Native writes above bypass StateDB. Any later SetAccount for this
	// address is an unintended export of an import-only journal entry.
	keeper.errAddr = addr
	require.NoError(t, db.ReconcileNativeBalances(native))
	require.NoError(t, db.ReconcileNativeBalances(native))
	require.Equal(t, uint256.NewInt(24), db.GetBalance(addr))
	require.NoError(t, db.FlushToCacheCtx())
	require.NoError(t, scope.Write())
	write()
	end()
	require.NoError(t, db.Commit())
	require.Equal(t, uint256.NewInt(24), keeper.GetAccount(ctx, addr).Balance)
}

func TestNativeBalanceRollbackRestoresImportedAndLazyAccounts(t *testing.T) {
	db, ctx, keeper := newNativeScopeDB(t)
	cached := common.HexToAddress("0x64")
	lazy := common.HexToAddress("0x65")
	for _, addr := range []common.Address{cached, lazy} {
		require.NoError(t, keeper.SetAccount(ctx, addr, statedb.Account{Balance: uint256.NewInt(10)}))
	}
	require.Equal(t, uint256.NewInt(10), db.GetBalance(cached))
	action, end, err := db.BeginNativeAction()
	require.NoError(t, err)
	child, _ := action.CacheContext()
	native, scope, err := db.NewNativeScope(child)
	require.NoError(t, err)
	defer scope.Rollback()
	for _, addr := range []common.Address{cached, lazy} {
		require.NoError(t, keeper.SetAccount(native, addr, statedb.Account{Balance: uint256.NewInt(27)}))
	}
	require.NoError(t, db.ReconcileNativeBalances(native))
	require.Equal(t, uint256.NewInt(27), db.GetBalance(cached))
	require.Equal(t, uint256.NewInt(27), db.GetBalance(lazy))
	scope.Rollback()
	require.Equal(t, uint256.NewInt(10), db.GetBalance(cached))
	require.Equal(t, uint256.NewInt(10), db.GetBalance(lazy))
	require.NoError(t, db.FlushToCacheCtx())
	end()
	require.NoError(t, db.Commit())
	for _, addr := range []common.Address{cached, lazy} {
		require.Equal(t, uint256.NewInt(10), keeper.GetAccount(ctx, addr).Balance)
	}
}
