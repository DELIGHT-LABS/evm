package integration

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm"
	"github.com/cosmos/evm/testutil/integration/evm/network"
	"github.com/cosmos/evm/x/vm/statedb"
	vmtracer "github.com/cosmos/evm/x/vm/tracer"
	vmglobal "github.com/cosmos/evm/x/vm/tracer/global"
	vmtypes "github.com/cosmos/evm/x/vm/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

type domainLogHooks struct {
	receipt    *ethtypes.Receipt
	logsAtHook int
}

func (h *domainLogHooks) PostTxProcessing(_ sdk.Context, _ common.Address, _ core.Message, receipt *ethtypes.Receipt) error {
	h.receipt = receipt
	h.logsAtHook = len(receipt.Logs)
	return nil
}

func (h *domainLogHooks) EstimatePostTxProcessing(ctx sdk.Context, sender common.Address, msg core.Message, receipt *ethtypes.Receipt) error {
	return h.PostTxProcessing(ctx, sender, msg, receipt)
}

func TestDomainFailureDiscardsEVMLogs(t *testing.T) {
	for _, boundary := range []string{"transaction without hooks", "transaction with hooks", "direct message"} {
		t.Run(boundary, func(t *testing.T) {
			nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
				return CreateEvmd(chainID, evmChainID, opts...)
			})
			ctx := nw.GetContext()
			keeper := nw.App.GetEVMKeeper()
			hooks := &domainLogHooks{}
			if boundary == "transaction with hooks" {
				keeper.SetHooks(hooks)
			}
			require.Equal(t, boundary == "transaction with hooks", keeper.HasHooks())
			target := common.HexToAddress("0x4567")
			slot := common.Hash{}
			initial := common.HexToHash("0x3")
			state := nw.GetStateDB()
			// SSTORE(0, 1), MSTORE8(0, 0x12), LOG0(0, 1), LOG1(0, 1, 0xab), RETURN(0, 1).
			code := common.FromHex("0x6001600055601260005360016000a060ab60016000a160016000f3")
			state.SetCode(target, code, tracing.CodeChangeUnspecified)
			state.SetState(target, slot, initial)
			require.NoError(t, state.Commit())
			key, err := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000001")
			require.NoError(t, err)
			signer := ethtypes.LatestSignerForChainID(vmtypes.GetEthChainConfig().ChainID)
			tx, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 200000, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
			require.NoError(t, err)
			var controlGas, controlMaxGas uint64

			for _, tc := range []struct {
				name  string
				cause error
			}{
				{"success retains logs", nil},
				{"domain failure discards logs", unmarkedDomainError{}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					actualCtx, _ := ctx.CacheContext()
					actualCtx = actualCtx.WithGasMeter(storetypes.NewGasMeter(200000))
					var ends []error
					var collector vmglobal.Collector
					factory := func(ctx sdk.Context, info vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
						ctx, raw := vmglobal.TxTraceFactory(ctx, info)
						collector = raw.(vmglobal.Collector)
						return ctx, &finalizingTrace{
							Collector: collector,
							failure:   tc.cause,
							end:       func(err error) { ends = append(ends, err) },
						}
					}
					var response *vmtypes.MsgEthereumTxResponse
					if boundary == "direct message" {
						cfg, err := keeper.EVMConfig(actualCtx, actualCtx.BlockHeader().ProposerAddress)
						require.NoError(t, err)
						message, err := core.TransactionToMessage(tx, signer, cfg.BaseFee)
						require.NoError(t, err)
						manager := vmtracer.New(nil)
						actualCtx = vmtracer.WithManager(actualCtx, manager)
						execCtx, tr := factory(actualCtx, vmtracer.ExecutionInfo{Message: *message, Commit: true})
						manager.InstallGlobal(tr)
						txConfig := statedb.NewEmptyTxConfig()
						db := statedb.New(execCtx, keeper, txConfig)
						response, err = keeper.ApplyMessageWithConfig(execCtx, db, *message, manager.Hooks(), true, false, cfg, txConfig, false, nil)
						require.NoError(t, err)
					} else {
						keeper.SetGlobalTracerFactories(factory)
						response, err = keeper.ApplyTransaction(actualCtx, tx)
						require.NoError(t, err)
						require.NotNil(t, response)
						require.Equal(t, response.GasUsed, actualCtx.GasMeter().GasConsumed())
					}
					require.NotNil(t, response)
					require.Len(t, ends, 1)
					require.NoError(t, ends[0], "the EVM must succeed before the domain cause is latched")
					var emitted []*ethtypes.Log
					for _, observation := range collector.Observations() {
						if observation.Kind == vmtracer.Log {
							emitted = append(emitted, observation.Log)
						}
					}
					require.Len(t, emitted, 2)
					require.Equal(t, target, emitted[0].Address)
					require.Equal(t, target, emitted[1].Address)
					require.Equal(t, []byte{0x12}, emitted[0].Data)
					require.Equal(t, []byte{0x12}, emitted[1].Data)
					require.Empty(t, emitted[0].Topics)
					require.Equal(t, []common.Hash{common.HexToHash("0xab")}, emitted[1].Topics)
					actualCtx = actualCtx.WithGasMeter(storetypes.NewInfiniteGasMeter())
					actualState := keeper.GetState(actualCtx, target, slot)
					t.Logf("code=%x EVMEnd=%v emitted=%d VmError=%q Ret=%x GasUsed=%d MaxUsedGas=%d responseLogs=%d state=%s parent=%s",
						code, ends[0], len(emitted), response.VmError, response.Ret, response.GasUsed, response.MaxUsedGas, len(response.Logs), actualState, keeper.GetState(ctx, target, slot))
					if hooks.receipt != nil {
						t.Logf("actual posthook receipt: atHookLogs=%d returnedLogs=%d status=%d gas=%d bloom=%x",
							hooks.logsAtHook, len(hooks.receipt.Logs), hooks.receipt.Status, hooks.receipt.GasUsed, hooks.receipt.Bloom)
					}
					require.Equal(t, initial, keeper.GetState(ctx, target, slot))
					if tc.cause == nil {
						require.False(t, response.Failed())
						require.Equal(t, []byte{0x12}, response.Ret)
						require.Len(t, response.Logs, 2)
						for _, log := range response.Logs {
							require.Equal(t, target.Hex(), log.Address)
							require.Equal(t, []byte{0x12}, log.Data)
						}
						require.Empty(t, response.Logs[0].Topics)
						require.Equal(t, []string{common.HexToHash("0xab").Hex()}, response.Logs[1].Topics)
						require.Equal(t, common.HexToHash("0x1"), actualState)
						require.Positive(t, response.GasUsed)
						require.Less(t, response.GasUsed, tx.Gas())
						controlGas, controlMaxGas = response.GasUsed, response.MaxUsedGas
						if hooks.receipt != nil {
							require.Equal(t, 2, hooks.logsAtHook)
							require.Equal(t, vmtypes.LogsToEthereum(response.Logs), hooks.receipt.Logs)
							require.Equal(t, ethtypes.ReceiptStatusSuccessful, hooks.receipt.Status)
							require.NotEqual(t, ethtypes.Bloom{}, hooks.receipt.Bloom)
						}
					} else {
						require.True(t, response.Failed())
						require.Equal(t, vm.ErrExecutionReverted.Error(), response.VmError)
						require.Equal(t, []byte{0xde, 0xad}, response.Ret)
						require.Equal(t, controlGas, response.GasUsed)
						require.Equal(t, controlMaxGas, response.MaxUsedGas)
						require.Equal(t, initial, actualState)
						require.Empty(t, response.Logs, "domain failure must discard successfully executed LOG0 and LOG1")
						if hooks.receipt != nil {
							require.Empty(t, hooks.receipt.Logs)
							require.Equal(t, ethtypes.ReceiptStatusFailed, hooks.receipt.Status)
							require.Equal(t, ethtypes.Bloom{}, hooks.receipt.Bloom)
							require.Zero(t, hooks.logsAtHook, "posthooks must receive the failed outcome without EVM logs")
						}
					}
					if boundary == "transaction with hooks" {
						require.NotNil(t, hooks.receipt)
						require.Equal(t, response.GasUsed, hooks.receipt.GasUsed)
					}
				})
			}
		})
	}
}
