package integration

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/cosmos-sdk/baseapp"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/evm"
	"github.com/cosmos/evm/testutil/integration/evm/network"
	"github.com/cosmos/evm/x/vm/statedb"
	vmtypes "github.com/cosmos/evm/x/vm/types"
)

func TestTracePostHookUnobservedState(t *testing.T) {
	nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	})
	keeper := nw.App.GetEVMKeeper()
	hooks := &traceResultHooks{}
	keeper.SetHooks(hooks)
	target, other := common.HexToAddress("0x1234"), common.HexToAddress("0x5678")
	slot := common.HexToHash("0x1")
	next := common.HexToHash("0x7")
	key, err := crypto.GenerateKey()
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		addr   common.Address
		create bool
		reject bool
		cache  bool
	}{
		{"new slot", target, false, false, false},
		{"unobserved account", other, false, false, false},
		{"created account", other, true, false, false},
		{"rejected slot write", target, false, true, false},
		{"rejected account creation", other, true, true, false},
		{"nested KV cache write", target, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := nw.GetContext().CacheContext()
			initial := common.Hash{}
			state := statedb.New(ctx, keeper, statedb.NewEmptyTxConfig())
			state.SetCode(target, []byte{0x00}, tracing.CodeChangeUnspecified)
			if !tc.create {
				initial = common.HexToHash("0x3")
				state.SetCode(tc.addr, []byte{0x00}, tracing.CodeChangeUnspecified)
				state.SetState(tc.addr, slot, initial)
			}
			require.NoError(t, state.Commit())
			hooks.run = func(hookCtx sdk.Context) error {
				if tc.create {
					state := statedb.New(hookCtx, keeper, statedb.NewEmptyTxConfig())
					state.SetCode(tc.addr, []byte{0x00}, tracing.CodeChangeUnspecified)
					state.SetNonce(tc.addr, 1, tracing.NonceChangeEoACall)
					state.SetState(tc.addr, slot, next)
					if err := state.Commit(); err != nil {
						return err
					}
				} else if tc.cache {
					cache := hookCtx.MultiStore().GetKVStore(nw.App.GetKey(vmtypes.StoreKey)).CacheWrap()
					cache.(storetypes.KVStore).Set(vmtypes.StateKey(tc.addr, slot.Bytes()), next.Bytes())
					cache.Write()
				} else {
					keeper.SetState(hookCtx, tc.addr, slot, next.Bytes())
				}
				if tc.reject {
					return traceDenial{}
				}
				return nil
			}
			signer := ethtypes.MakeSigner(vmtypes.GetEthChainConfig(), big.NewInt(ctx.BlockHeight()), uint64(ctx.BlockTime().Unix()))
			tx, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 200000, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
			require.NoError(t, err)
			deliveryCtx, _ := ctx.CacheContext()
			delivery, err := keeper.ApplyTransaction(deliveryCtx, tx)
			require.NoError(t, err)
			require.Equal(t, tc.reject, delivery.Failed())
			want := next
			if tc.reject {
				want = initial
			}
			require.Equal(t, want, keeper.GetState(deliveryCtx, tc.addr, slot))

			msg := &vmtypes.MsgEthereumTx{}
			msg.FromEthereumTx(tx)
			response, err := keeper.TraceTx(sdk.WrapSDKContext(ctx), &vmtypes.QueryTraceTxRequest{
				Msg: msg, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000,
				TraceConfig: &vmtypes.TraceConfig{Tracer: "prestateTracer", TracerJsonConfig: `{"diffMode":true}`},
			})
			require.NoError(t, err)
			type account struct {
				Storage map[common.Hash]common.Hash
				Code    hexutil.Bytes
				Nonce   uint64
			}
			var diff struct {
				Pre  map[common.Address]account
				Post map[common.Address]account
			}
			require.NoError(t, json.Unmarshal(response.Data, &diff))
			if tc.reject {
				require.NotContains(t, diff.Pre, tc.addr)
				require.NotContains(t, diff.Post, tc.addr)
			} else {
				require.Contains(t, diff.Post, tc.addr)
				require.Equal(t, next, diff.Post[tc.addr].Storage[slot])
				if tc.create {
					require.NotContains(t, diff.Pre, tc.addr)
					require.Equal(t, hexutil.Bytes{0x00}, diff.Post[tc.addr].Code)
					require.Equal(t, uint64(1), diff.Post[tc.addr].Nonce)
				} else {
					require.Equal(t, initial, diff.Pre[tc.addr].Storage[slot])
				}
			}
			require.Equal(t, initial, keeper.GetState(ctx, tc.addr, slot))
		})
	}
}
