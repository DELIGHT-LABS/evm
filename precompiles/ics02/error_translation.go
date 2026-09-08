package ics02

import (
	cmn "github.com/cosmos/evm/precompiles/common"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func (p Precompile) logUnmappedICS02Error(ctx sdk.Context, method string, translation cmn.ErrorTranslation) {
	if !translation.IsUnmapped {
		return
	}
	p.Logger(ctx).Warn(
		"unmapped registered Cosmos error",
		"precompile", p.Name(),
		"method", method,
		"codespace", translation.Key.Codespace,
		"code", translation.Key.Code,
	)
}

func (p Precompile) ics02KeeperError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveMsgServerError(p.ABI, method, err, nil)
	p.logUnmappedICS02Error(ctx, method, result.Translation)
	return result.Err
}

func (p Precompile) ics02ValidatedInputError(ctx sdk.Context, err error) error {
	result := cosmosErrorRegistry.ResolveMsgServerError(p.ABI, UpdateClientMethod, err, nil)
	p.logUnmappedICS02Error(ctx, UpdateClientMethod, result.Translation)
	return result.Err
}

func (p Precompile) ics02QueryError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveQueryError(p.ABI, method, err, nil)
	p.logUnmappedICS02Error(ctx, method, result.Translation)
	return result.Err
}
