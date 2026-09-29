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

// newDaemonNotifier runs gt from the town root as the daemon: BD_ACTOR=daemon
// identifies the daemon rather than the overseer as the actor of what gt
// writes (runDaemonRun also sets it process-wide).
func newDaemonNotifier(gtPath, townRoot string) notify.Notifier {
	return &notify.CLI{
		Bin: gtPath,
		Dir: townRoot,
		Env: func() []string { return append(os.Environ(), "BD_ACTOR=daemon") },
	}
}

// notify returns the Notifier the Dolt server manager sends its alerts
// through: gt run from the town root with the daemon's environment.
func (m *DoltServerManager) notify() notify.Notifier {
	if m.notifier != nil {
		return m.notifier
	}
	return &notify.CLI{Dir: m.townRoot}
}
