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
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/evm"
	"github.com/cosmos/evm/testutil/integration/evm/network"
	"github.com/cosmos/evm/x/vm/statedb"
	vmtypes "github.com/cosmos/evm/x/vm/types"
)

func TestTracePostHookSDKAccountChanges(t *testing.T) {
	nw := network.NewUnitTestNetwork(func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	})
	keeper, bank, accounts := nw.App.GetEVMKeeper(), nw.App.GetBankKeeper(), nw.App.GetAccountKeeper()
	hooks := &traceResultHooks{}
	keeper.SetHooks(hooks)
	target, from, to := common.HexToAddress("0x1234"), common.HexToAddress("0x5678"), common.HexToAddress("0x9abc")
	key, err := crypto.GenerateKey()
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		transfer bool
		reject   bool
	}{
		{"bank transfer", true, false},
		{"account nonce", false, false},
		{"rejected transfer", true, true},
		{"rejected nonce", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := nw.GetContext().CacheContext()
			state := statedb.New(ctx, keeper, statedb.NewEmptyTxConfig())
			state.SetCode(target, []byte{0x00}, tracing.CodeChangeUnspecified)
			require.NoError(t, state.Commit())
			for _, addr := range []common.Address{from, to} {
				account := accounts.NewAccountWithAddress(ctx, sdk.AccAddress(addr.Bytes()))
				require.NoError(t, account.SetSequence(1))
				accounts.SetAccount(ctx, account)
			}
			coins := func(amount int64) sdk.Coins {
				return sdk.NewCoins(sdk.NewInt64Coin(nw.GetBaseDenom(), amount))
			}
			require.NoError(t, bank.MintCoins(ctx, vmtypes.ModuleName, coins(11)))
			require.NoError(t, bank.SendCoinsFromModuleToAccount(ctx, vmtypes.ModuleName, from.Bytes(), coins(9)))
			require.NoError(t, bank.SendCoinsFromModuleToAccount(ctx, vmtypes.ModuleName, to.Bytes(), coins(2)))
			hooks.run = func(hookCtx sdk.Context) error {
				if tc.transfer {
					if err := bank.SendCoins(hookCtx, from.Bytes(), to.Bytes(), coins(4)); err != nil {
						return err
					}
				} else {
					account := accounts.GetAccount(hookCtx, from.Bytes())
					if err := account.SetSequence(7); err != nil {
						return err
					}
					accounts.SetAccount(hookCtx, account)
				}
				if tc.reject {
					return traceDenial{}
				}
				return nil
			}
			signer := ethtypes.MakeSigner(vmtypes.GetEthChainConfig(), big.NewInt(ctx.BlockHeight()), uint64(ctx.BlockTime().Unix()))
			tx, err := ethtypes.SignNewTx(key, signer, &ethtypes.LegacyTx{To: &target, Gas: 200000, GasPrice: big.NewInt(0), Value: big.NewInt(0)})
			require.NoError(t, err)
			msg := &vmtypes.MsgEthereumTx{}
			msg.FromEthereumTx(tx)
			response, err := keeper.TraceTx(sdk.WrapSDKContext(ctx), &vmtypes.QueryTraceTxRequest{
				Msg: msg, BlockNumber: ctx.BlockHeight(), BlockTime: ctx.BlockTime(), BlockMaxGas: 30000000,
				TraceConfig: &vmtypes.TraceConfig{Tracer: "prestateTracer", TracerJsonConfig: `{"diffMode":true}`},
			})
			require.NoError(t, err)
			type account struct {
				Balance *hexutil.Big
				Nonce   uint64
			}
			var diff struct {
				Pre  map[common.Address]account
				Post map[common.Address]account
			}
			require.NoError(t, json.Unmarshal(response.Data, &diff))
			if tc.reject {
				require.NotContains(t, diff.Pre, from)
				require.NotContains(t, diff.Post, from)
				require.NotContains(t, diff.Pre, to)
				require.NotContains(t, diff.Post, to)
			} else if tc.transfer {
				require.Contains(t, diff.Pre, from)
				require.Contains(t, diff.Post, from)
				require.Contains(t, diff.Pre, to)
				require.Contains(t, diff.Post, to)
				require.Equal(t, uint64(9), diff.Pre[from].Balance.ToInt().Uint64())
				require.Equal(t, uint64(5), diff.Post[from].Balance.ToInt().Uint64())
				require.Equal(t, uint64(2), diff.Pre[to].Balance.ToInt().Uint64())
				require.Equal(t, uint64(6), diff.Post[to].Balance.ToInt().Uint64())
			} else {
				require.Equal(t, uint64(1), diff.Pre[from].Nonce)
				require.Equal(t, uint64(7), diff.Post[from].Nonce)
			}
			require.Equal(t, uint64(1), accounts.GetAccount(ctx, from.Bytes()).GetSequence())
			require.Equal(t, int64(9), bank.GetBalance(ctx, from.Bytes(), nw.GetBaseDenom()).Amount.Int64())
			require.Equal(t, int64(2), bank.GetBalance(ctx, to.Bytes(), nw.GetBaseDenom()).Amount.Int64())
		})
	}
}
