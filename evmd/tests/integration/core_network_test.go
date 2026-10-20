package integration

import (
	"slices"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm"
	"github.com/cosmos/evm/ante"
	"github.com/cosmos/evm/evmd"
	"github.com/cosmos/evm/testutil/integration/evm/grpc"
	"github.com/cosmos/evm/testutil/integration/evm/network"
	"github.com/cosmos/evm/testutil/integration/evm/utils"
	testtx "github.com/cosmos/evm/testutil/tx"
	transfertypes "github.com/cosmos/ibc-go/v11/modules/apps/transfer/types"

	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// coreOnlyApp exposes the keepers required by the shared network and no
// optional keeper providers, even though the embedded example app has them.
type coreOnlyApp struct{ evm.IntegrationNetworkApp }

type erc20OnlyApp struct {
	evm.IntegrationNetworkApp
	evm.Erc20KeeperProvider
}

func TestCoreNetworkWithoutOptionalQueryProviders(t *testing.T) {
	create := func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return coreOnlyApp{IntegrationNetworkApp: CreateEvmd(chainID, evmChainID, opts...)}
	}
	nw := network.NewUnitTestNetwork(create)
	ctx := nw.GetContext()
	_, providesERC20 := nw.CoreApp().(evm.Erc20KeeperProvider)
	require.False(t, providesERC20)
	_, providesIBC := nw.CoreApp().(evm.IBCKeeperProvider)
	require.False(t, providesIBC)
	_, providesFeegrant := nw.CoreApp().(evm.FeeGrantKeeperProvider)
	require.False(t, providesFeegrant)
	require.PanicsWithValue(t, "keeper adapter: app does not implement Erc20KeeperProvider", func() {
		nw.App.(evm.Erc20KeeperProvider).GetErc20Keeper()
	})

	require.Panics(t, func() {
		nw.GetERC20Client()
	})
	_, err := nw.GetMintClient().Params(ctx, &minttypes.QueryParamsRequest{})
	require.ErrorContains(t, err, "handler not found")

	receiver := nw.App.GetAccountKeeper().GetModuleAddress("evm")
	_, err = utils.RegisterEvmosERC20Coins(*nw, receiver)
	require.ErrorContains(t, err, "app does not provide an ERC20 keeper")
	_, err = utils.RegisterIBCERC20Coins(nw, receiver, transfertypes.Denom{})
	require.ErrorContains(t, err, "app does not provide an ERC20 keeper")

	options := ante.HandlerOptions{
		Cdc:               nw.App.AppCodec(),
		AccountKeeper:     nw.App.GetAccountKeeper(),
		BankKeeper:        nw.App.GetBankKeeper(),
		IBCKeeper:         nil,
		FeeMarketKeeper:   nw.App.GetFeeMarketKeeper(),
		EvmKeeper:         nw.App.GetEVMKeeper(),
		SignModeHandler:   nw.App.GetTxConfig().SignModeHandler(),
		SigGasConsumer:    ante.SigVerificationGasConsumer,
		PendingTxListener: func(common.Hash) {},
	}
	require.NoError(t, options.Validate())
}

func TestCoreNetworkWithoutTransferProvider(t *testing.T) {
	create := func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		app := CreateEvmd(chainID, evmChainID, opts...)
		return erc20OnlyApp{IntegrationNetworkApp: app, Erc20KeeperProvider: app}
	}
	nw := network.NewUnitTestNetwork(create)
	receiver := nw.App.GetAccountKeeper().GetModuleAddress("evm")
	_, err := utils.RegisterIBCERC20Coins(nw, receiver, transfertypes.Denom{})
	require.ErrorContains(t, err, "app does not provide a transfer keeper")
}

