package common

import (
	"errors"
	"maps"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/stretchr/testify/require"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

func TestCosmosRegistryBoundABIAndLegacyArgument(t *testing.T) {
	api := mustTestABI(t, msgServerErrorABIJSON)
	mappings := CosmosErrorMappings{NewCosmosErrorMapping(errMsgServerSynthetic, "PrecompileFailure")}
	maps.Copy(api.Errors, mustTestABI(t, sharedErrorABIJSON).Errors)
	registry := MustNewCosmosErrorRegistry(api, mappings, CosmosErrorMappings{NewCosmosErrorMapping(sdkerrors.ErrUnauthorized, SolidityErrSDKUnauthorized)}, nil)
	inputs := []error{errMsgServerSynthetic, sdkerrors.ErrUnauthorized, errMsgServerUnmapped, errors.New("internal")}
	expected := make([]ErrorTranslation, len(inputs))
	for i, input := range inputs {
		expected[i] = TranslateCosmosError(api, registry, input)
		require.Equal(t, expected[i], registry.Translate(input))
	}
	mappings[0].SolidityError = "AFailure"
	api.Errors[SolidityErrUnmappedCosmosError].Inputs[0] = abi.Argument{}
	api.Errors["PrecompileFailure"] = api.Errors["AFailure"]
	delete(api.Errors, SolidityErrSDKUnauthorized)
	for i, input := range inputs {
		require.Equal(t, expected[i], registry.Translate(input))
	}
	// The exported legacy ABI argument remains effective, including mismatched ABI behavior.
	legacy := TranslateCosmosError(api, registry, errMsgServerSynthetic)
	require.Equal(t, errorSelector(api, "AFailure"), legacy.Revert.(RevertDataCarrier).RevertData())
	require.NotEqual(t, expected[0].Revert, legacy.Revert)
	// A legacy caller can still supply an incomplete ABI; the registry's bound
	// definitions must not silently replace its generic packing fallback.
	delete(api.Errors, SolidityErrUnmappedCosmosError)
	legacy = TranslateCosmosError(api, registry, errMsgServerUnmapped)
	require.Equal(t, expected[2].Kind, legacy.Kind)
	require.Equal(t, expected[2].Key, legacy.Key)
	require.True(t, legacy.IsUnmapped)
	require.Equal(t, []byte{0x08, 0xc3, 0x79, 0xa0}, legacy.Revert.(RevertDataCarrier).RevertData()[:4])
	require.Equal(t, expected[2], registry.Translate(errMsgServerUnmapped))
}

func TestCosmosRegistryConstructorRejectsMinimalABI(t *testing.T) {
	api := mustTestABI(t, `[{"type":"error","name":"PrecompileFailure","inputs":[]}]`)
	mappings := CosmosErrorMappings{NewCosmosErrorMapping(errMsgServerSynthetic, "PrecompileFailure")}
	// Validation-only callers retain their mapping-only ABI contract.
	require.NoError(t, ValidateCosmosErrorRegistry(api, mappings, nil, nil))
	require.Panics(t, func() { MustNewCosmosErrorRegistry(api, mappings, nil, nil) })
}
