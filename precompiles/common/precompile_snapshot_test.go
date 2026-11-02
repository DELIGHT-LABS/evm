package common_test

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	cmn "github.com/cosmos/evm/precompiles/common"
	cmnmocks "github.com/cosmos/evm/precompiles/common/mocks"
	"github.com/cosmos/evm/x/vm/statedb"
	evmtypes "github.com/cosmos/evm/x/vm/types"
	"github.com/cosmos/evm/x/vm/types/mocks"

	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/testutil"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

// Only native storage is under test. The existing keeper supplies EVM accounts
// and bytecode, while KVStoreKeys connects StateDB's snapshots to a real SDK store.
type snapshotPrecompileKeeper struct {
	*mocks.EVMKeeper
	key *storetypes.KVStoreKey
}

func (k snapshotPrecompileKeeper) KVStoreKeys() map[string]*storetypes.KVStoreKey {
	return map[string]*storetypes.KVStoreKey{k.key.Name(): k.key}
}

type snapshotPrecompile struct {
	cmn.Precompile
	action func(*vm.EVM, *vm.Contract, sdk.Context) ([]byte, error)
	calls  int
	gas    uint64
	err    error
}

func (p *snapshotPrecompile) RequiredGas([]byte) uint64 { return 0 }

func (p *snapshotPrecompile) Run(evm *vm.EVM, contract *vm.Contract, _ bool) ([]byte, error) {
	p.calls++
	before := contract.Gas
	bz, err := p.RunNativeAction(evm, contract, func(ctx sdk.Context) ([]byte, error) {
		return p.action(evm, contract, ctx)
	})
	p.gas = before - contract.Gas
	p.err = err
	return bz, err
}

// snapshotCallCode leaves CALL's success flag on the stack. All addresses and
// gas limits are explicit; no Solidity compiler or generated artifacts are needed.
func snapshotCallCode(addr common.Address) []byte {
	code := []byte{
		byte(vm.PUSH1), 0, // output size
		byte(vm.PUSH1), 0, // output offset
		byte(vm.PUSH1), 0, // input size
		byte(vm.PUSH1), 0, // input offset
		byte(vm.PUSH1), 0, // value
		byte(vm.PUSH20),
	}
	code = append(code, addr.Bytes()...)
	return append(code, byte(vm.PUSH3), 0x01, 0x86, 0xa0, byte(vm.CALL)) // 100,000 gas
}

func TestRunNativeActionSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name         string
		childRevert  bool
		nativeError  bool
		outOfGas     bool
		nested       bool
		openCache    bool
		recoverInner bool
		scoped       bool
		spentParent  bool
	}{
		{name: "successful child commits nested SDK caches"},
		{name: "child REVERT discards flushed SDK caches", childRevert: true},
		{name: "native error discards flushed SDK caches", nativeError: true},
		{name: "native out of gas discards flushed SDK caches", outOfGas: true},
		{name: "nested precompile success commits both actions", nested: true},
		{name: "child REVERT discards both nested precompiles", nested: true, childRevert: true},
		{name: "open SDK cache survives nested precompile", nested: true, openCache: true},
		{name: "child REVERT discards open SDK cache and nested precompile", nested: true, openCache: true, childRevert: true},
		{name: "open SDK cache survives reverted nested precompile", nested: true, openCache: true, nativeError: true, recoverInner: true},
		{name: "open SDK cache survives out of gas nested precompile", nested: true, openCache: true, outOfGas: true, recoverInner: true},
		{name: "native scope exposes an open SDK branch to EVM reentry", nested: true, openCache: true, scoped: true},
		{name: "outer EVM REVERT removes written native scopes", nested: true, openCache: true, scoped: true, childRevert: true},
		{name: "native scope continues after callback REVERT", nested: true, openCache: true, scoped: true, nativeError: true, recoverInner: true},
		{name: "native scope continues after callback out of gas", nested: true, openCache: true, scoped: true, outOfGas: true, recoverInner: true},
		{name: "child gas budget excludes spent parent SDK gas", nested: true, openCache: true, scoped: true, spentParent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := storetypes.NewKVStoreKey("native")
			ctx := testutil.DefaultContext(key, storetypes.NewTransientStoreKey("native_t")).
				WithEventManager(sdk.NewEventManager())
			ctx.EventManager().EmitEvent(sdk.NewEvent("existing"))
			ctx.KVStore(key).Set([]byte("escrow"), []byte("100"))
			ctx.KVStore(key).Set([]byte("balance"), []byte("60"))

			keeper := snapshotPrecompileKeeper{EVMKeeper: mocks.NewEVMKeeper(), key: key}
			db := statedb.New(ctx, keeper, statedb.TxConfig{})
			caller := common.HexToAddress("0x1000")
			outer := common.HexToAddress("0x1001")
			child := common.HexToAddress("0x1002")
			leafAddr := common.HexToAddress("0x1003")
			nestedAddr := common.HexToAddress("0x1004")
			survivorAddr := common.HexToAddress("0x1005")

			// Exercise two genuine SDK CacheContexts and both write callbacks.
			// Neither write may flush through StateDB's transaction boundary.
			writeNative := func(ctx sdk.Context, mutate func(sdk.Context), event string) {
				first, writeFirst := ctx.CacheContext()
				second, writeSecond := first.CacheContext()
				mutate(second)
				second.EventManager().EmitEvent(sdk.NewEvent(event))
				writeSecond()
				writeFirst()
			}
			leaf := &snapshotPrecompile{Precompile: cmn.Precompile{ContractAddress: leafAddr}}
			leaf.action = func(_ *vm.EVM, contract *vm.Contract, nativeCtx sdk.Context) ([]byte, error) {
				if tc.scoped {
					require.Equal(t, []byte("child"), nativeCtx.KVStore(key).Get([]byte("nested")),
						"callback must see the unmerged SDK branch")
				}
				writeNative(nativeCtx, func(c sdk.Context) {
					c.KVStore(key).Set([]byte("escrow"), []byte("40"))
					c.KVStore(key).Delete([]byte("balance"))
					c.KVStore(key).Set([]byte("receipt"), []byte("child"))
				}, "child")
				require.Equal(t, []byte("40"), nativeCtx.KVStore(key).Get([]byte("escrow")))
				if tc.outOfGas {
					nativeCtx.GasMeter().ConsumeGas(contract.Gas+1, "native test out of gas")
				}
				nativeCtx.GasMeter().ConsumeGas(123, "native test work")
				if tc.nativeError {
					return nil, errors.New("native action failed")
				}
				return nil, nil
			}
			survivor := &snapshotPrecompile{Precompile: cmn.Precompile{ContractAddress: survivorAddr}}
			survivor.action = func(_ *vm.EVM, _ *vm.Contract, nativeCtx sdk.Context) ([]byte, error) {
				writeNative(nativeCtx, func(c sdk.Context) {
					c.KVStore(key).Set([]byte("survivor"), []byte("kept"))
				}, "survivor")
				return nil, nil
			}
			nested := &snapshotPrecompile{Precompile: cmn.Precompile{ContractAddress: nestedAddr}}
			nested.action = func(evm *vm.EVM, contract *vm.Contract, nativeCtx sdk.Context) ([]byte, error) {
				if tc.spentParent {
					nativeCtx.GasMeter().ConsumeGas(60_000, "earlier parent SDK work")
				}
				var writeOpenCache func()
				var scope *statedb.NativeScope
				if tc.openCache {
					first, writeFirst := nativeCtx.CacheContext()
					second, writeSecond := first.CacheContext()
					second.KVStore(key).Set([]byte("nested"), []byte("child"))
					second.EventManager().EmitEvent(sdk.NewEvent("nested"))
					if tc.scoped {
						var err error
						_, scope, err = db.NewNativeScope(second)
						require.NoError(t, err)
						defer scope.Rollback()
					}
					writeOpenCache = func() {
						writeSecond()
						writeFirst()
					}
				} else {
					writeNative(nativeCtx, func(c sdk.Context) {
						c.KVStore(key).Set([]byte("nested"), []byte("child"))
					}, "nested")
				}
				gas := contract.Gas / 2
				if tc.spentParent {
					gas = 1_000
				}
				parentGas := nativeCtx.GasMeter().GasConsumed()
				bz, left, err := evm.Call(nestedAddr, leafAddr, nil, gas, new(uint256.Int))
				require.True(t, contract.UseGas(gas-left, nil, tracing.GasChangeCallPrecompiledContract))
				if tc.spentParent {
					require.NoError(t, err, "the child's actual native work fits its own gas allowance")
					require.Equal(t, parentGas, nativeCtx.GasMeter().GasConsumed(),
						"child execution must not reset or consume the parent's SDK meter")
				}
				if scope != nil && (err == nil || tc.recoverInner) {
					require.NoError(t, scope.Write())
				}
				if writeOpenCache != nil {
					writeOpenCache()
					require.Equal(t, []byte("child"), nativeCtx.KVStore(key).Get([]byte("nested")))
				}
				if tc.recoverInner {
					require.Error(t, err)
					return nil, nil
				}
				return bz, err
			}

			target := leafAddr
			if tc.nested {
				target = nestedAddr
			}
			childCode := append(snapshotCallCode(target), byte(vm.POP))
			if tc.childRevert {
				childCode = append(childCode, byte(vm.PUSH1), 0, byte(vm.PUSH1), 0, byte(vm.REVERT))
			} else {
				childCode = append(childCode, byte(vm.STOP))
			}
			outerCode := append(snapshotCallCode(survivorAddr), byte(vm.POP))
			outerCode = append(outerCode, snapshotCallCode(child)...)
			// The outer frame ignores failure and returns the child's CALL flag.
			outerCode = append(outerCode,
				byte(vm.PUSH1), 0, byte(vm.MSTORE),
				byte(vm.PUSH1), 32, byte(vm.PUSH1), 0, byte(vm.RETURN))
			for addr, code := range map[common.Address][]byte{outer: outerCode, child: childCode} {
				hash := crypto.Keccak256(code)
				require.NoError(t, keeper.SetAccount(ctx, addr, statedb.Account{CodeHash: hash}))
				keeper.SetCode(ctx, hash, code)
			}
			// Pre-existing precompile accounts avoid unrelated account creation.
			for _, addr := range []common.Address{caller, leafAddr, nestedAddr, survivorAddr} {
				require.NoError(t, keeper.SetAccount(ctx, addr, statedb.Account{}))
			}

			var entered []common.Address
			exited := make(map[common.Address]bool)
			frameErrors := make(map[common.Address]error)
			tracer := &tracing.Hooks{
				OnEnter: func(_ int, _ byte, _, to common.Address, _ []byte, _ uint64, _ *big.Int) {
					entered = append(entered, to)
				},
				OnExit: func(_ int, _ []byte, _ uint64, err error, _ bool) {
					addr := entered[len(entered)-1]
					entered = entered[:len(entered)-1]
					exited[addr] = true
					frameErrors[addr] = err
				},
			}
			evm := vm.NewEVM(vm.BlockContext{
				BlockNumber: big.NewInt(1),
				Time:        1,
				Difficulty:  new(big.Int),
				BaseFee:     new(big.Int),
				GasLimit:    1_000_000,
				Transfer:    func(vm.StateDB, common.Address, common.Address, *uint256.Int) {},
			}, db, params.AllEthashProtocolChanges, vm.Config{Tracer: tracer})
			evm.SetPrecompiles(vm.PrecompiledContracts{
				leafAddr: leaf, nestedAddr: nested, survivorAddr: survivor,
			})

			ret, left, err := evm.Call(caller, outer, nil, 1_000_000, new(uint256.Int))
			require.NoError(t, err, "outer bytecode must succeed despite child failure")
			require.Len(t, ret, 32)
			if tc.childRevert {
				require.Zero(t, new(big.Int).SetBytes(ret).Sign())
				require.ErrorIs(t, frameErrors[child], vm.ErrExecutionReverted)
			} else {
				require.Equal(t, int64(1), new(big.Int).SetBytes(ret).Int64())
				require.NoError(t, frameErrors[child])
			}
			require.True(t, exited[child], "the child must be a real EVM frame")
			require.Equal(t, 1, leaf.calls)
			require.Equal(t, 1, survivor.calls)
			require.NoError(t, survivor.err)
			if tc.nested {
				require.Equal(t, 1, nested.calls)
				require.NoError(t, nested.err)
			}
			switch {
			case tc.outOfGas:
				require.ErrorIs(t, leaf.err, vm.ErrOutOfGas)
				require.ErrorIs(t, frameErrors[leafAddr], vm.ErrOutOfGas)
			case tc.nativeError:
				require.ErrorIs(t, leaf.err, vm.ErrExecutionReverted)
				require.ErrorIs(t, frameErrors[leafAddr], vm.ErrExecutionReverted)
			default:
				require.NoError(t, leaf.err)
				require.Equal(t, uint64(123), leaf.gas, "SDK work must charge EVM gas even if its caller reverts")
			}
			require.Less(t, left, uint64(1_000_000))
			require.Positive(t, left, "ignored child failure must not consume the outer gas reserve")

			// A nested SDK write must remain private until the final Commit.
			assert.Equal(t, []byte("100"), ctx.KVStore(key).Get([]byte("escrow")))
			assert.Equal(t, []byte("60"), ctx.KVStore(key).Get([]byte("balance")))
			assert.Nil(t, ctx.KVStore(key).Get([]byte("receipt")))
			assert.Nil(t, ctx.KVStore(key).Get([]byte("survivor")))
			require.NoError(t, db.Commit())

			assert.Equal(t, []byte("kept"), ctx.KVStore(key).Get([]byte("survivor")))
			wantEvents := []string{"existing", "survivor"}
			if tc.childRevert || tc.nativeError || tc.outOfGas {
				// EVM.Call must have traversed precompileCallChange.Revert:
				// updates, deletions, insertions and native events all roll back.
				assert.Equal(t, []byte("100"), ctx.KVStore(key).Get([]byte("escrow")))
				assert.Equal(t, []byte("60"), ctx.KVStore(key).Get([]byte("balance")))
				assert.Nil(t, ctx.KVStore(key).Get([]byte("receipt")))
				if tc.recoverInner && !tc.childRevert {
					assert.Equal(t, []byte("child"), ctx.KVStore(key).Get([]byte("nested")))
					wantEvents = append(wantEvents, "nested")
				} else {
					assert.Nil(t, ctx.KVStore(key).Get([]byte("nested")))
				}
			} else {
				assert.Equal(t, []byte("40"), ctx.KVStore(key).Get([]byte("escrow")))
				assert.Nil(t, ctx.KVStore(key).Get([]byte("balance")))
				assert.Equal(t, []byte("child"), ctx.KVStore(key).Get([]byte("receipt")))
				if tc.nested {
					assert.Equal(t, []byte("child"), ctx.KVStore(key).Get([]byte("nested")))
					if !tc.openCache || tc.scoped {
						wantEvents = append(wantEvents, "nested")
					}
				}
				wantEvents = append(wantEvents, "child")
				if tc.openCache && !tc.scoped {
					wantEvents = append(wantEvents, "nested")
				}
			}
			var gotEvents []string
			for _, event := range ctx.EventManager().Events() {
				gotEvents = append(gotEvents, event.Type)
			}
			assert.Equal(t, wantEvents, gotEvents, "reverted events must not be emitted at Commit")
		})
	}
}

