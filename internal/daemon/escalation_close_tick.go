package daemon

// escalationCloseTick runs trigger unless the daemon is shutting down, the
// same rule the daemon loop applies to its other tick cases. During shutdown
// the seats are going away and the pass has no one to resume, so a tick that
// arrives then is dropped.
func escalationCloseTick(shuttingDown func() bool, trigger func()) {
	if shuttingDown() {
		return
	}
	trigger()
}
