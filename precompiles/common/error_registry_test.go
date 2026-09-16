package common

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/stretchr/testify/require"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

const (
	errorRegistryNilErrorMessage   = "requires a non-nil error value"
	errorRegistryCanonicalABIError = "canonical ABI error"
)

type errorRegistryPointerError struct{}

func (*errorRegistryPointerError) Error() string { return "pointer error" }

type errorRegistryValueError struct {
	id string
}

func (err errorRegistryValueError) Error() string { return err.id }

type errorRegistryNonComparableError struct {
	parts []string
}

func (err errorRegistryNonComparableError) Error() string {
	return fmt.Sprint(err.parts)
}

func (err errorRegistryNonComparableError) Is(target error) bool {
	want, ok := target.(errorRegistryNonComparableError)
	return ok && len(err.parts) > 0 && len(want.parts) > 0 && err.parts[0] == want.parts[0]
}

type errorRegistryMultiMatch struct {
	targets []error
}

func (err errorRegistryMultiMatch) Error() string { return "matches multiple declarations" }

func (err errorRegistryMultiMatch) Is(target error) bool {
	for _, candidate := range err.targets {
		if target == candidate {
			return true
		}
	}
	return false
}

func TestErrorRegistryTranslatesByErrorValue(t *testing.T) {
	nonceLow := errors.New("nonce too low")
	registry := MustNewErrorRegistry(SharedErrorABI,
		NewErrorMapping(nonceLow, SolidityErrNonceTooLow),
	)

	matched, revert := registry.Translate(fmt.Errorf("admission rejected: %w", nonceLow))
	require.True(t, matched)
	requireModuleRevert(t, SharedErrorABI, revert, SolidityErrNonceTooLow)
}

func TestErrorRegistryIntegratesWithResolveError(t *testing.T) {
	registry := MustNewErrorRegistry(SharedErrorABI,
		NewErrorMapping(sdkerrors.ErrInsufficientFunds, SolidityErrNonceTooLow),
	)
	cosmos := MustNewCosmosErrorRegistry(SharedErrorABI, nil, SharedSDKErrorMappings(), nil)

	input := fmt.Errorf("admission rejected: %w", sdkerrors.ErrInsufficientFunds)
	result := cosmos.ResolveError(SharedErrorABI, input, registry.Translate, nil)
	requireModuleRevert(t, SharedErrorABI, result.Err, SolidityErrNonceTooLow)
	require.Equal(t, ErrorTranslation{}, result.Translation)

	result = cosmos.ResolveError(SharedErrorABI, sdkerrors.ErrUnauthorized, registry.Translate, nil)
	requireModuleRevert(t, SharedErrorABI, result.Err, SolidityErrSDKUnauthorized)
	require.Equal(t, MappingKindSharedSDK, result.Translation.Kind)
	require.Equal(t, NewCosmosErrorKey(sdkerrors.ErrUnauthorized), result.Translation.Key)
}

func TestErrorRegistryUsesErrorsIsAndDeclarationOrder(t *testing.T) {
	first := errors.New("first")
	second := errors.New("second")
	registry := MustNewErrorRegistry(SharedErrorABI,
		NewErrorMapping(first, SolidityErrNonceTooLow),
		NewErrorMapping(second, SolidityErrNonceGap),
	)

	matched, revert := registry.Translate(errorRegistryMultiMatch{targets: []error{first, second}})
	require.True(t, matched)
	requireModuleRevert(t, SharedErrorABI, revert, SolidityErrNonceTooLow)
}

func TestErrorRegistrySupportsCustomIsWithNonComparableErrors(t *testing.T) {
	target := errorRegistryNonComparableError{parts: []string{"nonce", "target"}}
	registry, err := NewErrorRegistry(SharedErrorABI,
		NewErrorMapping(target, SolidityErrNonceTooLow),
	)
	require.NoError(t, err)

	input := errorRegistryNonComparableError{parts: []string{"nonce", "input"}}
	matched, revert := registry.Translate(fmt.Errorf("wrapped: %w", input))
	require.True(t, matched)
	requireModuleRevert(t, SharedErrorABI, revert, SolidityErrNonceTooLow)
}

func TestErrorRegistryDoesNotMatchByConcreteType(t *testing.T) {
	registered := &errorRegistryValueError{id: "registered"}
	registry := MustNewErrorRegistry(SharedErrorABI,
		NewErrorMapping(registered, SolidityErrNonceTooLow),
	)

	matched, revert := registry.Translate(&errorRegistryValueError{id: "other value of same type"})
	require.False(t, matched)
	require.NoError(t, revert)
}

func TestErrorRegistryDoesNotMatchUnregisteredErrorWithSameMessage(t *testing.T) {
	registered := errors.New("same message")
	registry := MustNewErrorRegistry(SharedErrorABI,
		NewErrorMapping(registered, SolidityErrNonceTooLow),
	)

	matched, revert := registry.Translate(errors.New("same message"))
	require.False(t, matched)
	require.NoError(t, revert)
}

func TestErrorRegistryReturnsNilAndUnknownAsUnmatched(t *testing.T) {
	registry := MustNewErrorRegistry(SharedErrorABI,
		NewErrorMapping(errors.New("known"), SolidityErrNonceTooLow),
	)

	for _, input := range []error{nil, errors.New("unknown")} {
		matched, revert := registry.Translate(input)
		require.False(t, matched)
		require.NoError(t, revert)
	}
}

