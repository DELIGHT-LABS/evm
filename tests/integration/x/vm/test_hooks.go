package vm

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/cosmos/evm/contracts"
	"github.com/cosmos/evm/x/vm/keeper"
	"github.com/cosmos/evm/x/vm/statedb"
	"github.com/cosmos/evm/x/vm/types"

	sdkmath "cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// LogRecordHook records all the logs
type LogRecordHook struct {
	Logs []*ethtypes.Log
}

func (dh *LogRecordHook) PostTxProcessing(_ sdk.Context, _ common.Address, _ core.Message, receipt *ethtypes.Receipt) error {
	dh.Logs = receipt.Logs
	return nil
}

// FailureHook always fail
type FailureHook struct{}

func (dh *FailureHook) PostTxProcessing(_ sdk.Context, _ common.Address, _ core.Message, _ *ethtypes.Receipt) error {
	return errors.New("post tx processing failed")
}

func (dh *FailureHook) EstimatePostTxProcessing(ctx sdk.Context, sender common.Address, msg core.Message, receipt *ethtypes.Receipt) error {
	return dh.PostTxProcessing(ctx, sender, msg, receipt)
}

type productionOnlyHook struct {
	Calls int
}

func (h *productionOnlyHook) PostTxProcessing(_ sdk.Context, _ common.Address, _ core.Message, _ *ethtypes.Receipt) error {
	h.Calls++
	return nil
}

type hookRevertError struct {
	data []byte
}

func (e hookRevertError) Error() string      { return "post tx hook reverted" }
func (e hookRevertError) RevertData() []byte { return e.data }

type RevertHook struct {
	Data []byte
}

func (h RevertHook) PostTxProcessing(_ sdk.Context, _ common.Address, _ core.Message, _ *ethtypes.Receipt) error {
	return hookRevertError{data: h.Data}
}

func (h RevertHook) EstimatePostTxProcessing(ctx sdk.Context, sender common.Address, msg core.Message, receipt *ethtypes.Receipt) error {
	return h.PostTxProcessing(ctx, sender, msg, receipt)
}

type NestedEVMGasHook struct {
	keeper   *keeper.Keeper
	contract common.Address
	GasUsed  uint64
}

func (h *NestedEVMGasHook) PostTxProcessing(ctx sdk.Context, from common.Address, _ core.Message, _ *ethtypes.Receipt) error {
	data, err := contracts.ERC20MinterBurnerDecimalsContract.ABI.Pack("balanceOf", from)
	if err != nil {
		return err
	}
	before := ctx.GasMeter().GasConsumed()
	response, err := h.keeper.CallEVMViewWithData(
		ctx,
		from,
		&h.contract,
		data,
		new(big.Int).SetUint64(ctx.GasMeter().GasRemaining()),
	)
	if err != nil {
		return err
	}
	h.GasUsed = response.GasUsed
	if delta := ctx.GasMeter().GasConsumed() - before; delta != response.GasUsed {
		return fmt.Errorf("nested EVM gas charged %d, response used %d", delta, response.GasUsed)
	}
	return nil
}

func (h *NestedEVMGasHook) EstimatePostTxProcessing(ctx sdk.Context, sender common.Address, msg core.Message, receipt *ethtypes.Receipt) error {
	return h.PostTxProcessing(ctx, sender, msg, receipt)
}

// txTraceByReceiptIndexHook looks up the candidate tx trace using the receipt
// index, matching downstream hooks that call GetTxTrace(ctx, receipt.TransactionIndex).
type txTraceByReceiptIndexHook struct {
	keeper       *keeper.Keeper
	ReceiptIndex uint
	Touches      int
	Transfers    int
	HookGas      uint64
}

func (h *txTraceByReceiptIndexHook) PostTxProcessing(ctx sdk.Context, _ common.Address, _ core.Message, receipt *ethtypes.Receipt) error {
	h.ReceiptIndex = receipt.TransactionIndex
	before := ctx.GasMeter().GasConsumed()
	touches, transfers := h.keeper.GetTxTrace(ctx, uint64(receipt.TransactionIndex))
	h.Touches = len(touches)
	h.Transfers = len(transfers)
	if h.Touches > 0 {
		ctx.GasMeter().ConsumeGas(uint64(h.Touches)*1042, "policy get per call touch")
	}
	h.HookGas = ctx.GasMeter().GasConsumed() - before
	return nil
}

