package ics20

import (
	"math/big"
	"testing"

<<<<<<< HEAD
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	transfertypes "github.com/cosmos/ibc-go/v11/modules/apps/transfer/types"
	clienttypes "github.com/cosmos/ibc-go/v11/modules/core/02-client/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestNewMsgTransferRejectsUnboundedSpendLimit(t *testing.T) {
	method, ok := ABI.Methods[TransferMethod]
	require.True(t, ok)

	sender := common.HexToAddress("0x1234567890123456789012345678901234567890")
	receiver := sdk.AccAddress(sender.Bytes()).String()
	newArgs := func(t *testing.T, amount *big.Int) []interface{} {
		t.Helper()

		input, err := method.Inputs.Pack(
			"transfer",
			"channel-0",
			"stake",
			amount,
			sender,
			receiver,
			clienttypes.NewHeight(0, 1),
			uint64(0),
			"",
		)
		require.NoError(t, err)

		args, err := method.Inputs.Unpack(input)
		require.NoError(t, err)
		return args
	}

	t.Run("rejects exact sentinel", func(t *testing.T) {
		amount := transfertypes.UnboundedSpendLimit().BigInt()

		msg, _, err := NewMsgTransfer(&method, newArgs(t, amount))

		require.ErrorIs(t, err, transfertypes.ErrInvalidAmount)
		require.ErrorContains(t, err, ErrUnboundedSpendLimit)
		require.Nil(t, msg)
	})

	t.Run("accepts value immediately below sentinel", func(t *testing.T) {
		amount := new(big.Int).Sub(transfertypes.UnboundedSpendLimit().BigInt(), big.NewInt(1))

		msg, returnAddr, err := NewMsgTransfer(&method, newArgs(t, amount))

		require.NoError(t, err)
		require.Equal(t, sender, returnAddr)
		require.Equal(t, amount, msg.Token.Amount.BigInt())
=======
	"github.com/stretchr/testify/require"

	cmn "github.com/cosmos/evm/precompiles/common"
	"github.com/cosmos/evm/precompiles/testutil"

	"github.com/cosmos/cosmos-sdk/types/query"
)

func TestNewDenomsRequest(t *testing.T) {
	method, ok := ABI.Methods[DenomsMethod]
	require.True(t, ok)

	pageRequest := query.PageRequest{
		Key:        []byte{1, 2, 3},
		Offset:     4,
		Limit:      5,
		CountTotal: true,
		Reverse:    true,
	}
	packed, err := method.Inputs.Pack(pageRequest)
	require.NoError(t, err)
	args, err := method.Inputs.Unpack(packed)
	require.NoError(t, err)

	t.Run("valid pagination", func(t *testing.T) {
		req, err := NewDenomsRequest(&method, args)

		require.NoError(t, err)
		require.NotNil(t, req)
		require.Equal(t, &pageRequest, req.Pagination)
	})

	t.Run("invalid pagination", func(t *testing.T) {
		req, err := NewDenomsRequest(&method, []interface{}{"bad-pagination"})
		wantErr := cmn.NewRevertWithSolidityError(
			ABI,
			cmn.SolidityErrInvalidPageRequest,
			DenomsMethod,
			big.NewInt(0),
			"bad-pagination",
		)

		testutil.RequireExactError(t, err, wantErr)
		require.Nil(t, req)
>>>>>>> f0b5980 (feat(precompiles): add shared page request validation)
	})
}
