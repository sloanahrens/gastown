package witness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// The detection logic itself is unit-tested in internal/tmux (see
// composer_stall_test.go there, including the live-capture fixtures). These
// tests cover the witness-side helpers: the action chosen for a stall and how
// a flush is confirmed. (The refinery stall detector these once wired up was
// removed with the refinery role, gt-v4ssj.6.)

const (
	// A refinery holding a nudge that was typed while it was mid-turn and never
	// submitted: the gt-hkhu signature.
	stalledRefineryPane = "⏺ Queue empty. Awaiting work.\n" +
		"\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"❯ [from gastown/sling] MERGE_READY received - check inbox for pending work\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage"

	// A refinery whose composer let the submitted input go: the flush worked.
	clearedComposerPane = "⏺ Waiting on the review.\n" +
		"────────────────────────────────────────────────────────────────────────────────\n" +
		"❯ \n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)"

	// A pane with no prompt prefix to reason about: readable, but it says
	// nothing either way about the composer. This is what
	// tmux.ErrComposerUnobservable is for.
	unclassifiablePane = "⏺ Build finished.\n  ⏵⏵ bypass permissions on (shift+tab to cycle)"
)

// fakeTmuxShim installs a tmux shim on PATH and returns the path of its call
// log. Responses are keyed by subcommand, or by "subcommand:needle" to vary the
// answer by the format string a display-message call asks for. Three values are
// special: "@now" (current Unix time), "@fail" (exit 1, as tmux does for a
// missing session), and "@missing" (exit 1 with tmux's "unknown variable"
// stderr, which is how a session that never set GT_PROCESS_NAMES answers).
func fakeTmuxShim(t *testing.T, responses map[string]string) string {
	t.Helper()
	return fakeTmuxShimSeq(t, responses, nil)
}

