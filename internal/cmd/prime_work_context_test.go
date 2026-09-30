package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/telemetry"
)

// injectWorkContextTest is a helper that calls injectWorkContext with OTel
// activated and TMUX unset (to skip tmux subprocess calls).
func setupWorkContextTest(t *testing.T) {
	t.Helper()
	t.Setenv(telemetry.EnvLogsURL, "http://localhost:9428/insert/opentelemetry/v1/logs")
	t.Setenv("TMUX", "") // prevent tmux set-environment subprocess calls
	t.Setenv("GT_WORK_RIG", "")
	t.Setenv("GT_WORK_BEAD", "")
	t.Setenv("GT_WORK_MOL", "")
	primeDryRun = false
}

func TestInjectWorkContext_NoBeadClearsVars(t *testing.T) {
	setupWorkContextTest(t)
	// Pre-populate with stale values from a previous cycle.
	t.Setenv("GT_WORK_RIG", "oldrig")
	t.Setenv("GT_WORK_BEAD", "old-bead")
	t.Setenv("GT_WORK_MOL", "old-mol")

	ctx := RoleContext{Rig: "gastown"}
	injectWorkContext(ctx, nil)

	if got := os.Getenv("GT_WORK_RIG"); got != "" {
		t.Errorf("GT_WORK_RIG = %q, want empty (no bead on hook)", got)
	}
	if got := os.Getenv("GT_WORK_BEAD"); got != "" {
		t.Errorf("GT_WORK_BEAD = %q, want empty (no bead on hook)", got)
	}
	if got := os.Getenv("GT_WORK_MOL"); got != "" {
		t.Errorf("GT_WORK_MOL = %q, want empty (no bead on hook)", got)
	}
}

func TestInjectWorkContext_BeadOnly(t *testing.T) {
	setupWorkContextTest(t)

	ctx := RoleContext{Rig: "gastown"}
	bead := &beads.Issue{ID: "sg-05iq", Description: ""}
	injectWorkContext(ctx, bead)

	if got := os.Getenv("GT_WORK_RIG"); got != "gastown" {
		t.Errorf("GT_WORK_RIG = %q, want %q", got, "gastown")
	}
	if got := os.Getenv("GT_WORK_BEAD"); got != "sg-05iq" {
		t.Errorf("GT_WORK_BEAD = %q, want %q", got, "sg-05iq")
	}
	if got := os.Getenv("GT_WORK_MOL"); got != "" {
		t.Errorf("GT_WORK_MOL = %q, want empty (no molecule attachment)", got)
	}
}

func TestInjectWorkContext_BeadWithMolecule(t *testing.T) {
	setupWorkContextTest(t)

	ctx := RoleContext{Rig: "gastown"}
	// Build a bead description that encodes an AttachedMolecule.
	desc := beads.FormatAttachmentFields(&beads.AttachmentFields{
		AttachedMolecule: "mol-abc123",
	})
	bead := &beads.Issue{ID: "sg-05iq", Description: desc}
	injectWorkContext(ctx, bead)

	if got := os.Getenv("GT_WORK_BEAD"); got != "sg-05iq" {
		t.Errorf("GT_WORK_BEAD = %q, want %q", got, "sg-05iq")
	}
	if got := os.Getenv("GT_WORK_MOL"); got != "mol-abc123" {
		t.Errorf("GT_WORK_MOL = %q, want %q", got, "mol-abc123")
	}
}

func TestInjectWorkContext_GenericPolecat_EmptyRig(t *testing.T) {
	setupWorkContextTest(t)

	// Generic polecats have no fixed rig (ctx.Rig is empty).
	ctx := RoleContext{Rig: ""}
	bead := &beads.Issue{ID: "sg-xyzw"}
	injectWorkContext(ctx, bead)

	if got := os.Getenv("GT_WORK_RIG"); got != "" {
		t.Errorf("GT_WORK_RIG = %q, want empty for generic polecat", got)
	}
	if got := os.Getenv("GT_WORK_BEAD"); got != "sg-xyzw" {
		t.Errorf("GT_WORK_BEAD = %q, want %q", got, "sg-xyzw")
	}
}