func TestErrorRegistryValidation(t *testing.T) {
	valid := errors.New("valid")
	var typedNil *errorRegistryPointerError

	tests := []struct {
		name    string
		mapping ErrorMapping
		want    string
	}{
		{"zero mapping", ErrorMapping{}, errorRegistryNilErrorMessage},
		{"nil error", NewErrorMapping(nil, SolidityErrNonceTooLow), errorRegistryNilErrorMessage},
		{"typed nil error", NewErrorMapping(typedNil, SolidityErrNonceTooLow), errorRegistryNilErrorMessage},
		{"empty Solidity error", NewErrorMapping(valid, ""), "missing ABI error"},
		{"missing Solidity error", NewErrorMapping(valid, "MissingError"), "missing ABI error"},
		{"argument-bearing Solidity error", NewErrorMapping(valid, SolidityErrChainIdMismatch), "must be no-argument"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewErrorRegistry(SharedErrorABI, tc.mapping)
			require.ErrorContains(t, err, tc.want)
		})
	}

	require.Panics(t, func() {
		MustNewErrorRegistry(SharedErrorABI, NewErrorMapping(nil, SolidityErrNonceTooLow))
	})
}

func TestErrorRegistryRejectsInvalidEffectiveABIAtConstruction(t *testing.T) {
	contractABI := cloneErrorRegistryABI(SharedErrorABI)
	first := contractABI.Errors[SolidityErrNonceTooLow]
	second := contractABI.Errors[SolidityErrNonceGap]
	second.Sig = first.Sig
	contractABI.Errors[SolidityErrNonceGap] = second

	_, err := NewErrorRegistry(contractABI,
		NewErrorMapping(errors.New("nonce"), SolidityErrNonceTooLow),
	)
	require.ErrorContains(t, err, "duplicate ABI signature")
}

func TestErrorRegistryRejectsNonCanonicalMappedError(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*abi.Error)
		want   string
	}{
		{"corrupted name", func(definition *abi.Error) { definition.Name = "OtherName" }, errorRegistryCanonicalABIError},
		{"corrupted signature", func(definition *abi.Error) { definition.Sig = "OtherName()" }, errorRegistryCanonicalABIError},
		{"corrupted selector", func(definition *abi.Error) { definition.ID[0] ^= 0xff }, errorRegistryCanonicalABIError},
		{"argument-bearing definition", func(definition *abi.Error) {
			definition.Inputs = append(abi.Arguments(nil), SharedErrorABI.Errors[SolidityErrChainIdMismatch].Inputs...)
		}, "must be no-argument"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contractABI := cloneErrorRegistryABI(SharedErrorABI)
			definition := contractABI.Errors[SolidityErrNonceTooLow]
			tc.mutate(&definition)
			contractABI.Errors[SolidityErrNonceTooLow] = definition

			_, err := NewErrorRegistry(contractABI,
				NewErrorMapping(errors.New("nonce"), SolidityErrNonceTooLow),
			)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestErrorRegistryRejectsDuplicateComparableErrorValue(t *testing.T) {
	cause := errors.New("duplicate")
	_, err := NewErrorRegistry(SharedErrorABI,
		NewErrorMapping(cause, SolidityErrNonceTooLow),
		NewErrorMapping(cause, SolidityErrNonceGap),
	)
	require.ErrorContains(t, err, "duplicate error value mapping")
}

func TestErrorRegistryAllowsEqualMessagesAndSharedSolidityErrors(t *testing.T) {
	first := errors.New("same message")
	second := errors.New("same message")
	third := errors.New("different message")
	registry, err := NewErrorRegistry(SharedErrorABI,
		NewErrorMapping(first, SolidityErrNonceTooLow),
		NewErrorMapping(second, SolidityErrNonceGap),
		NewErrorMapping(third, SolidityErrNonceTooLow),
	)
	require.NoError(t, err)

	for _, tc := range []struct {
		input error
		name  string
	}{
		{first, SolidityErrNonceTooLow},
		{second, SolidityErrNonceGap},
		{third, SolidityErrNonceTooLow},
	} {
		matched, revert := registry.Translate(tc.input)
		require.True(t, matched)
		requireModuleRevert(t, SharedErrorABI, revert, tc.name)
	}
}

func TestErrorRegistrySnapshotsDeclarationsAndReturnsFreshRevertData(t *testing.T) {
	first := errors.New("first")
	second := errors.New("second")
	contractABI := cloneErrorRegistryABI(SharedErrorABI)
	wantDefinition := contractABI.Errors[SolidityErrNonceTooLow]
	mappings := []ErrorMapping{NewErrorMapping(first, SolidityErrNonceTooLow)}
	registry := MustNewErrorRegistry(contractABI, mappings...)

	mappings[0].Error = second
	mappings[0].SolidityError = SolidityErrNonceGap
	contractABI.Errors[SolidityErrNonceTooLow] = contractABI.Errors[SolidityErrNonceGap]

	matched, revert := registry.Translate(first)
	require.True(t, matched)
	carrier := revert.(RevertDataCarrier)
	require.Equal(t, wantDefinition.ID[:4], carrier.RevertData()[:4])
	carrier.RevertData()[0] ^= 0xff

	matched, fresh := registry.Translate(first)
	require.True(t, matched)
	freshData := fresh.(RevertDataCarrier).RevertData()
	require.Equal(t, wantDefinition.ID[:4], freshData[:4])

	matched, unknown := registry.Translate(second)
	require.False(t, matched)
	require.NoError(t, unknown)
}

func cloneErrorRegistryABI(source abi.ABI) abi.ABI {
	errorsByName := make(map[string]abi.Error, len(source.Errors))
	for name, definition := range source.Errors {
		definition.Inputs = append(abi.Arguments(nil), definition.Inputs...)
		errorsByName[name] = definition
	}
	return abi.ABI{Errors: errorsByName}
}
