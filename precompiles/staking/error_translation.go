package staking

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cmn "github.com/cosmos/evm/precompiles/common"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func (p Precompile) logUnmappedStakingError(ctx sdk.Context, method string, translation cmn.ErrorTranslation) {
	if translation.IsUnmapped {
		p.Logger(ctx).Warn(
			"unmapped registered Cosmos error",
			"precompile", p.Name(),
			"method", method,
			"codespace", translation.Key.Codespace,
			"code", translation.Key.Code,
		)
	}
}

func (p Precompile) stakingMsgError(ctx sdk.Context, method string, err error) error {
	if !cmn.NeedsErrorTranslation(err) {
		return err
	}
	if method == CancelUnbondingDelegationMethod && status.Code(err) == codes.NotFound {
		return cmn.NewRevertWithSolidityError(p.ABI, SolidityErrStakingUnbondingDelegationNotFound)
	}
	result := cosmosErrorRegistry.ResolveMsgServerError(nil, method, err)
	p.logUnmappedStakingError(ctx, method, result.Translation)
	return result.Err
}

func (p Precompile) stakingQueryError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveQueryError(method, err)
	p.logUnmappedStakingError(ctx, method, result.Translation)
	return result.Err
}

func stakingQueryPreservesSuccess(method string, err error) bool {
	if !cmn.NeedsErrorTranslation(err) {
		return false
	}
	switch method {
	case DelegationMethod, UnbondingDelegationMethod, ValidatorMethod:
		return status.Code(err) == codes.NotFound
	default:
		return false
	}
}
