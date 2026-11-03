package types

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/core"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/rpc"

	common "github.com/cosmos/evm/precompiles/common"
	evmtypes "github.com/cosmos/evm/x/vm/types"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var txRejectErrorMappings = []common.ErrorMapping{
	common.NewErrorMapping(core.ErrNonceTooLow, common.SolidityErrNonceTooLow),
	common.NewErrorMapping(core.ErrNonceTooHigh, common.SolidityErrNonceGap),
	common.NewErrorMapping(core.ErrInsufficientFunds, common.SolidityErrSDKInsufficientFunds),
	common.NewErrorMapping(core.ErrInsufficientFundsForTransfer, common.SolidityErrSDKInsufficientFunds),
	common.NewErrorMapping(core.ErrIntrinsicGas, common.SolidityErrIntrinsicGasTooLow),
	common.NewErrorMapping(core.ErrFloorDataGas, common.SolidityErrFloorDataGasTooLow),
	common.NewErrorMapping(core.ErrTipAboveFeeCap, common.SolidityErrTipAboveFeeCap),
	common.NewErrorMapping(core.ErrFeeCapVeryHigh, common.SolidityErrFeeCapTooHigh),
	common.NewErrorMapping(core.ErrTipVeryHigh, common.SolidityErrTipTooHigh),
	// recoverPlain returns this sentinel for invalid signature values.
	common.NewErrorMapping(ethtypes.ErrInvalidSig, common.SolidityErrInvalidSender),
}

var (
	txRejectABI         = mustTxRejectErrorABI(common.SharedErrorABI)
	txRejectErrors      = common.MustNewErrorRegistry(txRejectABI, txRejectErrorMappings...)
	txRejectSDKMappings = append(common.SharedSDKErrorMappings(), common.NewCosmosErrorMapping(sdkerrors.ErrInsufficientFee, common.SolidityErrInsufficientFee))
	txRejectCosmos      = common.MustNewCosmosErrorRegistry(txRejectABI, nil, txRejectSDKMappings, nil)
)

// Validate once at startup and keep request-time packing independent of mutable
// exported ABI maps. The Cosmos resolver receives this caller-owned snapshot.
func mustTxRejectErrorABI(source abi.ABI) abi.ABI {
	if err := common.ValidateSharedErrorABI(source); err != nil {
		panic(err)
	}
	// Use the generated ABI definitions; compatibility tests pin their signatures
	// and selectors independently of this internal consistency check.
	for _, name := range []string{common.SolidityErrInsufficientFee, common.SolidityErrChainIdMismatch} {
		definition, ok := source.Errors[name]
		derived := abi.NewError(name, definition.Inputs)
		if !ok || definition.Name != name || definition.Sig != derived.Sig || definition.ID != derived.ID {
			panic(fmt.Sprintf("transaction rejection requires canonical ABI error %s", name))
		}
	}
	frozen := abi.ABI{Errors: make(map[string]abi.Error, len(source.Errors))}
	for name, definition := range source.Errors {
		definition.Inputs = append(abi.Arguments(nil), definition.Inputs...)
		frozen.Errors[name] = definition
	}
	return frozen
}

// ResolveTxRejectError normalizes a transaction preparation, simulation, or
// submission failure using shared Geth, Cosmos, and chain-ID mappings. Backend
// and keeper query handlers use the same conversion of existing errors; their
// execution paths own validation and acceptance decisions.
//
// Callers supply implementation-specific mappings through translators, keeping
// backend and mempool dependencies out of this package. These mappings precede
// the shared mappings.
func ResolveTxRejectError(original error, translators ...func(error) (bool, error)) error {
	if original == nil || errors.Is(original, vm.ErrOutOfGas) {
		return original
	}
	if _, ok := original.(*TxRejectError); ok {
		return original
	}
	var dataError rpc.DataError
	var carrier common.RevertDataCarrier
	hasCarrier := errors.As(original, &carrier)
	if errors.As(original, &dataError) || hasCarrier {
		var data []byte
		if hasCarrier {
			data = carrier.RevertData()
		}
		return NewTxRejectError(original, data)
	}
	// Transport/context failures are not transaction admission decisions.
	var timeout net.Error
	if errors.Is(original, context.Canceled) || errors.Is(original, context.DeadlineExceeded) ||
		errors.Is(original, sdkerrors.ErrIO) || (errors.As(original, &timeout) && timeout.Timeout()) {
		return original
	}
	resolution := txRejectCosmos.ResolveError(txRejectABI, original, func(err error) (bool, error) {
		for _, translate := range translators {
			if translate != nil {
				if matched, encoded := translate(err); matched {
					return true, encoded
				}
			}
		}
		return translateTxRejectError(err)
	}, nil)
	translation := resolution.Translation
	if translation.Kind == common.MappingKindInternal {
		// Custom translations have no Cosmos classification. Unknown errors
		// and failed chain-ID encodings retain the original error.
		if rejection, ok := resolution.Err.(*TxRejectError); ok {
			return rejection // Chain-ID translation already retained the original cause.
		}
		if errors.As(resolution.Err, &carrier) {
			return NewTxRejectError(original, carrier.RevertData())
		}
		return original
	}
	name := common.SolidityErrUnmappedCosmosError
	if translation.Kind == common.MappingKindSharedSDK {
		for _, mapping := range txRejectSDKMappings {
			if mapping.Key == translation.Key {
				name = mapping.SolidityError
				break
			}
		}
	}
	return checkedTxRejectError(original, resolution.Err, txRejectABI.Errors[name])
}

// Value and typed mappings precede Cosmos mappings. A matched chain-ID error
// stops resolution even when encoding fails; it must not fall through to Cosmos.
func translateTxRejectError(original error) (bool, error) {
	if matched, encoded := txRejectErrors.Translate(original); matched {
		return true, encoded
	}
	var chainID *evmtypes.ChainIDMismatchError
	if errors.As(original, &chainID) {
		return true, encodeTxRejectError(original, txRejectABI, common.SolidityErrChainIdMismatch, chainID.Expected, chainID.Actual)
	}
	return false, nil
}

func encodeTxRejectError(original error, api abi.ABI, name string, args ...interface{}) error {
	encoded := common.NewRevertWithSolidityError(api, name, args...)
	return checkedTxRejectError(original, encoded, api.Errors[name])
}

// The common packer intentionally returns Error(string) on failure. Only a
// canonical encoding of the expected custom error counts as a new mapping.
// Pre-existing carriers bypass this check and retain their bytes verbatim.
func checkedTxRejectError(original, encoded error, expected abi.Error) error {
	var carrier common.RevertDataCarrier
	if !errors.As(encoded, &carrier) {
		return original
	}
	data := carrier.RevertData()
	if len(data) < 4 || !bytes.Equal(data[:4], expected.ID[:4]) {
		return original
	}
	args, err := expected.Inputs.Unpack(data[4:])
	if err != nil {
		return original
	}
	canonical, err := expected.Inputs.Pack(args...)
	if err != nil || !bytes.Equal(canonical, data[4:]) {
		return original
	}
	return NewTxRejectError(original, data)
}