func (h *txTraceByReceiptIndexHook) EstimatePostTxProcessing(ctx sdk.Context, sender common.Address, msg core.Message, receipt *ethtypes.Receipt) error {
	return h.PostTxProcessing(ctx, sender, msg, receipt)
}

func (s *KeeperTestSuite) TestEvmHooks() {
	testCases := []struct {
		msg       string
		setupHook func() types.EvmHooks
		expFunc   func(hook types.EvmHooks, result error)
	}{
		{
			"log collect hook",
			func() types.EvmHooks {
				return &LogRecordHook{}
			},
			func(hook types.EvmHooks, result error) {
				s.Require().NoError(result)
				s.Require().Equal(1, len((hook.(*LogRecordHook).Logs)))
			},
		},
		{
			"always fail hook",
			func() types.EvmHooks {
				return &FailureHook{}
			},
			func(_ types.EvmHooks, result error) {
				s.Require().Error(result)
			},
		},
	}

	for _, tc := range testCases {
		s.SetupTest()
		hook := tc.setupHook()
		s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(hook))

		k := s.Network.App.GetEVMKeeper()
		ctx := s.Network.GetContext()
		txHash := common.BigToHash(big.NewInt(1))
		vmdb := statedb.New(ctx, k, statedb.NewTxConfig(
			txHash,
			0,
		))

		vmdb.AddLog(&ethtypes.Log{
			Topics:  []common.Hash{},
			Address: s.Keyring.GetAddr(0),
		})
		logs := vmdb.Logs()
		receipt := &ethtypes.Receipt{
			TxHash: txHash,
			Logs:   logs,
		}
		result := k.PostTxProcessing(ctx, s.Keyring.GetAddr(0), core.Message{}, receipt)

		tc.expFunc(hook, result)
	}
}

func (s *KeeperTestSuite) TestPostTxProcessingFailureLogReversion() {
	s.SetupTest()

	// Set up the failing hook
	hook := &FailureHook{}
	s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(hook))

	k := s.Network.App.GetEVMKeeper()
	ctx := s.Network.GetContext()

	// Fund the sender
	sender := s.Keyring.GetKey(0)
	recipient := s.Keyring.GetAddr(1)
	baseDenom := types.GetEVMCoinDenom()
	coins := sdk.NewCoins(sdk.NewCoin(baseDenom, sdkmath.NewInt(1e18)))
	err := s.Network.App.GetBankKeeper().MintCoins(ctx, "mint", coins)
	s.Require().NoError(err)
	err = s.Network.App.GetBankKeeper().SendCoinsFromModuleToAccount(ctx, "mint", sender.AccAddr, coins)
	s.Require().NoError(err)

	// Create a simple transfer transaction
	transferArgs := types.EvmTxArgs{
		To:       &recipient,
		Amount:   big.NewInt(100),
		GasLimit: 21000,
		GasPrice: big.NewInt(1000000000),
	}
	tx, err := s.Factory.GenerateSignedEthTx(sender.Priv, transferArgs)
	s.Require().NoError(err)
	msg := tx.GetMsgs()[0].(*types.MsgEthereumTx)

	// Execute transaction - should fail in PostTxProcessing
	res, err := k.EthereumTx(ctx, msg)

	// Verify the transaction execution itself doesn't error, but PostTxProcessing fails
	s.Require().NoError(err, "EthereumTx should not return error")
	s.Require().NotNil(res)
	s.Require().NotEmpty(res.VmError, "Should have VmError due to PostTxProcessing failure")
	s.Require().Contains(res.VmError, "failed to execute post transaction processing")

	// Critical test: Verify logs are completely cleared
	s.Require().Nil(res.Logs, "res.Logs should be nil after PostTxProcessing failure")

	var gasUsed, failure string
	for _, event := range ctx.EventManager().Events() {
		if event.Type != types.EventTypeEthereumTx {
			continue
		}
		for _, attr := range event.Attributes {
			switch attr.Key {
			case types.AttributeKeyTxGasUsed:
				gasUsed = attr.Value
			case types.AttributeKeyEthereumTxFailed:
				failure = attr.Value
			}
		}
	}
	s.Require().Equal(fmt.Sprintf("%d", res.GasUsed), gasUsed)
	s.Require().Equal(res.VmError, failure)
}
