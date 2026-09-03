package werc20

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	cmn "github.com/cosmos/evm/precompiles/common"
	erc20 "github.com/cosmos/evm/precompiles/erc20"
	precompiletest "github.com/cosmos/evm/precompiles/testutil"

	errorsmod "cosmossdk.io/errors"
	"cosmossdk.io/log/v2"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

var errSyntheticWERC20Drift = errorsmod.Register("werc20-phase-four-drift", 77, "unstable reason")

func TestWERC20UnmappedLogsExactlyOnceWithoutReason(t *testing.T) {
	var output bytes.Buffer
	ctx := sdk.Context{}.WithLogger(log.NewLogger(&output, log.OutputJSONOption()))
	p := Precompile{Precompile: &erc20.Precompile{ABI: ABI}}

	err := p.werc20MsgError(ctx, WithdrawMethod, errSyntheticWERC20Drift)
	carrier := err.(cmn.RevertDataCarrier)
	unmappedDefinition := ABI.Errors[cmn.SolidityErrUnmappedCosmosError]
	require.Equal(t, unmappedDefinition.ID[:4], carrier.RevertData()[:4])

	logs := output.String()
	require.Equal(t, 1, strings.Count(logs, "unmapped registered Cosmos error"))
	require.Contains(t, logs, `"precompile":"werc20"`)
	require.Contains(t, logs, `"method":"withdraw"`)
	require.Contains(t, logs, `"codespace":"werc20-phase-four-drift"`)
	require.Contains(t, logs, `"code":77`)
	require.NotContains(t, logs, "unstable reason")

	known := p.werc20MsgError(ctx, WithdrawMethod, banktypes.ErrSendDisabled)
	knownDefinition := ABI.Errors[erc20.SolidityErrBankSendDisabled]
	require.Equal(t, knownDefinition.ID[:4], known.(cmn.RevertDataCarrier).RevertData()[:4])
	require.Equal(t, 1, strings.Count(output.String(), "unmapped registered Cosmos error"), "known mappings must not emit the unmapped signal")
}

func TestWERC20WrappedBankSentinelReturnsConcreteParsedError(t *testing.T) {
	ctx := sdk.Context{}.WithLogger(log.NewNopLogger())
	p := Precompile{Precompile: &erc20.Precompile{ABI: ABI}}
	wrapped := errorsmod.Wrap(banktypes.ErrSendDisabled, "changed bank wrapper text")

	err := p.werc20MsgError(ctx, DepositMethod, wrapped)
	data := err.(cmn.RevertDataCarrier).RevertData()
	expected := cmn.NewRevertWithSolidityError(ABI, erc20.SolidityErrBankSendDisabled).(cmn.RevertDataCarrier).RevertData()
	require.Equal(t, expected, data)

	var selector [4]byte
	copy(selector[:], data[:4])
	definition, parseErr := ABI.ErrorByID(selector)
	require.NoError(t, parseErr)
	require.Equal(t, erc20.SolidityErrBankSendDisabled, definition.Name)
	_, parseErr = definition.Unpack(data)
	require.NoError(t, parseErr)

	for _, fallback := range []string{
		cmn.SolidityErrMsgServerFailed,
		cmn.SolidityErrQueryFailed,
		cmn.SolidityErrUnmappedCosmosError,
	} {
		fallbackDefinition := ABI.Errors[fallback]
		require.NotEqual(t, fallbackDefinition.ID[:4], data[:4])
	}
}

func TestWERC20BoundaryPreservationAndEquivalence(t *testing.T) {
	p := Precompile{Precompile: &erc20.Precompile{ABI: ABI}}
	t.Run("msg", func(t *testing.T) {
		adapter := func(ctx sdk.Context, err error) error { return p.werc20MsgError(ctx, "method", err) }
		precompiletest.TestBoundaryAdapter(t, adapter)
		precompiletest.TestBoundaryEquivalence(t, ABI, cosmosErrorRegistry, "method", true, adapter, errSyntheticWERC20Drift, sdkerrors.ErrUnauthorized, errors.New("internal"))
	})
}
