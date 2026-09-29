package refinery

import "github.com/steveyegge/gastown/internal/notify"

// notify returns the Notifier the engineer sends through: the injected one,
// or gt run from dir.
func (e *Engineer) notify(dir string) notify.Notifier {
	if e.notifier != nil {
		return e.notifier
	}
	return &notify.CLI{Dir: dir}
}

// notify returns the Notifier the manager sends through: the injected one,
// or gt run from the manager's work directory.
func (m *Manager) notify() notify.Notifier {
	if m.notifier != nil {
		return m.notifier
	}
	return &notify.CLI{Dir: m.workDir}
}
