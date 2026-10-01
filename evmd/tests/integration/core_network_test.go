package integration

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm"
	"github.com/cosmos/evm/ante"
	"github.com/cosmos/evm/testutil/integration/evm/network"
	"github.com/cosmos/evm/testutil/integration/evm/utils"
	transfertypes "github.com/cosmos/ibc-go/v11/modules/apps/transfer/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
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
