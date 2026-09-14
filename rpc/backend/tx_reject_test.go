package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/mempool"
	"github.com/cosmos/evm/mempool/txpool"
	common "github.com/cosmos/evm/precompiles/common"
	rpctypes "github.com/cosmos/evm/rpc/types"
	"github.com/cosmos/evm/testutil/constants"
	evmtypes "github.com/cosmos/evm/x/vm/types"

	errorsmod "cosmossdk.io/errors"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

func TestAdmissionMappings(t *testing.T) {
	cases := []struct {
		cause error
		name  string
	}{
		{core.ErrNonceTooLow, common.SolidityErrNonceTooLow},
		{mempool.ErrNonceLow, common.SolidityErrNonceTooLow},
		{core.ErrNonceTooHigh, common.SolidityErrNonceGap},
		{mempool.ErrNonceGap, common.SolidityErrNonceGap},
		{core.ErrInsufficientFunds, common.SolidityErrSDKInsufficientFunds},
		{core.ErrIntrinsicGas, common.SolidityErrIntrinsicGasTooLow},
		{core.ErrFloorDataGas, common.SolidityErrFloorDataGasTooLow},
		{core.ErrTipAboveFeeCap, common.SolidityErrTipAboveFeeCap},
		{core.ErrFeeCapVeryHigh, common.SolidityErrFeeCapTooHigh},
		{core.ErrTipVeryHigh, common.SolidityErrTipTooHigh},
		{txpool.ErrTxGasPriceTooLow, common.SolidityErrGasPriceTooLow},
		{txpool.ErrGasLimit, common.SolidityErrGasLimitExceeded},
		{txpool.ErrInvalidSender, common.SolidityErrInvalidSender},
		{ethtypes.ErrInvalidSig, common.SolidityErrInvalidSender},
		{sdkerrors.ErrInsufficientFee, common.SolidityErrInsufficientFee},
		{sdkerrors.ErrUnauthorized, common.SolidityErrSDKUnauthorized},
		{sdkerrors.ErrInsufficientFunds, common.SolidityErrSDKInsufficientFunds},
		{sdkerrors.ErrInvalidAddress, common.SolidityErrSDKInvalidAddress},
		{sdkerrors.ErrInvalidCoins, common.SolidityErrSDKInvalidCoins},
		{sdkerrors.ErrInvalidRequest, common.SolidityErrSDKInvalidRequest},
		{sdkerrors.ErrInvalidType, common.SolidityErrSDKInvalidType},
		{sdkerrors.ErrNotFound, common.SolidityErrSDKNotFound},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%d_%s", i, tc.name), func(t *testing.T) {
			for _, original := range []error{tc.cause, fmt.Errorf("outer: %w", tc.cause), errorsmod.Wrap(tc.cause, "cosmos"), fmt.Errorf("outer: %w", errorsmod.Wrap(tc.cause, "cosmos"))} {
				got := resolveTxRejectError(original)
				require.ErrorIs(t, got, tc.cause)
				require.Same(t, original, errors.Unwrap(got))
				require.Equal(t, original.Error(), got.Error())
				var encoded *rpctypes.TxRejectError
				require.ErrorAs(t, got, &encoded)
				require.Equal(t, -32000, encoded.ErrorCode())
				definition := common.SharedErrorABI.Errors[tc.name]
				require.Equal(t, definition.ID[:4], encoded.RevertData())
				if key, ok := common.ExtractCosmosErrorKey(original); ok {
					actual, found := common.ExtractCosmosErrorKey(got)
					require.True(t, found)
					require.Equal(t, key, actual)
				}
			}
			lookalike := errors.New(tc.cause.Error())
			require.Same(t, lookalike, resolveTxRejectError(lookalike))
		})
	}
	t.Run("registered unmapped", func(t *testing.T) {
		original := fmt.Errorf("ante: %w", sdkerrors.ErrInvalidSequence)
		got := resolveTxRejectError(original)
		require.ErrorIs(t, got, sdkerrors.ErrInvalidSequence)
		definition := common.SharedErrorABI.Errors[common.SolidityErrUnmappedCosmosError]
		data := got.(common.RevertDataCarrier).RevertData()
		require.Equal(t, definition.ID[:4], data[:4])
		args, err := definition.Inputs.Unpack(data[4:])
		require.NoError(t, err)
		require.Equal(t, []interface{}{sdkerrors.ErrInvalidSequence.Codespace(), sdkerrors.ErrInvalidSequence.ABCICode()}, args)
	})
	t.Run("chain ID arguments and cause", func(t *testing.T) {
		original := evmtypes.NewChainIDMismatchError(big.NewInt(9000), big.NewInt(9001))
		got := resolveTxRejectError(fmt.Errorf("outer: %w", original))
		var typed *evmtypes.ChainIDMismatchError
		require.ErrorAs(t, got, &typed)
		require.Same(t, original, typed)
		definition := common.SharedErrorABI.Errors[common.SolidityErrChainIdMismatch]
		args, err := definition.Inputs.Unpack(got.(common.RevertDataCarrier).RevertData()[4:])
		require.NoError(t, err)
		require.Equal(t, []interface{}{original.Expected, original.Actual}, args)
	})
	t.Run("value mapping precedes Cosmos mapping", func(t *testing.T) {
		original := errors.Join(core.ErrNonceTooLow, sdkerrors.ErrUnauthorized)
		got := resolveTxRejectError(original)
		require.ErrorIs(t, got, core.ErrNonceTooLow)
		require.ErrorIs(t, got, sdkerrors.ErrUnauthorized)
		require.Same(t, original, errors.Unwrap(got))
		definition := common.SharedErrorABI.Errors[common.SolidityErrNonceTooLow]
		require.Equal(t, definition.ID[:4], got.(common.RevertDataCarrier).RevertData())
	})
	t.Run("chain ID mapping precedes Cosmos mapping", func(t *testing.T) {
		chainID := evmtypes.NewChainIDMismatchError(big.NewInt(9000), big.NewInt(9001))
		original := errors.Join(chainID, sdkerrors.ErrUnauthorized)
		got := resolveTxRejectError(original)
		require.ErrorIs(t, got, chainID)
		require.ErrorIs(t, got, sdkerrors.ErrUnauthorized)
		require.Same(t, original, errors.Unwrap(got))
		definition := common.SharedErrorABI.Errors[common.SolidityErrChainIdMismatch]
		data := got.(common.RevertDataCarrier).RevertData()
		require.Equal(t, definition.ID[:4], data[:4])
		args, err := definition.Inputs.Unpack(data[4:])
		require.NoError(t, err)
		require.Equal(t, []interface{}{chainID.Expected, chainID.Actual}, args)
	})
}

