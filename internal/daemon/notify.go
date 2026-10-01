package daemon

import (
	"os"

	"github.com/steveyegge/gastown/internal/notify"
)

// notify returns the Notifier the daemon sends mail, nudges and escalations
// through. A Daemon built without one (a struct literal in a test) gets the
// production one.
func (d *Daemon) notify() notify.Notifier {
	if d.notifier != nil {
		return d.notifier
	}
	townRoot := ""
	if d.config != nil {
		townRoot = d.config.TownRoot
	}
	return newDaemonNotifier(d.gtPath, townRoot)
}

// newDaemonNotifier runs gt from the town root as the daemon: daemonGTEnv
// identifies the daemon rather than the overseer as the actor of what gt
// writes.
func newDaemonNotifier(gtPath, townRoot string) notify.Notifier {
	return &notify.CLI{
		Bin: gtPath,
		Dir: townRoot,
		Env: func() []string { return daemonGTEnv(os.Environ()) },
	}
}

// notify returns the Notifier the Dolt server manager sends its alerts
// through: gt from PATH run from the town root as the daemon.
func (m *DoltServerManager) notify() notify.Notifier {
	if m.notifier != nil {
		return m.notifier
	}
	return &notify.CLI{Dir: m.townRoot, Env: func() []string { return daemonGTEnv(os.Environ()) }}
}
