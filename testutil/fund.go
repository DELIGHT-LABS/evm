package testutil

import (
	evmtypes "github.com/cosmos/evm/x/vm/types"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
)

// FundAccount is a utility function that funds an account by minting and
// sending the coins to the address. The EVM module account provides the mint
// permission, including on applications that do not install x/mint.
func FundAccount(ctx sdk.Context, bankKeeper bankkeeper.Keeper, addr sdk.AccAddress, amounts sdk.Coins) error {
	if err := bankKeeper.MintCoins(ctx, evmtypes.ModuleName, amounts); err != nil {
		return err
	}

	return bankKeeper.SendCoinsFromModuleToAccount(ctx, evmtypes.ModuleName, addr, amounts)
}

// FundAccountWithBaseDenom is a utility function that uses the FundAccount function
// to fund an account with the default Cosmos EVM denomination.
func FundAccountWithBaseDenom(ctx sdk.Context, bankKeeper bankkeeper.Keeper, addr sdk.AccAddress, amount int64) error {
	baseDenom, err := sdk.GetBaseDenom()
	if err != nil {
		return err
	}

	coins := sdk.NewCoins(
		sdk.NewCoin(baseDenom, math.NewInt(amount)),
	)

	return FundAccount(ctx, bankKeeper, addr, coins)
}

// FundModuleAccount is a utility function that funds a module account by
// minting and sending the coins to the address. The EVM module account provides
// the mint permission, including on applications that do not install x/mint.
func FundModuleAccount(ctx sdk.Context, bankKeeper bankkeeper.Keeper, recipientMod string, amounts sdk.Coins) error {
	if err := bankKeeper.MintCoins(ctx, evmtypes.ModuleName, amounts); err != nil {
		return err
	}

	return bankKeeper.SendCoinsFromModuleToModule(ctx, evmtypes.ModuleName, recipientMod, amounts)
}