// A downstream domain can supply arbitrary ABI bytes without any module import.
type admissionCarrier struct {
	cause error
	data  []byte
}

func (e *admissionCarrier) Error() string      { return e.cause.Error() }
func (e *admissionCarrier) Unwrap() error      { return e.cause }
func (e *admissionCarrier) RevertData() []byte { return e.data }

type admissionRPCError struct {
	cause error
	data  interface{}
}

func (e *admissionRPCError) Error() string          { return e.cause.Error() }
func (e *admissionRPCError) Unwrap() error          { return e.cause }
func (e *admissionRPCError) ErrorCode() int         { return 3 }
func (e *admissionRPCError) ErrorData() interface{} { return e.data }

type admissionCodeError struct{ error }

func (e *admissionCodeError) ErrorCode() int { return -32042 }
func (e *admissionCodeError) Unwrap() error  { return e.error }

func TestAdmissionPreservesTerminalAndInfrastructure(t *testing.T) {
	for _, original := range []error{
		nil, vm.ErrOutOfGas, fmt.Errorf("gas: %w", vm.ErrOutOfGas),
		errors.New("unknown"), context.Canceled, context.DeadlineExceeded,
		fmt.Errorf("context creation: %w", errors.New("database unavailable")),
		errorsmod.Wrap(sdkerrors.ErrIO, "read failed"),
		errors.Join(sdkerrors.ErrInsufficientFee, context.Canceled),
		&admissionCarrier{cause: vm.ErrOutOfGas, data: []byte{1, 2, 3, 4}},
	} {
		require.Equal(t, original, resolveTxRejectError(original))
	}
	// Existing Error(string) fallback is legitimate carrier data.
	fallback := common.NewRevertWithSolidityError(common.SharedErrorABI, "MissingError")
	wrapped := fmt.Errorf("downstream: %w", fallback)
	got := resolveTxRejectError(wrapped)
	require.ErrorIs(t, got, fallback)
	require.Equal(t, fallback.(common.RevertDataCarrier).RevertData(), got.(common.RevertDataCarrier).RevertData())
}

