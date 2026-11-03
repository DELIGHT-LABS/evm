package common

const (
	// ErrNotRunInEvm is raised when a function is not called inside the EVM.
	ErrNotRunInEvm = "not run in EVM"
	// ErrRequesterIsNotMsgSender is raised when the requester address is not the same as the msg.sender.
	ErrRequesterIsNotMsgSender = "msg.sender address %s does not match the requester address %s"
	// ErrInvalidABI is raised when the ABI cannot be parsed.
	ErrInvalidABI = "invalid ABI: %w"
	// ErrInvalidAmount is raised when the amount cannot be cast to a big.Int.
	ErrInvalidAmount = "invalid amount: %v"
	// ErrInvalidHexAddress is raised when the hex address is not valid.
	ErrInvalidHexAddress = "invalid hex address address: %s"
	// ErrInvalidDelegator is raised when the delegator address is not valid.
	ErrInvalidDelegator = "invalid delegator address: %s"
	// ErrInvalidValidator is raised when the validator address is not valid.
	ErrInvalidValidator = "invalid validator address: %s"
	// ErrInvalidDenom is raised when the denom is not valid.
	ErrInvalidDenom = "invalid denom: %s"
	// ErrInvalidMsgType is raised when the transaction type is not valid for the given precompile.
	ErrInvalidMsgType = "invalid %s transaction type: %s"
	// ErrInvalidNumberOfArgs is raised when the number of arguments is not what is expected.
	ErrInvalidNumberOfArgs = "invalid number of arguments; expected %d; got: %d"
	// ErrUnknownMethod is raised when the method is not known.
	ErrUnknownMethod = "unknown method: %s"
	// ErrIntegerOverflow is raised when an integer overflow occurs.
	ErrIntegerOverflow = "integer overflow when increasing allowance"
	// ErrNegativeAmount is raised when an amount is negative.
	ErrNegativeAmount = "negative amount when decreasing allowance"
	// ErrInvalidType is raised when the provided type is different than the expected.
	ErrInvalidType = "invalid type for %s: expected %T, received %T"
	// ErrInvalidDescription is raised when the input description cannot be cast to stakingtypes.Description{}.
	ErrInvalidDescription = "invalid description: %v"
	// ErrInvalidCommission is raised when the input commission cannot be cast to stakingtypes.CommissionRates{}.
	ErrInvalidCommission = "invalid commission: %v"
	// ErrUnknownSolidityCustomError is raised when the ABI does not contain the provided custom error.
	ErrUnknownSolidityCustomError = "unknown solidity custom error: %s"
	// ErrPackSolidityCustomErrorFailed is raised when ABI packing custom error args fails.
	ErrPackSolidityCustomErrorFailed = "failed to pack solidity custom error %s: %s"

	// SolidityErrInvalidNumberOfArgs is invalid number of arguments
	SolidityErrInvalidNumberOfArgs = "InvalidNumberOfArgs"
	// SolidityErrInvalidPageRequest is invalid Cosmos pagination input
	SolidityErrInvalidPageRequest = "InvalidPageRequest"
	// SolidityErrInvalidAddress is invalid address
	SolidityErrInvalidAddress = "InvalidAddress"
	// SolidityErrInvalidAmount is invalid amount
	SolidityErrInvalidAmount = "InvalidAmount"
	// SolidityErrRequesterIsNotMsgSender is requester is not msg sender
	SolidityErrRequesterIsNotMsgSender = "RequesterIsNotMsgSender"
	// SolidityErrInvalidHeight is invalid height
	SolidityErrInvalidHeight = "InvalidHeight"
	// SolidityErrInvalidPubkey is invalid pubkey
	SolidityErrInvalidPubkey = "InvalidPubkey"
	// SolidityErrInvalidPubkeySize is invalid pubkey size
	SolidityErrInvalidPubkeySize = "InvalidPubkeySize"
	// SolidityErrABISetupFailed is abi setup failed
	SolidityErrABISetupFailed = "ABISetupFailed"
	// SolidityErrUnknownMethod is unknown method
	SolidityErrUnknownMethod = "UnknownMethod"
	// SolidityErrQueryFailed is query failed
	SolidityErrQueryFailed = "QueryFailed"
	// SolidityErrMsgServerFailed is msg server failed
	SolidityErrMsgServerFailed = "MsgServerFailed"
	// SolidityErrEventEmitFailed is event emit failed
	SolidityErrEventEmitFailed = "EventEmitFailed"

	// SolidityErrInsufficientFee is insufficient transaction fee.
	SolidityErrInsufficientFee = "InsufficientFee"
	// SolidityErrNonceTooLow is a transaction nonce below the account nonce.
	SolidityErrNonceTooLow = "NonceTooLow"
	// SolidityErrNonceGap is a transaction nonce rejected by the nonce gap policy.
	SolidityErrNonceGap = "NonceGap"
	// SolidityErrIntrinsicGasTooLow is gas below the transaction's intrinsic gas.
	SolidityErrIntrinsicGasTooLow = "IntrinsicGasTooLow"
	// SolidityErrFloorDataGasTooLow is gas below the transaction's floor data gas.
	SolidityErrFloorDataGasTooLow = "FloorDataGasTooLow"
	// SolidityErrTipAboveFeeCap is a priority fee above the transaction fee cap.
	SolidityErrTipAboveFeeCap = "TipAboveFeeCap"
	// SolidityErrFeeCapTooHigh is a transaction fee cap exceeding 256 bits.
	SolidityErrFeeCapTooHigh = "FeeCapTooHigh"
	// SolidityErrTipTooHigh is a transaction priority fee exceeding 256 bits.
	SolidityErrTipTooHigh = "TipTooHigh"
	// SolidityErrGasPriceTooLow is a gas price below the pool's minimum.
	SolidityErrGasPriceTooLow = "GasPriceTooLow"
	// SolidityErrGasLimitExceeded is a transaction gas limit exceeding the block limit.
	SolidityErrGasLimitExceeded = "GasLimitExceeded"
	// SolidityErrInvalidSender is an invalid transaction sender signature.
	SolidityErrInvalidSender = "InvalidSender"
	// SolidityErrChainIdMismatch is a transaction chain ID differing from the expected ID.
	SolidityErrChainIdMismatch = "ChainIdMismatch"
)
