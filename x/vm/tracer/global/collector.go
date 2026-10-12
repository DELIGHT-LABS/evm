package global

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"

	vmtracer "github.com/cosmos/evm/x/vm/tracer"
)

// Collector retains attempts, raw facts and current frame outcomes in one
// execution journal. Session queries reference these same records.
type Collector interface {
	vmtracer.Journal
	Observe(vmtracer.Observer) (func(), error)
}

// txTraceCollector owns the execution journal, including reverted attempts.
type txTraceCollector struct {
	events         []vmtracer.Observation
	frames         map[uint64]*observationFrame
	stack          []uint64
	subscribers    []observationSubscriber
	nextFrame      uint64
	nextOccurrence uint64
	nextSubscriber uint64
	maxEvents      uint64
	code           func(common.Address) []byte
	err            error
	executionErr   error
}

type observationFrame struct {
	index  int
	parent uint64
	status vmtracer.FrameStatus
}

type observationSubscriber struct {
	id uint64
	fn vmtracer.Observer
}

const DefaultObservationBudget uint64 = 100_000

var (
	erc20TransferSelector     = []byte{0xa9, 0x05, 0x9c, 0xbb}
	erc20TransferFromSelector = []byte{0x23, 0xb8, 0x72, 0xdd}
)

// NewCollector creates the default transaction-trace collector.
func NewCollector() Collector {
	c, _ := NewCollectorWithBudget(DefaultObservationBudget)
	return c
}

func NewCollectorWithBudget(budget uint64) (Collector, error) {
	if budget == 0 {
		return nil, errors.New("collector requires a positive budget")
	}
	return &txTraceCollector{
		frames:    make(map[uint64]*observationFrame),
		maxEvents: budget,
	}, nil
}

// Hooks returns the EVM callbacks used by the collector.
func (c *txTraceCollector) Hooks() *tracing.Hooks {
	if c == nil {
		return nil
	}
	return &tracing.Hooks{
		OnEnter: c.onEnter,
		OnExit:  c.onExit,
		OnBalanceChange: func(addr common.Address, prev, next *big.Int, reason tracing.BalanceChangeReason) {
			c.emitObservation(vmtracer.Observation{
				Kind:            vmtracer.BalanceChange,
				Target:          addr,
				PreviousBalance: prev,
				CurrentBalance:  next,
				BalanceReason:   reason,
			})
		},
		OnLog: func(log *ethtypes.Log) {
			c.emitObservation(vmtracer.Observation{
				Kind: vmtracer.Log,
				Log:  log,
			})
		},
	}
}

func (c *txTraceCollector) Error() error {
	if c == nil {
		return nil
	}

	return errors.Join(c.err, c.executionErr)
}

// LatchError retains execution errors across EVM and observer rollback.
func (c *txTraceCollector) LatchError(err error) {
	if c != nil && c.executionErr == nil {
		c.executionErr = err
	}
}

// ExecutionError reports enforcement errors, including incomplete observation
// coverage while a policy observer is installed. Passive tracing errors alone
// do not change ordinary VM execution.
func (c *txTraceCollector) ExecutionError() error {
	if c == nil {
		return nil
	}

	return c.executionErr
}

func (c *txTraceCollector) collectionError(err error) {
	if c == nil || err == nil {
		return
	}
	if c.err == nil {
		c.err = err
	}
	if len(c.subscribers) > 0 {
		c.LatchError(c.err)
	}
}

func (c *txTraceCollector) LastOccurrence() uint64 {
	if c == nil {
		return 0
	}

	return c.nextOccurrence
}

// SetCodeReader supplies the current StateDB code at frame entry. Keeper binds
// it before execution, including state overrides and EIP-7702 authorizations.
func (c *txTraceCollector) SetCodeReader(reader func(common.Address) []byte) func() {
	if c == nil {
		return func() {}
	}

	previous := c.code
	c.code = reader

	return func() { c.code = previous }
}

// status combines the local outcome with any enclosing rollback. Attempts
// remain stored; a later parent revert changes their effective status on read.
func (c *txTraceCollector) status(id uint64) vmtracer.FrameStatus {
	f := c.frames[id]
	if f == nil {
		return vmtracer.FrameStatus{}
	}

	result := f.status
	for parent := f.parent; parent != 0; parent = c.frames[parent].parent {
		if c.frames[parent].status.Reverted {
			result.Reverted = true
			result.Success = false
			break
		}
	}

	return result
}