type admissionMempool struct {
	Mempool
	err     error
	inserts int
}

func (m *admissionMempool) Insert(context.Context, sdk.Tx) error { m.inserts++; return m.err }

func admissionJSONResponse(t *testing.T, server *rpc.Server, raw []byte) map[string]json.RawMessage {
	t.Helper()
	request, err := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "eth_sendRawTransaction", "params": []interface{}{hexutil.Encode(raw)}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", bytes.NewReader(request))
	req.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, req)
	require.Equal(t, 200, recorder.Code)
	var response map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}

func TestSendRawTransactionAdmissionTransport(t *testing.T) {
	configurator := evmtypes.NewEVMConfigurator()
	configurator.ResetTestConfig()
require.NoError(t,
        configurator.
                WithEVMCoinInfo(constants.ChainsCoinInfo[constants.ExampleChainID.EVMChainID]).
                Configure(),
  )
	require.NoError(t, evmtypes.SetChainConfig(evmtypes.DefaultChainConfig(constants.ExampleChainID.EVMChainID)))
	t.Cleanup(configurator.ResetTestConfig)
	backend := setupMockBackend(t)
	pool := &admissionMempool{}
	backend.Mempool = pool
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	chainID := backend.EvmChainID
	raw := func(chain *big.Int, gas uint64, tip, feeCap int64, validSignature bool) []byte {
		tx := ethtypes.NewTx(&ethtypes.DynamicFeeTx{ChainID: chain, Gas: gas, GasFeeCap: big.NewInt(feeCap), GasTipCap: big.NewInt(tip)})
		if validSignature {
			var signErr error
			tx, signErr = ethtypes.SignTx(tx, ethtypes.LatestSignerForChainID(chain), key)
			require.NoError(t, signErr)
		}
		data, marshalErr := tx.MarshalBinary()
		require.NoError(t, marshalErr)
		return data
	}
	valid := raw(chainID, 100000, 1, 2, true)
	syntheticData := crypto.Keccak256([]byte("ExternalPolicyDenied()"))[:4]
	synthetic := fmt.Errorf("app: %w", rpctypes.NewTxRejectError(sdkerrors.ErrUnauthorized, syntheticData))
	existing := fmt.Errorf("wrapped revert: %w", &admissionRPCError{cause: &admissionCarrier{cause: core.ErrIntrinsicGas, data: []byte{1, 2, 3, 4}}, data: "0xdeadbeef"})
	objectData := fmt.Errorf("wrapped: %w", &admissionRPCError{cause: errors.New("provider error"), data: map[string]interface{}{"reason": "existing"}})
	stringFallback := common.NewRevertWithSolidityError(common.SharedErrorABI, "MissingError")
	cases := []struct {
		name    string
		raw     []byte
		poolErr error
		custom  string
		code    int
		data    interface{}
		inserts int
	}{
		{name: "malformed", raw: []byte{0xff}, code: -32000},
		{name: "chain ID", raw: raw(new(big.Int).Add(chainID, big.NewInt(1)), 100000, 1, 2, true), custom: common.SolidityErrChainIdMismatch, code: -32000},
		{name: "sender recovery", raw: raw(chainID, 100000, 1, 2, false), custom: common.SolidityErrInvalidSender, code: -32000},
		{name: "ValidateBasic intrinsic", raw: raw(chainID, 1, 1, 2, true), custom: common.SolidityErrIntrinsicGasTooLow, code: -32000},
		{name: "ValidateBasic tip cap", raw: raw(chainID, 100000, 3, 2, true), custom: common.SolidityErrTipAboveFeeCap, code: -32000},
		{name: "pool funds", raw: valid, poolErr: fmt.Errorf("pool: %w", core.ErrInsufficientFunds), custom: common.SolidityErrSDKInsufficientFunds, code: -32000, inserts: 1},
		{name: "pool SDK fee", raw: valid, poolErr: errorsmod.Wrap(sdkerrors.ErrInsufficientFee, "ante"), custom: common.SolidityErrInsufficientFee, code: -32000, inserts: 1},
		{name: "pool infrastructure", raw: valid, poolErr: context.DeadlineExceeded, code: -32000, inserts: 1},
		{name: "downstream constructor", raw: valid, poolErr: synthetic, code: -32000, data: hexutil.Encode(syntheticData), inserts: 1},
		{name: "plain carrier", raw: valid, poolErr: fmt.Errorf("domain: %w", &admissionCarrier{cause: sdkerrors.ErrUnauthorized, data: syntheticData}), code: -32000, data: hexutil.Encode(syntheticData), inserts: 1},
		{name: "existing revert wins", raw: valid, poolErr: existing, code: 3, data: "0xdeadbeef", inserts: 1},
		{name: "existing object data", raw: valid, poolErr: objectData, code: 3, data: map[string]interface{}{"reason": "existing"}, inserts: 1},
		{name: "existing nil data", raw: valid, poolErr: &admissionRPCError{cause: synthetic}, code: 3, inserts: 1},
		{name: "existing Error string", raw: valid, poolErr: fmt.Errorf("domain: %w", stringFallback), code: -32000, data: hexutil.Encode(stringFallback.(common.RevertDataCarrier).RevertData()), inserts: 1},
		{name: "existing code", raw: valid, poolErr: &admissionCodeError{error: core.ErrNonceTooLow}, custom: common.SolidityErrNonceTooLow, code: -32042, inserts: 1},
		{name: "success", raw: valid, inserts: 1},
	}
	server := rpc.NewServer()
	t.Cleanup(server.Stop)
	require.NoError(t, server.RegisterName("eth", backend))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool.err, pool.inserts = tc.poolErr, 0
			response := admissionJSONResponse(t, server, tc.raw)
			require.Equal(t, tc.inserts, pool.inserts)
			if tc.code == 0 {
				require.NotContains(t, response, "error")
				var tx ethtypes.Transaction
				require.NoError(t, tx.UnmarshalBinary(tc.raw))
				require.JSONEq(t, fmt.Sprintf("%q", tx.Hash().Hex()), string(response["result"]))
				return
			}
			var rpcErr struct {
				Code    int         `json:"code"`
				Message string      `json:"message"`
				Data    interface{} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(response["error"], &rpcErr))
			require.Equal(t, tc.code, rpcErr.Code)
			_, goErr := backend.SendRawTransaction(context.Background(), tc.raw)
			require.EqualError(t, goErr, rpcErr.Message)
			if tc.custom != "" {
				definition := common.SharedErrorABI.Errors[tc.custom]
				expected := definition.ID[:4]
				if tc.custom == common.SolidityErrChainIdMismatch {
					args, packErr := definition.Inputs.Pack(chainID, new(big.Int).Add(chainID, big.NewInt(1)))
					require.NoError(t, packErr)
					expected = append(append([]byte{}, expected...), args...)
					require.Equal(t, fmt.Sprintf("chainId does not match node's (have=%v, want=%v)", new(big.Int).Add(chainID, big.NewInt(1)), chainID), rpcErr.Message)
				}
				require.Equal(t, hexutil.Encode(expected), rpcErr.Data)
			} else {
				require.Equal(t, tc.data, rpcErr.Data)
			}
			if tc.poolErr != nil {
				require.ErrorIs(t, goErr, tc.poolErr)
			}
		})
	}
}
