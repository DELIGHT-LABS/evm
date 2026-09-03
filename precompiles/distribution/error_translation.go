package distribution

import (
	cmn "github.com/cosmos/evm/precompiles/common"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func (p Precompile) logUnmappedDistributionError(ctx sdk.Context, method string, translation cmn.ErrorTranslation) {
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

func (p Precompile) distributionMsgError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveMsgServerError(nil, method, err)
	p.logUnmappedDistributionError(ctx, method, result.Translation)
	return result.Err
}

func (p Precompile) distributionQueryError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveQueryError(method, err)
	p.logUnmappedDistributionError(ctx, method, result.Translation)
	return result.Err
}