// Mint genesis/storage remains installed, but mint never runs a BeginBlock.
// The wrapper also prevents the helper from discovering a mint provider.
func createNetworkAppWithoutMint(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
	app := CreateEvmd(chainID, evmChainID, opts...).(*evmd.EVMD)
	app.ModuleManager.OrderBeginBlockers = slices.DeleteFunc(app.ModuleManager.OrderBeginBlockers, func(name string) bool { return name == minttypes.ModuleName })
	return coreOnlyApp{IntegrationNetworkApp: app}
}

func TestAccrualWithoutMint(t *testing.T) {
	for _, name := range []string{"zero target", "rewards", "commission"} {
		commission := name == "commission"
		t.Run(name, func(t *testing.T) {
			nw := network.NewUnitTestNetwork(createNetworkAppWithoutMint)
			_, providesMint := nw.CoreApp().(evm.MintKeeperProvider)
			require.False(t, providesMint)
			bank := nw.App.GetBankKeeper()
			height := nw.GetContext().BlockHeight()
			gh := grpc.NewIntegrationHandler(nw)
			denom := nw.GetBaseDenom()
			supplyBefore := bank.GetSupply(nw.GetContext(), denom).Amount
			if name == "zero target" {
				empty, err := utils.WaitToAccrueRewards(&nw.IntegrationNetwork, gh, "", nil)
				require.NoError(t, err)
				require.Empty(t, empty)
				empty, err = utils.WaitToAccrueCommission(&nw.IntegrationNetwork, gh, "", nil)
				require.NoError(t, err)
				require.Empty(t, empty)
				require.Equal(t, height, nw.GetContext().BlockHeight())
				require.True(t, supplyBefore.Equal(bank.GetSupply(nw.GetContext(), denom).Amount))
				return
			}
			require.NoError(t, nw.NextBlockAfter(time.Hour*24*7))
			require.True(t, supplyBefore.Equal(bank.GetSupply(nw.GetContext(), denom).Amount), "blocks must not mint inflation")
			valAddr, err := sdk.ValAddressFromBech32(nw.GetValidators()[0].OperatorAddress)
			require.NoError(t, err)
			delegator := sdk.AccAddress(valAddr)
			expected := sdk.NewDecCoins(sdk.NewDecCoin(denom, math.NewInt(100000)))
			if !commission {
				amount := math.NewInt(1e18)
				delegator, _ = testtx.NewAccAddressAndKey()
				require.NoError(t, nw.FundAccount(delegator, sdk.NewCoins(sdk.NewCoin(denom, amount))))
				_, err = nw.App.GetStakingKeeper().Delegate(nw.GetContext(), delegator, amount, stakingtypes.Unbonded, nw.GetValidators()[0], true)
				require.NoError(t, err)
			}
			supplyBefore = bank.GetSupply(nw.GetContext(), denom).Amount
			balanceBefore := bank.GetBalance(nw.GetContext(), delegator, denom).Amount
			height = nw.GetContext().BlockHeight()
			var accrued sdk.DecCoins
			if commission {
				accrued, err = utils.WaitToAccrueCommission(&nw.IntegrationNetwork, gh, valAddr.String(), expected)
			} else {
				accrued, err = utils.WaitToAccrueRewards(&nw.IntegrationNetwork, gh, delegator.String(), expected)
			}
			require.Equal(t, int64(1), nw.GetContext().BlockHeight()-height)
			require.True(t, bank.GetSupply(nw.GetContext(), denom).Amount.Sub(supplyBefore).Equal(math.NewInt(1e18)), "fallback must fund once")
			require.NoError(t, err)
			require.True(t, accrued.AmountOf(denom).GTE(expected.AmountOf(denom)))
			var withdrawn sdk.Coins
			if commission {
				withdrawn, err = nw.App.GetDistrKeeper().WithdrawValidatorCommission(nw.GetContext(), valAddr)
			} else {
				withdrawn, err = nw.App.GetDistrKeeper().WithdrawDelegationRewards(nw.GetContext(), delegator, valAddr)
			}
			require.NoError(t, err)
			require.True(t, withdrawn.AmountOf(denom).GTE(expected.AmountOf(denom).TruncateInt()))
			require.True(t, bank.GetBalance(nw.GetContext(), delegator, denom).Amount.Equal(balanceBefore.Add(withdrawn.AmountOf(denom))))
		})
	}
}

