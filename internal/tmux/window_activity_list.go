package tmux

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// ListWindowActivity returns every session's last window activity in one
// tmux call. #{window_activity} advances on real pane output even for
// unattached sessions (see GetWindowActivity), so it is the liveness clock
// for unattended agents. A town with no tmux server has no sessions and is
// not an error. Callers that need many sessions use this instead of one
// GetWindowActivity call per session.
func (t *Tmux) ListWindowActivity() (map[string]time.Time, error) {
	out, err := t.run("list-sessions", "-F", "#{window_activity} #{session_name}")
	if err != nil {
		if errors.Is(err, ErrNoServer) {
			return map[string]time.Time{}, nil
		}
		return nil, err
	}
	return parseWindowActivityList(out), nil
}

// parseWindowActivityList reads "<unix seconds> <session name>" lines. A line
// that does not parse is skipped: one odd session must not hide the rest.
func parseWindowActivityList(out string) map[string]time.Time {
	m := map[string]time.Time{}
	for _, line := range strings.Split(out, "\n") {
		secs, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || name == "" {
			continue
		}
		n, err := strconv.ParseInt(secs, 10, 64)
		if err != nil {
			continue
		}
		m[name] = time.Unix(n, 0)
	}
	return m
}
