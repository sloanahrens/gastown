//go:build integration

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// TestFindTestSockets_Integration verifies that findTestSockets discovers
// active gt-test-* sockets. This test creates a temporary tmux server on a
// gt-test-* socket, verifies discovery, then cleans up.
func TestFindTestSockets_Integration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux socket discovery unreliable on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}

	// Create a unique test socket with gt-test- prefix.
	socketName := fmt.Sprintf("gt-test-discovery-%d", os.Getpid())
	sessionName := "probe-session"

	// Start a tmux server on this socket with a session.
	startCmd := exec.Command("tmux", "-L", socketName, "new-session", "-d", "-s", sessionName)
	if err := startCmd.Run(); err != nil {
		t.Fatalf("failed to create test tmux server: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socketName, "kill-server").Run()
		socketPath := filepath.Join(tmux.SocketDir(), socketName)
		_ = os.Remove(socketPath)
	})

	// findTestSockets should discover our socket.
	sockets := findTestSockets()
	found := false
	for _, s := range sockets {
		t.Logf("discovered test socket: %s", s)
		if s == socketName {
			found = true
		}
	}
	if !found {
		t.Errorf("findTestSockets() did not find %q, got: %v", socketName, sockets)
	}
}

// TestFindTestSockets_SkipsNonTestSockets verifies that findTestSockets only
// returns gt-test-* sockets, not the town socket or other custom sockets.
func TestFindTestSockets_SkipsNonTestSockets(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}

	sockets := findTestSockets()
	for _, s := range sockets {
		if !strings.HasPrefix(s, "gt-test-") {
			t.Errorf("findTestSockets() returned non-test socket: %q", s)
		}
	}
}

