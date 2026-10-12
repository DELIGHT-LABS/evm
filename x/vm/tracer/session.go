package tracer

import "errors"

// InstallSession installs hooks for a caller-managed execution session. Journals
// belong to the global execution and cannot be installed as session tracers.
// The returned cleanup function is idempotent, including when installation fails.
// Installation errors are returned directly and do not latch an execution error.
func (m *Manager) InstallSession(tracer Tracer) (func(), error) {
	if m == nil {
		return func() {}, errors.New("session requires an execution manager")
	}
	if tracer == nil {
		return func() {}, errors.New("session requires tracer hooks")
	}
	if _, ok := tracer.(Journal); ok {
		return func() {}, errors.New("session cannot install an execution journal; use session.Begin")
	}
	if tracer.Hooks() == nil {
		return func() {}, errors.New("session requires tracer hooks")
	}
	return m.install(tracer, true), nil
}
