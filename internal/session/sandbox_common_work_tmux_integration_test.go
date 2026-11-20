//go:build integration

package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tmuxHandshakeTimeout bounds the wait for the pane's pwd-written signal, so a
// pane that never signals fails the test at this deadline instead of hanging.
const tmuxHandshakeTimeout = 30 * time.Second

// TestIntegrationSandboxCommonWorkTmuxKeepsTheLink pins the premise of the
// sandbox common work path (gt-f80lq2): a tmux pane started with -c <link>
// keeps the link as its PWD instead of resolving it. Go takes its working
// directory from $PWD, so a resolved link would give each seat its own path.
func TestIntegrationSandboxCommonWorkTmuxKeepsTheLink(t *testing.T) {
	ver, err := exec.Command("tmux", "-V").CombinedOutput()
	if err != nil {
		t.Fatalf("tmux is required for this test: %v", err)
	}
	t.Logf("tmux version: %s", strings.TrimSpace(string(ver)))

	parent := t.TempDir()
	realDir := filepath.Join(parent, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	pwdFile := filepath.Join(parent, "pwd.txt")
	socket := filepath.Join(parent, "s")

	// The pane exits once it signals, and a sessionless server exits too,
	// taking the remembered signal: exit-empty off keeps both until the wait.
	if out, err := exec.Command("tmux", "-S", socket, "start-server", ";", "set-option", "-s", "exit-empty", "off").CombinedOutput(); err != nil {
		t.Fatalf("start tmux server on %s: %v: %s", socket, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })

	// The pane records its PWD, then signals pwd-written. tmux remembers a
	// signal sent before anyone waits, so the wait below returns whichever of
	// the pane and this test reaches it first, with no poll and no sleep.
	paneCmd := `echo "$PWD" > "$1"; tmux -S "$2" wait-for -S pwd-written`
	if out, err := exec.Command("tmux", "-S", socket, "new-session", "-d", "-c", link, "sh", "-c", paneCmd, "sh", pwdFile, socket).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session -c %s: %v: %s", link, err, strings.TrimSpace(string(out)))
	}

	ctx, cancel := context.WithTimeout(t.Context(), tmuxHandshakeTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "tmux", "-S", socket, "wait-for", "pwd-written").CombinedOutput(); err != nil {
		t.Fatalf("wait for the pane's pwd-written signal within %s: %v: %s", tmuxHandshakeTimeout, err, strings.TrimSpace(string(out)))
	}

	got, err := os.ReadFile(pwdFile)
	if err != nil {
		t.Fatalf("read the pane's PWD: %v", err)
	}
	if panePWD := strings.TrimSpace(string(got)); panePWD != link {
		t.Errorf("pane PWD = %q, want the link %q: tmux resolved the link, so Go's working directory would not be shared", panePWD, link)
	}
}
