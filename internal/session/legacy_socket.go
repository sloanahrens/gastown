package session

import (
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/tmux"
)

// legacySocketTmux is the subset of tmux.Tmux used by legacy socket cleanup.
// It is extracted to allow tests to avoid real tmux calls.
type legacySocketTmux interface {
	ListSessions() ([]string, error)
	KillSessionWithProcesses(name string) error
}

// legacySockets is what legacy socket cleanup reads: the socket this process
// uses, a tmux on another socket, and the rig prefixes that mark a session as
// Gas Town's. Tests build one with fakes; production uses defaultLegacySockets.
type legacySockets struct {
	current  string
	open     func(socket string) legacySocketTmux
	prefixes *PrefixRegistry
}

func defaultLegacySockets() legacySockets {
	return legacySockets{
		current:  tmux.GetDefaultSocket(),
		open:     func(socket string) legacySocketTmux { return tmux.NewTmuxWithSocket(socket) },
		prefixes: DefaultRegistry(),
	}
}

// CleanupLegacyDefaultSocket removes Gas Town sessions left on the "default"
// tmux socket by old binaries. Returns the number of sessions cleaned.
func CleanupLegacyDefaultSocket() int {
	return defaultLegacySockets().cleanupDefault()
}

// CountLegacyDefaultSocketSessions counts Gas Town sessions on the "default"
// tmux socket for dry-run output.
func CountLegacyDefaultSocketSessions() int {
	return defaultLegacySockets().countDefault()
}

// CleanupLegacyBaseSocket removes Gas Town sessions left on the old
// basename-only tmux socket by binaries from before path-hashed socket names
// were introduced. Returns the number of sessions cleaned.
func CleanupLegacyBaseSocket(townRoot string) int {
	return defaultLegacySockets().cleanupBase(townRoot)
}

// CountLegacyBaseSocketSessions counts Gas Town sessions on the old
// basename-only tmux socket for dry-run output.
func CountLegacyBaseSocketSessions(townRoot string) int {
	return defaultLegacySockets().countBase(townRoot)
}

func (l legacySockets) cleanupDefault() int {
	if l.current == "" || l.current == "default" {
		return 0 // Already on the default socket, nothing to clean up.
	}
	return l.cleanup(l.open("default"))
}

func (l legacySockets) countDefault() int {
	if l.current == "" || l.current == "default" {
		return 0
	}
	return l.count(l.open("default"))
}

func (l legacySockets) cleanupBase(townRoot string) int {
	legacySocket := LegacySocketName(townRoot)
	if l.current == legacySocket {
		return 0 // Same socket, no migration needed.
	}
	return l.cleanup(l.open(legacySocket))
}

func (l legacySockets) countBase(townRoot string) int {
	legacySocket := LegacySocketName(townRoot)
	if l.current == legacySocket {
		return 0
	}
	return l.count(l.open(legacySocket))
}

func (l legacySockets) count(legacyTmux legacySocketTmux) int {
	sessions, err := legacyTmux.ListSessions()
	if err != nil {
		return 0
	}

	var count int
	for _, sess := range sessions {
		if l.isCleanupSession(sess) {
			count++
		}
	}
	return count
}

func (l legacySockets) cleanup(legacyTmux legacySocketTmux) int {
	var cleaned int
	for range 3 {
		sessions, err := legacyTmux.ListSessions()
		if err != nil {
			return cleaned // No server on legacy socket.
		}
		targets := l.cleanupTargets(sessions)
		if len(targets) == 0 {
			return cleaned
		}
		for _, sess := range targets {
			if err := legacyTmux.KillSessionWithProcesses(sess); err == nil {
				cleaned++
			}
		}
	}
	return cleaned
}

func (l legacySockets) cleanupTargets(sessions []string) []string {
	targets := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		if l.isCleanupSession(sess) {
			targets = append(targets, sess)
		}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		return legacyCleanupPriority(targets[i]) < legacyCleanupPriority(targets[j])
	})
	return targets
}

func legacyCleanupPriority(sess string) int {
	switch sess {
	case DeaconSessionName(), BootSessionName():
		return 0
	case MayorSessionName():
		return 1
	}
	if strings.HasPrefix(sess, HQPrefix+"dog-") {
		return 2
	}
	if strings.HasSuffix(sess, "-witness") || strings.HasSuffix(sess, "-refinery") {
		return 3
	}
	if strings.Contains(sess, "-crew-") {
		return 4
	}
	return 5
}

func (l legacySockets) isCleanupSession(sess string) bool {
	switch sess {
	case MayorSessionName(), DeaconSessionName(), BootSessionName():
		return true
	}
	if strings.HasPrefix(sess, HQPrefix+"dog-") && strings.TrimPrefix(sess, HQPrefix+"dog-") != "" {
		return true
	}
	if l.prefixes.HasPrefix(sess) {
		return true
	}
	for _, p := range LegacyPrefixes {
		if p == strings.TrimSuffix(HQPrefix, "-") {
			continue
		}
		if strings.HasPrefix(sess, p+"-") {
			return true
		}
	}
	return false
}
