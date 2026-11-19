package types_test

import (
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/types"
)

type testCarrier struct {
	err  error
	data []byte
}

func (c testCarrier) Error() string      { return "carrier error" }
func (c testCarrier) RevertData() []byte { return c.data }
func (c testCarrier) Unwrap() error      { return c.err }

type testJoinCarrier struct {
	errs []error
	data []byte
}

func (c testJoinCarrier) Error() string      { return "join carrier" }
func (c testJoinCarrier) RevertData() []byte { return c.data }
func (c testJoinCarrier) Unwrap() []error    { return c.errs }

func TestDomainRevertData(t *testing.T) {
	data := []byte{0xde, 0xad, 0xbe, 0xef}
	carrier := testCarrier{data: data}
	joinCarrier := testJoinCarrier{data: data, errs: []error{errors.New("child")}}
	child := testCarrier{data: []byte{0xca, 0xfe}}
	testCases := []struct {
		name string
		err  error
		want []byte
		ok   bool
	}{
		{"non-empty carrier", carrier, data, true},
		{"wrapped carrier", fmt.Errorf("wrapped: %w", carrier), data, true},
		{"carrier wrapping join", testCarrier{data: data, err: errors.Join(errors.New("a"), errors.New("b"))}, data, true},
		{"join carrier with own data", joinCarrier, data, true},
		{"empty carrier", testCarrier{data: []byte{}}, nil, false},
		{"empty carrier blocks child", testCarrier{data: []byte{}, err: carrier}, nil, false},
		{"plain error", errors.New("plain error"), nil, false},
		{"join blocks carrier children", errors.Join(carrier, carrier), nil, false},
		{"non-empty RevertError", types.NewExecErrorWithReason(data), data, true},
		{"empty RevertError", types.NewExecErrorWithReason([]byte{}), nil, false},
		{"nil error", nil, nil, false},
		{"multiple single wraps", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", carrier)), data, true},
		{"wrapped join blocks carrier child", fmt.Errorf("wrapped: %w", errors.Join(carrier)), nil, false},
		{"empty join carrier blocks child", testJoinCarrier{errs: []error{carrier}}, nil, false},
		{"own data beats child data", testCarrier{data: data, err: child}, data, true},
		{"join own data beats child data", testJoinCarrier{data: data, errs: []error{child}}, data, true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := types.DomainRevertData(tc.err)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}

	t.Run("returned data is a copy", func(t *testing.T) {
		data := []byte{0xde, 0xad, 0xbe, 0xef}
		got, ok := types.DomainRevertData(testCarrier{data: data})
		require.True(t, ok)
		require.Equal(t, data, got)
		got[0] ^= 0xff
		require.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, data)
	})

	t.Run("constructor and getter copy data", func(t *testing.T) {
		data := []byte{0xde, 0xad, 0xbe, 0xef}
		require.Nil(t, types.NewRevertData(nil))
		require.Nil(t, types.NewRevertData([]byte{}))
		consCarrier := types.NewRevertData(data)
		require.NotNil(t, consCarrier)
		require.Equal(t, data, consCarrier.RevertData())
		data[0] ^= 0xff
		returned := consCarrier.RevertData()
		require.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, returned)
		returned[0] ^= 0xff
		require.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, consCarrier.RevertData())
	})
}

func TestNewChainIDMismatchErrorCopiesInputs(t *testing.T) {
	expected := new(big.Int).Lsh(big.NewInt(1), 100)
	actual := new(big.Int).Add(expected, big.NewInt(1))
	wantExpected, wantActual := new(big.Int).Set(expected), new(big.Int).Set(actual)

	err := types.NewChainIDMismatchError(expected, actual)
	require.Equal(t, wantExpected, err.Expected)
	require.Equal(t, wantActual, err.Actual)
	message := err.Error()

	expected.SetInt64(1)
	actual.SetInt64(2)
	require.Equal(t, wantExpected, err.Expected)
	require.Equal(t, wantActual, err.Actual)
	require.EqualError(t, err, message)
}

func TestNewExecErrorWithReason(t *testing.T) {
	testCases := []struct {
		name         string
		errorMessage string
		revertReason []byte
		data         string
	}{
		{
			"Empty reason",
			"execution reverted",
			nil,
			"0x",
		},
		{
			"With unpackable reason",
			"execution reverted",
			[]byte("a"),
			"0x61",
		},
		{
			"With packable reason but empty reason",
			"execution reverted",
			types.RevertSelector,
			"0x08c379a0",
		},
		{
			"With packable reason with reason",
			"execution reverted: COUNTER_TOO_LOW",
			hexutil.MustDecode("0x08C379A00000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000F434F554E5445525F544F4F5F4C4F570000000000000000000000000000000000"),
			"0x08c379a00000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000f434f554e5445525f544f4f5f4c4f570000000000000000000000000000000000",
		},
		{
			"With known custom error (QueryFailed)",
			"execution reverted",
			func() []byte {
				selector := crypto.Keccak256([]byte("QueryFailed(string,string)"))[:4]
				stringTy, err := abi.NewType("string", "", nil)
				require.NoError(t, err)
				packed, packErr := abi.Arguments{{Type: stringTy}, {Type: stringTy}}.Pack("getClientState", "not found")
				require.NoError(t, packErr)
				out := make([]byte, 0, 4+len(packed))
				out = append(out, selector...)
				out = append(out, packed...)
				return out
			}(),
			"0xeb02196500000000000000000000000000000000000000000000000000000000000000400000000000000000000000000000000000000000000000000000000000000080000000000000000000000000000000000000000000000000000000000000000e676574436c69656e74537461746500000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000096e6f7420666f756e640000000000000000000000000000000000000000000000",
		},
	}

	for _, tc := range testCases {
		errWithReason := types.NewExecErrorWithReason(tc.revertReason)
		require.Equal(t, tc.errorMessage, errWithReason.Error())
		require.Equal(t, tc.data, errWithReason.ErrorData())
		require.Equal(t, 3, errWithReason.ErrorCode())
	}
}
