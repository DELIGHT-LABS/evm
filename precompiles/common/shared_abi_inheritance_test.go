package common_test

import (
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/precompiles/bank"
	"github.com/cosmos/evm/precompiles/bech32"
	cmn "github.com/cosmos/evm/precompiles/common"
	"github.com/cosmos/evm/precompiles/distribution"
	"github.com/cosmos/evm/precompiles/erc20"
	"github.com/cosmos/evm/precompiles/gov"
	"github.com/cosmos/evm/precompiles/ics02"
	"github.com/cosmos/evm/precompiles/ics20"
	"github.com/cosmos/evm/precompiles/slashing"
	"github.com/cosmos/evm/precompiles/staking"
	"github.com/cosmos/evm/precompiles/werc20"
)

func TestEffectivePrecompileABIsInheritSharedErrors(t *testing.T) {
	tests := map[string]abi.ABI{
		"common":       cmn.SharedErrorABI,
		"bank":         bank.ABI,
		"bech32":       bech32.ABI,
		"distribution": distribution.ABI,
		"erc20":        erc20.ABI,
		"gov":          gov.ABI,
		"ics02":        ics02.ABI,
		"ics20":        ics20.ABI,
		"slashing":     slashing.ABI,
		"staking":      staking.ABI,
		"werc20":       werc20.ABI,
	}
	for name, contractABI := range tests {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, cmn.ValidateSharedErrorABI(contractABI))
		})
	}

	sharedPageRequest := cmn.SharedErrorABI.Errors[cmn.SolidityErrInvalidPageRequest]
	require.Equal(t, "InvalidPageRequest(string,uint256,string)", sharedPageRequest.Sig)

	definition, ok := slashing.ABI.Errors[slashing.SolidityErrSlashingInputInvalid]
	require.True(t, ok)
	require.Equal(t, "SlashingInputInvalid(string,string)", definition.Sig)

	require.NoError(t, cmn.ValidateCosmosErrorRegistry(erc20.ABI, nil, cmn.SharedSDKErrorMappings(), cmn.ApprovedOverrideDeclarations().ForABI("ERC20I")))
	require.NoError(t, cmn.ValidateCosmosErrorRegistry(werc20.ABI, nil, cmn.SharedSDKErrorMappings(), cmn.ApprovedOverrideDeclarations().ForABI("IWERC20")))
}

func TestSharedABIContainsAdmissionErrors(t *testing.T) {
	tests := []struct {
		name     string
		sig      string
		selector string
	}{
		{cmn.SolidityErrInsufficientFee, "InsufficientFee()", "0x025dbdd4"},
		{cmn.SolidityErrNonceTooLow, "NonceTooLow()", "0xd24d82a4"},
		{cmn.SolidityErrNonceGap, "NonceGap()", "0xe13c997f"},
		{cmn.SolidityErrIntrinsicGasTooLow, "IntrinsicGasTooLow()", "0xb524a1ce"},
		{cmn.SolidityErrFloorDataGasTooLow, "FloorDataGasTooLow()", "0x9e9f6ed7"},
		{cmn.SolidityErrTipAboveFeeCap, "TipAboveFeeCap()", "0xbb15f854"},
		{cmn.SolidityErrFeeCapTooHigh, "FeeCapTooHigh()", "0x02e22b83"},
		{cmn.SolidityErrTipTooHigh, "TipTooHigh()", "0x95521dd2"},
		{cmn.SolidityErrGasPriceTooLow, "GasPriceTooLow()", "0x8c19df83"},
		{cmn.SolidityErrGasLimitExceeded, "GasLimitExceeded()", "0xbe9179a6"},
		{cmn.SolidityErrInvalidSender, "InvalidSender()", "0xddb5de5e"},
		{cmn.SolidityErrChainIdMismatch, "ChainIdMismatch(uint256,uint256)", "0x21967608"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			definition, ok := cmn.SharedErrorABI.Errors[tc.name]
			require.True(t, ok)
			require.Equal(t, tc.sig, definition.Sig)
			require.Equal(t, tc.selector, hexutil.Encode(definition.ID[:4]))
		})
	}
}
