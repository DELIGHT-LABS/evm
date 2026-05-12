package types

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestTxTraceCodecRoundTrip(t *testing.T) {
	touch := TxCallTouch{
		From:     common.HexToAddress("0x1"),
		To:       common.HexToAddress("0x2"),
		Depth:    3,
		CallType: 4,
	}
	touchBz, err := EncodeTxCallTouchesV1([]TxCallTouch{touch})
	require.NoError(t, err)
	touches, err := DecodeTxCallTouchesV1(touchBz)
	require.NoError(t, err)
	require.Equal(t, []TxCallTouch{touch}, touches)

	transfer := TxValueTransfer{
		From:     touch.From,
		To:       touch.To,
		Depth:    touch.Depth,
		CallType: touch.CallType,
		Value:    big.NewInt(123),
	}
	transferBz, err := EncodeTxValueTransfersV1([]TxValueTransfer{transfer})
	require.NoError(t, err)
	transfers, err := DecodeTxValueTransfersV1(transferBz)
	require.NoError(t, err)
	require.Equal(t, []TxValueTransfer{transfer}, transfers)
}

func TestTxTraceCodecRejectsInvalidIntegerRanges(t *testing.T) {
	_, err := EncodeTxCallTouchesV1([]TxCallTouch{{Depth: -1}})
	require.EqualError(t, err, "negative touch depth")

	_, err = EncodeTxValueTransfersV1([]TxValueTransfer{{Depth: -1}})
	require.EqualError(t, err, "negative transfer depth")

	invalidCount := appendUvarint([]byte{TxTraceCodecV1}, math.MaxUint64)
	_, err = DecodeTxCallTouchesV1(invalidCount)
	require.EqualError(t, err, "invalid touch count")
	_, err = DecodeTxValueTransfersV1(invalidCount)
	require.EqualError(t, err, "invalid transfer count")

	invalidDepth := appendUvarint([]byte{TxTraceCodecV1}, 1)
	invalidDepth = append(invalidDepth, make([]byte, 40)...)
	invalidDepth = appendUvarint(invalidDepth, uint64(math.MaxInt)+1)
	invalidDepth = append(invalidDepth, 0)
	_, err = DecodeTxCallTouchesV1(invalidDepth)
	require.EqualError(t, err, "touch depth overflows int")
}
