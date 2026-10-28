package session

import (
	"errors"

	"github.com/ethereum/go-ethereum/core/tracing"

	vmtracer "github.com/cosmos/evm/x/vm/tracer"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// Tracer adapts application-owned hooks to the common tracer contract.
type Tracer struct {
	hooks *tracing.Hooks
}

// New creates a session tracer from application-owned hooks.
func New(hooks *tracing.Hooks) *Tracer {
	return &Tracer{hooks: hooks}
}

// Hooks returns the EVM callbacks installed for the session.
func (t *Tracer) Hooks() *tracing.Hooks {
	if t == nil {
		return nil
	}
	return t.hooks
}

// Install installs application-owned hooks into the execution Manager. It returns
// an error when the Manager or hooks are missing. Journals are read through Begin.
// The returned cleanup function is idempotent, including on failure.
func Install(ctx sdk.Context, tracer *Tracer) (cleanup func(), err error) {
	manager, ok := vmtracer.FromContext(ctx)
	if !ok {
		return func() {}, errors.New("session requires an execution manager")
	}
	return manager.InstallSession(tracer)
}

// Range is a view into the global journal. Closing fixes its observation range;
// later ancestor rollback is reflected by subsequent reads.
type Range struct {
	journal vmtracer.Journal
	CallID  uint64
	Start   uint64
	end     uint64
	closed  bool
}

// Begin references the active call and subsequent observations in the existing
// execution journal. It does not create a collector or install callbacks.
func Begin(ctx sdk.Context) (*Range, error) {
	manager, ok := vmtracer.FromContext(ctx)
	if !ok {
		return nil, errors.New("session requires an execution manager")
	}
	journal := manager.Journal()
	if journal == nil {
		return nil, errors.New("session requires an execution journal")
	}
	call, ok := journal.CurrentCall()
	if !manager.HasActiveCall() || !ok {
		return nil, errors.New("session requires an active execution call")
	}
	return &Range{
		journal: journal,
		CallID:  call.CallID,
		Start:   journal.LastObservationID() + 1,
	}, nil
}

func (r *Range) Close() {
	if r != nil && !r.closed {
		r.end = r.journal.LastObservationID() + 1
		r.closed = true
	}
}

func (r *Range) Observations() []vmtracer.Observation {
	if r == nil {
		return nil
	}
	end := r.end
	if !r.closed {
		end = r.journal.LastObservationID() + 1
	}
	var out []vmtracer.Observation
	for _, e := range r.journal.Observations() {
		if e.ObservationID >= r.Start && e.ObservationID < end {
			out = append(out, e)
		}
	}
	return out
}
