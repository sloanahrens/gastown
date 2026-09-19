package tmux

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Fixtures below are pane captures. The busy and idle-clean ones were taken
// live from the gastown refinery pane on 2026-09-18 (tmux capture-pane -p -e),
// with only the surrounding scrollback trimmed — the point of using real
// captures is that the TUI's exact escape layout is what the parser must
// survive.
const (
	// liveBusyPane: a refinery mid `make lint`, showing the spinner line and
	// the "esc to interrupt" status bar. Copied verbatim from the live pane.
	liveBusyPane = "\x1b[38;5;246m❯\x1b[39m Running \x1b[1m1\x1b[0m shell command\x1b[38;5;246m · 34s\x1b[39m…\n" +
		"\x1b[38;5;246m  ⏵  $ make lint 2>&1 | tail -3 (34s)\x1b[39m\n" +
		"     \x1b[38;5;246m(ctrl+b ctrl+b (twice) to run in background)\x1b[39m\n" +
		"\n" +
		"\x1b[38;5;174m✻\x1b[39m \x1b[38;5;174mScampering…\x1b[39m \x1b[38;5;246m(1m 17s · ↓\x1b[39m \x1b[38;5;246m2.4k tokens)\x1b[39m\n" +
		"\x1b[39m  \x1b[38;5;211m⏵⏵ bypass permissions on\x1b[38;5;246m (shift+tab to cycle) · esc to interrupt\x1b[39m"

	// liveIdleCleanPane: the same pane between turns — empty composer (the
	// reverse-video block is the cursor, not text), no busy marker.
	liveIdleCleanPane = "\x1b[38;5;246m❯\x1b[39m Guard passes. Rehearsing.\n" +
		"\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"\x1b[38;5;246m❯\x1b[39m\x1b[7m \x1b[0m\n" +
		"\x1b[38;5;244m────────────────────────────────────────────────────────────────────────────────\x1b[0m\n" +
		"\x1b[39m  \x1b[38;5;211m⏵⏵ bypass permissions on\x1b[38;5;246m (shift+tab to cycle) · ← for agents · ↓ to manage\x1b[39m"

	// pendingTypedPane: the gt-hkhu signature — a MERGE_READY nudge typed into
	// the composer and never submitted, agent otherwise idle.
	pendingTypedPane = "⏺ Queue empty. Awaiting work.\n" +
		"\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"❯ [from gastown/sling] MERGE_READY received - check inbox for pending work\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage"

	// queuedMessagesPane: input sitting in Claude Code's input queue — the
	// composer itself is empty, the footer below it is the signal.
	queuedMessagesPane = "⏺ Waiting on the review.\n" +
		"\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"❯ \n" +
		"  Press up to edit queued messages · ctrl+x ctrl+s to send now\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage"

	// dimPlaceholderPane: the TUI renders hint text in the composer dimmed. It
	// is not input and must not read as pending.
	dimPlaceholderPane = "\x1b[2m❯ Try \"fix the failing build\"\x1b[0m\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)"
)

func TestAnalyzeComposerState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		pane       string
		prefix     string
		want       ComposerState
		wantEvid   string
		wantQueued bool
	}{
		{
			name:     "live busy pane is busy, not stranded",
			pane:     liveBusyPane,
			prefix:   DefaultReadyPromptPrefix,
			want:     ComposerBusy,
			wantEvid: "busy indicator in pane",
		},
		{
			name:     "live idle pane with empty composer is clean",
			pane:     liveIdleCleanPane,
			prefix:   DefaultReadyPromptPrefix,
			want:     ComposerClean,
			wantEvid: "composer empty",
		},
		{
			name:     "typed but unsubmitted input is pending",
			pane:     pendingTypedPane,
			prefix:   DefaultReadyPromptPrefix,
			want:     ComposerPending,
			wantEvid: "MERGE_READY received",
		},
		{
			name:       "queued messages footer is pending and reported as queued",
			pane:       queuedMessagesPane,
			prefix:     DefaultReadyPromptPrefix,
			want:       ComposerPending,
			wantEvid:   "queued-messages footer visible",
			wantQueued: true,
		},
		{
			name:     "dim placeholder text is not input",
			pane:     dimPlaceholderPane,
			prefix:   DefaultReadyPromptPrefix,
			want:     ComposerClean,
			wantEvid: "composer empty",
		},
		{
			name:     "busy beats a queued footer",
			pane:     queuedMessagesPane + "\n✻ Simmering… (12s · ↓ 3.1k tokens)",
			prefix:   DefaultReadyPromptPrefix,
			want:     ComposerBusy,
			wantEvid: "busy indicator in pane",
		},
		{
			name:     "agent without a prompt prefix is unknown",
			pane:     pendingTypedPane,
			prefix:   "",
			want:     ComposerUnknown,
			wantEvid: "no prompt prefix",
		},
		{
			name:     "pane with no composer line is unknown",
			pane:     "⏺ Build finished.\n  ⏵⏵ bypass permissions on",
			prefix:   DefaultReadyPromptPrefix,
			want:     ComposerUnknown,
			wantEvid: "no composer line",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := analyzeComposerState(tt.pane, tt.prefix)
			if got.State != tt.want {
				t.Errorf("state = %s, want %s (evidence: %s)", got.State, tt.want, got.Evidence)
			}
			if !strings.Contains(got.Evidence, tt.wantEvid) {
				t.Errorf("evidence = %q, want it to contain %q", got.Evidence, tt.wantEvid)
			}
			if got.Queued != tt.wantQueued {
				t.Errorf("queued = %v, want %v", got.Queued, tt.wantQueued)
			}
		})
	}
}

// The queued-message footer is the one string-matched signal in the detector,
// so its wording is pinned: a rename upstream must land here deliberately.
func TestHasQueuedMessagesFooter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"live footer", "  Press up to edit queued messages · ctrl+x ctrl+s to send now", true},
		{"wrapped send-now tail", "ctrl+x ctrl+s to send now", true},
		{"bare send-now phrase", "to send now", true},
		{"ordinary prose mentioning queue", "the merge queue is empty", false},
		{"status bar", "⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage", false},
		{"empty", "", false},
		{"whitespace", "   ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasQueuedMessagesFooter(tt.line); got != tt.want {
				t.Errorf("hasQueuedMessagesFooter(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestComposerStateString(t *testing.T) {
	t.Parallel()
	for state, want := range map[ComposerState]string{
		ComposerUnknown: "unknown",
		ComposerClean:   "clean",
		ComposerBusy:    "busy",
		ComposerPending: "pending",
	} {
		if got := state.String(); got != want {
			t.Errorf("ComposerState(%d).String() = %q, want %q", state, got, want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	if got := truncateRunes("abc", 5); got != "abc" {
		t.Errorf("short string changed: %q", got)
	}
	if got := truncateRunes("abcdefgh", 3); got != "abc…" {
		t.Errorf("truncateRunes = %q, want %q", got, "abc…")
	}
	if got := truncateRunes(strings.Repeat("é", 10), 4); got != "éééé…" {
		t.Errorf("rune boundary broken: %q", got)
	}
}

// fakeTmuxLogging installs a tmux shim on PATH that records its argv and
// returns canned stdout keyed by the tmux subcommand. Returns the log path.
//
// Response value "@now" expands to the current Unix timestamp, which is what a
// session producing output right now reports for #{window_activity}.
func fakeTmuxLogging(t *testing.T, responses map[string]string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "tmux.log")
	scriptPath := filepath.Join(binDir, "tmux")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`printf '%s\n' "$*" >> "` + logPath + `"` + "\n")
	// The subcommand is the first argument naming a tmux command; -L and its
	// socket value, other flags, and trailing args are all ignored.
	b.WriteString(`for a in "$@"; do case "$a" in` + "\n")
	b.WriteString("\tcapture-pane|display-message|has-session|send-keys|show-environment) sub=$a; break;;\n")
	b.WriteString("\tesac; done\n")
	b.WriteString(`case "$sub" in` + "\n")
	for sub, out := range responses {
		if out == "@now" {
			b.WriteString("\t" + sub + ") date +%s; exit 0;;\n")
			continue
		}
		b.WriteString("\t" + sub + ") printf '%s' '" + out + "'; exit 0;;\n")
	}
	b.WriteString("\t*) exit 0;;\n")
	b.WriteString("esac\n")

	if err := os.WriteFile(scriptPath, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// SubmitPendingInput must send exactly the keystroke that matches the pending
// state: ctrl+x ctrl+s for queued messages (the operator's unblock on
// 2026-09-18) and a bare Enter for text typed into the composer.
func TestSubmitPendingInputKeystrokes(t *testing.T) {
	tests := []struct {
		name   string
		queued bool
		want   string
	}{
		{"queued uses send-now", true, "C-x C-s"},
		{"typed text uses Enter", false, "Enter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logPath := fakeTmuxLogging(t, nil)
			tm := NewTmuxWithSocket("gt-test-composer-stall")

			if err := tm.SubmitPendingInput("gt-refinery:0.0", tt.queued); err != nil {
				t.Fatalf("SubmitPendingInput: %v", err)
			}

			logged, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read tmux log: %v", err)
			}
			got := string(logged)
			if !strings.Contains(got, "send-keys") {
				t.Fatalf("no send-keys invocation logged: %q", got)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("logged %q, want keystroke %q", got, tt.want)
			}
			other := "Enter"
			if !tt.queued {
				other = "C-x C-s"
			}
			if strings.Contains(got, other) {
				t.Errorf("logged %q, must not send %q for this state", got, other)
			}
		})
	}
}

// DetectComposerStall must never report a stall on a pane that is busy, no
// matter how the composer looks — this is the false positive the witness hit
// live at 2026-09-18 16:13.
func TestDetectComposerStallBusyPaneIsNotStalled(t *testing.T) {
	fakeTmuxLogging(t, map[string]string{
		"capture-pane":    liveBusyPane,
		"display-message": "0",
		"has-session":     "",
	})
	tm := NewTmuxWithSocket("gt-test-composer-stall")

	stall, err := tm.DetectComposerStall("gt-refinery", 5*60*1e9)
	if err != nil {
		t.Fatalf("DetectComposerStall: %v", err)
	}
	if stall.State != ComposerBusy {
		t.Errorf("state = %s, want busy", stall.State)
	}
	if stall.Stalled {
		t.Error("busy pane reported as stalled")
	}
}

// A pending composer on a session that is still producing output is not a
// stall: that is a nudge mid-turn, which the running turn will consume.
func TestDetectComposerStallRecentActivityIsNotStalled(t *testing.T) {
	fakeTmuxLogging(t, map[string]string{
		"capture-pane": pendingTypedPane,
		// window_activity is a Unix timestamp; "now" means the session
		// produced output this second.
		"display-message": "@now",
		"has-session":     "",
	})
	tm := NewTmuxWithSocket("gt-test-composer-stall")

	stall, err := tm.DetectComposerStall("gt-refinery", 5*60*1e9)
	if err != nil {
		t.Fatalf("DetectComposerStall: %v", err)
	}
	if stall.State != ComposerPending {
		t.Fatalf("state = %s, want pending", stall.State)
	}
	if stall.Stalled {
		t.Error("pending composer with fresh activity reported as stalled")
	}
}

// The gt-hkhu signature: unsubmitted input plus a session that has produced
// nothing for the whole window. Both signals are required, so this is a stall.
func TestDetectComposerStallPendingAndFrozenIsStalled(t *testing.T) {
	fakeTmuxLogging(t, map[string]string{
		"capture-pane": pendingTypedPane,
		// 2023-11-14 — far older than any threshold, i.e. no pane output since.
		"display-message": "1700000000",
		"has-session":     "",
	})
	tm := NewTmuxWithSocket("gt-test-composer-stall")

	stall, err := tm.DetectComposerStall("gt-refinery", 5*60*1e9)
	if err != nil {
		t.Fatalf("DetectComposerStall: %v", err)
	}
	if !stall.Stalled {
		t.Fatalf("pending composer frozen well past the threshold not reported as stalled (state %s, inactivity %s)",
			stall.State, stall.Inactivity)
	}
	if stall.Queued {
		t.Error("typed composer text misreported as queued")
	}
}

// An empty composer on a frozen session is just an idle agent: no input is
// waiting, so nothing is stranded.
func TestDetectComposerStallEmptyComposerIsNotStalled(t *testing.T) {
	fakeTmuxLogging(t, map[string]string{
		"capture-pane":    liveIdleCleanPane,
		"display-message": "1700000000",
		"has-session":     "",
	})
	tm := NewTmuxWithSocket("gt-test-composer-stall")

	stall, err := tm.DetectComposerStall("gt-refinery", 5*60*1e9)
	if err != nil {
		t.Fatalf("DetectComposerStall: %v", err)
	}
	if stall.Stalled {
		t.Errorf("idle agent with an empty composer reported as stalled (state %s)", stall.State)
	}
	if stall.Inactivity != 0 {
		t.Errorf("inactivity measured for a non-pending composer: %s", stall.Inactivity)
	}
}

// A queued footer with a frozen session is the refinery failure from
// gt-hkhu's second instance, and must report Queued so recovery uses
// ctrl+x ctrl+s rather than Enter.
func TestDetectComposerStallQueuedFooterIsStalledAndQueued(t *testing.T) {
	fakeTmuxLogging(t, map[string]string{
		"capture-pane":    queuedMessagesPane,
		"display-message": "1700000000",
		"has-session":     "",
	})
	tm := NewTmuxWithSocket("gt-test-composer-stall")

	stall, err := tm.DetectComposerStall("gt-refinery", 5*60*1e9)
	if err != nil {
		t.Fatalf("DetectComposerStall: %v", err)
	}
	if !stall.Stalled || !stall.Queued {
		t.Errorf("queued footer on a frozen session: stalled=%v queued=%v, want both true", stall.Stalled, stall.Queued)
	}
}
