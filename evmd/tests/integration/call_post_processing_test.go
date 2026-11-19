package integration

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	cmtrpctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"

	"github.com/cosmos/evm"
	"github.com/cosmos/evm/rpc/backend"
	"github.com/cosmos/evm/rpc/backend/mocks"
	"github.com/cosmos/evm/rpc/namespaces/ethereum/debug"
	"github.com/cosmos/evm/rpc/namespaces/ethereum/eth"
	rpctypes "github.com/cosmos/evm/rpc/types"
	"github.com/cosmos/evm/testutil/integration/evm/network"
	vmkeeper "github.com/cosmos/evm/x/vm/keeper"
	vmtypes "github.com/cosmos/evm/x/vm/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/server"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// Only the transport is replaced: both methods execute the real keeper queries.
type callPostProcessingQueryClient struct {
	vmtypes.QueryClient
	ctx    sdk.Context
	keeper *vmkeeper.Keeper
}

func (c callPostProcessingQueryClient) EthCall(ctx context.Context, req *vmtypes.EthCallRequest, _ ...grpc.CallOption) (*vmtypes.MsgEthereumTxResponse, error) {
	return c.keeper.EthCall(c.ctx.WithContext(ctx), req)
}

func (c callPostProcessingQueryClient) TraceCall(ctx context.Context, req *vmtypes.QueryTraceCallRequest, _ ...grpc.CallOption) (*vmtypes.QueryTraceCallResponse, error) {
	return c.keeper.TraceCall(c.ctx.WithContext(ctx), req)
}

type callPostProcessingHooks struct {
	keeper      *vmkeeper.Keeper
	target      common.Address
	runOnRevert bool
	failure     error
	gas         uint64
	calls       []common.Hash
}

func (h *callPostProcessingHooks) PostTxProcessing(ctx sdk.Context, _ common.Address, _ core.Message, receipt *ethtypes.Receipt) error {
	if receipt.Status != ethtypes.ReceiptStatusSuccessful && !h.runOnRevert {
		h.calls = append(h.calls, common.Hash{})
		return nil
	}
	h.calls = append(h.calls, h.keeper.GetState(ctx, h.target, common.Hash{}))
	h.keeper.SetState(ctx, h.target, common.HexToHash("0x2"), common.HexToHash("0x7").Bytes())
	ctx.GasMeter().ConsumeGas(h.gas, "test post-processing")
	return h.failure
}

func (h *callPostProcessingHooks) EstimatePostTxProcessing(ctx sdk.Context, sender common.Address, msg core.Message, receipt *ethtypes.Receipt) error {
	return h.PostTxProcessing(ctx, sender, msg, receipt)
}

