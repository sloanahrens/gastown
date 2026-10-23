package tmux

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// SessionRouter answers which Tmux client should run a command that names
// session, or nil for the process's own tmux server. The session package
// installs one that returns a client driving the tmux server inside an
// OpenShell sandbox for sessions the sandbox registry lists, so every
// caller that holds a plain NewTmux() client (the daemon, gt sling, gt
// nudge) reaches a sandboxed polecat without knowing it is one.
//
// A routed client must have no router of its own (routing never recurses)
// and runs its ps and kill helpers through its own exec seam, which is what
// keeps host pids and sandbox pids apart: a pid read from a sandbox's tmux
// is only ever passed to a kill that runs inside that sandbox.
type SessionRouter func(session string) *Tmux

// ExtraSessions lists sessions that are not on the process's own server but
// should appear in ListSessions, such as the sandbox registry's entries.
type ExtraSessions func() []string

var (
	routerMu      sync.RWMutex
	sessionRouter SessionRouter
	extraSessions ExtraSessions
)

// SetSessionRouter installs (or, with nil, removes) the process-wide router.
func SetSessionRouter(r SessionRouter) {
	routerMu.Lock()
	defer routerMu.Unlock()
	sessionRouter = r
}

// SetExtraSessions installs (or removes) the process-wide extra-sessions hook.
func SetExtraSessions(f ExtraSessions) {
	routerMu.Lock()
	defer routerMu.Unlock()
	extraSessions = f
}

// routed is true for a client a router returned: it never routes again.
func (t *Tmux) routeFor(session string) *Tmux {
	if t == nil || t.noRoute || session == "" {
		return nil
	}
	routerMu.RLock()
	r := sessionRouter
	routerMu.RUnlock()
	if r == nil {
		return nil
	}
	rt := r(session)
	if rt == nil || rt == t {
		return nil
	}
	if !rt.noRoute {
		// A router should hand out routed clients (Routed); a plain one is
		// used through a routed copy so the shared value is never written.
		rt = rt.Routed()
	}
	return rt
}

// Routed returns a copy of t that never consults the session router: the
// form a SessionRouter must return, built once per session and cached.
func (t *Tmux) Routed() *Tmux {
	if t == nil || t.noRoute {
		return t
	}
	c := *t
	c.noRoute = true
	return &c
}

// sessionOfTarget extracts the session name from a tmux target: the text
// before the first ':' or '.', unless the target is a pane id (%N) or a
// window id (@N), which belong to whichever server issued them and are not
// routed.
func sessionOfTarget(target string) string {
	target = strings.TrimPrefix(target, "=") // exact-match prefix (exactSessionTarget)
	if target == "" || target[0] == '%' || target[0] == '@' || target[0] == '$' {
		return ""
	}
	if i := strings.IndexAny(target, ":."); i >= 0 {
		return target[:i]
	}
	return target
}

// sessionOfArgs finds the session a tmux argv addresses: the value of -t,
// or of -s for new-session. The first such flag wins.
func sessionOfArgs(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-t", "-s":
			return sessionOfTarget(args[i+1])
		}
	}
	return ""
}

// NewTmuxUnavailable is a client whose every command fails with reason. A
// SessionRouter returns one when it cannot tell which server owns a session
// (its registry is unreadable), so the command fails instead of running on
// the wrong server.
func NewTmuxUnavailable(reason string) *Tmux {
	err := errors.New("tmux unavailable: " + reason)
	return &Tmux{noRoute: true, exec: func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, nil, err
	}}
}

// extraSessionNames returns the registered extra sessions, deduplicated
// against have.
func extraSessionNames(have []string) []string {
	routerMu.RLock()
	f := extraSessions
	routerMu.RUnlock()
	if f == nil {
		return nil
	}
	seen := make(map[string]bool, len(have))
	for _, h := range have {
		seen[h] = true
	}
	var out []string
	for _, n := range f() {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
