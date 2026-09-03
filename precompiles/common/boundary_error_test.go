package common

import (
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/stretchr/testify/require"

	errorsmod "cosmossdk.io/errors"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

func TestBoundaryErrorResolution(t *testing.T) {
	api, module, _ := newMsgServerErrorTestRegistries(t)
	cosmos := MustNewCosmosErrorRegistry(api, CosmosErrorMappings{NewCosmosErrorMapping(errMsgServerSynthetic, "PrecompileFailure")}, CosmosErrorMappings{NewCosmosErrorMapping(sdkerrors.ErrUnauthorized, SolidityErrSDKUnauthorized)}, nil)
	carrier := &testRevertDataCarrier{data: []byte{0xde, 0xad, 0xbe, 0xef, 1}, err: errMsgServerSynthetic}
	for _, terminal := range []error{nil, vm.ErrOutOfGas, fmt.Errorf("wrapped: %w", vm.ErrOutOfGas), carrier, fmt.Errorf("wrapped: %w", carrier)} {
		for _, result := range []ErrorResolution{cosmos.ResolveQueryError("query", terminal), cosmos.ResolveMsgServerError(module, "msg", terminal)} {
			require.Equal(t, ErrorTranslation{}, result.Translation)
			require.Equal(t, terminal, result.Err)
		}
	}
	for _, input := range []error{errMsgServerSynthetic, sdkerrors.ErrUnauthorized, errMsgServerUnmapped, errors.New("internal")} {
		for _, msg := range []bool{false, true} {
			result := cosmos.ResolveQueryError("method", input)
			expected := QueryError(api, cosmos, "method", input)
			if msg {
				result = cosmos.ResolveMsgServerError(nil, "method", input)
				translation := TranslateCosmosError(api, cosmos, input)
				expected = translation.Revert
				if translation.Kind == MappingKindInternal {
					expected = NewRevertWithSolidityError(api, SolidityErrMsgServerFailed, "method", input.Error())
				}
			}
			require.Equal(t, expected, result.Err)
			require.Equal(t, cosmos.Translate(input), result.Translation)
		}
	}
	// A test MsgServer's typed error takes precedence over its registered cause.
	server := func() error { return msgServerModuleError{cause: errMsgServerSynthetic} }
	result := cosmos.ResolveMsgServerError(module, "msg", server())
	require.Equal(t, ErrorTranslation{}, result.Translation)
	data, err := ReturnRevertError(&vm.EVM{}, result.Err)
	require.ErrorIs(t, err, vm.ErrExecutionReverted)
	require.Equal(t, errorSelector(api, msgServerSolidityErrModuleFailure), data)
	query := cosmos.ResolveQueryError("query", server())
	require.Equal(t, MappingKindPrecompile, query.Translation.Kind)
	require.Equal(t, errorSelector(api, "PrecompileFailure"), query.Err.(RevertDataCarrier).RevertData())
	// Fallbacks also use frozen definitions after the caller mutates the ABI.
	before := cosmos.ResolveQueryError("query", errors.New("internal"))
	api.Errors[SolidityErrQueryFailed].Inputs[0].Name = "changed"
	delete(api.Errors, SolidityErrQueryFailed)
	require.Equal(t, before, cosmos.ResolveQueryError("query", errors.New("internal")))
	legacy := QueryError(api, cosmos, "query", errors.New("internal"))
	require.NotEqual(t, before.Err, legacy)
	beforeMsg := cosmos.ResolveMsgServerError(nil, "msg", errors.New("internal"))
	delete(api.Errors, SolidityErrMsgServerFailed)
	require.Equal(t, beforeMsg, cosmos.ResolveMsgServerError(nil, "msg", errors.New("internal")))
}

const (
	msgServerSolidityErrModuleFailure = "ModuleFailure"
	msgServerSolidityErrAFailure      = "AFailure"
)

const msgServerErrorABIJSON = `[
	{"type":"error","name":"ModuleFailure","inputs":[]},
	{"type":"error","name":"AFailure","inputs":[]},
	{"type":"error","name":"BFailure","inputs":[]},
	{"type":"error","name":"PrecompileFailure","inputs":[]},
	{"type":"error","name":"SDKUnauthorized","inputs":[]},
	{"type":"error","name":"UnmappedCosmosError","inputs":[{"name":"codespace","type":"string"},{"name":"code","type":"uint32"}]},
	{"type":"error","name":"MsgServerFailed","inputs":[{"name":"msgMethod","type":"string"},{"name":"reason","type":"string"}]}
]`

var (
	errMsgServerSynthetic = errorsmod.Register("msg-server-test", 7, "synthetic")
	errMsgServerUnmapped  = errorsmod.Register("msg-server-unmapped", 8, "unmapped")
)

type msgServerModuleError struct {
	cause error
}

func (err msgServerModuleError) Error() string { return "module error" }
func (err msgServerModuleError) Unwrap() error { return err.cause }

type msgServerCustomAsError struct {
	value msgServerModuleError
}

func (msgServerCustomAsError) Error() string { return "custom As" }

func (err msgServerCustomAsError) As(target any) bool {
	value, ok := target.(*msgServerModuleError)
	if !ok {
		return false
	}
	*value = err.value
	return true
}

func TestResolveMsgServerErrorPreservesTerminalErrors(t *testing.T) {
	_, _, cosmosRegistry := newMsgServerErrorTestRegistries(t)

	require.NoError(t, cosmosRegistry.ResolveMsgServerError(nil, "mint", nil).Err)
	require.Same(t, vm.ErrOutOfGas, cosmosRegistry.ResolveMsgServerError(nil, "mint", vm.ErrOutOfGas).Err)

	revertData := []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02}
	wrapped := fmt.Errorf("outer: %w", &testRevertDataCarrier{data: revertData, err: errMsgServerSynthetic})
	got := cosmosRegistry.ResolveMsgServerError(nil, "mint", wrapped).Err
	require.Same(t, wrapped, got)
	var carrier RevertDataCarrier
	require.ErrorAs(t, got, &carrier)
	require.Equal(t, revertData, carrier.RevertData())
}

