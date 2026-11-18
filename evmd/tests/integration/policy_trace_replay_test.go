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
	vmkeeper "github.com/cosmos/evm/x/vm/keeper"
	"github.com/cosmos/evm/x/vm/statedb"
	vmtracer "github.com/cosmos/evm/x/vm/tracer"
	vmglobal "github.com/cosmos/evm/x/vm/tracer/global"
	vmtypes "github.com/cosmos/evm/x/vm/types"

	sdkmath "cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/baseapp"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

type traceDenial struct{}

func (traceDenial) Error() string      { return "denied" }
func (traceDenial) RevertData() []byte { return []byte{0xde, 0xad} }

type encodedInfrastructure struct{}

func (encodedInfrastructure) Error() string { return "storage unavailable" }

// encodedObserver preserves joined causes without carrying its own revert data.
type encodedObserver struct{ cause error }

func (e encodedObserver) Error() string   { return e.cause.Error() }
func (e encodedObserver) Unwrap() []error { return []error{encodedInfrastructure{}, e.cause} }

type policyTestHooks struct {
	keeper       *vmkeeper.Keeper
	failure      *error
	override     *error
	firstFailure *error
	target       common.Address
	gas          *uint64
}

func (h policyTestHooks) PostTxProcessing(ctx sdk.Context, _ common.Address, _ core.Message, receipt *ethtypes.Receipt) error {
	ctx.GasMeter().ConsumeGas(*h.gas, "test post hook")
	if receipt.Status != ethtypes.ReceiptStatusSuccessful {
		return nil
	}
	if *h.override != nil {
		return *h.override
	}
	if receipt.TransactionIndex == 0 && *h.firstFailure != nil {
		return *h.firstFailure
	}
	if *h.failure != nil {
		return *h.failure
	}
	h.keeper.SetState(ctx, h.target, common.HexToHash("0x2"), common.HexToHash("0x7").Bytes())
	return nil
}

func (h policyTestHooks) EstimatePostTxProcessing(ctx sdk.Context, sender common.Address, msg core.Message, receipt *ethtypes.Receipt) error {
	return h.PostTxProcessing(ctx, sender, msg, receipt)
}

type finalizingTrace struct {
	vmglobal.Collector
	failure error
	start   func(*tracing.VMContext)
	end     func(error)
}

func (t *finalizingTrace) Hooks() *tracing.Hooks {
	hooks := *t.Collector.Hooks()
	start := hooks.OnTxStart
	hooks.OnTxStart = func(ctx *tracing.VMContext, tx *ethtypes.Transaction, sender common.Address) {
		if start != nil {
			start(ctx, tx, sender)
		}
		if t.start != nil {
			t.start(ctx)
		}
	}
	end := hooks.OnTxEnd
	hooks.OnTxEnd = func(receipt *ethtypes.Receipt, err error) {
		if t.end != nil {
			t.end(err)
		}
		if end != nil {
			end(receipt, err)
		}
		if t.failure != nil {
			t.LatchError(t.failure)
		}
	}
	return &hooks
}

