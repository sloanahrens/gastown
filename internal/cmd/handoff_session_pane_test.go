package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tmux"
)

// getSessionPane must ask through a tmux client, so the session router can send
// a sandboxed session's question to the sandbox's own tmux server. A raw host
// `tmux list-panes` cannot see that session, and StartSession then killed a
// healthy sandbox agent as "died during startup" (sling 8, gt-nb840). The
// router's answer here is an unavailable client: if the router is consulted for
// the session, its reason comes back in the error; a raw host call would never
// consult it. How a routed client answers is the tmux package's own tests.
//
//testpolicy:allow parallel — installs the process-wide session router, which another test must not see mid-run
func TestGetSessionPaneConsultsTheSessionRouter(t *testing.T) {
	var asked []string
	tmux.SetSessionRouter(func(session string) *tmux.Tmux {
		asked = append(asked, session)
		return tmux.NewTmuxUnavailable("sandbox registry unreadable")
	})
	t.Cleanup(func() { tmux.SetSessionRouter(nil) })

	pane, err := getSessionPane("gt-gastown-box")

	if err == nil || pane != "" {
		t.Fatalf("getSessionPane = %q, %v; want an error and no pane", pane, err)
	}
	if !strings.Contains(err.Error(), "sandbox registry unreadable") {
		t.Errorf("error %q does not carry the router's reason: the lookup never went through the router", err)
	}
	if len(asked) == 0 || asked[0] != "gt-gastown-box" {
		t.Errorf("the router was asked about %v, want gt-gastown-box", asked)
	}
}
