package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

const currentSpinner = "✵ Leavening… (3m 17s · ↓ 14.1k tokens)"

// busyServer is a fakeServer with one session whose harness is pinned to
// agent (GT_AGENT) under a throwaway town root, on a fake clock starting at a
// whole second so #{window_activity}'s second resolution is exact.
func busyServer(t *testing.T, agent string) (*fakeServer, *fpane, *Tmux, *clockwork.FakeClock) {
	t.Helper()
	f := newFakeServer()
	s := f.addSession("gt-x", "claude")
	clk := clockwork.NewFakeClockAt(time.Unix(1_700_000_000, 0))
	f.with(func() {
		if agent != "" {
			s.env["GT_AGENT"] = agent
		}
		s.env["GT_ROOT"] = t.TempDir()
		s.panes[0].activity = clk.Now().Unix()
	})
	tm, _ := f.tmux(clk)
	return f, s.panes[0], tm, clk
}

func setScreen(f *fakeServer, p *fpane, lines ...string) {
	f.with(func() { p.content = strings.Join(lines, "\n") })
}

// TestWaitForIdle_Timeout: a pane running a command with no prompt never
// reads idle; the wait gives up with ErrIdleTimeout.
func TestWaitForIdle_Timeout(t *testing.T) {
	t.Parallel()
	f, p, tm, clk := busyServer(t, "")
	setScreen(f, p, "$ sleep 60")
	err := driven(t, clk, 200*time.Millisecond, func() error { return tm.WaitForIdle("gt-x", 500*time.Millisecond) })
	if !errors.Is(err, ErrIdleTimeout) {
		t.Errorf("expected ErrIdleTimeout, got: %v", err)
	}
}

// TestWaitForIdle_NeedsTwoIdlePolls: a prompt returns nil once seen on two
// consecutive polls.
func TestWaitForIdle_NeedsTwoIdlePolls(t *testing.T) {
	t.Parallel()
	f, p, tm, clk := busyServer(t, "")
	setScreen(f, p, "⏺ done", "❯ ")
	if err := driven(t, clk, 200*time.Millisecond, func() error { return tm.WaitForIdle("gt-x", 5*time.Second) }); err != nil {
		t.Fatalf("WaitForIdle = %v, want nil", err)
	}
}

// TestWaitForIdle_BusyMarkerBeatsPrompt: a busy marker vetoes a visible
// prompt, and a session that disappears ends the wait at once.
func TestWaitForIdle_BusyMarkerBeatsPrompt(t *testing.T) {
	t.Parallel()
	f, p, tm, clk := busyServer(t, "")
	setScreen(f, p, "❯ ", currentSpinner)
	err := driven(t, clk, 200*time.Millisecond, func() error { return tm.WaitForIdle("gt-x", time.Second) })
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("busy pane: err = %v, want ErrIdleTimeout", err)
	}
	f.with(func() { delete(f.sessions, "gt-x") })
	if err := tm.WaitForIdle("gt-x", time.Second); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("gone session: err = %v, want ErrSessionNotFound", err)
	}
}

func TestShouldSendEscape_Pane(t *testing.T) {
	t.Parallel()
	f, p, tm, _ := busyServer(t, "")
	setScreen(f, p, "user@host:~$ ")
	if !tm.shouldSendEscape("gt-x") {
		t.Fatal("shouldSendEscape on idle pane = false, want true")
	}
	setScreen(f, p, "$ echo hi", "hi", "esc to interrupt")
	if tm.shouldSendEscape("gt-x") {
		t.Fatal("shouldSendEscape with the legacy busy marker = true, want false")
	}
}

// TestShouldSendEscape_CurrentTUIMarker is the gt-8bh guard: the CURRENT
// Claude Code spinner (verified live 2026-09-09, gastownhall/gastown#4240)
// suppresses Escape even with filler lines under it inside the capture window.
func TestShouldSendEscape_CurrentTUIMarker(t *testing.T) {
	t.Parallel()
	f, p, tm, _ := busyServer(t, "")
	setScreen(f, p, "filler line 1", "filler line 2", currentSpinner, "", "❯ ")
	if tm.shouldSendEscape("gt-x") {
		t.Fatal("shouldSendEscape did not detect the current busy marker")
	}
}

func TestShouldSendEscape_CaptureErrorSuppressesEscape(t *testing.T) {
	t.Parallel()
	_, _, tm, _ := busyServer(t, "")
	if tm.shouldSendEscape("missing-session-for-escape-check") {
		t.Fatal("shouldSendEscape on missing target = true, want false")
	}
}