// CurrentFrame returns the active frame's facts without requiring caller-side
// stack reconstruction. It does not infer AA evidence.
func (c *txTraceCollector) CurrentFrame() (vmtracer.Observation, bool) {
	if c == nil {
		return vmtracer.Observation{}, false
	}

	f := c.frames[c.currentObservationFrame()]
	if f == nil || f.index >= len(c.events) {
		return vmtracer.Observation{}, false
	}

	e := cloneObservation(c.events[f.index])
	e.Status = c.status(e.FrameID)

	return e, true
}

// Observe subscribes to the same occurrences stored in the journal. It can be
// installed at a precompile boundary; no separate collector or identity exists.
func (c *txTraceCollector) Observe(fn vmtracer.Observer) (func(), error) {
	if c == nil || fn == nil {
		return nil, errors.New("observations not enabled or nil observer")
	}

	if c.err != nil {
		c.LatchError(c.err)
		return nil, c.err
	}

	c.nextSubscriber++
	id := c.nextSubscriber
	c.subscribers = append(c.subscribers, observationSubscriber{
		id: id,
		fn: fn,
	})

	return func() {
		for i, sub := range c.subscribers {
			if sub.id == id {
				c.subscribers = append(c.subscribers[:i], c.subscribers[i+1:]...)
				return
			}
		}
	}, nil
}

func (c *txTraceCollector) Observations() []vmtracer.Observation {
	if c == nil {
		return nil
	}

	out := make([]vmtracer.Observation, len(c.events))

	for i, e := range c.events {
		out[i] = cloneObservation(e)
		out[i].Status = c.status(e.FrameID)
	}

	return out
}

func (c *txTraceCollector) currentObservationFrame() uint64 {
	if c != nil {
		for _, v := range slices.Backward(c.stack) {
			if v != 0 {
				return v
			}
		}
	}

	return 0
}

func (c *txTraceCollector) emitObservation(e vmtracer.Observation) {
	if c == nil {
		return
	}

	c.nextOccurrence++
	if uint64(len(c.events)) >= c.maxEvents {
		c.collectionError(errors.New("raw observation budget exceeded"))
		return
	}

	e.OccurrenceID = c.nextOccurrence
	if e.FrameID == 0 {
		e.FrameID = c.currentObservationFrame()
	}

	if f := c.frames[e.FrameID]; f != nil && f.index < len(c.events) {
		enter := c.events[f.index]
		e.ParentFrameID = f.parent
		e.Depth = enter.Depth
		e.ExecutionAddress = enter.ExecutionAddress
		e.CodeAddress = enter.CodeAddress
		e.CodeAddressKnown = enter.CodeAddressKnown
	}

	e.Status = c.status(e.FrameID)
	e = cloneObservation(e)
	c.events = append(c.events, e)
	subscribers := append([]observationSubscriber(nil), c.subscribers...)

	for _, sub := range subscribers {
		c.LatchError(sub.fn(cloneObservation(e)))
	}
}

