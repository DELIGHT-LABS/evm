package session

import (
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

// Install installs tracer into the Manager associated with ctx. The returned
// cleanup function is idempotent. ok is false when ctx has no Manager.
func Install(ctx sdk.Context, tracer vmtracer.Tracer) (cleanup func(), ok bool) {
	manager, ok := vmtracer.FromContext(ctx)
	if !ok {
		return func() {}, false
	}
	return manager.InstallSession(tracer), true
}
