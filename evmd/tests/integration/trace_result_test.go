package integration

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm"
	"github.com/cosmos/evm/testutil/integration/evm/network"
	"github.com/cosmos/evm/utils"
	"github.com/cosmos/evm/x/vm/statedb"
	vmtracer "github.com/cosmos/evm/x/vm/tracer"
	vmglobal "github.com/cosmos/evm/x/vm/tracer/global"
	vmtypes "github.com/cosmos/evm/x/vm/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

type traceResultHooks struct {
	run     func(sdk.Context) error
	receipt *ethtypes.Receipt
}

func (h *traceResultHooks) PostTxProcessing(ctx sdk.Context, _ common.Address, _ core.Message, receipt *ethtypes.Receipt) error {
	h.receipt = receipt
	return h.run(ctx)
}

func (h *traceResultHooks) EstimatePostTxProcessing(ctx sdk.Context, sender common.Address, msg core.Message, receipt *ethtypes.Receipt) error {
	return h.PostTxProcessing(ctx, sender, msg, receipt)
}

func TestTraceFinalState(t *testing.T) {
	nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	})
	keeper := nw.App.GetEVMKeeper()
	hooks := &traceResultHooks{}
	keeper.SetHooks(hooks)
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	target := common.HexToAddress("0x1234")
	slot := common.Hash{}
	initial := common.HexToHash("0x3")

	for _, tc := range []struct {
		name      string
		code      string
		write     bool
		gas       uint64
		hookErr   error
		wantValue common.Hash
	}{
		{"EVM write", "0x600160005500", false, 0, nil, common.HexToHash("0x1")},
		{"hook overwrites EVM write", "0x600160005500", true, 0, nil, common.HexToHash("0x7")},
		{"hook rejection discards writes", "0x600160005500", true, 0, traceDenial{}, initial},
		{"hook out of gas discards writes", "0x600160005500", true, 200000, nil, initial},
		{"reverted EVM retains hook write", "0x60005450601260005360016000fd", true, 0, nil, common.HexToHash("0x7")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := nw.GetContext().CacheContext()
			state := statedb.New(ctx, keeper, statedb.NewEmptyTxConfig())
			state.SetCode(target, common.FromHex(tc.code), tracing.CodeChangeUnspecified)
			state.SetState(target, slot, initial)
			require.NoError(t, state.Commit())
			hooks.run = func(ctx sdk.Context) error {
				if tc.write {
					keeper.SetState(ctx, target, slot, common.HexToHash("0x7").Bytes())
				}
				ctx.GasMeter().ConsumeGas(tc.gas, "test post hook")
				return tc.hookErr
			}
			blockTime, err := utils.SafeUint64(ctx.BlockTime().Unix())
			require.NoError(t, err)
			signer := ethtypes.MakeSigner(vmtypes.GetEthChainConfig(), big.NewInt(ctx.BlockHeight()), blockTime)
			tx, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 200000, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
			require.NoError(t, err)
			msg := &vmtypes.MsgEthereumTx{}
			msg.FromEthereumTx(tx)
			deliveryCtx, _ := ctx.CacheContext()
			_, err = keeper.ApplyTransaction(deliveryCtx, tx)
			require.NoError(t, err)
			require.Equal(t, tc.wantValue, keeper.GetState(deliveryCtx, target, slot))

			response, err := keeper.TraceTx(ctx, &vmtypes.QueryTraceTxRequest{
				Msg: msg, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000,
				TraceConfig: &vmtypes.TraceConfig{Tracer: "prestateTracer", TracerJsonConfig: `{"diffMode":true}`},
			})
			require.NoError(t, err)
			var diff struct {
				Pre  map[common.Address]struct{ Storage map[common.Hash]common.Hash }
				Post map[common.Address]struct{ Storage map[common.Hash]common.Hash }
			}
			require.NoError(t, json.Unmarshal(response.Data, &diff))
			if tc.wantValue == initial {
				require.NotContains(t, diff.Pre, target)
				require.NotContains(t, diff.Post, target)
			} else {
				require.Equal(t, initial, diff.Pre[target].Storage[slot])
				require.Equal(t, tc.wantValue, diff.Post[target].Storage[slot])
			}
			require.Equal(t, initial, keeper.GetState(ctx, target, slot))
		})
	}
}