func TestCallRPCsPreservePostProcessingOutcome(t *testing.T) {
	nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	})
	ctx := nw.GetContext()
	keeper := nw.App.GetEVMKeeper()
	target := common.HexToAddress("0x1234")
	state := nw.GetStateDB()
	state.SetCode(target, common.FromHex("0x6001600055601260005360016000f3"), tracing.CodeChangeUnspecified)
	require.NoError(t, state.Commit())

	hooks := &callPostProcessingHooks{keeper: keeper, target: target}
	keeper.SetHooks(hooks)
	header := &tmtypes.Header{Height: ctx.BlockHeight(), Time: ctx.BlockTime()}
	comet := mocks.NewClient(t)
	comet.On("Header", mock.Anything, &header.Height).Return(&cmtrpctypes.ResultHeader{Header: header}, nil)
	serverCtx := server.NewDefaultContext()
	b := &backend.Backend{
		RPCClient:   comet,
		QueryClient: &rpctypes.QueryClient{QueryClient: callPostProcessingQueryClient{ctx: ctx, keeper: keeper}},
		Logger:      serverCtx.Logger,
		EvmChainID:  vmtypes.GetEthChainConfig().ChainID,
	}
	b.Cfg.JSONRPC.GasCap = 200000
	rpcServer := rpc.NewServer()
	t.Cleanup(rpcServer.Stop)
	require.NoError(t, rpcServer.RegisterName("eth", eth.NewPublicAPI(serverCtx.Logger, b)))
	require.NoError(t, rpcServer.RegisterName("debug", debug.NewAPI(serverCtx, b, false)))
	client := rpc.DialInProc(rpcServer)
	t.Cleanup(client.Close)

	height := hexutil.EncodeUint64(uint64(header.Height))
	for _, tc := range []struct {
		name          string
		failure       error
		overrideCode  string
		gas           hexutil.Uint64
		hookGas       uint64
		wantVMError   error
		wantOutput    string
		wantHookState common.Hash
	}{
		{
			name: "domain rejection", failure: traceDenial{}, wantVMError: vm.ErrExecutionReverted,
			wantOutput: "0xdead", wantHookState: common.HexToHash("0x1"),
		},
		{name: "Go error", failure: encodedInfrastructure{}, wantHookState: common.HexToHash("0x1")},
		{name: "success", wantOutput: "0x12", wantHookState: common.HexToHash("0x1")},
		{
			name: "VM revert with state override", overrideCode: "0x6001600055603460005360016000fd",
			wantVMError: vm.ErrExecutionReverted, wantOutput: "0x34",
		},
		{
			name: "VM out of gas", gas: 21001,
			wantVMError: vm.ErrOutOfGas, wantOutput: "0x",
		},
		{
			name: "hook out of gas", hookGas: 200000,
			wantVMError: vm.ErrOutOfGas, wantOutput: "0x", wantHookState: common.HexToHash("0x1"),
		},
	} {
		hooks.failure, hooks.gas, hooks.calls = tc.failure, tc.hookGas, nil
		args := vmtypes.TransactionArgs{To: &target}
		if tc.gas != 0 {
			args.Gas = &tc.gas
		}
		callParams := []any{args, height}
		traceConfig := map[string]any{"tracer": "callTracer"}
		if tc.overrideCode != "" {
			code := hexutil.Bytes(common.FromHex(tc.overrideCode))
			overrides := rpctypes.StateOverride{target: {Code: &code}}
			callParams = append(callParams, overrides)
			traceConfig["stateOverrides"] = overrides
		}
		t.Run(tc.name+"/eth_call", func(t *testing.T) {
			var output hexutil.Bytes
			err := client.Call(&output, "eth_call", callParams...)
			switch {
			case tc.wantVMError == vm.ErrExecutionReverted:
				var dataErr rpc.DataError
				require.ErrorAs(t, err, &dataErr)
				require.Equal(t, tc.wantOutput, dataErr.ErrorData())
			case tc.wantVMError != nil:
				require.ErrorContains(t, err, tc.wantVMError.Error())
			case tc.failure != nil:
				require.ErrorContains(t, err, tc.failure.Error())
			default:
				require.NoError(t, err)
				require.Equal(t, tc.wantOutput, output.String())
			}
		})
		t.Run(tc.name+"/debug_traceCall", func(t *testing.T) {
			var result struct {
				Error  string        `json:"error"`
				Output hexutil.Bytes `json:"output"`
			}
			err := client.Call(&result, "debug_traceCall", args, height, traceConfig)
			switch {
			case tc.wantVMError != nil:
				require.NoError(t, err)
				require.Equal(t, tc.wantVMError.Error(), result.Error)
				require.Equal(t, tc.wantOutput, result.Output.String())
			case tc.failure != nil:
				require.ErrorContains(t, err, tc.failure.Error())
			default:
				require.NoError(t, err)
				require.Empty(t, result.Error)
				require.Equal(t, tc.wantOutput, result.Output.String())
			}
		})
		require.Equal(t, []common.Hash{tc.wantHookState, tc.wantHookState}, hooks.calls, tc.name)
		require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}), tc.name)
		require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.HexToHash("0x2")), tc.name)
	}

	t.Run("prestate diff after EVM revert", func(t *testing.T) {
		slot := common.HexToHash("0x2")
		initial := common.HexToHash("0x3")
		keeper.SetState(ctx, target, slot, initial.Bytes())
		hooks.runOnRevert = true
		// Read the hook's slot, write a different slot, then revert.
		code := hexutil.Bytes(common.FromHex("0x60025450600160005560006000fd"))
		args := vmtypes.TransactionArgs{To: &target}
		config := map[string]any{
			"tracer":         "prestateTracer",
			"tracerConfig":   map[string]bool{"diffMode": true},
			"stateOverrides": rpctypes.StateOverride{target: {Code: &code}},
		}
		for _, tc := range []struct {
			name    string
			failure error
			hookGas uint64
		}{
			{name: "successful hook"},
			{name: "rejected hook", failure: traceDenial{}},
			{name: "out of gas hook", hookGas: 200000},
		} {
			t.Run(tc.name, func(t *testing.T) {
				hooks.failure, hooks.gas, hooks.calls = tc.failure, tc.hookGas, nil
				type account struct {
					Storage map[common.Hash]common.Hash
				}
				var diff struct {
					Pre  map[common.Address]account
					Post map[common.Address]account
				}
				require.NoError(t, client.Call(&diff, "debug_traceCall", args, height, config))
				require.Equal(t, []common.Hash{{}}, hooks.calls, "the hook must see the reverted EVM write rolled back")
				if tc.failure == nil && tc.hookGas == 0 {
					require.Equal(t, map[common.Hash]common.Hash{slot: initial}, diff.Pre[target].Storage)
					require.Equal(t, map[common.Hash]common.Hash{slot: common.HexToHash("0x7")}, diff.Post[target].Storage)
				} else {
					require.Empty(t, diff.Pre[target].Storage)
					require.Empty(t, diff.Post[target].Storage)
				}
				require.Equal(t, initial, keeper.GetState(ctx, target, slot))
				require.Equal(t, common.Hash{}, keeper.GetState(ctx, target, common.Hash{}))
			})
		}
	})
}
