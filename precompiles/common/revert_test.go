package common_test

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/require"

	cmn "github.com/cosmos/evm/precompiles/common"
	"github.com/cosmos/evm/precompiles/staking"
	statedbmocks "github.com/cosmos/evm/x/vm/statedb/mocks"
	evmtypes "github.com/cosmos/evm/x/vm/types"
)

func TestReturnRevertError_WithCustomData(t *testing.T) {
	stateDB := statedbmocks.NewStateDB(t)
	evm := vm.NewEVM(vm.BlockContext{BlockNumber: big.NewInt(1), Time: 1}, stateDB, params.TestChainConfig, vm.Config{})

	customErr := cmn.NewRevertWithSolidityError(staking.ABI, "RequesterIsNotMsgSender", common.Address{0x1}, common.Address{0x2})
	var carrier *evmtypes.RevertWithData
	require.ErrorAs(t, customErr, &carrier)
	payload := carrier.RevertData()
	original := common.CopyBytes(payload)
	payload[0] ^= 0xff
	require.Equal(t, original, carrier.RevertData())

	ret, err := cmn.ReturnRevertError(evm, customErr)

	require.ErrorIs(t, err, vm.ErrExecutionReverted)
	require.Len(t, ret, 4+32+32)
	require.Equal(t, original, ret)
	require.Equal(t, ret, evm.ReturnData())
	definition := staking.ABI.Errors["RequesterIsNotMsgSender"]
	decoded, unpackErr := definition.Unpack(ret)
	require.NoError(t, unpackErr)
	require.Equal(t, []any{common.Address{0x1}, common.Address{0x2}}, decoded)

	ret[0] ^= 0xff
	require.Equal(t, original, carrier.RevertData())
}

func TestReturnRevertError_WithStringFallback(t *testing.T) {
	stateDB := statedbmocks.NewStateDB(t)
	evm := vm.NewEVM(vm.BlockContext{BlockNumber: big.NewInt(1), Time: 1}, stateDB, params.TestChainConfig, vm.Config{})

	ret, err := cmn.ReturnRevertError(evm, errors.New("fallback message"))

	require.ErrorIs(t, err, vm.ErrExecutionReverted)
	require.NotEmpty(t, ret)
}

func TestReturnRevertError_WhenSolidityErrorPackFails_FallsBackToErrorStringRevert(t *testing.T) {
	stateDB := statedbmocks.NewStateDB(t)
	evm := vm.NewEVM(vm.BlockContext{BlockNumber: big.NewInt(1), Time: 1}, stateDB, params.TestChainConfig, vm.Config{})

	// RequesterIsNotMsgSender(address,address) expects 2 args; provide 1 wrong-typed arg to force ABI pack failure.
	customErr := cmn.NewRevertWithSolidityError(staking.ABI, "RequesterIsNotMsgSender", 123)
	ret, err := cmn.ReturnRevertError(evm, customErr)

	require.ErrorIs(t, err, vm.ErrExecutionReverted)
	require.NotEmpty(t, ret)
	require.Equal(t, ret, evm.ReturnData())

	reason, unpackErr := abi.UnpackRevert(ret)
	require.NoError(t, unpackErr)
	require.Contains(t, reason, "failed to pack solidity custom error RequesterIsNotMsgSender")
}

func TestRevertDataCarrier_VMProtocol(t *testing.T) {
	data := []byte{0x12, 0x34, 0x56, 0x78}
	revert := cmn.NewRevertWithSolidityError(staking.ABI, "RequesterIsNotMsgSender", common.Address{0x1}, common.Address{0x2})

	var vmCarrier evmtypes.RevertDataCarrier
	require.ErrorAs(t, revert, &vmCarrier)

	require.NotEmpty(t, vmCarrier.RevertData())

	// Test with NewRevertData from evmtypes
	fromVM := evmtypes.NewRevertData(data)
	var asVM evmtypes.RevertDataCarrier
	require.ErrorAs(t, fromVM, &asVM)
	require.Equal(t, data, asVM.RevertData())
	var concrete *evmtypes.RevertWithData
	require.ErrorAs(t, fromVM, &concrete)
	require.Equal(t, data, concrete.RevertData())

	var empty evmtypes.RevertWithData
	require.EqualError(t, &empty, "execution reverted")
	require.Nil(t, empty.RevertData())
}