// fakeTmuxShimSeq is fakeTmuxShim with answers that change from call to call:
// the nth call to a key in sequences answers with the sequence's nth entry, and
// its last entry answers every call after that. A key named in both maps takes
// the sequence, and the special values above are per-entry, so ["@fail", pane]
// is a capture that misses once and then succeeds. Use it for a stateful pane
// instead of reimplementing the dispatch below inside a one-off script.
func fakeTmuxShimSeq(t *testing.T, responses map[string]string, sequences map[string][]string) string {
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
	for _, key := range sortedKeys(sequences) {
		seq := sequences[key]
		if len(seq) == 0 {
			continue
		}
		// The count has to survive between invocations, so it lives in a file
		// beside the shim rather than in the script.
		countPath := filepath.Join(binDir, key+".count")
		b.WriteString("\t'" + key + "')\n")
		b.WriteString("\t\tn=$(cat '" + countPath + "' 2>/dev/null || echo 0)\n")
		b.WriteString("\t\tn=$((n + 1))\n")
		b.WriteString("\t\tprintf '%s' \"$n\" > '" + countPath + "'\n")
		b.WriteString("\t\tcase \"$n\" in\n")
		for i, out := range seq {
			writeShimArm(&b, fmt.Sprintf("%d)", i+1), out)
		}
		writeShimArm(&b, "*)", seq[len(seq)-1])
		b.WriteString("\t\tesac\n")
		b.WriteString("\t\t;;\n")
	}
	for _, key := range sortedKeys(responses) {
		if _, ok := sequences[key]; ok {
			continue
		}
		out := responses[key]
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

// sortedKeys returns a response map's keys in a stable order, so the generated
// shim does not churn between runs.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// writeShimArm emits one case arm of the shim's dispatcher: the shell that
// answers with out, where the special values of fakeTmuxShim become the tmux
// behaviour they stand for.
func writeShimArm(b *strings.Builder, label, out string) {
	b.WriteString("\t\t\t" + label + " ")
	switch out {
	case "@now":
		b.WriteString("date +%s; exit 0;;\n")
	case "@fail":
		b.WriteString("exit 1;;\n")
	case "@missing":
		b.WriteString("printf '%s\\n' 'unknown variable: not set' 1>&2; exit 1;;\n")
	default:
		b.WriteString("printf '%s' '" + out + "'; exit 0;;\n")
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
	h := newTestHandlers()
	h.sleepFn = func(time.Duration) {}

	tests := []struct {
		name         string
		pane         string
		wantStranded bool
		wantUnverifi bool
	}{
		{
			// The flush worked: the composer is empty again.
			name: "cleared composer is a pass",
			pane: clearedComposerPane,
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
			pane:         unclassifiablePane,
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

			err := h.confirmComposerCleared(tmux.NewTmux(), "gt-refinery", 5*time.Minute)

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
	h := newTestHandlers()
	h.sleepFn = func(time.Duration) {}

	fakeTmuxShim(t, map[string]string{
		"has-session":                             "",
		"capture-pane":                            "@fail",
		"display-message:#{window_activity}":      "1700000000",
		"display-message:#{pane_current_command}": "claude",
	})

	err := h.confirmComposerCleared(tmux.NewTmux(), "gt-refinery", 5*time.Minute)
	if err == nil {
		t.Fatal("a pane that could not be captured at all was reported as cleared")
	}
	if errors.Is(err, errComposerUnverifiable) {
		t.Errorf("a capture failure was reported as an unverifiable composer: %v", err)
	}
}

// The verdict classes the caller branches on: it maps a strand and a capture
// failure alike onto a still-pending/restart, and reserves unverifiable for
// "submitted, could not confirm" beside the submit.
const (
	verdictCleared      = "cleared"
	verdictStranded     = "stranded"
	verdictUnreadable   = "unreadable"
	verdictUnverifiable = "unverifiable"
)

// verdictOf classifies what confirmComposerCleared reported, by the class the
// caller acts on rather than by the message.
func verdictOf(err error) string {
	switch {
	case err == nil:
		return verdictCleared
	case errors.Is(err, errComposerUnverifiable):
		return verdictUnverifiable
	case strings.Contains(err.Error(), "still holds input"):
		return verdictStranded
	default:
		return verdictUnreadable
	}
}

// One flaky capture must not end the recheck: the whole point of the retry
// loop is to ride out a transient miss three quarters of a second after
// ctrl+x ctrl+s. A single failed capture followed by a clean read of a
// recovered composer must report the recovery, not a still-pending/restart
// verdict off the first miss (gt-0b4z).
func TestConfirmComposerClearedRetriesPastASingleCaptureFailure(t *testing.T) {
	h := newTestHandlers()
	h.sleepFn = func(time.Duration) {}

	logPath := fakeTmuxShimSeq(t, map[string]string{
		"has-session":                             "",
		"display-message:#{window_activity}":      "1700000000",
		"display-message:#{pane_current_command}": "claude",
	}, map[string][]string{
		"capture-pane": {"@fail", clearedComposerPane},
	})

	err := h.confirmComposerCleared(tmux.NewTmux(), "gt-refinery", 5*time.Minute)
	if err != nil {
		t.Errorf("err = %v, want nil: a single capture miss followed by a cleared composer must be a pass", err)
	}

	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read tmux log: %v", readErr)
	}
	if n := strings.Count(string(logged), "capture-pane"); n < 2 {
		t.Errorf("capture-pane called %d time(s); the retry loop gave up after the first failure", n)
	}
}

// The loop can run out with no pass and a mix of the three failure shapes in
// it. The verdict must then be the strongest class any attempt observed, not
// the last one's: only a strand and a capture failure are acted on as a
// still-pending/restart, so a trailing unclassifiable read that buried either
// one would report a wedged composer as a benign "submitted, could not
// confirm" (gt-0b4z).
func TestConfirmComposerClearedRanksMixedFailureModes(t *testing.T) {
	h := newTestHandlers()
	h.sleepFn = func(time.Duration) {}

	tests := []struct {
		name     string
		attempts []string
		want     string
	}{
		{
			// The interleaving the priority logic exists for: two genuine
			// capture failures, then an ambiguous read. The capture failure is
			// the story, so the caller must see it as an error and not spend
			// its unverifiable branch on it.
			name:     "a trailing unclassifiable read does not bury an earlier capture failure",
			attempts: []string{"@fail", "@fail", unclassifiablePane},
			want:     verdictUnreadable,
		},
		{
			name:     "a capture failure between unclassifiable reads is still the verdict",
			attempts: []string{unclassifiablePane, "@fail", unclassifiablePane},
			want:     verdictUnreadable,
		},
		{
			// Positive evidence beats a later failure of any kind: the input
			// was seen in the composer, so the flush demonstrably did not take.
			name:     "input seen in the composer outranks a later unclassifiable read",
			attempts: []string{stalledRefineryPane, unclassifiablePane, unclassifiablePane},
			want:     verdictStranded,
		},
		{
			name:     "input seen in the composer outranks a later capture failure",
			attempts: []string{stalledRefineryPane, "@fail", unclassifiablePane},
			want:     verdictStranded,
		},
		{
			name:     "unclassifiable reads and nothing else stay unverifiable",
			attempts: []string{unclassifiablePane, unclassifiablePane, unclassifiablePane},
			want:     verdictUnverifiable,
		},
		{
			name:     "capture failures and nothing else stay a check failure",
			attempts: []string{"@fail", "@fail", "@fail"},
			want:     verdictUnreadable,
		},
		{
			// A pass needs no ranking: whatever came before it, a pane that is
			// observed clear means the submit worked.
			name:     "a clean read at the end clears over any earlier failure",
			attempts: []string{"@fail", unclassifiablePane, clearedComposerPane},
			want:     verdictCleared,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeTmuxShimSeq(t, map[string]string{
				"has-session":                             "",
				"display-message:#{window_activity}":      "1700000000",
				"display-message:#{pane_current_command}": "claude",
			}, map[string][]string{"capture-pane": tt.attempts})

			err := h.confirmComposerCleared(tmux.NewTmux(), "gt-refinery", 5*time.Minute)
			if got := verdictOf(err); got != tt.want {
				t.Errorf("verdict = %s, want %s (err: %v)", got, tt.want, err)
			}
		})
	}
}