func (c *txTraceCollector) onEnter(depth int, typ byte, from, to common.Address, input []byte, gas uint64, value *big.Int) {
	if c == nil {
		return
	}

	// geth's SELFDESTRUCT enter/exit pair is synthetic, not a call frame.
	// Attribute the executed movement to the still-active enclosing frame so the
	// exported call tree contains only CALL/CREATE-family entries.
	if typ == byte(vm.SELFDESTRUCT) {
		if c.currentObservationFrame() == 0 {
			c.collectionError(errors.New("selfdestruct outside recorded call frame"))
		}

		if from != to && value != nil && value.Sign() > 0 {
			c.emitObservation(vmtracer.Observation{
				Kind:   vmtracer.NativeTransfer,
				From:   from,
				To:     to,
				Amount: value,
				Opcode: typ,
			})
		}

		c.stack = append(c.stack, 0)
		return
	}

	parent := c.currentObservationFrame()
	c.nextFrame++
	e := vmtracer.Observation{
		OccurrenceID:     c.nextOccurrence + 1,
		Kind:             vmtracer.CallEnter,
		Depth:            depth,
		FrameID:          c.nextFrame,
		ParentFrameID:    parent,
		Opcode:           typ,
		Caller:           from,
		Target:           to,
		ExecutionAddress: to,
		CodeAddress:      to,
		Input:            input,
		Gas:              gas,
		Value:            value,
	}

	if typ == byte(vm.DELEGATECALL) || typ == byte(vm.CALLCODE) {
		e.ExecutionAddress = from
	}

	switch {
	case typ == byte(vm.CREATE) || typ == byte(vm.CREATE2):
		e.CodeAddressKnown = true

	case c.code != nil:
		e.CodeAddressKnown = true
		if addr, ok := ethtypes.ParseDelegation(c.code(to)); ok {
			e.CodeAddress = addr
		}

	default:
		c.collectionError(errors.New("code reader unavailable at frame entry"))
	}

	c.frames[e.FrameID] = &observationFrame{
		index:  len(c.events),
		parent: parent,
	}
	c.stack = append(c.stack, e.FrameID)
	c.emitObservation(e)

	if typ == byte(vm.CALL) && len(input) >= 4 && (bytes.Equal(input[:4], erc20TransferSelector) || bytes.Equal(input[:4], erc20TransferFromSelector)) {
		c.emitObservation(vmtracer.Observation{
			Kind:   vmtracer.ERC20Call,
			Caller: from,
			Target: to,
			Input:  input,
			Opcode: typ,
		})
	}
}

func (c *txTraceCollector) onExit(_ int, output []byte, gas uint64, err error, reverted bool) {
	if c == nil {
		return
	}

	if len(c.stack) > 0 && c.stack[len(c.stack)-1] == 0 {
		c.stack = c.stack[:len(c.stack)-1]
		return
	}

	id := c.currentObservationFrame()
	if id == 0 {
		c.collectionError(errors.New("call exit without frame"))
		return
	}

	f := c.frames[id]
	f.status = vmtracer.FrameStatus{
		Exited:   true,
		Success:  err == nil && !reverted,
		Reverted: reverted,
		Error:    err,
	}

	c.emitObservation(vmtracer.Observation{
		Kind:     vmtracer.CallExit,
		FrameID:  id,
		Output:   output,
		Gas:      gas,
		Err:      err,
		Reverted: reverted,
	})

	c.stack = c.stack[:len(c.stack)-1]
}

// RecordNativeTransfer is called only after the EVM Transfer function executes.
// Balance callbacks are separate facts and are never tuple-deduplicated or paired.
func (c *txTraceCollector) RecordNativeTransfer(from, to common.Address, value *big.Int) {
	if c == nil || value == nil || value.Sign() == 0 {
		return
	}

	id := c.currentObservationFrame()
	f := c.frames[id]
	if f == nil || f.index >= len(c.events) {
		c.collectionError(errors.New("movement outside recorded frame"))
		return
	}

	opcode := c.events[f.index].Opcode
	if opcode != byte(vm.CALL) && opcode != byte(vm.CREATE) && opcode != byte(vm.CREATE2) {
		c.collectionError(fmt.Errorf("movement in unsupported opcode %x", opcode))
		return
	}

	c.emitObservation(vmtracer.Observation{
		Kind:   vmtracer.NativeTransfer,
		From:   from,
		To:     to,
		Amount: value,
		Opcode: opcode,
	})
}

func cloneObservation(e vmtracer.Observation) vmtracer.Observation {
	e.Input = append([]byte(nil), e.Input...)
	e.Output = append([]byte(nil), e.Output...)
	if e.Value != nil {
		e.Value = new(big.Int).Set(e.Value)
	}
	if e.PreviousBalance != nil {
		e.PreviousBalance = new(big.Int).Set(e.PreviousBalance)
	}
	if e.CurrentBalance != nil {
		e.CurrentBalance = new(big.Int).Set(e.CurrentBalance)
	}
	if e.Amount != nil {
		e.Amount = new(big.Int).Set(e.Amount)
	}
	if e.UserOperation != nil {
		u := *e.UserOperation
		e.UserOperation = &u
	}
	if e.Log != nil {
		l := *e.Log
		l.Topics = append([]common.Hash(nil), l.Topics...)
		l.Data = append([]byte(nil), l.Data...)
		e.Log = &l
	}
	return e
}
