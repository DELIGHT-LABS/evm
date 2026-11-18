package integration

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm"
	rpctypes "github.com/cosmos/evm/rpc/types"
	"github.com/cosmos/evm/testutil/integration/evm/network"
	vmkeeper "github.com/cosmos/evm/x/vm/keeper"
	"github.com/cosmos/evm/x/vm/statedb"
	vmtracer "github.com/cosmos/evm/x/vm/tracer"
	vmglobal "github.com/cosmos/evm/x/vm/tracer/global"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// Exercise native reentry while the real StateDB publishes to the app keeper.
type reenteringAccountKeeper struct {
	*vmkeeper.Keeper
	reenter func(common.Address)
}

func (k reenteringAccountKeeper) SetAccount(ctx sdk.Context, addr common.Address, account statedb.Account) error {
	k.reenter(addr)
	return k.Keeper.SetAccount(ctx, addr, account)
}

func TestApplyMessageReaderLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nested   bool
		gas      uint64
		override bool
	}{
		{"commit", false, 200000, false},
		{"flush", true, 200000, false},
		{"intrinsic rejection", false, 1, false},
		{"override rejection", false, 200000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
				return CreateEvmd(chainID, evmChainID, opts...)
			})
			keeper := nw.App.GetEVMKeeper()
			ctx, _ := nw.GetContext().CacheContext()
			cfg, err := keeper.EVMConfig(ctx, ctx.BlockHeader().ProposerAddress)
			require.NoError(t, err)
			target := common.HexToAddress("0x4567")
			probe := common.HexToAddress("0x5678")
			parentImpl := common.HexToAddress("0x6789")
			nextImpl := common.HexToAddress("0x789a")
			msg := core.Message{To: &target, GasLimit: tc.gas, Value: big.NewInt(0), GasPrice: big.NewInt(0)}
			manager := vmtracer.New(nil)
			ctx = vmtracer.WithManager(ctx, manager)
			ctx, raw := vmglobal.TxTraceFactory(ctx, vmtracer.ExecutionInfo{Message: msg, Commit: true})
			collector := raw.(vmglobal.Collector)
			var ends []error
			var cause error
			if tc.gas == 1 {
				cause = traceDenial{}
			}
			manager.InstallGlobal(&finalizingTrace{Collector: collector, failure: cause, end: func(err error) {
				ends = append(ends, err)
			}})
			txConfig := statedb.NewEmptyTxConfig()
			parentDB := statedb.New(ctx, keeper, txConfig)
			parentDB.SetCode(probe, types.AddressToDelegation(parentImpl), tracing.CodeChangeUnspecified)
			parentDB.SetCode(parentImpl, common.FromHex("0x604260005360016000f3"), tracing.CodeChangeUnspecified)
			parentEVM := keeper.NewEVM(ctx, msg, cfg, manager.NestedHooks(), parentDB)
			continuation := parentEVM
			db := statedb.New(ctx, reenteringAccountKeeper{Keeper: keeper, reenter: func(addr common.Address) {
				if addr != target {
					return
				}
				output, _, err := parentEVM.Call(common.Address{}, probe, nil, 50000, uint256.NewInt(0))
				require.NoError(t, err)
				require.Equal(t, []byte{0x42}, output)
				nextDB := statedb.New(ctx, keeper, txConfig)
				nextDB.SetCode(probe, types.AddressToDelegation(nextImpl), tracing.CodeChangeUnspecified)
				nextDB.SetCode(nextImpl, common.FromHex("0x604360005360016000f3"), tracing.CodeChangeUnspecified)
				continuation = keeper.NewEVM(ctx, msg, cfg, manager.NestedHooks(), nextDB)
			}}, txConfig)
			db.SetCode(target, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
			var overrides *rpctypes.StateOverride
			if tc.override {
				storage := map[common.Hash]common.Hash{}
				overrides = &rpctypes.StateOverride{target: {State: &storage, StateDiff: &storage}}
			}
			hooks := manager.Hooks()
			if tc.nested {
				_, err := db.GetCacheContext()
				require.NoError(t, err)
				hooks = manager.NestedHooks()
			}

			response, err := keeper.ApplyMessageWithConfig(ctx, db, msg, hooks, true, tc.nested, cfg, txConfig, false, overrides)

			wantAddresses := []common.Address{parentImpl, nextImpl}
			wantOutput := []byte{0x43}
			switch {
			case tc.override:
				require.ErrorContains(t, err, "failed to apply state override")
				require.Nil(t, response)
				require.Empty(t, ends)
			case tc.gas == 1:
				require.ErrorIs(t, err, core.ErrIntrinsicGas)
				require.Nil(t, response)
				require.Len(t, ends, 1)
				require.ErrorIs(t, ends[0], core.ErrIntrinsicGas)
				require.ErrorIs(t, manager.ExecutionError(), cause)
			default:
				require.NoError(t, err)
				require.False(t, response.Failed())
				stateCtx := ctx
				if tc.nested {
					stateCtx, err = db.GetCacheContext()
					require.NoError(t, err)
					require.Empty(t, ends)
					require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}))
				} else {
					require.Len(t, ends, 1)
					require.NoError(t, ends[0])
				}
				require.Equal(t, common.HexToHash("0x1"), keeper.GetState(stateCtx, target, common.Hash{}))
			}
			if tc.override || tc.gas == 1 {
				wantAddresses = []common.Address{parentImpl}
				wantOutput = []byte{0x42}
				require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}))
			}
			output, _, err := continuation.Call(common.Address{}, probe, nil, 50000, uint256.NewInt(0))
			require.NoError(t, err)
			require.Equal(t, wantOutput, output)
			var codeAddresses []common.Address
			for _, observation := range collector.Observations() {
				if observation.Kind == vmtracer.CallEnter && observation.Target == probe {
					codeAddresses = append(codeAddresses, observation.CodeAddress)
				}
			}
			require.Equal(t, wantAddresses, codeAddresses)
		})
	}
}
