package erc20

import (
	cmn "github.com/cosmos/evm/precompiles/common"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func (p *Precompile) logUnmappedERC20Error(ctx sdk.Context, method string, translation cmn.ErrorTranslation) {
	if translation.IsUnmapped {
		ctx.Logger().With("evm extension", "erc20").Warn(
			"unmapped registered Cosmos error",
			"precompile", p.Name(),
			"method", method,
			"codespace", translation.Key.Codespace,
			"code", translation.Key.Code,
		)
	}
}

func (p *Precompile) erc20MsgError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveMsgServerError(p.ABI, nil, method, err)
	p.logUnmappedERC20Error(ctx, method, result.Translation)
	return result.Err
}

func (p *Precompile) erc20QueryError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveQueryError(p.ABI, method, err)
	p.logUnmappedERC20Error(ctx, method, result.Translation)
	return result.Err
}
