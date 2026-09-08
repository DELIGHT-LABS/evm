package common

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/core/vm"
)

// ErrorResolution preserves Cosmos classification for caller-owned logging.
// Translation is populated only when the Cosmos tier was reached.
type ErrorResolution struct {
	Err         error
	Translation ErrorTranslation
}

// validateBoundaryErrors checks the canonical definitions required by boundary
// resolution during MustNewCosmosErrorRegistry initialization.
func validateBoundaryErrors(api abi.ABI) error {
	for _, expected := range []struct{ name, signature string }{
		{SolidityErrUnmappedCosmosError, "UnmappedCosmosError(string,uint32)"},
		{SolidityErrQueryFailed, "QueryFailed(string,string)"},
		{SolidityErrMsgServerFailed, "MsgServerFailed(string,string)"},
	} {
		definition, ok := api.Errors[expected.name]
		if !ok {
			return fmt.Errorf("boundary resolution requires ABI error %s", expected.signature)
		}
		// Sig alone is caller-mutable metadata; packing actually uses Inputs and ID.
		derived := abi.NewError(expected.name, definition.Inputs)
		if definition.Name != expected.name || definition.Sig != expected.signature || derived.Sig != expected.signature || definition.ID != derived.ID {
			return fmt.Errorf("boundary resolution requires canonical ABI error %s", expected.signature)
		}
	}
	return nil
}

// ResolveQueryError applies Cosmos mappings and then QueryFailed.
func (registry *CosmosErrorRegistry) ResolveQueryError(api abi.ABI, method string, err error) ErrorResolution {
	return resolveBoundaryError(api, nil, registry, SolidityErrQueryFailed, method, err)
}

// ResolveMsgServerError applies optional typed mappings before Cosmos mappings
// and MsgServerFailed. A nil module registry skips the typed tier.
func (registry *CosmosErrorRegistry) ResolveMsgServerError(api abi.ABI, module *ModuleErrorRegistry, method string, err error) ErrorResolution {
	return resolveBoundaryError(api, module, registry, SolidityErrMsgServerFailed, method, err)
}

// NeedsErrorTranslation reports whether an error needs mapping or fallback
// encoding. It returns false for nil, out-of-gas, and existing revert carriers,
// including wrapped errors, so callers can return those values unchanged.
func NeedsErrorTranslation(err error) bool {
	if err == nil || errors.Is(err, vm.ErrOutOfGas) {
		return false
	}
	var carrier RevertDataCarrier
	return !errors.As(err, &carrier)
}

func resolveBoundaryError(api abi.ABI, module *ModuleErrorRegistry, cosmos *CosmosErrorRegistry, fallback, method string, err error) ErrorResolution {
	if !NeedsErrorTranslation(err) {
		return ErrorResolution{Err: err}
	}
	if module != nil {
		if revert, matched := module.Translate(err); matched {
			return ErrorResolution{Err: revert}
		}
	}
	translation := TranslateCosmosError(api, cosmos, err)
	if translation.Kind != MappingKindInternal {
		return ErrorResolution{Err: translation.Revert, Translation: translation}
	}
	return ErrorResolution{Err: NewRevertWithSolidityError(api, fallback, method, err.Error()), Translation: translation}
}
