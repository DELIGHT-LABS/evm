package keeper

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"

	rpctypes "github.com/cosmos/evm/rpc/types"
	"github.com/cosmos/evm/x/vm/statedb"
	"github.com/cosmos/evm/x/vm/types"

	errorsmod "cosmossdk.io/errors"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

type txCandidateInput struct {
	msg       core.Message
	txType    uint8
	cfg       *statedb.EVMConfig
	txConfig  statedb.TxConfig
	overrides *rpctypes.StateOverride
	simulate  bool
}

type txCandidateResult struct {
	response   *types.MsgEthereumTxResponse
	receipt    *ethtypes.Receipt
	rawEVMGas  uint64
	hookGas    uint64
	chargedGas uint64
	outOfGas   bool
	hookFailed bool
}

func (r *txCandidateResult) setChargedGas(gas uint64) {
	r.chargedGas = gas
	if r.response != nil {
		r.response.GasUsed = gas
	}
	if r.receipt != nil {
		r.receipt.GasUsed = gas
	}
}

func (k *Keeper) runTxCandidate(parentCtx sdk.Context, input txCandidateInput) (*txCandidateResult, error) {
	if input.cfg == nil {
		return nil, fmt.Errorf("nil EVM config")
	}

	// create a cache context to revert state. The cache context is only committed when both tx and hooks executed successfully.
	// Didn't use `Snapshot` because the context stack has exponential complexity on certain operations,
	// thus restricted to be used only inside `ApplyMessage`.
	candidateCtx, commitCandidate := parentCtx.CacheContext()
	var observed *txCandidateResult
	defer func() {
		if observed != nil && !input.simulate && (observed.hookFailed || observed.outOfGas) {
			k.Logger(parentCtx).Error(
				"tx post processing failed",
				"tx_hash", input.txConfig.TxHash.Hex(),
				"tx_type", input.txType,
				"tx_index", input.txConfig.TxIndex,
				"evm_raw_gas_used", observed.rawEVMGas,
				"post_hook_gas_used", observed.hookGas,
				"charged_gas_used", observed.chargedGas,
				"gas_limit", input.msg.GasLimit,
				"out_of_gas", observed.outOfGas,
			)
		}
	}()
	evmCtx := buildTraceCtx(candidateCtx, input.msg.GasLimit)

	// tx-wide trace collection (non-consensus; best-effort)
	collector := newTxTraceCollector()
	var innerTracer *tracing.Hooks
	if k.tracer != "" {
		innerTracer = k.Tracer(evmCtx, input.msg, types.GetEthChainConfig())
	}
	wrapperTracer := newTxTraceHooks(innerTracer, collector)
	// pass true to commit the StateDB
	stateDB := statedb.New(evmCtx, k, input.txConfig)
	response, rawEVMGas, err := k.applyMessageWithConfig(
		evmCtx,
		stateDB,
		input.msg,
		wrapperTracer,
		true,
		false,
		input.cfg,
		input.txConfig,
		false,
		input.overrides,
	)
	if err != nil {
		return nil, err
	}

	result := &txCandidateResult{
		response:  response,
		rawEVMGas: rawEVMGas,
	}
	observed = result
	if response == nil {
		return nil, fmt.Errorf("EVM execution returned no response")
	}
	if result.rawEVMGas > input.msg.GasLimit {
		result.outOfGas = true
		result.setChargedGas(input.msg.GasLimit)
		return result, nil
	}

	activeCtx := evmCtx
	if response.Failed() {
		// If the tx failed we discard the old context and create a new one, so
		// PostTxProcessing can persist data even if the tx fails.
		candidateCtx, commitCandidate = parentCtx.CacheContext()
		activeCtx = buildTraceCtx(candidateCtx, input.msg.GasLimit)
	}

	ethLogs := types.LogsToEthereum(response.Logs)
	receipt := newCandidateReceipt(parentCtx, input, response, ethLogs)
	result.receipt = receipt

	// Persist tx-wide trace into the currently active cache context so that
	// PostTxProcessing can read it, and so the failed-tx path (tmpCtx reset)
	// writes into the correct object store.
	persistTxTraceObject(activeCtx, k.objectKey, uint64(input.txConfig.TxIndex), collector)
	hookLimit := input.msg.GasLimit - result.rawEVMGas
	hookGas, hookOutOfGas, hookErr := executePostTxHooks(activeCtx, hookLimit, func(hookCtx sdk.Context) error {
		if input.simulate {
			return k.estimatePostTxProcessing(hookCtx, input.msg.From, input.msg, receipt)
		}
		return k.PostTxProcessing(hookCtx, input.msg.From, input.msg, receipt)
	})
	result.hookGas = hookGas

	switch {
	case hookOutOfGas:
		result.outOfGas = true
		result.setChargedGas(input.msg.GasLimit)
		markHookFailure(response, receipt, vm.ErrOutOfGas.Error(), nil)
		return result, nil
	case hookErr != nil:
		result.hookFailed = true
		markHookError(response, receipt, hookErr)
	}

	chargedGas, err := calculateChargedGas(
		input.msg.GasLimit,
		result.rawEVMGas,
		result.hookGas,
		input.cfg.FeeMarketParams.MinGasMultiplier,
	)
	if err != nil {
		if errors.Is(err, types.ErrGasOverflow) {
			return nil, err
		}
		result.outOfGas = true
		result.setChargedGas(input.msg.GasLimit)
		markHookFailure(response, receipt, vm.ErrOutOfGas.Error(), nil)
		return result, nil
	}
	result.setChargedGas(chargedGas)

	// Since the post-processing can alter the log, we need to update the result
	if response.Failed() {
		markHookFailure(response, receipt, response.VmError, response.Ret)
	} else {
		receipt.Status = ethtypes.ReceiptStatusSuccessful
		receipt.Bloom = ethtypes.CreateBloom(receipt)
		response.Logs = types.NewLogsFromEth(receipt.Logs)
		if len(receipt.Logs) > 0 {
			k.SetTxBloom(activeCtx, new(big.Int).SetBytes(receipt.Bloom.Bytes()))
		}
	}

	if !input.simulate && !result.hookFailed {
		commitCandidate()
	}
	return result, nil
}

