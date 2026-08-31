package global

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/vm"

	vmtracer "github.com/cosmos/evm/x/vm/tracer"
)

// TxCallTouch records a call frame observed during EVM execution.
type TxCallTouch struct {
	From     common.Address
	To       common.Address
	Depth    int
	CallType byte
}

// TxValueTransfer records a native balance transfer observed during EVM execution.
type TxValueTransfer struct {
	From     common.Address
	To       common.Address
	Depth    int
	CallType byte
	Value    *big.Int
}

// Collector is the standard transaction-trace contract. Applications can
// provide their own collection policy while retaining GetTxTrace compatibility.
type Collector interface {
	vmtracer.Tracer
	Touches() []TxCallTouch
	Transfers() []TxValueTransfer
}

// txTraceCollector records transaction-wide call touches and successful native
// balance transfers. Entries created inside reverted frames are discarded.
type txTraceCollector struct {
	touches   []TxCallTouch
	transfers []TxValueTransfer

	touchesFrameStart   []int
	transfersFrameStart []int
}

// NewCollector creates the default transaction-trace collector.
func NewCollector() Collector {
	return &txTraceCollector{}
}

// Hooks returns the EVM callbacks used by the collector.
func (c *txTraceCollector) Hooks() *tracing.Hooks {
	if c == nil {
		return nil
	}
	return &tracing.Hooks{
		OnEnter: c.onEnter,
		OnExit:  c.onExit,
	}
}

// Touches returns a copy of the collected call touches.
func (c *txTraceCollector) Touches() []TxCallTouch {
	if c == nil {
		return nil
	}
	return append([]TxCallTouch(nil), c.touches...)
}

// Transfers returns a deep copy of the collected native value transfers.
func (c *txTraceCollector) Transfers() []TxValueTransfer {
	if c == nil {
		return nil
	}
	transfers := make([]TxValueTransfer, len(c.transfers))
	for i, transfer := range c.transfers {
		transfers[i] = transfer
		if transfer.Value != nil {
			transfers[i].Value = new(big.Int).Set(transfer.Value)
		}
	}
	return transfers
}

func isNativeBalanceTransfer(typ byte, value *big.Int) bool {
	if value == nil || value.Sign() <= 0 {
		return false
	}
	// Only CALL/CREATE/CREATE2 can move native balance between accounts.
	// DELEGATECALL, STATICCALL, and CALLCODE can surface a value in tracing
	// callbacks without transferring it to the callee.
	return typ == byte(vm.CALL) || typ == byte(vm.CREATE) || typ == byte(vm.CREATE2)
}

func (c *txTraceCollector) onEnter(depth int, typ byte, from, to common.Address, _ []byte, _ uint64, value *big.Int) {
	c.touchesFrameStart = append(c.touchesFrameStart, len(c.touches))
	c.transfersFrameStart = append(c.transfersFrameStart, len(c.transfers))

	c.touches = append(c.touches, TxCallTouch{
		From:     from,
		To:       to,
		Depth:    depth,
		CallType: typ,
	})

	if isNativeBalanceTransfer(typ, value) {
		c.transfers = append(c.transfers, TxValueTransfer{
			From:     from,
			To:       to,
			Depth:    depth,
			CallType: typ,
			Value:    new(big.Int).Set(value),
		})
	}
}

func (c *txTraceCollector) onExit(_ int, _ []byte, _ uint64, _ error, reverted bool) {
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
