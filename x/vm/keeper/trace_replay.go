package keeper

import (
	"encoding/json"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
)

// invalidPredecessorError recognizes only transaction gas validation errors.
// Do not accept a joined error, which may also contain an infrastructure error.
func invalidPredecessorError(err error) bool {
	for err != nil {
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			err = wrapped.Unwrap()
			continue
		}
		return errors.Is(err, core.ErrIntrinsicGas) || errors.Is(err, core.ErrFloorDataGas) || errors.Is(err, core.ErrGasUintOverflow)
	}
	return false
}

type deferredTraceHooks struct {
	hooks *tracing.Hooks
	exit  func(error, []byte)
	end   func(*ethtypes.Receipt)
	state traceStateDB
}

// traceStateDB follows the live EVM during execution, then the published SDK
// state during finalization. Tracers may retain this interface at OnTxStart.
type traceStateDB struct {
	tracing.StateDB
}

func deferRootTrace(base *tracing.Hooks) *deferredTraceHooks {
	copyHooks := *base
	d := &deferredTraceHooks{hooks: &copyHooks}
	copyHooks.OnTxStart = func(env *tracing.VMContext, tx *ethtypes.Transaction, from common.Address) {
		d.state.StateDB = env.StateDB
		traceCtx := *env
		traceCtx.StateDB = &d.state
		if base.OnTxStart != nil {
			base.OnTxStart(&traceCtx, tx, from)
		}
	}
	copyHooks.OnExit = func(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
		if depth != 0 {
			if base.OnExit != nil {
				base.OnExit(depth, output, gasUsed, err, reverted)
			}
			return
		}
		output = append([]byte(nil), output...)
		d.exit = func(finalErr error, finalOutput []byte) {
			if finalErr != nil {
				output, err, reverted = finalOutput, finalErr, true
			}
			if base.OnExit != nil {
				base.OnExit(depth, output, gasUsed, err, reverted)
			}
		}
	}
	copyHooks.OnTxEnd = func(receipt *ethtypes.Receipt, err error) {
		d.end = func(finalReceipt *ethtypes.Receipt) {
			if finalReceipt != nil {
				receipt = finalReceipt
				// The root exit already carries VM failure; a completed receipt
				// must still reach tracers so they record final charged gas.
				err = nil
			}
			if base.OnTxEnd != nil {
				base.OnTxEnd(receipt, err)
			}
		}
	}
	return d
}

func (d *deferredTraceHooks) finish(finalErr error, output []byte, receipt *ethtypes.Receipt, state tracing.StateDB) {
	d.state.StateDB = state
	if d.exit != nil {
		d.exit(finalErr, output)
	}
	if d.end != nil {
		d.end(receipt)
	}
}

// finalizeLegacyCallTrace makes the final root outcome authoritative for the
// legacy JS call tracer, which otherwise prefers errors cached by opcode hooks.
// Adapt its known result schema rather than emitting a synthetic EVM fault.
func finalizeLegacyCallTrace(tracer *tracers.Tracer) {
	var rootErr error
	var rootOutput []byte
	onExit := tracer.OnExit
	tracer.OnExit = func(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
		if depth == 0 {
			rootErr = err
			rootOutput = common.CopyBytes(output)
		}
		onExit(depth, output, gasUsed, err, reverted)
	}
	getResult := tracer.GetResult
	tracer.GetResult = func() (json.RawMessage, error) {
		raw, err := getResult()
		if err != nil || rootErr == nil {
			return raw, err
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		result["error"], err = json.Marshal(rootErr.Error())
		if err != nil {
			return nil, err
		}
		if errors.Is(rootErr, vm.ErrExecutionReverted) && len(rootOutput) > 0 {
			result["output"], err = json.Marshal(hexutil.Encode(rootOutput))
			if err != nil {
				return nil, err
			}
		} else {
			delete(result, "output")
		}
		return json.Marshal(result)
	}
}