func TestInjectWorkContext_NoopWhenOTelDisabled(t *testing.T) {
	t.Setenv(telemetry.EnvMetricsURL, "")
	t.Setenv(telemetry.EnvLogsURL, "")
	t.Setenv("GT_WORK_RIG", "")
	t.Setenv("GT_WORK_BEAD", "")
	t.Setenv("TMUX", "")
	primeDryRun = false

	ctx := RoleContext{Rig: "gastown"}
	bead := &beads.Issue{ID: "sg-05iq"}
	injectWorkContext(ctx, bead)

	// Env vars should NOT be set since OTel is disabled.
	if got := os.Getenv("GT_WORK_BEAD"); got != "" {
		t.Errorf("GT_WORK_BEAD = %q, want empty when OTel disabled", got)
	}
}

func TestInjectWorkContext_NoopInDryRun(t *testing.T) {
	setupWorkContextTest(t)
	primeDryRun = true
	t.Cleanup(func() { primeDryRun = false })

	ctx := RoleContext{Rig: "gastown"}
	bead := &beads.Issue{ID: "sg-05iq"}
	injectWorkContext(ctx, bead)

	if got := os.Getenv("GT_WORK_BEAD"); got != "" {
		t.Errorf("GT_WORK_BEAD = %q, want empty in dry-run mode", got)
	}
}

// TestRenderDependencyMergeStatusWarnsOnUnmergedBlocker is acceptance criterion
// 5 at the point the worker actually reads it: a starting polecat is told, in
// words, that its prerequisite is submitted but not landed.
func TestRenderDependencyMergeStatusWarnsOnUnmergedBlocker(t *testing.T) {
	out := captureOutput(func() {
		renderDependencyMergeStatus([]beads.DependencyMergeStatus{
			{ID: "gt-blocker", Status: "closed", State: beads.DependencyMergeUnmerged, MR: "gt-wisp-mr1", Branch: "polecat/onyx/gt-blocker"},
			{ID: "gt-done", Status: "closed", State: beads.DependencyMergeLanded},
		})
	})

	// The blocker and its MR must both be named, so the worker can go look.
	if !strings.Contains(out, "gt-blocker") || !strings.Contains(out, "gt-wisp-mr1") {
		t.Fatalf("output must name the unmerged blocker and its MR, got:\n%s", out)
	}
	if !strings.Contains(out, "NOT merged") {
		t.Fatalf("output must say the dependency is not merged, got:\n%s", out)
	}
	// It must not claim a merely-queued dependency has landed.
	if strings.Contains(out, "gt-blocker — landed") {
		t.Fatalf("unmerged blocker must not be reported as landed, got:\n%s", out)
	}
	// And it must not warn about a dependency that has landed.
	if strings.Contains(out, "gt-done is closed but its work is NOT") {
		t.Fatalf("landed dependency must not raise the unmerged warning, got:\n%s", out)
	}
}

// TestBeadWithFullDependenciesSkipsShowWhenNoDependencies verifies the fast
// path: a bead with no dependencies is returned unchanged, without shelling
// out to `bd show`. Proven by using an ID `bd show` cannot resolve — if the
// fast path did not short-circuit, the re-fetch would fail and the function
// would still (correctly) fall back to the original bead, so this test would
// pass either way; what it actually pins is same-pointer identity, which only
// the short-circuit produces.
func TestBeadWithFullDependenciesSkipsShowWhenNoDependencies(t *testing.T) {
	t.Parallel()
	bead := &beads.Issue{ID: "gt-does-not-exist-anywhere", DependencyCount: 0}
	if got := beadWithFullDependencies(RoleContext{}, bead); got != bead {
		t.Fatalf("beadWithFullDependencies() = %#v, want the same bead pointer unchanged", got)
	}
}