func TestHistoricalPolicyTrace(t *testing.T) {
	nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	})
	ctx := nw.GetContext()
	keeper := nw.App.GetEVMKeeper()
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	target := common.HexToAddress("0x1234")
	state := nw.GetStateDB()
	state.SetCode(target, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
	require.NoError(t, state.Commit())
	blockTime, err := utils.SafeUint64(ctx.BlockTime().Unix())
	require.NoError(t, err)
	signer := ethtypes.MakeSigner(vmtypes.GetEthChainConfig(), big.NewInt(ctx.BlockHeight()), blockTime)
	tx, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 200000, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
	require.NoError(t, err)
	msg := &vmtypes.MsgEthereumTx{}
	msg.FromEthereumTx(tx)
	var failure error
	var hookFailure error
	var firstHookFailure error
	var infos []vmtracer.ExecutionInfo
	var observed []common.Hash
	var hookObserved []common.Hash
	var endings []error
	var hookGas uint64
	keeper.SetHooks(policyTestHooks{keeper: keeper, failure: &failure, override: &hookFailure, firstFailure: &firstHookFailure, target: target, gas: &hookGas})
	keeper.SetGlobalTracerFactories(func(ctx sdk.Context, info vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		infos = append(infos, info)
		ctx, tr := vmglobal.TxTraceFactory(ctx, info)
		return ctx, &finalizingTrace{Collector: tr.(vmglobal.Collector), failure: failure, end: func(err error) { endings = append(endings, err) }, start: func(ctx *tracing.VMContext) {
			observed = append(observed, ctx.StateDB.GetState(target, common.Hash{}))
			hookObserved = append(hookObserved, ctx.StateDB.GetState(target, common.HexToHash("0x2")))
		}}
	})
	req := &vmtypes.QueryTraceTxRequest{Msg: msg, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000}
	for _, tracerName := range []string{"", "callTracer", "callTracerLegacy"} {
		t.Run(tracerName+" denial", func(t *testing.T) {
			failure = traceDenial{}
			req.TraceConfig = &vmtypes.TraceConfig{Tracer: tracerName}
			response, err := keeper.TraceTx(ctx, req)
			require.NoError(t, err)
			var result map[string]any
			require.NoError(t, json.Unmarshal(response.Data, &result))
			if tracerName == "" {
				require.Equal(t, true, result["failed"])
				require.Equal(t, "0xdead", result["returnValue"])
			} else {
				require.Equal(t, "execution reverted", result["error"])
				require.Equal(t, "0xdead", result["output"])
			}
			require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
			require.True(t, infos[len(infos)-1].Commit)
		})
	}
	t.Run("infrastructure", func(t *testing.T) {
		for _, tc := range []struct {
			cause   error
			message string
		}{
			{errors.Join(traceDenial{}, errors.New("observer failure")), "observer failure"},
			{encodedInfrastructure{}, "storage unavailable"},
		} {
			failure = tc.cause
			deliveryCtx, _ := ctx.CacheContext()
			deliveryCtx = deliveryCtx.WithGasMeter(storetypes.NewGasMeter(200000))
			delivery, err := keeper.ApplyTransaction(deliveryCtx, tx)
			require.ErrorContains(t, err, tc.message)
			require.Nil(t, delivery)
			require.Equal(t, uint64(200000), deliveryCtx.GasMeter().GasConsumed())
			deliveryCtx = deliveryCtx.WithGasMeter(storetypes.NewInfiniteGasMeter())
			require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, target, common.Hash{}))
			response, err := keeper.TraceTx(ctx, req)
			require.ErrorContains(t, err, tc.message)
			require.Nil(t, response)
			t.Logf("infrastructure reject: %v; delivery and trace response=nil; state unchanged", err)
		}
	})
	t.Run("trace matches delivery for domain failures and rejects", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			hook     error
			observer error
			reject   string
		}{
			{"hook encoded", traceDenial{}, nil, ""},
			{"hook mixed", errors.Join(traceDenial{}, encodedInfrastructure{}), nil, "storage unavailable"},
			{"hook plain", errors.New("hook unavailable"), nil, "hook unavailable"},
			{"reverted root encoded", nil, encodedObserver{cause: encodedInfrastructure{}}, "storage unavailable"},
			{"reverted root mixed", nil, encodedObserver{cause: errors.Join(traceDenial{}, encodedInfrastructure{})}, "storage unavailable"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				state := nw.GetStateDB()
				code := "0x600160005500"
				failure, hookFailure = nil, tc.hook
				if tc.observer != nil {
					code = "0x601260005360016000fd"
				}
				state.SetCode(target, common.FromHex(code), tracing.CodeChangeUnspecified)
				require.NoError(t, state.Commit())
				if tc.observer != nil {
					baselineCtx, _ := ctx.CacheContext()
					baselineCtx = baselineCtx.WithGasMeter(storetypes.NewGasMeter(200000))
					response, err := keeper.ApplyTransaction(baselineCtx, tx)
					require.NoError(t, err)
					require.Equal(t, vm.ErrExecutionReverted.Error(), response.VmError)
					require.Equal(t, []byte{0x12}, response.Ret)
				}
				failure = tc.observer
				deliveryCtx, _ := ctx.CacheContext()
				deliveryCtx = deliveryCtx.WithGasMeter(storetypes.NewGasMeter(200000))
				delivery, err := keeper.ApplyTransaction(deliveryCtx, tx)
				req.TraceConfig = &vmtypes.TraceConfig{Tracer: "callTracer"}
				if tc.reject != "" {
					require.ErrorContains(t, err, "EVM execution rejected")
					require.ErrorContains(t, err, tc.reject)
					require.Nil(t, delivery)
					require.Equal(t, uint64(200000), deliveryCtx.GasMeter().GasConsumed())
					deliveryCtx = deliveryCtx.WithGasMeter(storetypes.NewInfiniteGasMeter())
					require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, target, common.Hash{}))
					require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, target, common.HexToHash("0x2")))
					response, err := keeper.TraceTx(ctx, req)
					require.ErrorContains(t, err, "EVM execution rejected")
					require.ErrorContains(t, err, tc.reject)
					require.Nil(t, response)
					t.Logf("delivery and trace reject: %v; responses=nil; EVM and hook state unchanged", err)
					return
				}
				require.NoError(t, err)
				require.True(t, delivery.Failed())
				require.Equal(t, vm.ErrExecutionReverted.Error(), delivery.VmError)
				require.Equal(t, []byte{0xde, 0xad}, delivery.Ret)
				require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, target, common.Hash{}))
				require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, target, common.HexToHash("0x2")))
				response, err := keeper.TraceTx(ctx, req)
				require.NoError(t, err)
				var result map[string]any
				require.NoError(t, json.Unmarshal(response.Data, &result))
				require.Equal(t, delivery.VmError, result["error"])
				traceGas, err := hexutil.DecodeUint64(result["gasUsed"].(string))
				require.NoError(t, err)
				require.Equal(t, delivery.GasUsed, traceGas)
				require.Equal(t, "0xdead", result["output"])
				require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
				t.Logf("hook domain failure: VmError=%q Ret=%s GasUsed=%d TraceTx=%s; state unchanged", delivery.VmError, hexutil.Encode(delivery.Ret), delivery.GasUsed, response.Data)
			})
		}
		failure, hookFailure = nil, nil
	})
	t.Run("child failure and later sibling match delivery", func(t *testing.T) {
		failure, hookFailure = nil, nil
		child := common.HexToAddress("0x4567")
		later := common.HexToAddress("0x4568")
		state := nw.GetStateDB()
		state.SetCode(child, common.FromHex("0x60006000fd"), tracing.CodeChangeUnspecified)
		state.SetCode(later, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
		for _, caught := range []bool{true, false} {
			name := "uncaught"
			code := "0x6000600060006000600061456761fffff150600160005360016000fd"
			if caught {
				name = "caught"
				code = "0x6000600060006000600061456761fffff1506000600060006000600061456861fffff15000"
			}
			t.Run(name, func(t *testing.T) {
				state := nw.GetStateDB()
				state.SetCode(target, common.FromHex(code), tracing.CodeChangeUnspecified)
				require.NoError(t, state.Commit())
				deliveryCtx, _ := ctx.CacheContext()
				delivery, err := keeper.ApplyTransaction(deliveryCtx, tx)
				require.NoError(t, err)
				require.Equal(t, !caught, delivery.Failed())
				req.TraceConfig = &vmtypes.TraceConfig{Tracer: "callTracer"}
				response, err := keeper.TraceTx(ctx, req)
				require.NoError(t, err)
				var result map[string]any
				require.NoError(t, json.Unmarshal(response.Data, &result))
				calls := result["calls"].([]any)
				require.Equal(t, "execution reverted", calls[0].(map[string]any)["error"])
				if caught {
					require.NotContains(t, result, "error")
					require.Len(t, calls, 2)
					require.NotContains(t, calls[1].(map[string]any), "error")
					require.Equal(t, common.HexToHash("0x1"), keeper.GetState(deliveryCtx, later, common.Hash{}))
				} else {
					require.Equal(t, delivery.VmError, result["error"])
					require.Equal(t, "0x"+common.Bytes2Hex(delivery.Ret), result["output"])
					require.Len(t, calls, 1)
					require.Equal(t, common.Hash{}, keeper.GetState(deliveryCtx, later, common.Hash{}))
				}
				require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(later, common.Hash{}))
			})
		}
		state = nw.GetStateDB()
		state.SetCode(target, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
	})
	t.Run("success", func(t *testing.T) {
		failure = nil
		endings = nil
		req.TraceConfig = &vmtypes.TraceConfig{}
		response, err := keeper.TraceTx(ctx, req)
		require.NoError(t, err)
		var result map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &result))
		require.Equal(t, false, result["failed"])
		require.Len(t, endings, 1)
		require.NoError(t, endings[0])
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
	})
	t.Run("invalid predecessor is skipped", func(t *testing.T) {
		invalid, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 1, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
		require.NoError(t, err)
		predecessor := &vmtypes.MsgEthereumTx{}
		predecessor.FromEthereumTx(invalid)
		req.Predecessors = []*vmtypes.MsgEthereumTx{predecessor}
		defer func() { req.Predecessors = nil }()
		failure, infos, observed, endings = nil, nil, nil, nil
		response, err := keeper.TraceTx(ctx, req)
		require.NoError(t, err)
		var result map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &result))
		require.Equal(t, false, result["failed"])
		require.Len(t, infos, 2)
		require.Len(t, endings, 2)
		require.ErrorIs(t, endings[0], core.ErrIntrinsicGas)
		require.NoError(t, endings[1])
		require.Equal(t, []common.Hash{{}, {}}, observed)
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
		failure = errors.New("observer failure")
		_, err = keeper.TraceTx(ctx, req)
		require.ErrorContains(t, err, "observer failure")
	})
	t.Run("trace call remains read only", func(t *testing.T) {
		failure, infos = nil, nil
		args, err := json.Marshal(map[string]string{"to": target.Hex(), "gas": "0x30d40"})
		require.NoError(t, err)
		call := &vmtypes.QueryTraceCallRequest{Args: args, GasCap: 200000, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime()}
		_, err = keeper.TraceCall(ctx, call)
		require.NoError(t, err)
		require.Len(t, infos, 1)
		require.False(t, infos[0].Commit)
		failure = traceDenial{}
		response, err := keeper.TraceCall(ctx, call)
		require.NoError(t, err)
		t.Logf("TraceCall=%s", response.Data)
		var result map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &result))
		require.Equal(t, true, result["failed"])
		require.Equal(t, "0xdead", result["returnValue"])
		require.Len(t, infos, 2)
		require.False(t, infos[1].Commit)
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.HexToHash("0x2")))
		t.Logf("TraceCall Commit=false; EVM and hook storage unchanged")
	})
	t.Run("call and estimate remain read only", func(t *testing.T) {
		failure, infos = nil, nil
		args, err := json.Marshal(map[string]string{"to": target.Hex(), "gas": "0x30d40"})
		require.NoError(t, err)
		request := &vmtypes.EthCallRequest{Args: args, GasCap: 200000}
		_, err = keeper.EthCall(ctx, request)
		require.NoError(t, err)
		_, err = keeper.EstimateGas(ctx, request)
		require.NoError(t, err)
		require.NotEmpty(t, infos)
		for _, info := range infos {
			require.False(t, info.Commit)
		}
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.HexToHash("0x2")))
	})
	t.Run("estimate domain failure retains revert data without persisting state", func(t *testing.T) {
		failure, infos = traceDenial{}, nil
		defer func() { failure = nil }()
		args, err := json.Marshal(map[string]string{"to": target.Hex(), "gas": "0x30d40"})
		require.NoError(t, err)

		response, err := keeper.EstimateGas(ctx, &vmtypes.EthCallRequest{Args: args, GasCap: 200000})

		require.NoError(t, err)
		require.NotNil(t, response)
		evmState := nw.GetStateDB().GetState(target, common.Hash{})
		hookState := nw.GetStateDB().GetState(target, common.HexToHash("0x2"))
		t.Logf("EstimateGas: err=%v VmError=%q Ret=%s Gas=%d EVM storage=%s hook storage=%s", err, response.VmError, hexutil.Encode(response.Ret), response.Gas, evmState, hookState)
		require.Equal(t, vm.ErrExecutionReverted.Error(), response.VmError)
		require.Equal(t, []byte{0xde, 0xad}, response.Ret)
		require.NotEmpty(t, infos)
		for _, info := range infos {
			require.False(t, info.Commit)
		}
		require.Equal(t, common.Hash{}, evmState)
		require.Equal(t, common.Hash{}, hookState)
	})
	t.Run("final tracer gas includes hook and hook out of gas", func(t *testing.T) {
		failure = nil
		req.TraceConfig = &vmtypes.TraceConfig{}
		hookGas = 0
		response, err := keeper.TraceTx(ctx, req)
		require.NoError(t, err)
		var before map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &before))
		hookGas = 80000
		response, err = keeper.TraceTx(ctx, req)
		require.NoError(t, err)
		var after map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &after))
		require.Greater(t, after["gas"].(float64), before["gas"].(float64))
		hookGas = 200000
		response, err = keeper.TraceTx(ctx, req)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &out))
		require.Equal(t, true, out["failed"])
		require.Equal(t, float64(200000), out["gas"])
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
		hookGas = 0
	})
	t.Run("preceding and block denial rollback", func(t *testing.T) {
		failure = traceDenial{}
		observed, infos = nil, nil
		req.Predecessors = []*vmtypes.MsgEthereumTx{msg}
		_, err := keeper.TraceTx(ctx, req)
		require.NoError(t, err)
		require.Equal(t, []common.Hash{{}, {}}, observed)
		require.Len(t, infos, 2)
		require.True(t, infos[0].Commit)
		require.True(t, infos[1].Commit)
		req.Predecessors = nil
		observed, infos = nil, nil
		block, err := keeper.TraceBlock(ctx, &vmtypes.QueryTraceBlockRequest{Txs: []*vmtypes.MsgEthereumTx{msg, msg}, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000})
		require.NoError(t, err)
		var results []struct {
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		}
		require.NoError(t, json.Unmarshal(block.Data, &results))
		require.Len(t, results, 2)
		for _, result := range results {
			require.Empty(t, result.Error)
			require.Contains(t, string(result.Result), `"failed":true`)
		}
		require.Equal(t, []common.Hash{{}, {}}, observed)
		require.True(t, infos[0].Commit)
		require.True(t, infos[1].Commit)
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
		failure = nil
		observed, hookObserved = nil, nil
		_, err = keeper.TraceBlock(ctx, &vmtypes.QueryTraceBlockRequest{Txs: []*vmtypes.MsgEthereumTx{msg, msg}, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000})
		require.NoError(t, err)
		require.Equal(t, []common.Hash{{}, common.HexToHash("0x1")}, observed)
		require.Equal(t, []common.Hash{{}, common.HexToHash("0x7")}, hookObserved)
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.HexToHash("0x2")))
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
	})
	t.Run("failed first transaction does not fail a later transaction", func(t *testing.T) {
		failure, hookFailure, firstHookFailure = nil, nil, traceDenial{}
		defer func() { firstHookFailure = nil; req.Predecessors = nil }()
		observed = nil
		req.TraceConfig = &vmtypes.TraceConfig{}
		req.Predecessors = []*vmtypes.MsgEthereumTx{msg}
		response, err := keeper.TraceTx(ctx, req)
		require.NoError(t, err)
		var targetResult map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &targetResult))
		require.Equal(t, false, targetResult["failed"])
		require.Equal(t, []common.Hash{{}, {}}, observed)
		observed = nil
		block, err := keeper.TraceBlock(ctx, &vmtypes.QueryTraceBlockRequest{Txs: []*vmtypes.MsgEthereumTx{msg, msg}, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000})
		require.NoError(t, err)
		var results []struct {
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		}
		require.NoError(t, json.Unmarshal(block.Data, &results))
		require.Len(t, results, 2)
		for i, expected := range []bool{true, false} {
			require.Empty(t, results[i].Error)
			var entry map[string]any
			require.NoError(t, json.Unmarshal(results[i].Result, &entry))
			require.Equal(t, expected, entry["failed"])
			if i == 0 {
				require.Equal(t, "0xdead", entry["returnValue"])
			}
		}
		require.Equal(t, []common.Hash{{}, {}}, observed)
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
	})
	t.Run("domain failure overrides existing VM revert", func(t *testing.T) {
		state := nw.GetStateDB()
		state.SetCode(target, common.FromHex("0x601260005360016000fd"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
		failure = traceDenial{}
		endings = nil
		req.TraceConfig = &vmtypes.TraceConfig{Tracer: "callTracer"}
		response, err := keeper.TraceTx(ctx, req)
		require.NoError(t, err)
		var result map[string]any
		require.NoError(t, json.Unmarshal(response.Data, &result))
		require.Equal(t, "execution reverted", result["error"])
		require.Equal(t, "0xdead", result["output"])
		require.Len(t, endings, 1)
		require.ErrorIs(t, endings[0], vm.ErrExecutionReverted)
	})
	t.Run("read-only reverted root classifies domain and observer errors", func(t *testing.T) {
		state := nw.GetStateDB()
		state.SetCode(target, common.FromHex("0x601260005360016000fd"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
		args, err := json.Marshal(map[string]string{"to": target.Hex(), "gas": "0x30d40"})
		require.NoError(t, err)
		failure = traceDenial{}
		semantic, err := keeper.EthCall(ctx, &vmtypes.EthCallRequest{Args: args, GasCap: 200000})
		require.NoError(t, err)
		require.True(t, semantic.Failed())
		require.Equal(t, "execution reverted", semantic.VmError)
		require.Equal(t, []byte{0xde, 0xad}, semantic.Ret)
		require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}))
		t.Logf("EthCall domain over VM revert: VmError=%q Ret=%s; state unchanged", semantic.VmError, hexutil.Encode(semantic.Ret))
		failure = encodedObserver{cause: encodedInfrastructure{}}
		response, err := keeper.EthCall(ctx, &vmtypes.EthCallRequest{Args: args, GasCap: 200000})
		require.ErrorContains(t, err, "storage unavailable")
		require.Nil(t, response)
		trace, err := keeper.TraceCall(ctx, &vmtypes.QueryTraceCallRequest{Args: args, GasCap: 200000, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), TraceConfig: &vmtypes.TraceConfig{Tracer: "callTracer"}})
		require.ErrorContains(t, err, "storage unavailable")
		require.Nil(t, trace)
		t.Logf("read-only observer reject: %v; EthCall and TraceCall responses=nil", err)
		failure = errors.Join(traceDenial{}, errors.New("independent observer failure"))
		_, err = keeper.EthCall(ctx, &vmtypes.EthCallRequest{Args: args, GasCap: 200000})
		require.ErrorContains(t, err, "independent observer failure")
		_, err = keeper.TraceCall(ctx, &vmtypes.QueryTraceCallRequest{Args: args, GasCap: 200000, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime()})
		require.ErrorContains(t, err, "independent observer failure")
		failure = nil
	})
	t.Run("read-only semantic denial materializes domain outcome", func(t *testing.T) {
		state := nw.GetStateDB()
		state.SetCode(target, common.FromHex("0x5b600056"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
		failure = traceDenial{}
		args, err := json.Marshal(map[string]string{"to": target.Hex(), "gas": "0x30d40"})
		require.NoError(t, err)
		response, err := keeper.EthCall(ctx, &vmtypes.EthCallRequest{Args: args, GasCap: 200000})
		require.NoError(t, err)
		require.True(t, response.Failed())
		require.Equal(t, vm.ErrExecutionReverted.Error(), response.VmError)
		require.Equal(t, []byte{0xde, 0xad}, response.Ret)
		require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}))
		t.Logf("EthCall domain after nonrevert code: VmError=%q Ret=%s; state unchanged", response.VmError, hexutil.Encode(response.Ret))
		failure = nil
	})
	t.Run("committing internal call rejects failed outcome and consumes full meter", func(t *testing.T) {
		state := nw.GetStateDB()
		state.SetCode(target, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
		sender := crypto.PubkeyToAddress(key.PublicKey)
		require.NoError(t, nw.FundAccount(sdk.AccAddress(sender.Bytes()), sdk.NewCoins(sdk.NewCoin(nw.GetBaseDenom(), sdkmath.NewInt(1)))))
		failure = traceDenial{}
		callCtx := ctx.WithGasMeter(storetypes.NewGasMeter(10000000))
		stateDB := statedb.New(callCtx, keeper, statedb.NewEmptyTxConfig())
		response, err := keeper.CallEVMWithData(callCtx, stateDB, sender, &target, nil, true, false, big.NewInt(200000))
		require.ErrorIs(t, err, vmtypes.ErrVMExecution)
		require.ErrorContains(t, err, "execution reverted")
		require.NotNil(t, response)
		require.True(t, response.Failed())
		require.Equal(t, "execution reverted", response.VmError)
		require.Equal(t, []byte{0xde, 0xad}, response.Ret)
		require.Positive(t, response.GasUsed)
		require.Equal(t, callCtx.GasMeter().Limit(), callCtx.GasMeter().GasConsumed())
		require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}))
		t.Logf("CallEVMWithData: err=%v VmError=%q Ret=%s meter=%d/%d; SSTORE discarded", err, response.VmError, hexutil.Encode(response.Ret), callCtx.GasMeter().GasConsumed(), callCtx.GasMeter().Limit())
		failure = nil
	})
	t.Run("VM error takes priority over later Go error at tx end", func(t *testing.T) {
		state := nw.GetStateDB()
		state.SetCode(target, common.FromHex("0x601260005360016000fd"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
		cfg, err := keeper.EVMConfig(ctx, ctx.BlockHeader().ProposerAddress)
		require.NoError(t, err)
		cfg.FeeMarketParams.MinGasMultiplier = sdkmath.LegacyNewDec(2)
		message, err := core.TransactionToMessage(tx, signer, cfg.BaseFee)
		require.NoError(t, err)
		var ends []error
		hooks := &tracing.Hooks{OnTxEnd: func(_ *ethtypes.Receipt, err error) { ends = append(ends, err) }}
		_, err = keeper.ApplyMessageWithConfig(ctx, nw.GetStateDB(), *message, hooks, false, false, cfg, statedb.NewEmptyTxConfig(), false, nil)
		require.Error(t, err)
		require.Len(t, ends, 1)
		require.ErrorIs(t, ends[0], vm.ErrExecutionReverted)
	})
	t.Run("direct execution keeps VM result and enforcement cause", func(t *testing.T) {
		state := nw.GetStateDB()
		state.SetCode(target, common.FromHex("0x601260005360016000fd"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
		cfg, err := keeper.EVMConfig(ctx, ctx.BlockHeader().ProposerAddress)
		require.NoError(t, err)
		message, err := core.TransactionToMessage(tx, signer, cfg.BaseFee)
		require.NoError(t, err)
		manager := vmtracer.New(nil)
		execCtx := vmtracer.WithManager(ctx, manager)
		txConfig := statedb.NewEmptyTxConfig()
		execCtx, raw := vmglobal.TxTraceFactory(execCtx, vmtracer.ExecutionInfo{Message: *message, Commit: true})
		manager.InstallGlobal(&finalizingTrace{Collector: raw.(vmglobal.Collector), failure: encodedObserver{cause: encodedInfrastructure{}}})
		stateDB := statedb.New(execCtx, keeper, txConfig)
		response, err := keeper.ApplyMessageWithConfig(execCtx, stateDB, *message, manager.Hooks(), true, false, cfg, txConfig, false, nil)
		require.ErrorContains(t, err, "storage unavailable")
		require.NotNil(t, response)
		require.Equal(t, "execution reverted", response.VmError)
		require.Equal(t, []byte{0x12}, response.Ret)
		require.Positive(t, response.GasUsed)
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
	})
	t.Run("encoded observer failure survives invalid predecessor", func(t *testing.T) {
		req.TraceConfig = &vmtypes.TraceConfig{Tracer: "callTracer"}
		state := nw.GetStateDB()
		state.SetCode(target, common.FromHex("0x601260005360016000fd"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
		invalid, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 1, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
		require.NoError(t, err)
		predecessor := &vmtypes.MsgEthereumTx{}
		predecessor.FromEthereumTx(invalid)
		req.Predecessors = []*vmtypes.MsgEthereumTx{predecessor}
		for _, cause := range []error{encodedInfrastructure{}, errors.Join(traceDenial{}, encodedInfrastructure{})} {
			failure = encodedObserver{cause: cause}
			_, err := keeper.TraceTx(ctx, req)
			require.ErrorContains(t, err, "storage unavailable")
		}
		req.Predecessors, failure = nil, nil
	})
	t.Run("delivery remains committing", func(t *testing.T) {
		state := nw.GetStateDB()
		state.SetCode(target, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
		require.NoError(t, state.Commit())
		failure, infos = nil, nil
		response, err := keeper.ApplyTransaction(ctx, tx)
		require.NoError(t, err)
		require.False(t, response.Failed())
		require.Len(t, infos, 1)
		require.True(t, infos[0].Commit)
		require.Equal(t, common.HexToHash("0x1"), nw.GetStateDB().GetState(target, common.Hash{}))
		state = nw.GetStateDB()
		state.SetState(target, common.Hash{}, common.Hash{})
		require.NoError(t, state.Commit())
		failure = traceDenial{}
		response, err = keeper.ApplyTransaction(ctx, tx)
		require.NoError(t, err)
		require.True(t, response.Failed())
		require.Equal(t, []byte{0xde, 0xad}, response.Ret)
		require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}))
	})
}

func TestDeliveryWithoutHooksClassifiesDomainFailures(t *testing.T) {
	nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	})
	ctx := nw.GetContext()
	keeper := nw.App.GetEVMKeeper()
	require.False(t, keeper.HasHooks())
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	target := common.HexToAddress("0x4567")
	state := nw.GetStateDB()
	state.SetCode(target, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
	require.NoError(t, state.Commit())
	blockTime, err := utils.SafeUint64(ctx.BlockTime().Unix())
	require.NoError(t, err)
	signer := ethtypes.MakeSigner(vmtypes.GetEthChainConfig(), big.NewInt(ctx.BlockHeight()), blockTime)
	tx, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 200000, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
	require.NoError(t, err)
	var failure error
	keeper.SetGlobalTracerFactories(func(ctx sdk.Context, info vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		ctx, tr := vmglobal.TxTraceFactory(ctx, info)
		return ctx, &finalizingTrace{Collector: tr.(vmglobal.Collector), failure: failure}
	})
	baselineCtx, _ := ctx.CacheContext()
	baselineCtx = baselineCtx.WithGasMeter(storetypes.NewGasMeter(200000))
	baseline, err := keeper.ApplyTransaction(baselineCtx, tx)
	require.NoError(t, err)
	require.False(t, baseline.Failed())
	require.Equal(t, common.HexToHash("0x1"), keeper.GetState(baselineCtx, target, common.Hash{}))
	for _, tc := range []struct {
		name   string
		cause  error
		reject bool
	}{
		{"observer rejects", encodedObserver{cause: encodedInfrastructure{}}, true},
		{"domain fails without committing", traceDenial{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure = tc.cause
			actualCtx, _ := ctx.CacheContext()
			actualCtx = actualCtx.WithGasMeter(storetypes.NewGasMeter(200000))
			actual, err := keeper.ApplyTransaction(actualCtx, tx)
			if tc.reject {
				require.ErrorContains(t, err, "EVM execution rejected")
				require.ErrorContains(t, err, "storage unavailable")
				require.Nil(t, actual)
				require.Equal(t, actualCtx.GasMeter().Limit(), actualCtx.GasMeter().GasConsumed())
				t.Logf("no-hook observer: err=%v response=nil meter=%d", err, actualCtx.GasMeter().GasConsumed())
			} else {
				require.NoError(t, err)
				require.True(t, actual.Failed())
				require.Equal(t, vm.ErrExecutionReverted.Error(), actual.VmError)
				require.Equal(t, []byte{0xde, 0xad}, actual.Ret)
				require.Positive(t, actual.GasUsed)
				require.Equal(t, actual.GasUsed, actualCtx.GasMeter().GasConsumed())
				require.Less(t, actualCtx.GasMeter().GasConsumed(), actualCtx.GasMeter().Limit())
				t.Logf("no-hook domain: VmError=%q Ret=%s GasUsed=%d meter=%d/%d", actual.VmError, hexutil.Encode(actual.Ret), actual.GasUsed, actualCtx.GasMeter().GasConsumed(), actualCtx.GasMeter().Limit())
			}
			actualCtx = actualCtx.WithGasMeter(storetypes.NewInfiniteGasMeter())
			require.Equal(t, common.Hash{}, keeper.GetState(actualCtx, target, common.Hash{}))
			require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}))
			t.Log("SSTORE discarded in delivery cache and parent")
		})
	}
}

