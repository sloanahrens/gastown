package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// Pane fixtures for the immediate-mode consumption warning (gt-eigw). They are
// trimmed copies of the live captures internal/tmux/composer_stall_test.go
// pins; this package cannot reach those constants, so the two shapes that
// matter are restated here.
const (
	// cmdWedgedPane is the gt-eigw signature: an idle prompt with the nudge
	// stranded in Claude Code's input queue.
	cmdWedgedPane = "⏺ Waiting on the review.\n" +
		"\n" +
		"────────────────────────────────────────\n" +
		"❯ \n" +
		"  Press up to edit queued messages · ctrl+x ctrl+s to send now\n" +
		"────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage"

	// cmdCleanIdlePane is a healthy idle agent: empty composer, no queue, no
	// busy marker. Accepting a nudge here is normal and must not warn.
	cmdCleanIdlePane = "⏺ Queue empty. Awaiting work.\n" +
		"\n" +
		"────────────────────────────────────────\n" +
		"❯\n" +
		"────────────────────────────────────────\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage"

	// cmdBusyPane is an agent mid-turn. Nudges to a busy target are refused by
	// immediate mode, but if one lands it has been consumed by definition.
	cmdBusyPane = "⏺ Running 1 shell command · 34s…\n" +
		"     (ctrl+b ctrl+b (twice) to run in background)\n" +
		"\n" +
		"✻ Scampering… (1m 17s · ↓ 2.4k tokens)\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt"
)

// fakeTmuxPane installs a tmux shim on PATH that answers capture-pane with the
// given pane, so the probe can be exercised without a tmux server. It returns
// the path of the shim's invocation log.
func fakeTmuxPane(t *testing.T, pane string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "tmux.log")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`printf '%s\n' "$*" >> "` + logPath + `"` + "\n")
	b.WriteString(`for a in "$@"; do case "$a" in` + "\n")
	b.WriteString("\tcapture-pane|display-message|has-session|send-keys|show-environment) sub=$a; break;;\n")
	b.WriteString("\tesac; done\n")
	b.WriteString(`if [ "$sub" = "capture-pane" ]; then printf '%s' '` + pane + `'; exit 0; fi` + "\n")
	b.WriteString("exit 0\n")

	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// fakeTmuxPaneLostAfterFirstRead answers the probe's baseline capture-pane call
// with the given pane and fails every later call the way a session that died
// does, so a test can blind the probe mid-window (gt-7xnv).
func fakeTmuxPaneLostAfterFirstRead(t *testing.T, pane string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "tmux.log")
	countPath := filepath.Join(binDir, "capture.count")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`printf '%s\n' "$*" >> "` + logPath + `"` + "\n")
	b.WriteString(`for a in "$@"; do case "$a" in` + "\n")
	b.WriteString("\tcapture-pane|display-message|has-session|send-keys|show-environment) sub=$a; break;;\n")
	b.WriteString("\tesac; done\n")
	b.WriteString(`if [ "$sub" = "capture-pane" ]; then` + "\n")
	b.WriteString(`  n=$(cat "` + countPath + `" 2>/dev/null || echo 0)` + "\n")
	b.WriteString(`  echo $((n+1)) > "` + countPath + `"` + "\n")
	b.WriteString(`  if [ "$n" = "0" ]; then printf '%s' '` + pane + `'; exit 0; fi` + "\n")
	b.WriteString("  printf '%s\\n' 'session not found' >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")
	b.WriteString("exit 0\n")

	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// withShortImmediateProbe shrinks the immediate-mode probe window so the test
// does not wait the production default. Package state, so callers must not run
// in parallel.
func withShortImmediateProbe(t *testing.T) {
	t.Helper()
	original := immediateTurnProbeWindow
	immediateTurnProbeWindow = 40 * time.Millisecond
	t.Cleanup(func() { immediateTurnProbeWindow = original })
}

