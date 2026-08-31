package tracer

import sdk "github.com/cosmos/cosmos-sdk/types"

type managerContextKey struct{}

// WithManager stores manager in an SDK context so code running inside an EVM
// precompile can install a session tracer without replacing vm.Config.Tracer.
func WithManager(ctx sdk.Context, manager *Manager) sdk.Context {
	return ctx.WithValue(managerContextKey{}, manager)
}

// FromContext returns the tracing manager associated with an EVM execution.
func FromContext(ctx sdk.Context) (*Manager, bool) {
	manager, ok := ctx.Value(managerContextKey{}).(*Manager)
	return manager, ok && manager != nil
}
