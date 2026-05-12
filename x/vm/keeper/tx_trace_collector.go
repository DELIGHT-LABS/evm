package keeper

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"

	"github.com/cosmos/evm/x/vm/types"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// GetTxTrace returns tx-wide call touches and native value transfers for a given tx index within a block.
// Data is stored in the EVM object store by the tx-wide tracer hooks (best-effort; non-consensus).
func (k Keeper) GetTxTrace(ctx sdk.Context, txIndex uint64) (touches []types.TxCallTouch, transfers []types.TxValueTransfer) {
	store := ctx.ObjectStore(k.objectKey)

	if touchesBz, ok := store.Get(types.TxTraceKey(types.KeyPrefixObjectTxCallTouches, txIndex)).([]byte); ok && len(touchesBz) > 0 {
		if decoded, err := types.DecodeTxCallTouchesV1(touchesBz); err == nil {
			touches = decoded
		}
	}

	if transfersBz, ok := store.Get(types.TxTraceKey(types.KeyPrefixObjectTxValueTransfers, txIndex)).([]byte); ok && len(transfersBz) > 0 {
		if decoded, err := types.DecodeTxValueTransfersV1(transfersBz); err == nil {
			transfers = decoded
		}
	}

	return touches, transfers
}

type txTraceCollector struct {
	touches   []types.TxCallTouch
	transfers []types.TxValueTransfer

	touchesFrameStart   []int
	transfersFrameStart []int
}

func newTxTraceCollector() *txTraceCollector {
	return &txTraceCollector{}
}

func isNativeBalanceTransfer(typ byte, value *big.Int) bool {
	if value == nil || value.Sign() <= 0 {
		return false
	}
	// Only CALL/CREATE/CREATE2 can move native balance between accounts.
	// Other call types (e.g. DELEGATECALL/STATICCALL/CALLCODE) may still surface a value
	// via tracing hooks, but do not represent an actual balance transfer to the callee.
	return typ == byte(vm.CALL) || typ == byte(vm.CREATE) || typ == byte(vm.CREATE2)
}

func (c *txTraceCollector) onEnter(depth int, typ byte, from common.Address, to common.Address, value *big.Int) {
	c.touchesFrameStart = append(c.touchesFrameStart, len(c.touches))
	c.transfersFrameStart = append(c.transfersFrameStart, len(c.transfers))

	c.touches = append(c.touches, types.TxCallTouch{
		From:     from,
		To:       to,
		Depth:    depth,
		CallType: typ,
	})

	if isNativeBalanceTransfer(typ, value) {
		v := new(big.Int).Set(value)
		c.transfers = append(c.transfers, types.TxValueTransfer{
			From:     from,
			To:       to,
			Depth:    depth,
			CallType: typ,
			Value:    v,
		})
	}
}

func (c *txTraceCollector) onExit(reverted bool) {
	if reverted {
		if n := len(c.touchesFrameStart); n > 0 {
			start := c.touchesFrameStart[n-1]
			if start >= 0 && start <= len(c.touches) {
				c.touches = c.touches[:start]
			}
		}
		if n := len(c.transfersFrameStart); n > 0 {
			start := c.transfersFrameStart[n-1]
			if start >= 0 && start <= len(c.transfers) {
				c.transfers = c.transfers[:start]
			}
		}
	}

	if n := len(c.touchesFrameStart); n > 0 {
		c.touchesFrameStart = c.touchesFrameStart[:n-1]
	}
	if n := len(c.transfersFrameStart); n > 0 {
		c.transfersFrameStart = c.transfersFrameStart[:n-1]
	}
}

func newTxTraceHooks(inner *tracing.Hooks, collector *txTraceCollector) *tracing.Hooks {
	// ApplyMessageWithConfig assumes OnTxStart is non-nil when Tracer is set.
	// If we don't have an inner tracer, use a minimal base tracer to avoid panics
	// without installing opcode/gas hooks (performance).
	if inner == nil {
		inner = &tracing.Hooks{
			OnTxStart: func(_ *tracing.VMContext, _ *ethtypes.Transaction, _ common.Address) {},
		}
	}

	callInnerOnEnter := func(depth int, typ byte, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
		if inner != nil && inner.OnEnter != nil {
			inner.OnEnter(depth, typ, from, to, input, gas, value)
		}
	}
	callInnerOnExit := func(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
		if inner != nil && inner.OnExit != nil {
			inner.OnExit(depth, output, gasUsed, err, reverted)
		}
	}

	out := &tracing.Hooks{}
	if inner != nil {
		*out = *inner
	}

	out.OnEnter = func(depth int, typ byte, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
		callInnerOnEnter(depth, typ, from, to, input, gas, value)
		if collector != nil {
			collector.onEnter(depth, typ, from, to, value)
		}
	}

	out.OnExit = func(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
		callInnerOnExit(depth, output, gasUsed, err, reverted)
		if collector != nil {
			collector.onExit(reverted)
		}
	}

	return out
}

func persistTxTraceObject(ctx sdk.Context, objectKey storetypes.StoreKey, txIndex uint64, collector *txTraceCollector) {
	if collector == nil {
		return
	}
	store := ctx.ObjectStore(objectKey)

	if len(collector.touches) > 0 {
		if bz, err := types.EncodeTxCallTouchesV1(collector.touches); err == nil && len(bz) > 0 {
			store.Set(types.TxTraceKey(types.KeyPrefixObjectTxCallTouches, txIndex), bz)
		}
	}
	if len(collector.transfers) > 0 {
		if bz, err := types.EncodeTxValueTransfersV1(collector.transfers); err == nil && len(bz) > 0 {
			store.Set(types.TxTraceKey(types.KeyPrefixObjectTxValueTransfers, txIndex), bz)
		}
	}
}
