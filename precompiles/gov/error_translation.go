package gov

import (
	cmn "github.com/cosmos/evm/precompiles/common"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func (p Precompile) logUnmappedGovError(ctx sdk.Context, method string, translation cmn.ErrorTranslation) {
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

func (p Precompile) govMsgError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveMsgServerError(nil, method, err)
	p.logUnmappedGovError(ctx, method, result.Translation)
	return result.Err
}

func (p Precompile) govQueryError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveQueryError(method, err)
	p.logUnmappedGovError(ctx, method, result.Translation)
	return result.Err
}