type meteredSnapshotKeeper struct {
	snapshotPrecompileKeeper
	readGas uint64
}

func (k meteredSnapshotKeeper) GetBalance(ctx sdk.Context, address common.Address) *uint256.Int {
	ctx.GasMeter().ConsumeGas(k.readGas, "native balance read")
	return k.EVMKeeper.GetBalance(ctx, address)
}

func TestRunNativeActionChargesBalanceReconciliation(t *testing.T) {
	// Given two loaded accounts, only one of which has native balance events.
	setupBalanceHandlerTest(t)
	key := storetypes.NewKVStoreKey("native")
	ctx := testutil.DefaultContext(key, storetypes.NewTransientStoreKey("native_t"))
	keeper := meteredSnapshotKeeper{
		snapshotPrecompileKeeper: snapshotPrecompileKeeper{EVMKeeper: mocks.NewEVMKeeper(), key: key},
		readGas:                  7,
	}
	caller := common.HexToAddress("0x7100")
	target := common.HexToAddress("0x7200")
	for _, address := range []common.Address{caller, target} {
		require.NoError(t, keeper.SetAccount(ctx, address, *statedb.NewEmptyAccount()))
	}
	require.NoError(t, keeper.SetAccount(ctx, caller, statedb.Account{Balance: uint256.NewInt(5)}))
	db := statedb.New(ctx, keeper, statedb.TxConfig{})
	db.GetBalance(caller)
	db.GetBalance(target)
	bank := cmnmocks.NewBankKeeper(t)
	bank.On("BlockedAddr", mock.Anything).Return(false)
	precompile := &snapshotPrecompile{Precompile: cmn.Precompile{
		ContractAddress:       target,
		BalanceHandlerFactory: cmn.NewBalanceHandlerFactory(bank),
	}}
	precompile.action = func(_ *vm.EVM, _ *vm.Contract, native sdk.Context) ([]byte, error) {
		native.GasMeter().ConsumeGas(123, "native work")
		require.NoError(t, keeper.SetAccount(native, caller, statedb.Account{Balance: uint256.NewInt(9)}))
		native.EventManager().EmitEvent(banktypes.NewCoinReceivedEvent(sdk.AccAddress(caller.Bytes()),
			sdk.NewCoins(sdk.NewInt64Coin(evmtypes.GetEVMCoinDenom(), 3))))
		native.EventManager().EmitEvent(banktypes.NewCoinReceivedEvent(sdk.AccAddress(caller.Bytes()),
			sdk.NewCoins(sdk.NewInt64Coin(evmtypes.GetEVMCoinDenom(), 3))))
		return nil, nil
	}
	evm := vm.NewEVM(vm.BlockContext{
		BlockNumber: big.NewInt(1), Time: 1, Difficulty: new(big.Int), BaseFee: new(big.Int),
		GasLimit: 100_000,
		Transfer: func(vm.StateDB, common.Address, common.Address, *uint256.Int) {},
	}, db, params.AllEthashProtocolChanges, vm.Config{})
	evm.SetPrecompiles(vm.PrecompiledContracts{target: precompile})

	// When native execution returns and its balances are reconciled.
	_, _, err := evm.Call(caller, target, nil, 100_000, new(uint256.Int))

	// Then the affected account's read is charged, the unrelated account is
	// not reread, and the event's amount is not applied as a delta.
	require.NoError(t, err)
	require.Equal(t, uint64(123+7), precompile.gas)
	require.Equal(t, uint256.NewInt(9), db.GetBalance(caller))
}