// TestEscapeSafe_CustomClaudeAgentOnIdlePane guards claude-9a8: a custom town
// agent whose provider is claude never gets Escape, even on an idle pane,
// while codex on the same pane does.
func TestEscapeSafe_CustomClaudeAgentOnIdlePane(t *testing.T) {
	t.Parallel()
	f, p, tm, _ := busyServer(t, "")
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "settings", "config.json"),
		[]byte(`{"type":"town-settings","version":1,"agents":{"test-9a8-flash":{"provider":"claude","command":"claude"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setScreen(f, p, "user@host:~$ ")
	f.with(func() {
		f.sessions["gt-x"].env["GT_ROOT"] = town
		f.sessions["gt-x"].env["GT_AGENT"] = "test-9a8-flash"
	})
	if tm.escapeSafe("gt-x", "gt-x", "") {
		t.Fatal("escapeSafe for custom claude agent on idle pane = true, want false")
	}
	f.with(func() { f.sessions["gt-x"].env["GT_AGENT"] = "codex" })
	if !tm.escapeSafe("gt-x", "gt-x", "") {
		t.Fatal("escapeSafe for codex on idle pane = false, want true")
	}
}

// TestIsBusy_Pane: idle reads not-busy, the current spinner reads busy
// (gt-cyyg gate for --mode=immediate).
func TestIsBusy_Pane(t *testing.T) {
	t.Parallel()
	f, p, tm, _ := busyServer(t, "")
	setScreen(f, p, "user@host:~$ ")
	if tm.IsBusy("gt-x") {
		t.Fatal("IsBusy on idle pane = true, want false")
	}
	setScreen(f, p, "filler", currentSpinner)
	if !tm.IsBusy("gt-x") {
		t.Fatal("IsBusy did not detect the busy marker")
	}
}

// TestIsBusy_StaleIndicatorExpires covers both sides of gt-z4gs's threshold on
// a Claude Code pane: a marker under a pane that went silent is trusted until
// isBusyStaleAfter and discounted after.
func TestIsBusy_StaleIndicatorExpires(t *testing.T) {
	t.Parallel()
	f, p, tm, clk := busyServer(t, "claude")
	setScreen(f, p, "filler", currentSpinner)
	clk.Advance(isBusyStaleAfter / 4)
	if !tm.IsBusy("gt-x") {
		t.Fatal("IsBusy a quarter of isBusyStaleAfter into silence = false, want true (marker still young)")
	}
	clk.Advance(isBusyStaleAfter)
	if tm.IsBusy("gt-x") {
		t.Fatal("IsBusy past isBusyStaleAfter of silence = true, want false (stale indicator)")
	}
}

// TestIsBusy_RefreshedMarkerStaysBusy: a pane that keeps repainting under its
// marker stays busy however long that goes on; the discount measures silence,
// not the marker's age.
func TestIsBusy_RefreshedMarkerStaysBusy(t *testing.T) {
	t.Parallel()
	f, p, tm, clk := busyServer(t, "claude")
	setScreen(f, p, currentSpinner)
	for elapsed := time.Duration(0); elapsed < 3*isBusyStaleAfter; elapsed += time.Second {
		clk.Advance(time.Second)
		f.with(func() { p.activity = clk.Now().Unix() })
		if !tm.IsBusy("gt-x") {
			t.Fatalf("IsBusy on a repainting pane = false after %s, want true", elapsed+time.Second)
		}
	}
}

// TestIsBusy_NonClaudeStaleMarkerStaysBusy is the gt-cyyg guard on the
// discount: only Claude Code markers are discounted for silence.
func TestIsBusy_NonClaudeStaleMarkerStaysBusy(t *testing.T) {
	t.Parallel()
	f, p, tm, clk := busyServer(t, "codex")
	setScreen(f, p, "filler", "esc to interrupt")
	clk.Advance(10 * isBusyStaleAfter)
	if !tm.IsBusy("gt-x") {
		t.Fatal("IsBusy on a silent pane showing a non-Claude marker = false, want true")
	}
}

// TestIsBusy_IgnoresQuotedSpinnerInTranscript reproduces gt-dq6pi: prose that
// quotes another pane's spinner is not this pane's status line.
func TestIsBusy_IgnoresQuotedSpinnerInTranscript(t *testing.T) {
	t.Parallel()
	f, p, tm, _ := busyServer(t, "")
	// What the old live-pane version of this test actually captured: the
	// pinned test shell's prompt echoing the command, then its output.
	setScreen(f, p, `bash-5.2$ printf 'live turn "Sautéing… 8m16s · ↓14.6k tokens"\\n'`, `live turn "Sautéing… 8m16s · ↓14.6k tokens"`, "bash-5.2$ ")
	if tm.IsBusy("gt-x") {
		t.Fatal("IsBusy on a pane whose only token-count text is quoted transcript = true, want false")
	}
}

// TestCapturePaneVisibleTail pins the capture the busy checks use: the
// visible screen only (no -S into history), trimmed to its last
// busyCaptureLines lines. Real tmux's -S semantics are pinned by
// TestIntegrationCapturePaneVisibleTail_ExcludesHistory.
func TestCapturePaneVisibleTail(t *testing.T) {
	t.Parallel()
	f, p, tm, _ := busyServer(t, "")
	var lines []string
	for i := 0; i < busyCaptureLines+5; i++ {
		lines = append(lines, "line")
	}
	lines[0] = "TOP_OF_SCREEN"
	setScreen(f, p, lines...)
	s := newScripted(f.answer)
	tm = unitTmux(s, nil)
	tail, err := tm.capturePaneVisibleTail("gt-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != busyCaptureLines || strings.Contains(strings.Join(tail, "\n"), "TOP_OF_SCREEN") {
		t.Fatalf("tail = %d lines (top included: %v), want the last %d", len(tail), strings.Contains(strings.Join(tail, "\n"), "TOP_OF_SCREEN"), busyCaptureLines)
	}
	if c := s.find("capture-pane"); len(c) != 1 || c[0].has("-S") {
		t.Fatalf("capture-pane = %v, want no -S history window", c)
	}
}

func TestIsBusy_CaptureErrorFailsSafeBusy(t *testing.T) {
	t.Parallel()
	_, _, tm, _ := busyServer(t, "")
	if !tm.IsBusy("missing-session-for-busy-check") {
		t.Fatal("IsBusy on missing target = false, want true (fail safe)")
	}
}