type traceResultRevert []byte

func (traceResultRevert) Error() string        { return "post hook reverted" }
func (e traceResultRevert) RevertData() []byte { return []byte(e) }

func TestTracePostHookOverridesRootOutcome(t *testing.T) {
	nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	})
	keeper := nw.App.GetEVMKeeper()
	hooks := &traceResultHooks{}
	keeper.SetHooks(hooks)
	target := common.HexToAddress("0x1234")
	key, err := crypto.GenerateKey()
	require.NoError(t, err)

	for _, tc := range []struct {
		name      string
		code      string
		hookGas   uint64
		hookErr   error
		wantError string
		wantRet   []byte
		fullGas   bool
		reject    bool
		cause     error
	}{
		{"revert then hook out of gas", "0x601260005360016000fd", 200000, nil, vm.ErrOutOfGas.Error(), nil, true, false, nil},
		{"fault then hook revert", "0xfe", 0, traceResultRevert{0xde, 0xad}, vm.ErrExecutionReverted.Error(), []byte{0xde, 0xad}, true, false, nil},
		{"revert then hook revert", "0x601260005360016000fd", 0, traceResultRevert{0xde, 0xad}, vm.ErrExecutionReverted.Error(), []byte{0xde, 0xad}, false, false, nil},
		{"revert then empty hook revert", "0x601260005360016000fd", 0, traceResultRevert{}, "", nil, false, true, nil},
		{"success then plain hook error", "0x600160005500", 0, errors.New("hook unavailable"), "", nil, false, true, nil},
		{"success then joined hook revert", "0x600160005500", 0, errors.Join(traceResultRevert{0xde, 0xad}, errors.New("hook unavailable")), "", nil, false, true, nil},
		{"latched domain then hook revert", "0x600160005500", 0, traceResultRevert{0xbe, 0xef}, vm.ErrExecutionReverted.Error(), []byte{0xbe, 0xef}, false, false, traceDenial{}},
		{"latched domain then hook admission error", "0x600160005500", 0, core.ErrIntrinsicGas, "", nil, false, true, traceDenial{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := nw.GetContext().CacheContext()
			state := statedb.New(ctx, keeper, statedb.NewEmptyTxConfig())
			state.SetCode(target, common.FromHex(tc.code), tracing.CodeChangeUnspecified)
			require.NoError(t, state.Commit())
			keeper.SetGlobalTracerFactories(func(ctx sdk.Context, info vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
				ctx, tr := vmglobal.TxTraceFactory(ctx, info)
				return ctx, &finalizingTrace{Collector: tr.(vmglobal.Collector), failure: tc.cause}
			})
			hooks.run = func(ctx sdk.Context) error {
				if tc.cause != nil {
					manager, ok := vmtracer.FromContext(ctx)
					require.True(t, ok)
					data, ok := vmtypes.DomainRevertData(manager.ExecutionError())
					require.True(t, ok)
					require.Equal(t, []byte{0xde, 0xad}, data)
					require.Equal(t, ethtypes.ReceiptStatusFailed, hooks.receipt.Status)
					require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}))
					t.Logf("before hook: latched domain=%s receipt=%d EVM slot discarded", hexutil.Encode(data), hooks.receipt.Status)
				}
				if tc.reject || tc.cause != nil {
					keeper.SetState(ctx, target, common.HexToHash("0x2"), common.HexToHash("0x7").Bytes())
				}
				ctx.GasMeter().ConsumeGas(tc.hookGas, "test post hook")
				return tc.hookErr
			}
			blockTime, err := utils.SafeUint64(ctx.BlockTime().Unix())
			require.NoError(t, err)
			signer := ethtypes.MakeSigner(vmtypes.GetEthChainConfig(), big.NewInt(ctx.BlockHeight()), blockTime)
			tx, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 200000, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
			require.NoError(t, err)
			msg := &vmtypes.MsgEthereumTx{}
			msg.FromEthereumTx(tx)
			deliveryCtx, _ := ctx.CacheContext()
			deliveryCtx = deliveryCtx.WithGasMeter(storetypes.NewGasMeter(200000))
			gasBefore := keeper.GetTransientGasUsed(deliveryCtx)
			delivery, err := keeper.ApplyTransaction(deliveryCtx, tx)
			if tc.reject {
				require.ErrorContains(t, err, "EVM execution rejected")
				require.EqualError(t, err, "failed to apply ethereum core message: EVM execution rejected: "+tc.hookErr.Error())
				require.Nil(t, delivery)
				deliveryCtx = deliveryCtx.WithGasMeter(storetypes.NewInfiniteGasMeter())
				require.Equal(t, gasBefore, keeper.GetTransientGasUsed(deliveryCtx))
				require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, target, common.Hash{}))
				require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, target, common.HexToHash("0x2")))
				t.Logf("rejected hook: %v; response=nil; transientGas=%d; EVM and hook state discarded", err, gasBefore)
				if tc.cause != nil {
					response, err := keeper.TraceTx(ctx, &vmtypes.QueryTraceTxRequest{
						Msg: msg, Predecessors: []*vmtypes.MsgEthereumTx{msg},
						BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000,
						TraceConfig: &vmtypes.TraceConfig{Tracer: "callTracer"},
					})
					require.Nil(t, response)
					require.ErrorContains(t, err, tc.cause.Error())
					require.NotContains(t, err.Error(), core.ErrIntrinsicGas.Error())
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantError, delivery.VmError)
			require.Equal(t, tc.wantRet, delivery.Ret)
			require.True(t, delivery.Failed())
			if tc.cause != nil {
				require.Equal(t, ethtypes.ReceiptStatusFailed, hooks.receipt.Status)
				require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, target, common.Hash{}))
				require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, target, common.HexToHash("0x2")))
				t.Logf("after hook: receipt=%d EVM slot=0 hook slot=0", hooks.receipt.Status)
			}
			t.Logf("failed outcome: VmError=%q Ret=%s GasUsed=%d", delivery.VmError, hexutil.Encode(delivery.Ret), delivery.GasUsed)
			if tc.fullGas {
				require.Equal(t, uint64(200000), delivery.GasUsed)
			}
			for _, name := range []string{"callTracer", "callTracerLegacy"} {
				t.Run(name, func(t *testing.T) {
					response, err := keeper.TraceTx(ctx, &vmtypes.QueryTraceTxRequest{
						Msg: msg, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000,
						TraceConfig: &vmtypes.TraceConfig{Tracer: name},
					})
					require.NoError(t, err)
					t.Logf("%s TraceTx=%s", name, response.Data)
					var result map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(response.Data, &result))
					var vmError, gasUsed string
					require.NoError(t, json.Unmarshal(result["error"], &vmError))
					require.NoError(t, json.Unmarshal(result["gasUsed"], &gasUsed))
					require.Equal(t, delivery.VmError, vmError)
					gas, err := hexutil.DecodeUint64(gasUsed)
					require.NoError(t, err)
					require.Equal(t, delivery.GasUsed, gas)
					if len(tc.wantRet) == 0 {
						require.NotContains(t, result, "output")
					} else {
						require.Contains(t, result, "output")
						var output string
						require.NoError(t, json.Unmarshal(result["output"], &output))
						require.Equal(t, hexutil.Encode(tc.wantRet), output)
					}
					if tc.cause != nil {
						require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}))
						require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.HexToHash("0x2")))
					}
				})
			}
		})
	}
}
