package witness

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// The detection logic itself is unit-tested in internal/tmux (see
// composer_stall_test.go there, including the live-capture fixtures). These
// tests cover the witness-side wiring: which sessions it looks at, what it does
// on a confirmed stall, and — most importantly — that it does nothing at all in
// the cases where the refinery is simply not there.

const (
	// A refinery holding a nudge that was typed while it was mid-turn and never
	// submitted: the gt-hkhu signature.
	stalledRefineryPane = "⏺ Queue empty. Awaiting work.\n" +
		"\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"❯ [from gastown/sling] MERGE_READY received - check inbox for pending work\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage"

	// A healthy working refinery: busy spinner present.
	workingRefineryPane = "❯ Running 1 shell command · 12s…\n" +
		"\x1b[38;5;174m✻\x1b[39m \x1b[38;5;174mScampering…\x1b[39m \x1b[38;5;246m(1m 17s · ↓ 2.4k tokens)\x1b[39m\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt"
)

// fakeTmuxShim installs a tmux shim on PATH and returns the path of its call
// log. Responses are keyed by subcommand, or by "subcommand:needle" to vary the
// answer by the format string a display-message call asks for. Three values are
// special: "@now" (current Unix time), "@fail" (exit 1, as tmux does for a
// missing session), and "@missing" (exit 1 with tmux's "unknown variable"
// stderr, which is how a session that never set GT_PROCESS_NAMES answers).
func fakeTmuxShim(t *testing.T, responses map[string]string) string {
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
	b.WriteString(`for a in "$@"; do case "$a" in` + "\n")
	b.WriteString("\tcapture-pane|display-message|has-session|send-keys|show-environment|list-panes) sub=$a; break;;\n")
	b.WriteString("\tesac; done\n")
	// display-message's last argument is its format string; make it part of the
	// key so one shim can answer pane_current_command and window_activity.
	b.WriteString(`key=$sub` + "\n")
	b.WriteString(`for a in "$@"; do case "$a" in '#'*) key="$sub:$a";; esac; done` + "\n")
	b.WriteString(`case "$key" in` + "\n")
	for key, out := range responses {
		switch out {
		case "@now":
			b.WriteString("\t'" + key + "') date +%s; exit 0;;\n")
		case "@fail":
			b.WriteString("\t'" + key + "') exit 1;;\n")
		case "@missing":
			b.WriteString("\t'" + key + "') printf '%s\\n' 'unknown variable: not set' 1>&2; exit 1;;\n")
		default:
			b.WriteString("\t'" + key + "') printf '%s' '" + out + "'; exit 0;;\n")
		}
	}
	b.WriteString("\t*) exit 0;;\n")
	b.WriteString("esac\n")

	if err := os.WriteFile(scriptPath, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// A refinery that is alive, has had its composer holding an unsubmitted nudge
// for well past the frozen window, and has produced no output in that time is
// the gt-hkhu failure. The scan must submit the input and say so.
func TestDetectStalledRefinerySubmitsPendingInput(t *testing.T) {
	// The shim keeps serving the same stalled pane, so the post-submit recheck
	// can never observe a recovery; zero the settle delay so the test does not
	// pay three of them.
	defer func(d time.Duration) { composerStallRecheckDelay = d }(composerStallRecheckDelay)
	composerStallRecheckDelay = 0

	logPath := fakeTmuxShim(t, map[string]string{
		"has-session":                             "",
		"show-environment":                        "@missing",
		"capture-pane":                            stalledRefineryPane,
		"display-message:#{window_activity}":      "1700000000", // 2023-11-14: silent since
		"display-message:#{pane_current_command}": "claude",
	})

	result := DetectStalledRefinery(t.TempDir(), "gastown", false)

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}

	if result.Checked != 1 {
		t.Fatalf("Checked = %d, want 1", result.Checked)
	}
	if len(result.Stalls) != 1 {
		t.Fatalf("Stalls = %d, want 1 (errors: %v)\ntmux calls:\n%s", len(result.Stalls), result.Errors, logged)
	}
	stall := result.Stalls[0]
	if stall.StallType != "composer-stall" {
		t.Errorf("StallType = %q, want composer-stall", stall.StallType)
	}
	if stall.Action != ComposerStallActionStillPending && stall.Action != ComposerStallActionSubmittedEnter {
		t.Errorf("Action = %q, want a submit action or still-pending", stall.Action)
	}
	if stall.Session != "gt-refinery" {
		t.Errorf("Session = %q, want gt-refinery", stall.Session)
	}
	if stall.Inactivity < 5*60*1e9 {
		t.Errorf("Inactivity = %s, want at least the 5m frozen window", stall.Inactivity)
	}

	if !strings.Contains(string(logged), "send-keys") {
		t.Errorf("no keystroke sent to the stalled refinery; log:\n%s", logged)
	}
}

// dryRun must report the same confirmed stall without touching the live
// session — the gate the om major on gt-wisp-q9os asked for.
func TestDetectStalledRefineryDryRunSkipsSubmit(t *testing.T) {
	logPath := fakeTmuxShim(t, map[string]string{
		"has-session":                             "",
		"show-environment":                        "@missing",
		"capture-pane":                            stalledRefineryPane,
		"display-message:#{window_activity}":      "1700000000", // 2023-11-14: silent since
		"display-message:#{pane_current_command}": "claude",
	})

	result := DetectStalledRefinery(t.TempDir(), "gastown", true)

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}

	if result.Checked != 1 {
		t.Fatalf("Checked = %d, want 1", result.Checked)
	}
	if len(result.Stalls) != 1 {
		t.Fatalf("Stalls = %d, want 1 (errors: %v)\ntmux calls:\n%s", len(result.Stalls), result.Errors, logged)
	}
	stall := result.Stalls[0]
	if stall.Action != ComposerStallActionDetectedDryRun {
		t.Errorf("Action = %q, want %q", stall.Action, ComposerStallActionDetectedDryRun)
	}
	if stall.Error != nil {
		t.Errorf("Error = %v, want nil on a dry-run detection", stall.Error)
	}

	if strings.Contains(string(logged), "send-keys") {
		t.Errorf("keystrokes sent to the refinery on a dry run; log:\n%s", logged)
	}
}