type unmarkedDomainError struct{}

func (unmarkedDomainError) Error() string      { return "unmarked domain rejection" }
func (unmarkedDomainError) RevertData() []byte { return []byte{0xde, 0xad} }

func TestUnmarkedDomainCauseIsFailedOutcome(t *testing.T) {
	nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	})
	ctx := nw.GetContext()
	keeper := nw.App.GetEVMKeeper()
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	target := common.HexToAddress("0x4567")
	state := nw.GetStateDB()
	state.SetCode(target, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
	require.NoError(t, state.Commit())
	blockTime, err := utils.SafeUint64(ctx.BlockTime().Unix())
	require.NoError(t, err)
	signer := ethtypes.MakeSigner(vmtypes.GetEthChainConfig(), big.NewInt(ctx.BlockHeight()), blockTime)
	tx, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 200000, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
	require.NoError(t, err)

	keeper.SetGlobalTracerFactories(func(ctx sdk.Context, info vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		ctx, tr := vmglobal.TxTraceFactory(ctx, info)
		return ctx, &finalizingTrace{Collector: tr.(vmglobal.Collector), failure: unmarkedDomainError{}}
	})

	response, err := keeper.ApplyTransaction(ctx, tx)
	require.NoError(t, err, "expected unmarked domain error with revert data to yield failed receipt, not Go error")
	require.NotNil(t, response)
	require.True(t, response.Failed())
	require.Equal(t, "execution reverted", response.VmError)
	require.Equal(t, []byte{0xde, 0xad}, response.Ret)
	require.Equal(t, common.Hash{}, nw.GetStateDB().GetState(target, common.Hash{}), "state changes must not be committed on domain failure")
}

