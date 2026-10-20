package utils

import (
	"errors"
	"fmt"
	"time"

	"github.com/cosmos/evm"
	"github.com/cosmos/evm/testutil"
	"github.com/cosmos/evm/testutil/integration/evm/grpc"
	"github.com/cosmos/evm/testutil/integration/evm/network"

	errorsmod "cosmossdk.io/errors"
	"cosmossdk.io/math"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
)

// WaitToAccrueRewards is a helper function that waits for rewards to
// accumulate up to a specified expected amount
func WaitToAccrueRewards(n network.Network, gh grpc.Handler, delegatorAddr string, expRewards sdk.DecCoins) (sdk.DecCoins, error) {
	var (
		err     error
		lapse   = time.Hour * 24 * 7 // one week
		rewards = sdk.DecCoins{}
	)

	expAmt := expRewards.AmountOf(n.GetBaseDenom())
	hasMint, err := prepareRewardAccrual(n, expAmt.IsPositive())
	if err != nil {
		return nil, err
	}

	for rewards.AmountOf(n.GetBaseDenom()).LT(expAmt) {
		rewards, err = checkRewardsAfter(n, gh, delegatorAddr, lapse)
		if err != nil {
			return nil, errorsmod.Wrap(err, "error checking rewards")
		}
		if !hasMint && rewards.AmountOf(n.GetBaseDenom()).LT(expAmt) {
			return nil, fmt.Errorf("rewards did not reach %s after fee distribution (got %s)", expRewards, rewards)
		}
	}

	return rewards, err
}

// checkRewardsAfter is a helper function that checks the accrued rewards
// after the provided timelapse
func checkRewardsAfter(n network.Network, gh grpc.Handler, delegatorAddr string, lapse time.Duration) (sdk.DecCoins, error) {
	err := n.NextBlockAfter(lapse)
	if err != nil {
		return nil, errorsmod.Wrap(err, "failed to commit block after voting period ends")
	}

	res, err := gh.GetDelegationTotalRewards(delegatorAddr)
	if err != nil {
		return nil, errorsmod.Wrapf(err, "error while querying for delegation rewards")
	}

	return res.Total, nil
}

// WaitToAccrueCommission is a helper function that waits for commission to
// accumulate up to a specified expected amount
func WaitToAccrueCommission(n network.Network, gh grpc.Handler, validatorAddr string, expCommission sdk.DecCoins) (sdk.DecCoins, error) {
	var (
		err        error
		lapse      = time.Hour * 24 * 7 // one week
		commission = sdk.DecCoins{}
	)

	expAmt := expCommission.AmountOf(n.GetBaseDenom())
	hasMint, err := prepareRewardAccrual(n, expAmt.IsPositive())
	if err != nil {
		return nil, err
	}

	for commission.AmountOf(n.GetBaseDenom()).LT(expAmt) {
		commission, err = checkCommissionAfter(n, gh, validatorAddr, lapse)
		if err != nil {
			return nil, errorsmod.Wrap(err, "error checking commission")
		}
		if !hasMint && commission.AmountOf(n.GetBaseDenom()).LT(expAmt) {
			return nil, fmt.Errorf("commission did not reach %s after fee distribution (got %s)", expCommission, commission)
		}
	}

	return commission, err
}

// checkCommissionAfter is a helper function that checks the accrued commission
// after the provided time lapse
func checkCommissionAfter(n network.Network, gh grpc.Handler, valAddr string, lapse time.Duration) (sdk.DecCoins, error) {
	err := n.NextBlockAfter(lapse)
	if err != nil {
		return nil, errorsmod.Wrap(err, "failed to commit block after voting period ends")
	}

	res, err := gh.GetValidatorCommission(valAddr)
	if err != nil {
		return nil, errorsmod.Wrapf(err, "error while querying for delegation rewards")
	}

	return res.Commission.Commission, nil
}

// prepareRewardAccrual reports whether mint is available and checks inflation.
// Without mint, it funds the fee collector once when rewards are requested.
func prepareRewardAccrual(n network.Network, fundFees bool) (bool, error) {
	if core, ok := n.(interface {
		CoreApp() evm.IntegrationNetworkApp
	}); ok {
		app := core.CoreApp()
		if _, hasMint := app.(evm.MintKeeperProvider); !hasMint {
			if !fundFees {
				return false, nil
			}
			coins := sdk.NewCoins(sdk.NewCoin(n.GetBaseDenom(), math.NewInt(1e18)))
			ctx, write := n.GetContext().CacheContext()
			if err := testutil.FundModuleAccount(ctx, app.GetBankKeeper(), authtypes.FeeCollectorName, coins); err != nil {
				return false, err
			}
			write()
			// Commit resets the app's finalize state. Flush the network's retained
			// cache so the next block can distribute these fees.
			if cache, ok := n.GetContext().MultiStore().(storetypes.CacheMultiStore); ok {
				cache.Write()
			}
			return false, nil
		}
	}

	res, err := n.GetMintClient().Inflation(n.GetContext(), &minttypes.QueryInflationRequest{})
	if err != nil {
		return true, errorsmod.Wrap(err, "failed to get inflation")
	}

	if res.Inflation.IsZero() {
		return true, errors.New("inflation is zero; must be non-zero for rewards or commission to be distributed")
	}

	return true, nil
}