func newCandidateReceipt(parentCtx sdk.Context, input txCandidateInput, response *types.MsgEthereumTxResponse, logs []*ethtypes.Log) *ethtypes.Receipt {
	var contractAddr common.Address
	if input.msg.To == nil {
		contractAddr = crypto.CreateAddress(input.msg.From, input.msg.Nonce)
	}

	status := ethtypes.ReceiptStatusSuccessful
	if response.Failed() {
		status = ethtypes.ReceiptStatusFailed
	}
	receipt := &ethtypes.Receipt{
		Type:              input.txType,
		PostState:         nil,
		CumulativeGasUsed: calculateCumulativeGasFromEthResponse(parentCtx.GasMeter(), response),
		Logs:              logs,
		TxHash:            input.txConfig.TxHash,
		ContractAddress:   contractAddr,
		GasUsed:           response.GasUsed,
		BlockHash:         common.BytesToHash(parentCtx.HeaderHash()),
		BlockNumber:       big.NewInt(parentCtx.BlockHeight()),
		TransactionIndex:  input.txConfig.TxIndex,
		Status:            status,
	}
	receipt.Bloom = ethtypes.CreateBloom(receipt)
	return receipt
}

func markHookError(response *types.MsgEthereumTxResponse, receipt *ethtypes.Receipt, hookErr error) {
	// If hooks returns an error, revert the whole tx.
	// If the error carries revert data (ABI-encoded custom error),
	// propagate it so clients can decode the reason.
	var revertData interface{ RevertData() []byte }
	if errors.As(hookErr, &revertData) {
		markHookFailure(response, receipt, vm.ErrExecutionReverted.Error(), revertData.RevertData())
		return
	}
	markHookFailure(response, receipt, errorsmod.Wrap(hookErr, "failed to execute post transaction processing").Error(), nil)
}

func markHookFailure(response *types.MsgEthereumTxResponse, receipt *ethtypes.Receipt, vmError string, revertData []byte) {
	// If the tx failed in post processing hooks, we should clear all log-related data
	// to match EVM behavior where transaction reverts clear all effects including logs
	response.VmError = vmError
	response.Ret = revertData
	response.Logs = nil
	receipt.Status = ethtypes.ReceiptStatusFailed
	receipt.Logs = nil
	receipt.Bloom = ethtypes.Bloom{} // Clear bloom filter
}

func (k *Keeper) finalizeTransactionGas(ctx sdk.Context, msg core.Message, result *txCandidateResult) error {
	if result == nil || result.response == nil || result.receipt == nil {
		return fmt.Errorf("cannot finalize empty transaction candidate")
	}
	chargedGas := result.chargedGas
	previous := k.GetTransientGasUsed(ctx)
	if previous > ^uint64(0)-chargedGas {
		return errorsmod.Wrap(types.ErrGasOverflow, "transient gas used")
	}
	cumulative := previous + chargedGas

	result.setChargedGas(chargedGas)
	result.receipt.CumulativeGasUsed = cumulative

	// refund gas to match the Ethereum gas consumption instead of the default SDK one.
	remainingGas := msg.GasLimit - chargedGas
	if err := k.RefundGas(ctx, msg, remainingGas, types.GetEVMCoinDenom()); err != nil {
		return errorsmod.Wrapf(err, "failed to refund leftover gas to sender %s", msg.From)
	}
	actualCumulative, err := k.AddTransientGasUsed(ctx, chargedGas)
	if err != nil {
		return errorsmod.Wrap(err, "failed to add transient gas used")
	}
	if actualCumulative != cumulative {
		return errorsmod.Wrapf(types.ErrGasOverflow, "transient gas mismatch: got %d, expected %d", actualCumulative, cumulative)
	}
	// reset the gas meter for current cosmos transaction
	k.ResetGasMeterAndConsumeGas(ctx, cumulative)
	return nil
}