func TestUnmarkedDomainCauseTraceMatchesDelivery(t *testing.T) {
	nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	})
	ctx := nw.GetContext()
	keeper := nw.App.GetEVMKeeper()
	require.False(t, keeper.HasHooks())
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	target := common.HexToAddress("0x4567")
	initial := common.HexToHash("0x3")
	blockTime, err := utils.SafeUint64(ctx.BlockTime().Unix())
	require.NoError(t, err)
	signer := ethtypes.MakeSigner(vmtypes.GetEthChainConfig(), big.NewInt(ctx.BlockHeight()), blockTime)
	tx, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 200000, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
	require.NoError(t, err)
	msg := &vmtypes.MsgEthereumTx{}
	msg.FromEthereumTx(tx)
	args, err := json.Marshal(map[string]string{"to": target.Hex(), "gas": "0x30d40"})
	require.NoError(t, err)
	var failure error
	var infos []vmtracer.ExecutionInfo
	keeper.SetGlobalTracerFactories(func(ctx sdk.Context, info vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		infos = append(infos, info)
		ctx, tr := vmglobal.TxTraceFactory(ctx, info)
		return ctx, &finalizingTrace{Collector: tr.(vmglobal.Collector), failure: failure}
	})

	for _, tc := range []struct {
		name    string
		code    string
		cause   error
		vmError string
		ret     []byte
	}{
		{"domain after write", "0x600160005500", unmarkedDomainError{}, vm.ErrExecutionReverted.Error(), []byte{0xde, 0xad}},
		{"domain overrides VM revert", "0x6001600055601260005360016000fd", unmarkedDomainError{}, vm.ErrExecutionReverted.Error(), []byte{0xde, 0xad}},
		{"VM revert control", "0x6001600055601260005360016000fd", nil, vm.ErrExecutionReverted.Error(), []byte{0x12}},
		{"VM fault control", "0x6001600055fe", nil, "invalid opcode: INVALID", nil},
	} {
		state := nw.GetStateDB()
		state.SetCode(target, common.FromHex(tc.code), tracing.CodeChangeUnspecified)
		state.SetState(target, common.Hash{}, initial)
		require.NoError(t, state.Commit())
		failure = tc.cause
		deliveryCtx, _ := ctx.CacheContext()
		delivery, err := keeper.ApplyTransaction(deliveryCtx, tx)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.vmError, delivery.VmError, tc.name)
		require.Equal(t, tc.ret, delivery.Ret, tc.name)
		require.Equal(t, initial, keeper.GetState(deliveryCtx, target, common.Hash{}), tc.name)
		t.Logf("%s delivery: VmError=%q Ret=%s GasUsed=%d state=%s", tc.name, delivery.VmError, hexutil.Encode(delivery.Ret), delivery.GasUsed, keeper.GetState(deliveryCtx, target, common.Hash{}))

		for _, tracerName := range []string{"", "callTracer", "callTracerLegacy"} {
			config := &vmtypes.TraceConfig{Tracer: tracerName}
			infos = nil
			trace, err := keeper.TraceTx(ctx, &vmtypes.QueryTraceTxRequest{
				Msg: msg, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000, TraceConfig: config,
			})
			require.NoError(t, err, tc.name, tracerName)
			t.Logf("%s %q TraceTx=%s", tc.name, tracerName, trace.Data)
			var result map[string]any
			require.NoError(t, json.Unmarshal(trace.Data, &result))
			if tracerName == "" {
				require.Equal(t, true, result["failed"], tc.name)
				require.Equal(t, hexutil.Encode(delivery.Ret), result["returnValue"], tc.name)
				require.Equal(t, float64(delivery.GasUsed), result["gas"], tc.name)
			} else {
				require.Equal(t, delivery.VmError, result["error"], tc.name)
				if len(delivery.Ret) == 0 {
					require.NotContains(t, result, "output", tc.name)
				} else {
					require.Equal(t, hexutil.Encode(delivery.Ret), result["output"], tc.name)
				}
				require.Equal(t, hexutil.EncodeUint64(delivery.GasUsed), result["gasUsed"], tc.name)
			}
			require.Len(t, infos, 1)
			require.True(t, infos[0].Commit)
			require.Equal(t, initial, keeper.GetState(ctx, target, common.Hash{}), tc.name)

			call, err := keeper.TraceCall(ctx, &vmtypes.QueryTraceCallRequest{
				Args: args, GasCap: 200000, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), TraceConfig: config,
			})
			require.NoError(t, err, tc.name, tracerName)
			t.Logf("%s %q TraceCall=%s", tc.name, tracerName, call.Data)
			var callResult map[string]any
			require.NoError(t, json.Unmarshal(call.Data, &callResult))
			if tracerName == "" {
				require.Equal(t, true, callResult["failed"], tc.name)
				require.Equal(t, hexutil.Encode(tc.ret), callResult["returnValue"], tc.name)
			} else {
				require.Equal(t, tc.vmError, callResult["error"], tc.name)
				if len(tc.ret) == 0 {
					require.NotContains(t, callResult, "output", tc.name)
				} else {
					require.Equal(t, hexutil.Encode(tc.ret), callResult["output"], tc.name)
				}
			}
			require.Len(t, infos, 2)
			require.False(t, infos[1].Commit)
			require.Equal(t, initial, keeper.GetState(ctx, target, common.Hash{}), tc.name)
			t.Logf("%s %q: TraceTx Commit=true; TraceCall Commit=false; storage remains %s", tc.name, tracerName, initial)
		}
	}
}
