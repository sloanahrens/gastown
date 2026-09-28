//go:build integration

package tmux_test

import (
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
)

func TestIntegrationSessionsContract(t *testing.T) {
	tmuxfake.RunSessionsContract(t, func(t *testing.T) tmuxfake.Sessions {
		// TestSocketName embeds a nanosecond timestamp, so each subtest gets
		// its own server.
		tm := tmux.NewTmuxWithSocket(constants.TestSocketName("gt-test-contract"))
		t.Cleanup(func() { _ = tm.KillServer() })
		return tm
	})
}
