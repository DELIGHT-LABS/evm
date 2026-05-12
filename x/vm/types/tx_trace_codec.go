package types

import (
	"encoding/binary"
	"errors"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// TxTraceCodecV1 is the current codec version byte for tx-wide traces.
const TxTraceCodecV1 byte = 1

type TxCallTouch struct {
	From     common.Address
	To       common.Address
	Depth    int
	CallType byte
}

type TxValueTransfer struct {
	From     common.Address
	To       common.Address
	Depth    int
	CallType byte
	Value    *big.Int
}

// TxTraceKey returns a transient-store key for a specific txIndex under a prefix.
// The key format is: prefixByte(1) || txIndex(8 big-endian).
func TxTraceKey(prefix []byte, txIndex uint64) []byte {
	bz := make([]byte, 1+8)
	bz[0] = prefix[0]
	binary.BigEndian.PutUint64(bz[1:], txIndex)
	return bz
}

func appendUvarint(dst []byte, x uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], x)
	return append(dst, tmp[:n]...)
}

func EncodeTxCallTouchesV1(touches []TxCallTouch) ([]byte, error) {
	if len(touches) == 0 {
		return nil, nil
	}
	if len(touches) > math.MaxUint32 {
		return nil, errors.New("touches too large")
	}

	out := make([]byte, 0, 1+10+len(touches)*64)
	out = append(out, TxTraceCodecV1)
	out = appendUvarint(out, uint64(len(touches)))
	for _, t := range touches {
		out = append(out, t.From.Bytes()...)
		out = append(out, t.To.Bytes()...)
		out = appendUvarint(out, uint64(t.Depth))
		out = append(out, t.CallType)
	}
	return out, nil
}

func DecodeTxCallTouchesV1(bz []byte) ([]TxCallTouch, error) {
	if len(bz) < 1 || bz[0] != TxTraceCodecV1 {
		return nil, errors.New("unsupported tx trace codec")
	}
	i := 1
	n, nsz := binary.Uvarint(bz[i:])
	if nsz <= 0 {
		return nil, errors.New("invalid varint")
	}
	i += nsz

	out := make([]TxCallTouch, 0, int(n))
	for j := uint64(0); j < n; j++ {
		if i+40 > len(bz) {
			return nil, errors.New("truncated touch entry")
		}
		from := common.BytesToAddress(bz[i : i+20])
		to := common.BytesToAddress(bz[i+20 : i+40])
		i += 40

		depthU, dsz := binary.Uvarint(bz[i:])
		if dsz <= 0 {
			return nil, errors.New("invalid depth varint")
		}
		i += dsz
		if i >= len(bz) {
			return nil, errors.New("truncated callType")
		}
		callType := bz[i]
		i++

		out = append(out, TxCallTouch{
			From:     from,
			To:       to,
			Depth:    int(depthU),
			CallType: callType,
		})
	}
	return out, nil
}

func EncodeTxValueTransfersV1(transfers []TxValueTransfer) ([]byte, error) {
	if len(transfers) == 0 {
		return nil, nil
	}
	if len(transfers) > math.MaxUint32 {
		return nil, errors.New("transfers too large")
	}

	out := make([]byte, 0, 1+10+len(transfers)*96)
	out = append(out, TxTraceCodecV1)
	out = appendUvarint(out, uint64(len(transfers)))
	for _, tr := range transfers {
		out = append(out, tr.From.Bytes()...)
		out = append(out, tr.To.Bytes()...)
		out = appendUvarint(out, uint64(tr.Depth))
		out = append(out, tr.CallType)

		var vb []byte
		if tr.Value != nil {
			if tr.Value.Sign() < 0 {
				return nil, errors.New("negative transfer value")
			}
			vb = tr.Value.Bytes()
		}
		out = appendUvarint(out, uint64(len(vb)))
		out = append(out, vb...)
	}
	return out, nil
}

func DecodeTxValueTransfersV1(bz []byte) ([]TxValueTransfer, error) {
	if len(bz) < 1 || bz[0] != TxTraceCodecV1 {
		return nil, errors.New("unsupported tx trace codec")
	}
	i := 1
	n, nsz := binary.Uvarint(bz[i:])
	if nsz <= 0 {
		return nil, errors.New("invalid varint")
	}
	i += nsz

	out := make([]TxValueTransfer, 0, int(n))
	for j := uint64(0); j < n; j++ {
		if i+40 > len(bz) {
			return nil, errors.New("truncated transfer entry")
		}
		from := common.BytesToAddress(bz[i : i+20])
		to := common.BytesToAddress(bz[i+20 : i+40])
		i += 40

		depthU, dsz := binary.Uvarint(bz[i:])
		if dsz <= 0 {
			return nil, errors.New("invalid depth varint")
		}
		i += dsz
		if i >= len(bz) {
			return nil, errors.New("truncated callType")
		}
		callType := bz[i]
		i++

		vlen, vsz := binary.Uvarint(bz[i:])
		if vsz <= 0 {
			return nil, errors.New("invalid value length varint")
		}
		i += vsz
		if i+int(vlen) > len(bz) {
			return nil, errors.New("truncated value bytes")
		}
		vb := bz[i : i+int(vlen)]
		i += int(vlen)
		val := new(big.Int).SetBytes(vb)

		out = append(out, TxValueTransfer{
			From:     from,
			To:       to,
			Depth:    int(depthU),
			CallType: callType,
			Value:    val,
		})
	}
	return out, nil
}

