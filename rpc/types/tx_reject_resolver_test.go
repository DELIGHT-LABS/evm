package types

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/core"
	"github.com/stretchr/testify/require"

	common "github.com/cosmos/evm/precompiles/common"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

func TestTxRejectEncodingFailureAndInitialization(t *testing.T) {
	original := core.ErrIntrinsicGas
	require.Same(t, original, encodeTxRejectError(original, txRejectABI, "MissingError"))
	require.Same(t, original, encodeTxRejectError(original, txRejectABI, common.SolidityErrIntrinsicGasTooLow, "unexpected argument"))
	require.Same(t, original, encodeTxRejectError(original, txRejectABI, common.SolidityErrChainIdMismatch, "bad", big.NewInt(1)))
	expected := txRejectABI.Errors[common.SolidityErrIntrinsicGasTooLow]
	for _, data := range [][]byte{nil, {1, 2, 3}, append(append([]byte{}, expected.ID[:4]...), 0), {0, 0, 0, 0}} {
		require.Same(t, original, checkedTxRejectError(original, NewTxRejectError(original, data), expected))
	}
	require.Same(t, original, checkedTxRejectError(original, errors.New("packing failed"), expected))
	require.Same(t, original, checkedTxRejectError(original, common.NewRevertWithSolidityError(txRejectABI, common.SolidityErrNonceGap), expected))
	require.NotSame(t, original, encodeTxRejectError(original, txRejectABI, common.SolidityErrIntrinsicGasTooLow))
	for _, mutate := range []func(abi.ABI){
		func(a abi.ABI) { delete(a.Errors, common.SolidityErrInsufficientFee) },
		func(a abi.ABI) { delete(a.Errors, common.SolidityErrChainIdMismatch) },
		func(a abi.ABI) { delete(a.Errors, "QueryFailed") },
		func(a abi.ABI) { d := a.Errors[common.SolidityErrChainIdMismatch]; d.ID[0] ^= 1; a.Errors[d.Name] = d },
		func(a abi.ABI) {
			d := a.Errors[common.SolidityErrChainIdMismatch]
			d.Inputs = nil
			a.Errors[d.Name] = d
		},
		func(a abi.ABI) {
			d := a.Errors[common.SolidityErrChainIdMismatch]
			d.Name = common.SolidityErrInsufficientFee
			a.Errors[common.SolidityErrChainIdMismatch] = d
		},
		func(a abi.ABI) {
			d := a.Errors[common.SolidityErrIntrinsicGasTooLow]
			d.Inputs = txRejectABI.Errors[common.SolidityErrChainIdMismatch].Inputs
			a.Errors[d.Name] = d
		},
	} {
		snapshot := mustTxRejectErrorABI(common.SharedErrorABI)
		mutate(snapshot)
		require.Panics(t, func() {
			common.MustNewErrorRegistry(mustTxRejectErrorABI(snapshot), txRejectErrorMappings...)
		})
	}
	source := mustTxRejectErrorABI(common.SharedErrorABI)
	snapshot := mustTxRejectErrorABI(source)
	mappings := append(common.SharedSDKErrorMappings(), common.NewCosmosErrorMapping(sdkerrors.ErrInsufficientFee, common.SolidityErrInsufficientFee))
	registry := common.MustNewCosmosErrorRegistry(snapshot, nil, mappings, nil)
	before := registry.Translate(snapshot, sdkerrors.ErrInsufficientFee)
	delete(source.Errors, common.SolidityErrInsufficientFee)
	mappings[len(mappings)-1].SolidityError = common.SolidityErrSDKUnauthorized
	after := registry.Translate(snapshot, sdkerrors.ErrInsufficientFee)
	require.Equal(t, common.MappingKindSharedSDK, after.Kind)
	require.Equal(t, before.Revert.(common.RevertDataCarrier).RevertData(), after.Revert.(common.RevertDataCarrier).RevertData())
	// The RPC-only fee mapping must not change precompile translation policy.
	shared := common.MustNewCosmosErrorRegistry(common.SharedErrorABI, nil, common.SharedSDKErrorMappings(), nil)
	require.Equal(t, common.MappingKindUnmapped, shared.Translate(common.SharedErrorABI, sdkerrors.ErrInsufficientFee).Kind)
}
