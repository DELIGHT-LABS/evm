package werc20

import (
	cmn "github.com/cosmos/evm/precompiles/common"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func (p Precompile) logUnmappedWERC20Error(ctx sdk.Context, method string, translation cmn.ErrorTranslation) {
	if translation.IsUnmapped {
		ctx.Logger().With("evm extension", "werc20").Warn(
			"unmapped registered Cosmos error",
			"precompile", "werc20",
			"method", method,
			"codespace", translation.Key.Codespace,
			"code", translation.Key.Code,
		)
	}
}

func (p Precompile) werc20MsgError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveMsgServerError(p.ABI, nil, method, err)
	p.logUnmappedWERC20Error(ctx, method, result.Translation)
	return result.Err
}