// The gt-eigw case: the target accepted the nudge and started no turn. The
// warning must name the target and point at the recovery, because the whole
// bug was that every delivery reported success while nothing happened.
func TestImmediateConsumptionWarningOnWedgedTarget(t *testing.T) {
	withShortImmediateProbe(t)
	fakeTmuxPane(t, cmdWedgedPane)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption")

	warning := immediateConsumptionWarning(tm, "gt-beads-refinery")
	if warning == "" {
		t.Fatal("wedged target produced no warning — immediate mode still reports success on a session that started no turn")
	}
	for _, want := range []string{"gt-beads-refinery", "started no turn", "gt-eigw"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning %q does not mention %q", warning, want)
		}
	}
}

// A healthy idle target is the case attempt 1 of gt-eigw got wrong by rejecting
// it. It must stay silent.
func TestImmediateConsumptionWarningSilentOnIdleTarget(t *testing.T) {
	withShortImmediateProbe(t)
	fakeTmuxPane(t, cmdCleanIdlePane)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption")

	if warning := immediateConsumptionWarning(tm, "gt-beads-refinery"); warning != "" {
		t.Errorf("idle target warned on: %q", warning)
	}
}

// A busy target consumed the nudge by definition. No warning.
func TestImmediateConsumptionWarningSilentOnBusyTarget(t *testing.T) {
	withShortImmediateProbe(t)
	fakeTmuxPane(t, cmdBusyPane)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption")

	if warning := immediateConsumptionWarning(tm, "gt-beads-refinery"); warning != "" {
		t.Errorf("busy target warned on: %q", warning)
	}
}

// The probe must stay bounded: an immediate nudge that blocks would be worse
// than the silence it replaces.
func TestImmediateConsumptionWarningIsBounded(t *testing.T) {
	withShortImmediateProbe(t)
	fakeTmuxPane(t, cmdWedgedPane)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption")

	start := time.Now()
	_ = immediateConsumptionWarning(tm, "gt-beads-refinery")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("probe took %s, want it bounded by the probe window", elapsed)
	}
}

// The probe reads the pane it is judging, not the session's exit status. When
// the pane cannot be read, whether the nudge was consumed is unknown — and
// unknown must not be rendered as silence, because silence is exactly what a
// consumed nudge looks like (gt-7xnv). The line says so, and does not claim the
// target is wedged in either direction.
func TestImmediateConsumptionWarningReportsUnknownWhenPaneUnreadable(t *testing.T) {
	withShortImmediateProbe(t)
	// No shim and no tmux server: every tmux call fails.
	t.Setenv("PATH", t.TempDir())
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption-missing")

	warning := immediateConsumptionWarning(tm, "gt-beads-refinery")
	if warning == "" {
		t.Fatal("an unreadable pane produced no line — a probe that could not look is indistinguishable from a consumed nudge")
	}
	for _, want := range []string{"gt-beads-refinery", "UNKNOWN", "gt session health"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning %q does not mention %q", warning, want)
		}
	}
	// Not knowing is not a stall claim: the wedge wording must not appear, or
	// the fix trades a fail-open for a false positive.
	if strings.Contains(warning, "started no turn") {
		t.Errorf("unreadable pane was reported as a strand: %q", warning)
	}
}

// Losing the pane halfway through the window is the same unknown as never
// reading it: the probe saw the baseline and nothing after, so it has no
// evidence about the rest of the window and must not fall back to the shape it
// started with (gt-7xnv).
func TestImmediateConsumptionWarningReportsUnknownWhenPaneLostMidProbe(t *testing.T) {
	withShortImmediateProbe(t)
	fakeTmuxPaneLostAfterFirstRead(t, cmdWedgedPane)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption")

	warning := immediateConsumptionWarning(tm, "gt-beads-refinery")
	if warning == "" {
		t.Fatal("a pane lost mid-probe produced no line")
	}
	if !strings.Contains(warning, "UNKNOWN") {
		t.Errorf("warning %q does not report the unknown", warning)
	}
	if strings.Contains(warning, "started no turn") {
		t.Errorf("a pane that could not be observed for the whole window was reported as a strand: %q", warning)
	}
}