func TestAccrualWithoutMintUnreachableTarget(t *testing.T) {
	for _, name := range []string{"rewards", "commission"} {
		t.Run(name, func(t *testing.T) {
			nw := network.NewUnitTestNetwork(createNetworkAppWithoutMint)
			startHeight := nw.GetContext().BlockHeight()
			supply := nw.App.GetBankKeeper().GetSupply(nw.GetContext(), nw.GetBaseDenom()).Amount
			gh := grpc.NewIntegrationHandler(nw)
			var err error
			if name == "commission" {
				_, err = utils.WaitToAccrueCommission(nw, gh, nw.GetValidators()[0].OperatorAddress, sdk.NewDecCoins(sdk.NewInt64DecCoin(nw.GetBaseDenom(), 1e18)))
			} else {
				addr, _ := testtx.NewAccAddressAndKey()
				_, err = utils.WaitToAccrueRewards(nw, gh, addr.String(), sdk.NewDecCoins(sdk.NewInt64DecCoin(nw.GetBaseDenom(), 1)))
			}
			require.ErrorContains(t, err, name+" did not reach")
			require.Equal(t, int64(1), nw.GetContext().BlockHeight()-startHeight)
			require.True(t, nw.App.GetBankKeeper().GetSupply(nw.GetContext(), nw.GetBaseDenom()).Amount.Sub(supply).Equal(math.NewInt(1e18)))
		})
	}
}

func TestAccrualWithZeroInflation(t *testing.T) {
	create := func(chainID string, evmChainID uint64, opts ...func(*baseapp.BaseApp)) evm.IntegrationNetworkApp {
		return CreateEvmd(chainID, evmChainID, opts...)
	}
	nw := network.NewUnitTestNetwork(create)
	keeper := nw.CoreApp().(evm.MintKeeperProvider).GetMintKeeper()
	minter, err := keeper.Minter.Get(nw.GetContext())
	require.NoError(t, err)
	minter.Inflation = math.LegacyZeroDec()
	require.NoError(t, keeper.Minter.Set(nw.GetContext(), minter))
	supply := nw.App.GetBankKeeper().GetSupply(nw.GetContext(), nw.GetBaseDenom())
	valAddr, err := sdk.ValAddressFromBech32(nw.GetValidators()[0].OperatorAddress)
	require.NoError(t, err)
	gh := grpc.NewIntegrationHandler(nw)
	expected := sdk.NewDecCoins(sdk.NewInt64DecCoin(nw.GetBaseDenom(), 1))
	_, err = utils.WaitToAccrueRewards(nw, gh, sdk.AccAddress(valAddr).String(), expected)
	require.ErrorContains(t, err, "inflation is zero")
	_, err = utils.WaitToAccrueCommission(nw, gh, valAddr.String(), expected)
	require.ErrorContains(t, err, "inflation is zero")
	require.Equal(t, supply, nw.App.GetBankKeeper().GetSupply(nw.GetContext(), nw.GetBaseDenom()))

	// A broken installed mint must return its query error, not fund fallback rewards.
	height := nw.GetContext().BlockHeight()
	require.NoError(t, keeper.Minter.Remove(nw.GetContext()))
	_, err = utils.WaitToAccrueRewards(nw, gh, sdk.AccAddress(valAddr).String(), expected)
	require.ErrorContains(t, err, "failed to get inflation")
	_, err = utils.WaitToAccrueCommission(nw, gh, valAddr.String(), expected)
	require.ErrorContains(t, err, "failed to get inflation")
	require.Equal(t, height, nw.GetContext().BlockHeight())
	require.Equal(t, supply, nw.App.GetBankKeeper().GetSupply(nw.GetContext(), nw.GetBaseDenom()))
}
