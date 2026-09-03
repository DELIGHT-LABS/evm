package ics20

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cmn "github.com/cosmos/evm/precompiles/common"
	host "github.com/cosmos/ibc-go/v11/modules/core/24-host"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func isHostInvalidID(err error) bool {
	key, ok := cmn.ExtractCosmosErrorKey(err)
	return ok && key == cmn.NewCosmosErrorKey(host.ErrInvalidID)
}

func invalidSourceChannelError() error {
	return cmn.NewRevertWithSolidityError(ABI, SolidityErrInvalidSourceChannel, TransferMethod, ErrInvalidSourceChannel)
}

func (p Precompile) logUnmappedICS20Error(ctx sdk.Context, method string, translation cmn.ErrorTranslation) {
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

func (p Precompile) ics20MsgError(ctx sdk.Context, err error) error {
	result := cosmosErrorRegistry.ResolveMsgServerError(nil, TransferMethod, err)
	p.logUnmappedICS20Error(ctx, TransferMethod, result.Translation)
	return result.Err
}

func (p Precompile) ics20ValidatedInputError(ctx sdk.Context, err error) error {
	return p.ics20MsgError(ctx, err)
}

func (p Precompile) ics20QueryError(ctx sdk.Context, method string, err error) error {
	result := cosmosErrorRegistry.ResolveQueryError(method, err)
	p.logUnmappedICS20Error(ctx, method, result.Translation)
	return result.Err
}

func ics20QueryPreservesSuccess(method string, err error) bool {
	if !cmn.NeedsErrorTranslation(err) {
		return false
	}
	switch method {
	case DenomMethod, DenomHashMethod:
		return status.Code(err) == codes.NotFound
	default:
		return false
	}
}