func TestResolveMsgServerErrorPrefersModuleMappings(t *testing.T) {
	moduleABI, moduleRegistry, cosmosRegistry := newMsgServerErrorTestRegistries(t)

	tests := []struct {
		name      string
		err       error
		errorName string
	}{
		{
			name:      "before Cosmos mapping",
			err:       msgServerModuleError{cause: errMsgServerSynthetic},
			errorName: msgServerSolidityErrModuleFailure,
		},
		{
			name:      "wrapped error",
			err:       fmt.Errorf("wrapped: %w", msgServerModuleError{}),
			errorName: msgServerSolidityErrModuleFailure,
		},
		{
			name:      "custom As",
			err:       msgServerCustomAsError{value: msgServerModuleError{}},
			errorName: msgServerSolidityErrModuleFailure,
		},
		{
			name:      "join uses registry declaration order",
			err:       errors.Join(registryErrorB{}, registryErrorA{}),
			errorName: msgServerSolidityErrAFailure,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cosmosRegistry.ResolveMsgServerError(moduleRegistry, "mint", tc.err).Err
			require.Equal(t, errorSelector(moduleABI, tc.errorName), got.(RevertDataCarrier).RevertData())
		})
	}
}

func TestResolveMsgServerErrorTranslatesCosmosErrors(t *testing.T) {
	moduleABI, _, cosmosRegistry := newMsgServerErrorTestRegistries(t)

	t.Run("precompile mapping", func(t *testing.T) {
		got := cosmosRegistry.ResolveMsgServerError(
			nil,
			"mint",
			errorsmod.Wrap(errMsgServerSynthetic, "diagnostic"),
		).Err
		require.Equal(t, errorSelector(moduleABI, "PrecompileFailure"), got.(RevertDataCarrier).RevertData())
	})

	t.Run("shared SDK mapping", func(t *testing.T) {
		got := cosmosRegistry.ResolveMsgServerError(
			nil,
			"mint",
			errorsmod.Wrap(sdkerrors.ErrUnauthorized, "diagnostic"),
		).Err
		require.Equal(t, errorSelector(moduleABI, SolidityErrSDKUnauthorized), got.(RevertDataCarrier).RevertData())
	})

	t.Run("registered unmapped Cosmos error", func(t *testing.T) {
		got := cosmosRegistry.ResolveMsgServerError(nil, "mint", errMsgServerUnmapped).Err
		data := got.(RevertDataCarrier).RevertData()
		require.Equal(t, errorSelector(moduleABI, SolidityErrUnmappedCosmosError), data[:4])
		decoded, err := moduleABI.Errors[SolidityErrUnmappedCosmosError].Inputs.Unpack(data[4:])
		require.NoError(t, err)
		require.Equal(t, []interface{}{"msg-server-unmapped", uint32(8)}, decoded)
	})
}

func TestResolveMsgServerErrorHandlesMissingModuleMappings(t *testing.T) {
	moduleABI, _, cosmosRegistry := newMsgServerErrorTestRegistries(t)
	emptyModuleRegistry := MustNewModuleErrorRegistry(moduleABI)

	tests := []struct {
		name           string
		moduleRegistry *ModuleErrorRegistry
	}{
		{name: "nil registry", moduleRegistry: nil},
		{name: "empty registry", moduleRegistry: emptyModuleRegistry},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := errors.New("mint backend unavailable")
			got := cosmosRegistry.ResolveMsgServerError(tc.moduleRegistry, "mint", input).Err
			data := got.(RevertDataCarrier).RevertData()
			require.Equal(t, []byte{0x23, 0x7d, 0x7e, 0xdd}, data[:4], "MsgServerFailed(string,string) selector must remain stable")
			require.Equal(t, errorSelector(moduleABI, SolidityErrMsgServerFailed), data[:4])
			decoded, err := moduleABI.Errors[SolidityErrMsgServerFailed].Inputs.Unpack(data[4:])
			require.NoError(t, err)
			require.Equal(t, []interface{}{"mint", input.Error()}, decoded)
		})
	}
}

func newMsgServerErrorTestRegistries(t *testing.T) (abi.ABI, *ModuleErrorRegistry, *CosmosErrorRegistry) {
	t.Helper()
	moduleABI := mustTestABI(t, msgServerErrorABIJSON)
	moduleRegistry := MustNewModuleErrorRegistry(
		moduleABI,
		NewNoArgsModuleErrorMapping[msgServerModuleError](msgServerSolidityErrModuleFailure),
		NewNoArgsModuleErrorMapping[registryErrorA](msgServerSolidityErrAFailure),
		NewNoArgsModuleErrorMapping[registryErrorB]("BFailure"),
	)
	maps.Copy(moduleABI.Errors, mustTestABI(t, sharedErrorABIJSON).Errors)
	cosmosRegistry := MustNewCosmosErrorRegistry(
		moduleABI,
		CosmosErrorMappings{NewCosmosErrorMapping(errMsgServerSynthetic, "PrecompileFailure")},
		CosmosErrorMappings{NewCosmosErrorMapping(sdkerrors.ErrUnauthorized, SolidityErrSDKUnauthorized)},
		nil,
	)
	return moduleABI, moduleRegistry, cosmosRegistry
}
