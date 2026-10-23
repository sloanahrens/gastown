package tmux

import (
	"context"
	"fmt"
	"time"
)

// processTableTimeout bounds one process-table read.
const processTableTimeout = 10 * time.Second

// SessionPanes returns `list-panes -s` for one session as "<session>\t<pane pid>"
// lines. It goes through run, so a sandboxed session is answered by the tmux
// server inside its sandbox; a raw host `list-panes -a` cannot see it.
func (t *Tmux) SessionPanes(session string) (string, error) {
	// -t comes before -s: the router takes the first of -t and -s as the session,
	// and for list-panes -s only means "every window of the session".
	return t.run("list-panes", "-t", exactSessionTarget(session), "-s", "-F", "#{session_name}\t#{pane_pid}")
}

// ProcessTable returns `ps -ww -eo pid,ppid,args` where the session lives: the
// sandbox's process table for a sandboxed session, the host's otherwise. -ww
// because an invocation looked for sits at the end of a long wrapper argv, and a
// width-truncated line drops exactly that part. The read is bounded by
// processTableTimeout.
func (t *Tmux) ProcessTable(session string) (string, error) {
	if rt := t.routeFor(session); rt != nil {
		return rt.ProcessTable(session)
	}
	ctx, cancel := context.WithTimeout(context.Background(), processTableTimeout)
	defer cancel()
	out, _, err := t.runner()(ctx, "ps", "-ww", "-eo", "pid,ppid,args")
	if err != nil {
		return "", fmt.Errorf("listing processes: %w", err)
	}
	return string(out), nil
}
