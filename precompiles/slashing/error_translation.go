package slashing

import (
	cmn "github.com/cosmos/evm/precompiles/common"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func (p Precompile) logUnmappedSlashingError(ctx sdk.Context, method string, translation cmn.ErrorTranslation) {
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

func (p Precompile) slashingMsgError(ctx sdk.Context, err error) error {
	result := cosmosErrorRegistry.ResolveMsgServerError(p.ABI, nil, UnjailMethod, err)
	p.logUnmappedSlashingError(ctx, UnjailMethod, result.Translation)
	return result.Err
}

func (p Precompile) slashingQueryError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveQueryError(p.ABI, method, err)
	p.logUnmappedSlashingError(ctx, method, result.Translation)
	return result.Err
}
