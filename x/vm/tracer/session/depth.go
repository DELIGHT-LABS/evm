package session

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
)

// WithDepthOffset creates a session tracer that adds offset to depth-bearing
// EVM callbacks. Other callbacks are forwarded unchanged. Nil hooks produce a
// tracer with no hooks.
func WithDepthOffset(hooks *tracing.Hooks, offset int) *Tracer {
	if hooks == nil {
		return New(nil)
	}

	out := *hooks
	if hooks.OnEnter != nil {
		out.OnEnter = func(depth int, typ byte, from, to common.Address, input []byte, gas uint64, value *big.Int) {
			hooks.OnEnter(depth+offset, typ, from, to, input, gas, value)
		}
	}
	if hooks.OnExit != nil {
		out.OnExit = func(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
			hooks.OnExit(depth+offset, output, gasUsed, err, reverted)
		}
	}
	if hooks.OnOpcode != nil {
		out.OnOpcode = func(pc uint64, op byte, gas, cost uint64, scope tracing.OpContext, returnData []byte, depth int, err error) {
			hooks.OnOpcode(pc, op, gas, cost, scope, returnData, depth+offset, err)
		}
	}
	if hooks.OnFault != nil {
		out.OnFault = func(pc uint64, op byte, gas, cost uint64, scope tracing.OpContext, depth int, err error) {
			hooks.OnFault(pc, op, gas, cost, scope, depth+offset, err)
		}
	}
	return New(&out)
}
