package staking

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cmn "github.com/cosmos/evm/precompiles/common"
	precompiletest "github.com/cosmos/evm/precompiles/testutil"

	errorsmod "cosmossdk.io/errors"
	"cosmossdk.io/log/v2"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

var errSyntheticStakingDrift = errorsmod.Register("staking-phase-two-drift", 77, "unstable reason")

func TestTranslateStakingRegisteredErrorsDirectAndWrapped(t *testing.T) {
	p := Precompile{ABI: ABI}
	ctx := sdk.Context{}.WithLogger(log.NewNopLogger())
	for _, err := range []error{
		stakingtypes.ErrNoValidatorFound,
		errorsmod.Wrap(stakingtypes.ErrNoValidatorFound, "message changed"),
		fmt.Errorf("standard wrapper: %w", stakingtypes.ErrNoValidatorFound),
	} {
		translated := p.stakingQueryError(ctx, DelegateMethod, err)
		carrier := translated.(cmn.RevertDataCarrier)
		require.Equal(t, stakingErrorSelector(SolidityErrStakingValidatorNotFound), carrier.RevertData()[:4])
		require.NotEqual(t, stakingErrorSelector(cmn.SolidityErrMsgServerFailed), carrier.RevertData()[:4])
		require.NotEqual(t, stakingErrorSelector(cmn.SolidityErrUnmappedCosmosError), carrier.RevertData()[:4])
	}
}

func TestStakingErrorMappingsReturnsCopyWithoutChangingRuntimeTranslation(t *testing.T) {
	mappings := ErrorMappings()
	require.NotEmpty(t, mappings)
	mappings[0].Key = cmn.NewCosmosErrorKey(stakingtypes.ErrNoDelegation)
	mappings[0].SolidityError = SolidityErrStakingNoDelegation

	fresh := ErrorMappings()
	require.Equal(t, cmn.NewCosmosErrorKey(stakingtypes.ErrNoValidatorFound), fresh[0].Key)
	require.Equal(t, SolidityErrStakingValidatorNotFound, fresh[0].SolidityError)

	p := Precompile{ABI: ABI}
	translated := p.stakingQueryError(
		sdk.Context{}.WithLogger(log.NewNopLogger()),
		DelegateMethod,
		stakingtypes.ErrNoValidatorFound,
	)
	require.Equal(t, stakingErrorSelector(SolidityErrStakingValidatorNotFound), translated.(cmn.RevertDataCarrier).RevertData()[:4])
}

func TestTranslateStakingUnmappedLogsExactlyOnceWithoutReason(t *testing.T) {
	var output bytes.Buffer
	ctx := sdk.Context{}.WithLogger(log.NewLogger(&output, log.OutputJSONOption()))
	p := Precompile{ABI: ABI}

	err := p.stakingMsgError(ctx, DelegateMethod, errSyntheticStakingDrift)
	carrier := err.(cmn.RevertDataCarrier)
	require.Equal(t, stakingErrorSelector(cmn.SolidityErrUnmappedCosmosError), carrier.RevertData()[:4])

	logs := output.String()
	require.Equal(t, 1, strings.Count(logs, "unmapped registered Cosmos error"))
	require.Contains(t, logs, `"precompile":"staking"`)
	require.Contains(t, logs, `"method":"delegate"`)
	require.Contains(t, logs, `"codespace":"staking-phase-two-drift"`)
	require.Contains(t, logs, `"code":77`)
	require.NotContains(t, logs, "unstable reason")

	_ = p.stakingMsgError(ctx, DelegateMethod, stakingtypes.ErrNoValidatorFound)
	require.Equal(t, 1, strings.Count(output.String(), "unmapped registered Cosmos error"), "known mappings must not emit the unmapped signal")
}

func TestStakingMsgAdapterGRPCDispositionIsMessageIndependent(t *testing.T) {
	p := Precompile{ABI: ABI}
	ctx := sdk.Context{}.WithLogger(log.NewNopLogger())
	for _, message := range []string{"not found", "message text changed"} {
		err := p.stakingMsgError(
			ctx,
			CancelUnbondingDelegationMethod,
			status.Error(codes.NotFound, message),
		)
		carrier := err.(cmn.RevertDataCarrier)
		require.Equal(t, []byte{0x46, 0x41, 0xdb, 0x46}, carrier.RevertData())
		require.NotEqual(t, stakingErrorSelector(cmn.SolidityErrSDKNotFound), carrier.RevertData())
	}

	internal := errors.New("infrastructure")
	expected := cmn.NewRevertWithSolidityError(ABI, cmn.SolidityErrMsgServerFailed, DelegateMethod, internal.Error())
	actual := p.stakingMsgError(ctx, DelegateMethod, internal)
	require.Equal(t, expected.(cmn.RevertDataCarrier).RevertData(), actual.(cmn.RevertDataCarrier).RevertData())
}

func stakingErrorSelector(name string) []byte {
	definition := ABI.Errors[name]
	return definition.ID[:4]
}

func TestStakingBoundaryPreservationAndEquivalence(t *testing.T) {
	p := Precompile{ABI: ABI}
	t.Run("query", func(t *testing.T) {
		adapter := func(ctx sdk.Context, err error) error { return p.stakingQueryError(ctx, "method", err) }
		precompiletest.TestBoundaryAdapter(t, adapter)
		precompiletest.TestBoundaryEquivalence(t, ABI, cosmosErrorRegistry, "method", false, adapter, errSyntheticStakingDrift, sdkerrors.ErrUnauthorized, errors.New("internal"))
	})
	t.Run("msg", func(t *testing.T) {
		adapter := func(ctx sdk.Context, err error) error {
			return p.stakingMsgError(ctx, CancelUnbondingDelegationMethod, err)
		}
		precompiletest.TestBoundaryAdapter(t, adapter)
		precompiletest.TestBoundaryEquivalence(t, ABI, cosmosErrorRegistry, CancelUnbondingDelegationMethod, true, adapter, errSyntheticStakingDrift, sdkerrors.ErrUnauthorized, errors.New("internal"))
	})
}

func TestStakingMsgGRPCPolicyScope(t *testing.T) {
	p := Precompile{ABI: ABI}
	for _, tc := range []struct {
		name   string
		method string
		err    error
	}{
		{"other method", DelegateMethod, status.Error(codes.NotFound, "missing")},
		{"other status", CancelUnbondingDelegationMethod, status.Error(codes.Internal, "missing")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual := p.stakingMsgError(testContext(), tc.method, tc.err)
			expected := cmn.NewRevertWithSolidityError(ABI, cmn.SolidityErrMsgServerFailed, tc.method, tc.err.Error())
			require.Equal(t, expected.(cmn.RevertDataCarrier).RevertData(), actual.(cmn.RevertDataCarrier).RevertData())
		})
	}
}
