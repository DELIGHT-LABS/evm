package ics20

import (
	"math/big"
	"testing"

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
	})
}
