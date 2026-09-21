package tmux

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
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

// analyzeComposerStateNoBusy skips the busy indicator check, so it can see
// pending input that a stale busy indicator would otherwise mask (gt-ncon).
func TestAnalyzeComposerStateNoBusy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		pane       string
		prefix     string
		want       ComposerState
		wantQueued bool
	}{
		{
			name:       "stale busy indicator with queued footer is pending",
			pane:       queuedMessagesPane + "\n✻ Simmering… (12s · ↓ 3.1k tokens)",
			prefix:     DefaultReadyPromptPrefix,
			want:       ComposerPending,
			wantQueued: true,
		},
		{
			name:   "stale busy indicator with typed input is pending",
			pane:   pendingTypedPane + "\n✻ Simmering… (12s · ↓ 3.1k tokens)",
			prefix: DefaultReadyPromptPrefix,
			want:   ComposerPending,
		},
		{
			name:   "stale busy indicator with empty composer is clean",
			pane:   liveIdleCleanPane + "\n✻ Simmering… (12s · ↓ 3.1k tokens)",
			prefix: DefaultReadyPromptPrefix,
			want:   ComposerClean,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := analyzeComposerStateNoBusy(tt.pane, tt.prefix)
			if got.State != tt.want {
				t.Errorf("state = %s, want %s (evidence: %s)", got.State, tt.want, got.Evidence)
			}
			if got.Queued != tt.wantQueued {
				t.Errorf("queued = %v, want %v", got.Queued, tt.wantQueued)
			}
		})
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
// live at 2026-09-18 16:13. A genuinely busy pane has recent activity because
// it's actively producing output.
func TestDetectComposerStallBusyPaneIsNotStalled(t *testing.T) {
	fakeTmuxLogging(t, map[string]string{
		"capture-pane":    liveBusyPane,
		"display-message": "@now", // A genuinely busy pane has recent activity
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

// A stale busy indicator (from a previous turn that has ended) with old
// activity and pending input is the gt-ncon signature: the detector must
// report a stall, not be blinded by the stale indicator.
func TestDetectComposerStallStaleBusyIndicatorIsStalled(t *testing.T) {
	fakeTmuxLogging(t, map[string]string{
		"capture-pane":    pendingTypedPane + "\n✻ Simmering… (12s · ↓ 3.1k tokens)",
		"display-message": "0", // Old activity: the busy indicator is stale
		"has-session":     "",
	})
	tm := NewTmuxWithSocket("gt-test-composer-stall")

	stall, err := tm.DetectComposerStall("gt-refinery", 5*60*1e9)
	if err != nil {
		t.Fatalf("DetectComposerStall: %v", err)
	}
	if stall.State != ComposerPending {
		t.Errorf("state = %s, want pending (stale busy indicator should be ignored)", stall.State)
	}
	if !stall.Stalled {
		t.Error("stale busy indicator with pending input should be reported as stalled")
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

// --- Input-consumption liveness (gt-eigw) --------------------------------

// consumptionVerdict is the whole classification, and it is pure, so every case
// here is a captured pane rather than a live session. The fixtures are the same
// live captures TestAnalyzeComposerState uses.
func TestConsumptionVerdict(t *testing.T) {
	t.Parallel()
	const prefix = DefaultReadyPromptPrefix

	tests := []struct {
		name     string
		baseline string
		current  string
		prefix   string
		want     InputConsumption
	}{
		{
			// The gt-eigw signature: the pane is byte-identical across the
			// window and the input is still sitting in Claude Code's queue.
			name:     "frozen pane still holding queued input is not consumed",
			baseline: queuedMessagesPane,
			current:  queuedMessagesPane,
			prefix:   prefix,
			want:     InputConsumptionNotConsumed,
		},
		{
			name:     "frozen pane still holding typed input is not consumed",
			baseline: pendingTypedPane,
			current:  pendingTypedPane,
			prefix:   prefix,
			want:     InputConsumptionNotConsumed,
		},
		{
			// The case the previous attempt got wrong: an idle target whose
			// composer is clean and whose turn may already have finished. That
			// is no evidence of a strand, and must never read as one.
			name:     "frozen pane with a clean composer is inconclusive",
			baseline: liveIdleCleanPane,
			current:  liveIdleCleanPane,
			prefix:   prefix,
			want:     InputConsumptionInconclusive,
		},
		{
			// Dim placeholder text is not input, so a frozen prompt showing it
			// is idle, not stranded.
			name:     "frozen pane showing dim placeholder is inconclusive",
			baseline: dimPlaceholderPane,
			current:  dimPlaceholderPane,
			prefix:   prefix,
			want:     InputConsumptionInconclusive,
		},
		{
			name:     "busy indicator means the turn started",
			baseline: liveIdleCleanPane,
			current:  liveBusyPane,
			prefix:   prefix,
			want:     InputConsumptionStartedTurn,
		},
		{
			// Busy wins over a still-visible queue footer: the running turn owns
			// whatever is queued behind it.
			name:     "busy indicator beats a queued footer",
			baseline: queuedMessagesPane,
			current:  queuedMessagesPane + "\n✻ Simmering… (12s · ↓ 3.1k tokens)",
			prefix:   prefix,
			want:     InputConsumptionStartedTurn,
		},
		{
			// A pane already busy at the baseline is a session that is working;
			// whatever we handed it is in the turn's queue.
			name:     "pane busy at the baseline counts as started",
			baseline: liveBusyPane,
			current:  liveBusyPane,
			prefix:   prefix,
			want:     InputConsumptionStartedTurn,
		},
		{
			// The input was acted on: the transcript grew.
			name:     "pane output means the input was consumed",
			baseline: liveIdleCleanPane,
			current:  "⏺ Resume the patrol.\n\n" + liveIdleCleanPane,
			prefix:   prefix,
			want:     InputConsumptionPaneChanged,
		},
		{
			// Consumed and back to idle: the queued input is gone, so there is
			// nothing stranded even though the pane is quiet now.
			name:     "queue drained back to a clean composer is pane-changed",
			baseline: queuedMessagesPane,
			current:  liveIdleCleanPane,
			prefix:   prefix,
			want:     InputConsumptionPaneChanged,
		},
		{
			name:     "agent without a prompt prefix is unknown",
			baseline: queuedMessagesPane,
			current:  queuedMessagesPane,
			prefix:   "",
			want:     InputConsumptionUnknown,
		},
		{
			name:     "no composer line in the capture is inconclusive",
			baseline: "⏺ Build finished.\n  ⏵⏵ bypass permissions on",
			current:  "⏺ Build finished.\n  ⏵⏵ bypass permissions on",
			prefix:   prefix,
			want:     InputConsumptionInconclusive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := consumptionVerdict(tt.baseline, tt.current, tt.prefix); got != tt.want {
				t.Errorf("consumptionVerdict() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestInputConsumptionString(t *testing.T) {
	t.Parallel()
	for verdict, want := range map[InputConsumption]string{
		InputConsumptionInconclusive: "inconclusive",
		InputConsumptionUnknown:      "unknown",
		InputConsumptionStartedTurn:  "turn-started",
		InputConsumptionPaneChanged:  "pane-changed",
		InputConsumptionNotConsumed:  "not-consumed",
	} {
		if got := verdict.String(); got != want {
			t.Errorf("InputConsumption(%d).String() = %q, want %q", verdict, got, want)
		}
	}
}

// Only the two positive verdicts count as consumption: neither "no evidence"
// verdict may be mistaken for a healthy session by a caller that forgets to
// distinguish them.
func TestInputConsumptionConsumed(t *testing.T) {
	t.Parallel()
	tests := map[InputConsumption]bool{
		InputConsumptionInconclusive: false,
		InputConsumptionUnknown:      false,
		InputConsumptionStartedTurn:  true,
		InputConsumptionPaneChanged:  true,
		InputConsumptionNotConsumed:  false,
	}
	for verdict, want := range tests {
		if got := verdict.Consumed(); got != want {
			t.Errorf("%s.Consumed() = %v, want %v", verdict, got, want)
		}
	}
}

// fakeTmuxCaptures installs a tmux shim that returns captures[i] for the i-th
// capture-pane call and repeats the last entry thereafter, so a test can drive
// the probe through a sequence of pane states. It returns the call-count file
// so a test can assert how many times the pane was read.
func fakeTmuxCaptures(t *testing.T, captures []string) (logPath, countPath string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}
	if len(captures) == 0 {
		t.Fatal("fakeTmuxCaptures needs at least one capture")
	}

	binDir := t.TempDir()
	logPath = filepath.Join(binDir, "tmux.log")
	countPath = filepath.Join(binDir, "capture.count")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`printf '%s\n' "$*" >> "` + logPath + `"` + "\n")
	b.WriteString(`for a in "$@"; do case "$a" in` + "\n")
	b.WriteString("\tcapture-pane|display-message|has-session|send-keys|show-environment) sub=$a; break;;\n")
	b.WriteString("\tesac; done\n")
	b.WriteString(`if [ "$sub" = "capture-pane" ]; then` + "\n")
	b.WriteString(`  n=$(cat "` + countPath + `" 2>/dev/null || echo 0)` + "\n")
	b.WriteString(`  echo $((n+1)) > "` + countPath + `"` + "\n")
	b.WriteString("  case \"$n\" in\n")
	for i, capture := range captures {
		if i == len(captures)-1 {
			break
		}
		b.WriteString("\t" + strconv.Itoa(i) + ") printf '%s' '" + capture + "'; exit 0;;\n")
	}
	b.WriteString("\t*) printf '%s' '" + captures[len(captures)-1] + "'; exit 0;;\n")
	b.WriteString("  esac\n")
	b.WriteString("fi\n")
	b.WriteString("exit 0\n")

	scriptPath := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(scriptPath, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath, countPath
}

// withFastConsumptionPolling shrinks the probe's poll interval for the duration
// of a test. It is package state, so callers must not run in parallel with it.
func withFastConsumptionPolling(t *testing.T) {
	t.Helper()
	original := inputConsumptionPollInterval
	inputConsumptionPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { inputConsumptionPollInterval = original })
}

func captureCallCount(t *testing.T, countPath string) int {
	t.Helper()
	data, err := os.ReadFile(countPath)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing capture count %q: %v", data, err)
	}
	return n
}

// The wedged session from gt-eigw: the pane never changes and the input stays
// queued, so the probe must report NotConsumed rather than a successful
// delivery.
func TestWaitForInputConsumedDetectsWedgedSession(t *testing.T) {
	withFastConsumptionPolling(t)
	_, countPath := fakeTmuxCaptures(t, []string{queuedMessagesPane})
	tm := NewTmuxWithSocket("gt-test-consumption")

	verdict, err := tm.WaitForInputConsumed("gt-refinery", 40*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForInputConsumed: %v", err)
	}
	if verdict != InputConsumptionNotConsumed {
		t.Fatalf("verdict = %s, want %s", verdict, InputConsumptionNotConsumed)
	}
	// One baseline read plus at least one observation: the verdict has to be
	// reached by watching, not by reading once.
	if got := captureCallCount(t, countPath); got < 2 {
		t.Errorf("capture-pane called %d time(s), want the pane to be observed over the window", got)
	}
}

// A healthy target that starts a turn must be reported as consumed, and the
// probe must return as soon as it sees the reaction rather than waiting out the
// window.
func TestWaitForInputConsumedReturnsOnTurnStart(t *testing.T) {
	withFastConsumptionPolling(t)
	// Impossible to satisfy honestly in wall-clock terms with a long window:
	// the probe can only succeed early.
	_, countPath := fakeTmuxCaptures(t, []string{queuedMessagesPane, liveBusyPane})
	tm := NewTmuxWithSocket("gt-test-consumption")

	verdict, err := tm.WaitForInputConsumed("gt-refinery", 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForInputConsumed: %v", err)
	}
	if verdict != InputConsumptionStartedTurn {
		t.Fatalf("verdict = %s, want %s", verdict, InputConsumptionStartedTurn)
	}
	if got := captureCallCount(t, countPath); got > 3 {
		t.Errorf("capture-pane called %d times before returning, want an early return", got)
	}
}

// The probe must wait out the whole window before judging: a target that is
// slow to start is not a wedged one. Here the pane is frozen and pending for
// the first observations and only repaints later.
func TestWaitForInputConsumedWaitsBeforeJudging(t *testing.T) {
	withFastConsumptionPolling(t)
	repainted := "⏺ Resuming the patrol.\n\n" + liveIdleCleanPane
	_, countPath := fakeTmuxCaptures(t, []string{
		queuedMessagesPane,
		queuedMessagesPane,
		queuedMessagesPane,
		queuedMessagesPane,
		repainted,
	})
	tm := NewTmuxWithSocket("gt-test-consumption")

	verdict, err := tm.WaitForInputConsumed("gt-refinery", 2*time.Second)
	if err != nil {
		t.Fatalf("WaitForInputConsumed: %v", err)
	}
	if verdict != InputConsumptionPaneChanged {
		t.Fatalf("verdict = %s, want %s", verdict, InputConsumptionPaneChanged)
	}
	if got := captureCallCount(t, countPath); got < 5 {
		t.Errorf("capture-pane called %d times, want the probe to keep watching past a slow start", got)
	}
}

// An idle target whose composer is clean is inconclusive, never a strand. This
// is the regression guard for the rejection of attempt 1 (gt-eigw).
func TestWaitForInputConsumedIdleTargetIsInconclusive(t *testing.T) {
	withFastConsumptionPolling(t)
	fakeTmuxCaptures(t, []string{liveIdleCleanPane})
	tm := NewTmuxWithSocket("gt-test-consumption")

	verdict, err := tm.WaitForInputConsumed("gt-refinery", 40*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForInputConsumed: %v", err)
	}
	if verdict == InputConsumptionNotConsumed {
		t.Fatal("an idle target with a clean composer was reported as not consuming input")
	}
	if verdict != InputConsumptionInconclusive {
		t.Fatalf("verdict = %s, want %s", verdict, InputConsumptionInconclusive)
	}
}

func TestWaitForInputConsumedRejectsEmptySession(t *testing.T) {
	tm := NewTmuxWithSocket("gt-test-consumption")
	verdict, err := tm.WaitForInputConsumed("  ", time.Second)
	if err == nil {
		t.Fatal("expected an error for an empty session name")
	}
	if verdict != InputConsumptionUnknown {
		t.Errorf("verdict = %s, want %s", verdict, InputConsumptionUnknown)
	}
}