// installFakeBdShow puts a POSIX-shell `bd` on PATH whose `show` prints
// showJSON and exits with showExit, and returns the file that logs each
// invocation's argv (one per line). Not parallel-safe: it sets PATH.
func installFakeBdShow(t *testing.T, showJSON string, showExit int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fake bd")
	}
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	payload := filepath.Join(binDir, "show.json")
	if err := os.WriteFile(payload, []byte(showJSON), 0o644); err != nil {
		t.Fatalf("write fake show payload: %v", err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + logPath + "'\n" +
		"case \"$1\" in\n" +
		"  show) cat '" + payload + "'; exit " + strconv.Itoa(showExit) + ";;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestBeadWithFullDependenciesRefetchesViaShow is the positive half of the
// fast-path test above: a bead that `bd list` reported with dependencies
// (DependencyCount > 0 but bare relation records, so no usable IDs) is
// replaced by the full record `bd show` returns, whose dependencies carry the
// id and status the merge-status check needs.
func TestBeadWithFullDependenciesRefetchesViaShow(t *testing.T) {
	logPath := installFakeBdShow(t,
		`[{"id":"gt-hooked","title":"Hooked","status":"hooked","dependency_count":1,`+
			`"dependencies":[{"id":"gt-blocker","title":"Blocker","status":"closed","dependency_type":"blocks"}]}]`, 0)

	listed := &beads.Issue{ID: "gt-hooked", DependencyCount: 1}
	ctx := RoleContext{WorkDir: t.TempDir()}

	got := beadWithFullDependencies(ctx, listed)
	if got == listed {
		t.Fatal("beadWithFullDependencies() returned the listed bead; want the re-fetched one")
	}
	if len(got.Dependencies) != 1 || got.Dependencies[0].ID != "gt-blocker" || got.Dependencies[0].Status != "closed" {
		t.Fatalf("re-fetched dependencies = %#v, want gt-blocker (closed)", got.Dependencies)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	if !strings.Contains(string(data), "show gt-hooked") {
		t.Fatalf("bd was not asked to show gt-hooked; log:\n%s", data)
	}
}

// TestBeadWithFullDependenciesFallsBackWhenShowFails: a re-fetch that errors
// must not lose the hooked bead.
func TestBeadWithFullDependenciesFallsBackWhenShowFails(t *testing.T) {
	installFakeBdShow(t, "", 1)

	listed := &beads.Issue{ID: "gt-hooked", DependencyCount: 1}
	ctx := RoleContext{WorkDir: t.TempDir()}

	if got := beadWithFullDependencies(ctx, listed); got != listed {
		t.Fatalf("beadWithFullDependencies() = %#v, want the listed bead after a failed show", got)
	}
}

// TestBeadWithFullDependenciesNilBead: findAgentWork can return a nil bead
// (no work hooked); the re-fetch helper must not panic on it.
func TestBeadWithFullDependenciesNilBead(t *testing.T) {
	t.Parallel()
	if got := beadWithFullDependencies(RoleContext{}, nil); got != nil {
		t.Fatalf("beadWithFullDependencies(nil) = %#v, want nil", got)
	}
}

// TestRenderDependencyMergeStatusSilentWithoutDependencies: a bead with no
// dependencies gets no dependency noise in its starting context.
func TestRenderDependencyMergeStatusSilentWithoutDependencies(t *testing.T) {
	if out := captureOutput(func() { renderDependencyMergeStatus(nil) }); out != "" {
		t.Fatalf("expected no output, got %q", out)
	}
}

// TestRenderDependencyMergeStatusReportsUnknownHonestly: a blocker we could not
// check must not read as landed.
func TestRenderDependencyMergeStatusReportsUnknownHonestly(t *testing.T) {
	out := captureOutput(func() {
		renderDependencyMergeStatus([]beads.DependencyMergeStatus{
			{ID: "gt-unchecked", Status: "closed", State: beads.DependencyMergeUnknown},
		})
	})
	if !strings.Contains(out, "unknown") {
		t.Fatalf("unchecked blocker must report unknown, got:\n%s", out)
	}
	if strings.Contains(out, "landed") {
		t.Fatalf("unchecked blocker must never read as landed, got:\n%s", out)
	}
}
