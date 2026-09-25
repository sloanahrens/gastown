package cmd

import (
	"fmt"
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
// given pane, so the probe can be exercised without a tmux server. With
// failAfter > 0 the shim exits nonzero on the capture-pane invocation number
// that exceeds failAfter (a pane that is readable at first and then lost, or
// unreadable from the start with failAfter=0). It returns the path of the
// shim's invocation log.
func fakeTmuxPane(t *testing.T, pane string, failAfter int) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "tmux.log")
	countPath := filepath.Join(binDir, "tmux.capture-count")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`printf '%s\n' "$*" >> "` + logPath + `"` + "\n")
	b.WriteString(`for a in "$@"; do case "$a" in` + "\n")
	b.WriteString("\tcapture-pane|display-message|has-session|send-keys|show-environment) sub=$a; break;;\n")
	b.WriteString("\tesac; done\n")
	if failAfter >= 0 {
		b.WriteString("if [ \"$sub\" = \"capture-pane\" ]; then\n")
		b.WriteString("  n=$(cat '" + countPath + "' 2>/dev/null || echo 0); n=$((n+1)); printf '%s' \"$n\" > '" + countPath + "'\n")
		b.WriteString("  if [ \"$n\" -gt " + fmt.Sprint(failAfter) + ` ]; then echo "tmux: no such pane" >&2; exit 1; fi\n`)
		b.WriteString(`fi\n`)
	}
	b.WriteString(`if [ "$sub" = "capture-pane" ]; then printf '%s' '` + pane + `'; exit 0; fi` + "\n")
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
	fakeTmuxPane(t, cmdWedgedPane, -1)
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
	fakeTmuxPane(t, cmdCleanIdlePane, -1)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption")

	if warning := immediateConsumptionWarning(tm, "gt-beads-refinery"); warning != "" {
		t.Errorf("idle target warned on: %q", warning)
	}
}

// A busy target consumed the nudge by definition. No warning.
func TestImmediateConsumptionWarningSilentOnBusyTarget(t *testing.T) {
	withShortImmediateProbe(t)
	fakeTmuxPane(t, cmdBusyPane, -1)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption")

	if warning := immediateConsumptionWarning(tm, "gt-beads-refinery"); warning != "" {
		t.Errorf("busy target warned on: %q", warning)
	}
}

// The probe must stay bounded: an immediate nudge that blocks would be worse
// than the silence it replaces.
func TestImmediateConsumptionWarningIsBounded(t *testing.T) {
	withShortImmediateProbe(t)
	fakeTmuxPane(t, cmdWedgedPane, -1)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption")

	start := time.Now()
	_ = immediateConsumptionWarning(tm, "gt-beads-refinery")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("probe took %s, want it bounded by the probe window", elapsed)
	}
}

// An unreadable pane is an undelivered verdict, not a delivered one: the
// warning must say consumption is UNKNOWN, not read as a wedge and not as
// silence (gt-7xnv).
func TestImmediateConsumptionWarningReportsUnknownWhenPaneUnreadable(t *testing.T) {
	withShortImmediateProbe(t)
	// Shim exists but capture-pane fails on every invocation.
	fakeTmuxPane(t, cmdWedgedPane, 0)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption-missing")

	warning := immediateConsumptionWarning(tm, "gt-beads-refinery")
	if warning == "" {
		t.Fatal("unreadable pane produced no output — the probe is fail-open (gt-7xnv)")
	}
	for _, want := range []string{"UNKNOWN", "gt session health gt-beads-refinery"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning %q does not mention %q", warning, want)
		}
	}
	if strings.Contains(warning, "started no turn") {
		t.Errorf("unreadable pane must not claim a wedge: %q", warning)
	}
}

// The gt-8hi4w case: wait-idle's direct-delivery path shares the same probe as
// immediate mode, and the warning must say "wait-idle", not "immediate", so an
// operator reading it knows which delivery path produced a false idle-read.
func TestConsumptionWarningLabelsWaitIdleMode(t *testing.T) {
	withShortImmediateProbe(t)
	fakeTmuxPane(t, cmdWedgedPane, -1)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption")

	warning := consumptionWarning(tm, "gt-mayor", NudgeModeWaitIdle)
	if warning == "" {
		t.Fatal("wedged target produced no warning for wait-idle mode")
	}
	if !strings.HasPrefix(warning, "wait-idle: ") {
		t.Errorf("warning %q does not start with the wait-idle label", warning)
	}
	if strings.Contains(warning, "immediate:") {
		t.Errorf("wait-idle warning mislabeled as immediate: %q", warning)
	}
}

// A pane that is readable at baseline but gone before the final judgment is the
// same fail-open in a rarer shape (session vanishing mid-probe, gt-7xnv).
func TestImmediateConsumptionWarningReportsUnknownWhenPaneLostMidProbe(t *testing.T) {
	withShortImmediateProbe(t)
	// First capture-pane succeeds, the pane is gone after.
	fakeTmuxPane(t, cmdWedgedPane, 1)
	tm := tmux.NewTmuxWithSocket("gt-test-nudge-consumption-lost")

	warning := immediateConsumptionWarning(tm, "gt-beads-refinery")
	if !strings.Contains(warning, "UNKNOWN") {
		t.Fatalf("pane lost mid-probe must report UNKNOWN, got %q", warning)
	}
	if strings.Contains(warning, "started no turn") {
		t.Errorf("pane lost mid-probe must not claim a wedge: %q", warning)
	}
}