func TestIsAgentSessionHealthy_DeadPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	tm := tmux.NewTmux()
	sessionName := "zzrig-dead-pane-test"
	_ = tm.KillSession(sessionName)
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	for _, args := range [][]string{
		{"new-session", "-d", "-s", sessionName},
		{"set-option", "-t", sessionName, "remain-on-exit", "on"},
		{"respawn-pane", "-k", "-t", sessionName, "false"},
	} {
		if out, err := tmux.BuildCommand(args...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, strings.TrimSpace(string(out)))
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	observedDead := false
	for time.Now().Before(deadline) {
		out, err := tmux.BuildCommand("display-message", "-p", "-t", sessionName, "#{pane_dead}").Output()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			observedDead = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !observedDead {
		t.Fatal("expected retained pane to report pane_dead=1")
	}

	hasSession, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !hasSession {
		t.Fatal("expected retained tmux session to exist")
	}
	if isAgentSessionHealthy(tm, sessionName) {
		t.Fatal("dead retained pane must not be reported healthy")
	}
}

func TestFindRigSessions(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	setupRigTestRegistry(t)

	tm := tmux.NewTmux()

	// Create sessions that match our test rig prefix (zztr- for testrig1223)
	matching := []string{
		"zztr-witness",
		"zztr-refinery",
		"zztr-alpha",
	}
	// Create a non-matching session (zzor- for otherrig)
	nonMatching := "zzor-witness"

	for _, name := range append(matching, nonMatching) {
		_ = tm.KillSession(name) // clean up any leftovers
		if err := tm.NewSessionWithCommand(name, "", "sleep 300"); err != nil {
			t.Fatalf("creating session %s: %v", name, err)
		}
	}
	defer func() {
		for _, name := range append(matching, nonMatching) {
			_ = tm.KillSession(name)
		}
	}()

	got, err := findRigSessions(tm, "testrig1223")
	if err != nil {
		t.Fatalf("findRigSessions: %v", err)
	}

	// Verify all matching sessions are returned
	gotSet := make(map[string]bool, len(got))
	for _, s := range got {
		gotSet[s] = true
	}

	for _, want := range matching {
		if !gotSet[want] {
			t.Errorf("expected session %q in results, got %v", want, got)
		}
	}

	// Verify non-matching session is excluded
	if gotSet[nonMatching] {
		t.Errorf("did not expect session %q in results, got %v", nonMatching, got)
	}

	// Verify count
	if len(got) != len(matching) {
		t.Errorf("expected %d sessions, got %d: %v", len(matching), len(got), got)
	}
}

func TestFindRigSessions_NoSessions(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	// Register a unique prefix for a rig that has no sessions
	reg := session.NewPrefixRegistry()
	reg.Register("zz", "nonexistentrig999")
	old := session.DefaultRegistry()
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	tm := tmux.NewTmux()
	got, err := findRigSessions(tm, "nonexistentrig999")
	if err != nil {
		t.Fatalf("findRigSessions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 sessions, got %d: %v", len(got), got)
	}
}

// TestDeliverNudge_ImmediateMode_RefusesBusyTarget guards gt-cyyg:
// --mode=immediate must not send straight into a busy target. It reproduces
// the shape of the incident (gastown/refinery interrupted mid a long-running
// tool call) by rendering the Claude Code busy spinner into a real pane, then
// asserts the nudge is queued for wait-idle delivery instead of typed
// directly into the busy composer.
func TestDeliverNudge_ImmediateMode_RefusesBusyTarget(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	tm := tmux.NewTmux()
	sessionName := "gt-test-nudge-immediate-busy-refusal"
	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	if err := tm.SetEnvironment(sessionName, "GT_AGENT", "claude"); err != nil {
		t.Fatalf("SetEnvironment: %v", err)
	}

	// Render the busy spinner into the pane and leave it displayed — no
	// further shell output pushes it out of the capture window, so the pane
	// stays "busy" for the life of the test (same technique as
	// tmux.TestIsBusy_LivePane).
	if err := tm.SendKeys(sessionName, "printf '✵ Leavening… (3m 17s · ↓ 14.1k tokens)\\n'"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !tm.IsBusy(sessionName) {
		if time.Now().After(deadline) {
			out, _ := tm.CapturePane(sessionName, 20)
			t.Fatalf("target pane never went busy; pane:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// deliverNudge resolves townRoot via workspace.FindFromCwd(), so the
	// refusal's wait-idle fallback needs a real (fake) workspace on disk.
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	// Shorten the wait-idle/queue-watcher timeouts so the refusal's fallback
	// path (which polls for idle before giving up and leaving the message
	// queued) doesn't hang the test — the pane stays busy throughout.
	origWaitIdle, origIdleTimeout, origIdleInterval := waitIdleTimeout, idleWatcherTimeout, idleWatcherPollInterval
	waitIdleTimeout = 300 * time.Millisecond
	idleWatcherTimeout = 300 * time.Millisecond
	idleWatcherPollInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		waitIdleTimeout, idleWatcherTimeout, idleWatcherPollInterval = origWaitIdle, origIdleTimeout, origIdleInterval
	})

	origMode, origForce := nudgeModeFlag, nudgeForceFlag
	nudgeModeFlag = NudgeModeImmediate
	nudgeForceFlag = false
	t.Cleanup(func() { nudgeModeFlag, nudgeForceFlag = origMode, origForce })

	const message = "should-not-be-typed-into-the-busy-pane"
	if err := deliverNudge(tm, sessionName, message, "tester"); err != nil {
		t.Fatalf("deliverNudge: %v", err)
	}

	// Refused and queued, not sent: the message must be waiting in the
	// nudge queue rather than having been typed into the busy pane.
	if got := nudge.QueueLen(townRoot, sessionName); got != 1 {
		t.Errorf("QueueLen after immediate-mode busy refusal = %d, want 1 (message should be queued, not sent)", got)
	}

	out, err := tm.CapturePane(sessionName, 40)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if strings.Contains(out, message) {
		t.Fatalf("busy pane received the nudge text directly — immediate mode should have refused and queued it instead:\n%s", out)
	}
}

// TestDeliverNudge_ImmediateMode_ForceOverridesBusyRefusal guards the
// escape-hatch half of gt-cyyg: --force must still deliver immediately even
// when the target is busy, since --mode=immediate --force is the documented
// way to break through a stuck agent.
func TestDeliverNudge_ImmediateMode_ForceOverridesBusyRefusal(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	tm := tmux.NewTmux()
	sessionName := "gt-test-nudge-immediate-force-busy"
	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	if err := tm.SendKeys(sessionName, "printf '✵ Leavening… (3m 17s · ↓ 14.1k tokens)\\n'"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !tm.IsBusy(sessionName) {
		if time.Now().After(deadline) {
			out, _ := tm.CapturePane(sessionName, 20)
			t.Fatalf("target pane never went busy; pane:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}

	origMode, origForce := nudgeModeFlag, nudgeForceFlag
	nudgeModeFlag = NudgeModeImmediate
	nudgeForceFlag = true
	t.Cleanup(func() { nudgeModeFlag, nudgeForceFlag = origMode, origForce })

	const message = "should-be-typed-into-the-pane-because-forced"
	if err := deliverNudge(tm, sessionName, message, "tester"); err != nil {
		t.Fatalf("deliverNudge: %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for {
		out, err := tm.CapturePane(sessionName, 40)
		if err != nil {
			t.Fatalf("CapturePane: %v", err)
		}
		// The message is one long hyphenated token with no spaces, typed
		// after a real shell prompt whose rendered width varies (hostname,
		// cwd, async prompt redraws). When prompt+message overflows the
		// pane's column width, the pane (or the shell's own line editor)
		// splits the token across two captured rows with a bare "\n" and no
		// character added or removed at the break — so the delivered text
		// is intact, but a direct Contains against the raw capture misses it
		// depending on exactly where that break lands. This is what made
		// the test flake (main is RED again, gt-isp0): the failure tracked
		// pane-render width, not nudge delivery. Flatten newlines before
		// searching so the check is independent of where the pane wrapped.
		flat := strings.ReplaceAll(out, "\n", "")
		if strings.Contains(flat, message) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("--force did not deliver to the busy pane within timeout; pane:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
