package tracer

// InstallSession installs a tracer for a caller-managed execution session. The
// returned cleanup function is idempotent and should be called when the
// session ends.
func (m *Manager) InstallSession(tracer Tracer) func() {
	return m.install(tracer, true)
}
