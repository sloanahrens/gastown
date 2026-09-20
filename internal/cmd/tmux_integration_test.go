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

// paneBarrierTimeout bounds how long a test waits for the pane to show
// evidence that a keystroke was processed. It is a barrier, not a sleep: the
// wait ends the moment the evidence appears, so a loaded host makes these
// tests slower rather than flaky, and no assertion depends on how long the
// shell took to get there.
const paneBarrierTimeout = 60 * time.Second

// keeperSession is a long-lived dumb session that keeps the harness's isolated
// tmux server alive for the whole test binary.
//
// tmux exits when its last session is killed, leaving a stale socket behind.
// internal/cmd's hermetic harness starts no session of its own, so before this
// keeper existed every test that killed its session shut the server down, and
// the next test's NewSession had to bring it back up — through the staleness
// probe in internal/tmux's socket guard, under whatever load the host was
// carrying. That window is where a session could vanish out from under a test
// that was actively using it ("send Enter: session not found", gt-k9sb).
// internal/tmux's own TestMain keeps a sentinel session for exactly this
// reason (see its comment); this is the same fix for this package.
const keeperSession = "gt-test-keeper"

// ensureServerKeeper starts the keeper session if it isn't already running.
// The name is deliberately outside every rig's session prefix, so callers that
// enumerate sessions (findRigSessions, TestFindTestSockets_*) ignore it.
func ensureServerKeeper(t *testing.T, tm *tmux.Tmux) {
	t.Helper()

	if alive, err := tm.HasSession(keeperSession); err == nil && alive {
		return
	}
	if err := tm.NewSessionWithCommand(keeperSession, "", "sleep 3600"); err != nil {
		// A concurrent test in this binary may have won the race; that is
		// fine as long as the session exists now.
		if alive, probeErr := tm.HasSession(keeperSession); probeErr != nil || !alive {
			t.Fatalf("start keeper session: %v", err)
		}
	}
}

// flattenPane joins the newlines a pane inserts when a long line wraps.
//
// A message typed into a pane is rendered intact but can be split across two
// captured rows at the pane's column width, with no character added or removed
// at the break. Matching against the raw capture therefore misses it depending
// on where the break lands — the pane-render-width flake gt-isp0 fixed. Every
// pane text search in this file goes through here.
func flattenPane(capture string) string {
	return strings.ReplaceAll(capture, "\n", "")
}

// waitForPaneText blocks until the pane's flattened capture contains want.
// Callers use it as a barrier: wait for the pane to *show* that a keystroke
// took effect, instead of sleeping a fixed interval and hoping the shell got
// there in time.
func waitForPaneText(t *testing.T, tm *tmux.Tmux, session, want string, lines int) {
	t.Helper()

	deadline := time.Now().Add(paneBarrierTimeout)
	var last string
	for {
		out, err := tm.CapturePane(session, lines)
		if err == nil {
			last = out
			if strings.Contains(flattenPane(out), want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane never rendered %q within %s; last capture:\n%s", want, paneBarrierTimeout, last)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// markPaneBusy renders the Claude Code busy spinner into the session's pane
// and blocks until the shell has demonstrably run the command that prints it,
// then asserts the pane reads as busy.
//
// The spinner and a unique proof marker are printed by one command, so seeing
// the marker in the capture proves the spinner line above it is already
// rendered. The pane then stays busy for the life of the test: nothing pushes
// the spinner out of the capture window.
//
// The previous version sent the printf and then polled IsBusy against a fixed
// 5s deadline, failing the test if the pane had not gone busy by then. Under a
// loaded host (the full suite at -p=8 on the same box) that deadline is a
// wall-clock race against the shell's scheduling — the gt-38ss flake. The
// barrier below has no such race: the only bounded wait is for real work the
// test can see the result of.
func markPaneBusy(t *testing.T, tm *tmux.Tmux, session string) {
	t.Helper()

	// Spinner text is the shape IsBusy detects (busyTokenSpinnerPattern);
	// neither it nor the marker contains shell metacharacters, so a single
	// printf with both lines is safe to type into the pane.
	const (
		spinner = `✵ Leavening… (3m 17s · ↓ 14.1k tokens)`
		proof   = "gt-test-busy-proof"
	)

	if err := tm.SendKeys(session, fmt.Sprintf("printf '%s\\n%s\\n'", spinner, proof)); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	waitForPaneText(t, tm, session, proof, 20)

	if !tm.IsBusy(session) {
		out, _ := tm.CapturePane(session, 20)
		t.Fatalf("pane rendered the busy spinner but IsBusy reported false; pane:\n%s", out)
	}
}

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
	ensureServerKeeper(t, tm)
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
	markPaneBusy(t, tm, sessionName)

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
	// flattenPane is the more sensitive check here: removing the wrap
	// newlines can only join text back together, so a leak that landed across
	// a wrap is still caught.
	if strings.Contains(flattenPane(out), message) {
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
	ensureServerKeeper(t, tm)
	sessionName := "gt-test-nudge-immediate-force-busy"
	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	markPaneBusy(t, tm, sessionName)

	origMode, origForce := nudgeModeFlag, nudgeForceFlag
	nudgeModeFlag = NudgeModeImmediate
	nudgeForceFlag = true
	t.Cleanup(func() { nudgeModeFlag, nudgeForceFlag = origMode, origForce })

	const message = "should-be-typed-into-the-pane-because-forced"
	if err := deliverNudge(tm, sessionName, message, "tester"); err != nil {
		t.Fatalf("deliverNudge: %v", err)
	}

	// Wait for the delivered text to actually appear in the pane. The wait is
	// a barrier on the pane's contents, not a fixed budget: the message is one
	// long hyphenated token typed after a real shell prompt whose rendered
	// width varies, so flattenPane (see its comment) is what makes the search
	// independent of where the pane wrapped (gt-isp0).
	waitForPaneText(t, tm, sessionName, message, 40)
}