// A refinery that is actively working must be left completely alone: this is
// the false positive the witness hit live on 2026-09-18 16:13.
func TestDetectStalledRefineryLeavesWorkingRefineryAlone(t *testing.T) {
	logPath := fakeTmuxShim(t, map[string]string{
		"has-session":                             "",
		"show-environment":                        "@missing",
		"capture-pane":                            workingRefineryPane,
		"display-message:#{window_activity}":      "@now",
		"display-message:#{pane_current_command}": "claude",
	})

	result := DetectStalledRefinery(t.TempDir(), "gastown", false)

	if len(result.Stalls) != 0 {
		t.Errorf("working refinery reported as stalled: %+v", result.Stalls)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	if strings.Contains(string(logged), "send-keys") {
		t.Errorf("keystrokes sent to a working refinery; log:\n%s", logged)
	}
}

// A refinery with no running session is the daemon's business, not this scan's.
func TestDetectStalledRefinerySkipsMissingSession(t *testing.T) {
	logPath := fakeTmuxShim(t, map[string]string{
		"has-session":      "@fail",
		"show-environment": "@missing",
		"capture-pane":     stalledRefineryPane,
	})

	result := DetectStalledRefinery(t.TempDir(), "gastown", false)

	if result.Checked != 0 {
		t.Errorf("Checked = %d, want 0 for a missing session", result.Checked)
	}
	if len(result.Stalls) != 0 {
		t.Errorf("stall reported for a missing session: %+v", result.Stalls)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	if strings.Contains(string(logged), "send-keys") {
		t.Errorf("keystrokes sent to a missing refinery; log:\n%s", logged)
	}
}

// A dead agent process inside a live session belongs to the daemon's respawn
// path. The pane may still show the stale composed text, so this guards against
// poking a session whose Claude has already exited.
func TestDetectStalledRefinerySkipsDeadAgent(t *testing.T) {
	logPath := fakeTmuxShim(t, map[string]string{
		"has-session":                             "",
		"show-environment":                        "@missing",
		"capture-pane":                            stalledRefineryPane,
		"display-message:#{window_activity}":      "1700000000",
		"display-message:#{pane_current_command}": "zsh",
		"list-panes":                              "zsh\t1",
	})

	result := DetectStalledRefinery(t.TempDir(), "gastown", false)

	if len(result.Stalls) != 0 {
		t.Errorf("stall reported for a session with no live agent: %+v", result.Stalls)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	if strings.Contains(string(logged), "send-keys") {
		t.Errorf("keystrokes sent to a session with no live agent; log:\n%s", logged)
	}
}

func TestComposerStallActionFor(t *testing.T) {
	t.Parallel()
	if got := composerStallActionFor(true); got != ComposerStallActionSubmittedQueued {
		t.Errorf("queued action = %q, want %q", got, ComposerStallActionSubmittedQueued)
	}
	if got := composerStallActionFor(false); got != ComposerStallActionSubmittedEnter {
		t.Errorf("typed action = %q, want %q", got, ComposerStallActionSubmittedEnter)
	}
}

// The post-submit recheck has three outcomes, and the third one is the reason
// this exists as its own function: a pane that cannot be classified is neither
// a pass nor a strand. An unclassifiable capture is very likely what a pane
// looks like three quarters of a second after ctrl+x ctrl+s, while it repaints
// into its new turn — reading that as "still holding input" would send the
// patrol after a restart for a session that had just recovered (gt-afa7).
func TestConfirmComposerClearedDistinguishesUnverifiableFromStranded(t *testing.T) {
	defer func(d time.Duration) { composerStallRecheckDelay = d }(composerStallRecheckDelay)
	composerStallRecheckDelay = 0

	tests := []struct {
		name         string
		pane         string
		wantStranded bool
		wantUnverifi bool
	}{
		{
			// The flush worked: the composer is empty again.
			name: "cleared composer is a pass",
			pane: "⏺ Waiting on the review.\n" +
				"────────────────────────────────────────────────────────────────────────────────\n" +
				"❯ \n" +
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
		},
		{
			// Positive evidence the input is still sitting there.
			name:         "input still in the composer is a strand",
			pane:         stalledRefineryPane,
			wantStranded: true,
		},
		{
			// No prompt prefix, so the pane says nothing either way.
			name:         "unclassifiable pane is unverifiable",
			pane:         "⏺ Build finished.\n  ⏵⏵ bypass permissions on (shift+tab to cycle)",
			wantUnverifi: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeTmuxShim(t, map[string]string{
				"has-session":                             "",
				"capture-pane":                            tt.pane,
				"display-message:#{window_activity}":      "1700000000",
				"display-message:#{pane_current_command}": "claude",
			})

			err := confirmComposerCleared(tmux.NewTmux(), "gt-refinery", 5*time.Minute)

			if got := errors.Is(err, errComposerUnverifiable); got != tt.wantUnverifi {
				t.Errorf("unverifiable = %v, want %v (err: %v)", got, tt.wantUnverifi, err)
			}
			if got := err != nil && !errors.Is(err, errComposerUnverifiable); got != tt.wantStranded {
				t.Errorf("stranded = %v, want %v (err: %v)", got, tt.wantStranded, err)
			}
		})
	}
}

// A capture that fails outright is neither of those: it is a failure of the
// check, and it must not be reported as an unverifiable composer, because the
// caller treats that as "submitted, could not confirm" and reports it beside
// the submit rather than as an error.
func TestConfirmComposerClearedPropagatesACaptureFailure(t *testing.T) {
	defer func(d time.Duration) { composerStallRecheckDelay = d }(composerStallRecheckDelay)
	composerStallRecheckDelay = 0

	fakeTmuxShim(t, map[string]string{
		"has-session":                             "",
		"capture-pane":                            "@fail",
		"display-message:#{window_activity}":      "1700000000",
		"display-message:#{pane_current_command}": "claude",
	})

	err := confirmComposerCleared(tmux.NewTmux(), "gt-refinery", 5*time.Minute)
	if err == nil {
		t.Fatal("a pane that could not be captured at all was reported as cleared")
	}
	if errors.Is(err, errComposerUnverifiable) {
		t.Errorf("a capture failure was reported as an unverifiable composer: %v", err)
	}
}
