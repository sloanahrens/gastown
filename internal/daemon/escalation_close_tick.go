package daemon

// escalationCloseTick runs trigger unless the daemon is shutting down, the
// same rule the daemon loop applies to its other tick cases. A tick that
// arrives while shutdown is in progress is dropped.
func escalationCloseTick(shuttingDown func() bool, trigger func()) {
	if shuttingDown() {
		return
	}
	trigger()
}
