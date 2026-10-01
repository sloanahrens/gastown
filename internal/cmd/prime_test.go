package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/constants"
)

func writeTestRoutes(t *testing.T, townRoot string, routes []beads.Route) {
	t.Helper()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("create beads dir: %v", err)
	}
	if err := beads.WriteRoutes(beadsDir, routes); err != nil {
		t.Fatalf("write routes: %v", err)
	}
}

func TestGetAgentBeadID_UsesRigPrefix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "bd-", Path: "beads/mayor/rig"},
	})

	cases := []struct {
		name string
		ctx  RoleContext
		want string
	}{
		{
			name: "mayor",
			ctx: RoleContext{
				Role:     RoleMayor,
				TownRoot: townRoot,
			},
			want: "hq-mayor",
		},
		{
			name: "polecat",
			ctx: RoleContext{
				Role:     RolePolecat,
				Rig:      "beads",
				Polecat:  "lex",
				TownRoot: townRoot,
			},
			want: "bd-beads-polecat-lex",
		},
		{
			name: "crew",
			ctx: RoleContext{
				Role:     RoleCrew,
				Rig:      "beads",
				Polecat:  "lex",
				TownRoot: townRoot,
			},
			want: "bd-beads-crew-lex",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := getAgentBeadID(tc.ctx)
			if got != tc.want {
				t.Fatalf("getAgentBeadID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRigBeadsRootPrefersRouteResolvedRigDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTestRoutes(t, townRoot, []beads.Route{
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
		{Prefix: "hq-", Path: "."},
	})

	ctx := RoleContext{
		Role:     RolePolecat,
		Rig:      "gastown",
		Polecat:  "toast",
		TownRoot: townRoot,
		WorkDir:  filepath.Join(townRoot, "gastown", "polecats", "toast", "gastown"),
	}

	got := rigBeadsRoot(ctx)
	want := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if got != want {
		t.Fatalf("rigBeadsRoot() = %q, want route-resolved %q", got, want)
	}
}

func TestRigBeadsRootFallsBackWhenRouteMissing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ctx := RoleContext{
		Role:     RolePolecat,
		Rig:      "gastown",
		TownRoot: townRoot,
		WorkDir:  filepath.Join(townRoot, "gastown", "polecats", "toast", "gastown"),
	}

	got := rigBeadsRoot(ctx)
	want := filepath.Join(townRoot, "gastown")
	if got != want {
		t.Fatalf("rigBeadsRoot() = %q, want fallback %q", got, want)
	}
}

// TestCheckHandoffMarkerDryRun tests that dry-run mode doesn't remove the handoff marker.
func TestCheckHandoffMarkerDryRun(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()

	// Create .runtime directory and handoff marker
	runtimeDir := filepath.Join(workDir, constants.DirRuntime)
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("create runtime dir: %v", err)
	}

	markerPath := filepath.Join(runtimeDir, constants.FileHandoffMarker)
	prevSession := "test-session-123"
	if err := os.WriteFile(markerPath, []byte(prevSession), 0644); err != nil {
		t.Fatalf("write handoff marker: %v", err)
	}

	// Capture stdout to verify explain output
	var outputBuf bytes.Buffer
	checkHandoffMarkerDryRun(&outputBuf, true, workDir)
	output := outputBuf.String()

	// Verify marker still exists (not removed in dry-run)
	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		t.Fatalf("handoff marker was removed in dry-run mode")
	}

	// Verify marker content unchanged
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read handoff marker: %v", err)
	}
	if string(data) != prevSession {
		t.Fatalf("marker content changed: got %q, want %q", string(data), prevSession)
	}

	// Verify explain output mentions dry-run
	if !strings.Contains(output, "dry-run") {
		t.Fatalf("expected explain output to mention dry-run, got: %s", output)
	}
}

// TestCheckHandoffMarkerDryRun_NoMarker tests dry-run when no marker exists.
func TestCheckHandoffMarkerDryRun_NoMarker(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()

	// Create .runtime directory but no marker
	runtimeDir := filepath.Join(workDir, constants.DirRuntime)
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("create runtime dir: %v", err)
	}

	// Should not panic when marker doesn't exist
	var outputBuf bytes.Buffer
	checkHandoffMarkerDryRun(&outputBuf, true, workDir)
	output := outputBuf.String()

	// Verify explain output indicates no marker
	if !strings.Contains(output, "no handoff marker") {
		t.Fatalf("expected explain output to indicate no marker, got: %s", output)
	}
}

// TestDetectSessionState tests detectSessionState for all states.
func TestDetectSessionState(t *testing.T) {
	t.Parallel()
	t.Run("normal_state", func(t *testing.T) {
		workDir := t.TempDir()
		ctx := RoleContext{
			Role:    RoleMayor,
			WorkDir: workDir,
		}

		state := detectSessionState(ctx)

		if state.State != "normal" {
			t.Fatalf("expected state 'normal', got %q", state.State)
		}
		if state.Role != RoleMayor {
			t.Fatalf("expected role Mayor, got %q", state.Role)
		}
	})

	t.Run("post_handoff_state", func(t *testing.T) {
		workDir := t.TempDir()

		// Create handoff marker
		runtimeDir := filepath.Join(workDir, constants.DirRuntime)
		if err := os.MkdirAll(runtimeDir, 0755); err != nil {
			t.Fatalf("create runtime dir: %v", err)
		}
		prevSession := "predecessor-session-abc"
		markerPath := filepath.Join(runtimeDir, constants.FileHandoffMarker)
		if err := os.WriteFile(markerPath, []byte(prevSession), 0644); err != nil {
			t.Fatalf("write handoff marker: %v", err)
		}

		ctx := RoleContext{
			Role:    RolePolecat,
			Rig:     "beads",
			Polecat: "jade",
			WorkDir: workDir,
		}

		state := detectSessionState(ctx)

		if state.State != "post-handoff" {
			t.Fatalf("expected state 'post-handoff', got %q", state.State)
		}
		if state.PrevSession != prevSession {
			t.Fatalf("expected prev_session %q, got %q", prevSession, state.PrevSession)
		}
	})

	t.Run("crash_recovery_state", func(t *testing.T) {
		workDir := t.TempDir()

		// Create a checkpoint (simulating a crashed session)
		cp := &checkpoint.Checkpoint{
			SessionID:  "crashed-session",
			HookedBead: "bd-test123",
			StepTitle:  "Working on feature X",
			Timestamp:  time.Now().Add(-1 * time.Hour), // 1 hour old
		}
		if err := checkpoint.Write(workDir, cp); err != nil {
			t.Fatalf("write checkpoint: %v", err)
		}

		ctx := RoleContext{
			Role:    RolePolecat,
			Rig:     "beads",
			Polecat: "jade",
			WorkDir: workDir,
		}

		state := detectSessionState(ctx)

		if state.State != "crash-recovery" {
			t.Fatalf("expected state 'crash-recovery', got %q", state.State)
		}
		if state.CheckpointAge == "" {
			t.Fatalf("expected checkpoint_age to be set")
		}
	})

	t.Run("crash_recovery_only_for_workers", func(t *testing.T) {
		workDir := t.TempDir()

		// Create a checkpoint
		cp := &checkpoint.Checkpoint{
			SessionID:  "crashed-session",
			HookedBead: "bd-test123",
			StepTitle:  "Working on feature X",
			Timestamp:  time.Now().Add(-1 * time.Hour),
		}
		if err := checkpoint.Write(workDir, cp); err != nil {
			t.Fatalf("write checkpoint: %v", err)
		}

		// Mayor should NOT enter crash-recovery (only polecat/crew)
		ctx := RoleContext{
			Role:    RoleMayor,
			WorkDir: workDir,
		}

		state := detectSessionState(ctx)

		// Mayor should see normal state, not crash-recovery
		if state.State != "normal" {
			t.Fatalf("expected Mayor to have 'normal' state despite checkpoint, got %q", state.State)
		}
	})
}

// TestOutputState tests outputState function output formats.
func TestOutputState(t *testing.T) {
	t.Parallel()
	t.Run("text_output", func(t *testing.T) {
		workDir := t.TempDir()
		ctx := RoleContext{
			Role:    RoleMayor,
			WorkDir: workDir,
		}

		var outputBuf bytes.Buffer
		outputState(&outputBuf, ctx, false)
		output := outputBuf.String()

		if !strings.Contains(output, "state: normal") {
			t.Fatalf("expected 'state: normal' in output, got: %s", output)
		}
		if !strings.Contains(output, "role: mayor") {
			t.Fatalf("expected 'role: mayor' in output, got: %s", output)
		}
	})

	t.Run("json_output", func(t *testing.T) {
		workDir := t.TempDir()
		ctx := RoleContext{
			Role:    RolePolecat,
			Rig:     "beads",
			Polecat: "jade",
			WorkDir: workDir,
		}

		var outputBuf bytes.Buffer
		outputState(&outputBuf, ctx, true)
		output := outputBuf.String()

		// Parse JSON output
		var state SessionState
		if err := json.Unmarshal([]byte(output), &state); err != nil {
			t.Fatalf("failed to parse JSON output: %v, output was: %s", err, output)
		}

		if state.State != "normal" {
			t.Fatalf("expected state 'normal', got %q", state.State)
		}
		if state.Role != RolePolecat {
			t.Fatalf("expected role 'polecat', got %q", state.Role)
		}
	})

	t.Run("json_output_post_handoff", func(t *testing.T) {
		workDir := t.TempDir()

		// Create handoff marker
		runtimeDir := filepath.Join(workDir, constants.DirRuntime)
		if err := os.MkdirAll(runtimeDir, 0755); err != nil {
			t.Fatalf("create runtime dir: %v", err)
		}
		prevSession := "prev-session-xyz"
		markerPath := filepath.Join(runtimeDir, constants.FileHandoffMarker)
		if err := os.WriteFile(markerPath, []byte(prevSession), 0644); err != nil {
			t.Fatalf("write marker: %v", err)
		}

		ctx := RoleContext{
			Role:    RolePolecat,
			Rig:     "beads",
			Polecat: "jade",
			WorkDir: workDir,
		}

		var outputBuf bytes.Buffer
		outputState(&outputBuf, ctx, true)
		output := outputBuf.String()

		// Parse JSON
		var state SessionState
		if err := json.Unmarshal([]byte(output), &state); err != nil {
			t.Fatalf("failed to parse JSON: %v", err)
		}

		if state.State != "post-handoff" {
			t.Fatalf("expected state 'post-handoff', got %q", state.State)
		}
		if state.PrevSession != prevSession {
			t.Fatalf("expected prev_session %q, got %q", prevSession, state.PrevSession)
		}
	})
}

// TestExplain tests the explain function output.
func TestExplain(t *testing.T) {
	t.Parallel()
	t.Run("explain_enabled_condition_true", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		explainTo(&buf, true, true, "This is a test explanation")
		output := buf.String()
		if !strings.Contains(output, "[EXPLAIN]") {
			t.Fatalf("expected [EXPLAIN] tag in output, got: %s", output)
		}
		if !strings.Contains(output, "This is a test explanation") {
			t.Fatalf("expected explanation text in output, got: %s", output)
		}
	})

	t.Run("explain_enabled_condition_false", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		explainTo(&buf, true, false, "This should not appear")
		if strings.Contains(buf.String(), "[EXPLAIN]") {
			t.Fatalf("expected no [EXPLAIN] tag when condition is false, got: %s", buf.String())
		}
	})

	t.Run("explain_disabled", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		explainTo(&buf, false, true, "This should not appear either")
		if strings.Contains(buf.String(), "[EXPLAIN]") {
			t.Fatalf("expected no [EXPLAIN] tag when explain mode disabled, got: %s", buf.String())
		}
	})
}

// TestIsCompactResume tests the isCompactResume detection logic including
// compaction-triggered handoff cycles (GH#1965).
func TestIsCompactResume(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		hookSource    string
		handoffReason string
		wantCompact   bool
	}{
		{
			name:        "fresh_startup",
			hookSource:  "startup",
			wantCompact: false,
		},
		{
			name:        "compact_source",
			hookSource:  "compact",
			wantCompact: true,
		},
		{
			name:        "resume_source",
			hookSource:  "resume",
			wantCompact: true,
		},
		{
			name:        "clear_source",
			hookSource:  "clear",
			wantCompact: false,
		},
		{
			name:          "compaction_handoff_cycle",
			hookSource:    "startup",
			handoffReason: "compaction",
			wantCompact:   true,
		},
		{
			name:          "normal_handoff_not_compact",
			hookSource:    "startup",
			handoffReason: "",
			wantCompact:   false,
		},
		{
			name:          "idle_handoff_not_compact",
			hookSource:    "startup",
			handoffReason: "idle",
			wantCompact:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isCompactResumeFor(tc.hookSource, tc.handoffReason)
			if got != tc.wantCompact {
				t.Fatalf("isCompactResume() = %v, want %v (source=%q, reason=%q)",
					got, tc.wantCompact, tc.hookSource, tc.handoffReason)
			}
		})
	}
}

func TestHookSessionBeaconLines(t *testing.T) {
	t.Parallel()
	lines := hookSessionBeaconLinesFor(false, "", "abc", "startup")
	if len(lines) != 2 || lines[0] != "[session:abc]" || lines[1] != "[source:startup]" {
		t.Fatalf("hookSessionBeaconLines() = %v", lines)
	}

	lines = hookSessionBeaconLinesFor(true, "SessionStart", "abc", "startup")
	if len(lines) != 0 {
		t.Fatalf("hookSessionBeaconLines() in structured mode = %v, want no beacon lines", lines)
	}
}

func TestFormatSessionMetadataLine(t *testing.T) {
	t.Parallel()
	if got := formatSessionMetadataLineFor(false, "crew/quick", "sess-1"); !strings.HasPrefix(got, "[GAS TOWN] ") {
		t.Fatalf("formatSessionMetadataLine() = %q, want bracketed prefix", got)
	}
	if got := formatSessionMetadataLineFor(true, "crew/quick", "sess-1"); strings.HasPrefix(got, "[") {
		t.Fatalf("formatSessionMetadataLine() structured = %q, should not start with '['", got)
	}
}

func TestStructuredOutputOnlyForSessionStart(t *testing.T) {
	t.Parallel()
	// A non-SessionStart hook event (e.g., Stop) does not get structured output.
	r := primeHookEnv{
		getenv: envMap(nil),
		stdin:  func() *hookInput { return &hookInput{SessionID: "abc", HookEventName: "Stop"} },
		newID:  func() string { return "fresh" },
	}.readHookSession()
	if r.structuredSessionStart {
		t.Fatal("structured SessionStart output should be false for HookEventName=Stop")
	}

	// Verify beacon lines are emitted (not suppressed) for non-SessionStart
	lines := hookSessionBeaconLinesFor(r.structuredSessionStart, r.eventName, "abc", "startup")
	if len(lines) != 2 {
		t.Fatalf("hookSessionBeaconLines() for non-SessionStart = %v, want 2 beacon lines", lines)
	}

	// Verify metadata line retains brackets for non-SessionStart
	if got := formatSessionMetadataLineFor(r.structuredSessionStart, "crew/quick", "sess-1"); !strings.HasPrefix(got, "[GAS TOWN]") {
		t.Fatalf("formatSessionMetadataLine() for non-SessionStart = %q, want bracketed prefix", got)
	}
}

// TestCheckHandoffMarkerParsesReason tests that checkHandoffMarker correctly
// parses the reason field from the marker file (GH#1965).
func TestCheckHandoffMarkerParsesReason(t *testing.T) {
	t.Parallel()

	t.Run("marker_with_reason", func(t *testing.T) {
		t.Parallel()
		workDir := t.TempDir()

		runtimeDir := filepath.Join(workDir, constants.DirRuntime)
		if err := os.MkdirAll(runtimeDir, 0755); err != nil {
			t.Fatalf("create runtime dir: %v", err)
		}

		// Write marker with session ID and reason
		markerPath := filepath.Join(runtimeDir, constants.FileHandoffMarker)
		if err := os.WriteFile(markerPath, []byte("test-session-456\ncompaction"), 0644); err != nil {
			t.Fatalf("write marker: %v", err)
		}

		// Verify reason was parsed
		if reason := checkHandoffMarkerTo(io.Discard, workDir); reason != "compaction" {
			t.Fatalf("handoff reason = %q, want %q", reason, "compaction")
		}

		// Verify marker was removed
		if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
			t.Fatalf("handoff marker was not removed")
		}
	})

	t.Run("marker_without_reason", func(t *testing.T) {
		t.Parallel()
		workDir := t.TempDir()

		runtimeDir := filepath.Join(workDir, constants.DirRuntime)
		if err := os.MkdirAll(runtimeDir, 0755); err != nil {
			t.Fatalf("create runtime dir: %v", err)
		}

		// Write marker with session ID only (backward compat)
		markerPath := filepath.Join(runtimeDir, constants.FileHandoffMarker)
		if err := os.WriteFile(markerPath, []byte("test-session-789"), 0644); err != nil {
			t.Fatalf("write marker: %v", err)
		}

		// Verify reason is empty (backward compatible)
		if reason := checkHandoffMarkerTo(io.Discard, workDir); reason != "" {
			t.Fatalf("handoff reason = %q, want empty", reason)
		}
	})

	t.Run("no_marker", func(t *testing.T) {
		t.Parallel()
		workDir := t.TempDir()

		// Verify reason is still empty
		if reason := checkHandoffMarkerTo(io.Discard, workDir); reason != "" {
			t.Fatalf("handoff reason = %q, want empty", reason)
		}
	})
}

// TestOutputContinuationDirective tests that the continuation directive
// outputs the expected content without the full autonomous mode block (GH#1965).
func TestOutputContinuationDirective(t *testing.T) {
	t.Parallel()
	t.Run("basic_bead", func(t *testing.T) {
		bead := &beads.Issue{
			ID:    "gt-test123",
			Title: "Test bead title",
		}
		var outputBuf bytes.Buffer
		outputContinuationDirective(&outputBuf, bead, false)
		output := outputBuf.String()

		// Should contain continuation directive
		if !strings.Contains(output, "CONTINUE HOOKED WORK") {
			t.Fatalf("expected 'CONTINUE HOOKED WORK' in output, got: %s", output)
		}
		if !strings.Contains(output, "gt-test123") {
			t.Fatalf("expected bead ID in output, got: %s", output)
		}

		// Should NOT contain autonomous mode language
		if strings.Contains(output, "AUTONOMOUS WORK MODE") {
			t.Fatalf("continuation directive should NOT contain 'AUTONOMOUS WORK MODE', got: %s", output)
		}
		if strings.Contains(output, "Announce:") {
			t.Fatalf("continuation directive should NOT contain 'Announce:', got: %s", output)
		}
	})

	t.Run("bead_with_molecule", func(t *testing.T) {
		bead := &beads.Issue{
			ID:    "gt-mol456",
			Title: "Molecule bead",
		}
		var outputBuf bytes.Buffer
		outputContinuationDirective(&outputBuf, bead, true)
		output := outputBuf.String()

		if !strings.Contains(output, " mol current`") {
			t.Fatalf("expected molecule hint in output, got: %s", output)
		}
	})
}

func TestCheckSlungWork_StandaloneFormulaUsesWorkflowOutput(t *testing.T) {
	t.Parallel()
	notFound := &fakeCook{kind: "not_found", msg: "formula mol-nonexistent not found"}
	ctx := RoleContext{Role: RoleCrew, formulaRun: notFound.run}
	hookedBead := &beads.Issue{
		ID:    "gt-wisp-xyz",
		Title: "Standalone formula work",
		Description: strings.Join([]string{
			"attached_formula: mol-nonexistent",
			`attached_vars: ["version=1.2.3"]`,
		}, "\n"),
	}

	var outputBuf bytes.Buffer
	found, gotErr := checkSlungWorkIn(&outputBuf, false, ctx, hookedBead)
	output := outputBuf.String()
	if gotErr != nil {
		t.Fatalf("checkSlungWork() error = %v", gotErr)
	}

	if !found {
		t.Fatalf("checkSlungWork() = false, want true")
	}
	if !strings.Contains(output, "ATTACHED FORMULA") {
		t.Fatalf("expected standalone formula hook to use workflow output, got:\n%s", output)
	}
	if strings.Contains(output, "Bead details:") {
		t.Fatalf("expected standalone formula hook to skip plain bead preview, got:\n%s", output)
	}
	if !strings.Contains(output, "--var version=1.2.3") {
		t.Fatalf("expected standalone formula context to be shown, got:\n%s", output)
	}
}

func TestOutputAutonomousDirectiveForkRigAvoidsMergeQueueGuidance(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "myrig"), 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "myrig", "config.json"), []byte(`{"upstream_url":"https://token@example.com/upstream/repo.git"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var outputBuf bytes.Buffer
	outputAutonomousDirective(&outputBuf, RoleContext{Role: RolePolecat, Rig: "myrig", TownRoot: townRoot, Polecat: "scout"}, &beads.Issue{ID: "gt-test", Title: "test"}, false)
	output := outputBuf.String()
	if !strings.Contains(output, "FORK-BACKED RIG") {
		t.Fatalf("expected fork-backed directive, got:\n%s", output)
	}
	for _, forbidden := range []string{"submit to the merge queue", "use the merge queue", "token"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("fork autonomous output contains forbidden %q:\n%s", forbidden, output)
		}
	}
}

func TestOutputMoleculeWorkflowForkRigOverridesFormulaMergeQueueReminder(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "myrig"), 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "myrig", "config.json"), []byte(`{"upstream_url":"https://github.com/upstream/repo"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf bytes.Buffer
	if err := outputMoleculeWorkflow(&buf, RoleContext{Role: RolePolecat, Rig: "myrig", TownRoot: townRoot}, &beads.AttachmentFields{AttachedFormula: "mol-polecat-work"}); err != nil {
		t.Fatalf("outputMoleculeWorkflow: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "FORK-BACKED RIG OVERRIDE") {
		t.Fatalf("expected fork override, got:\n%s", output)
	}
	for _, forbidden := range []string{"REQUIRED: When all steps complete", "gt done", "submit to the merge queue"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("fork molecule workflow kept unsafe formula text %q:\n%s", forbidden, output)
		}
	}
}

// TestCompactResumeReminder_PolecatGetsGtDone verifies that polecats get a
// gt done reminder after context compaction. This is the regression test for
// the polecats-no-gt-done bug: after long work sessions, compaction drops the
// formula checklist and the agent forgets to call gt done.
func TestCompactResumeReminder_PolecatGetsGtDone(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{Role: RolePolecat}
	var buf bytes.Buffer
	_ = primeCompactResume(&buf, ctx, "compact", "")
	output := buf.String()

	if !strings.Contains(output, "gt done") {
		t.Fatalf("compact/resume for polecat must remind about gt done, got:\n%s", output)
	}
}

// TestCompactResumeReminder_NonPolecatNoGtDone verifies that non-polecat roles
// do NOT get the gt done reminder (it's polecat-specific).
func TestCompactResumeReminder_NonPolecatNoGtDone(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{Role: RoleCrew}
	var buf bytes.Buffer
	_ = primeCompactResume(&buf, ctx, "compact", "")
	output := buf.String()

	if strings.Contains(output, "gt done") {
		t.Fatalf("compact/resume for non-polecat should NOT mention gt done, got:\n%s", output)
	}
}

func TestEnsureBeadsRedirect_RepairsExistingRedirectChain(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigRoot := filepath.Join(townRoot, "testrig")
	rigBeadsDir := filepath.Join(rigRoot, ".beads")
	mayorBeadsDir := filepath.Join(rigRoot, "mayor", "rig", ".beads")
	workDir := filepath.Join(rigRoot, "polecats", "worker1", "testrig")
	workBeadsDir := filepath.Join(workDir, ".beads")

	if err := os.MkdirAll(mayorBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir mayor beads dir: %v", err)
	}
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir rig beads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatalf("write rig redirect: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "metadata.json"), []byte(`{"dolt_database":"hq","backend":"dolt"}`), 0644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}
	if err := os.MkdirAll(workBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir work beads dir: %v", err)
	}

	// Old polecat worktrees can keep this bd-incompatible chain:
	// worktree/.beads -> rig/.beads -> mayor/rig/.beads.
	redirectPath := filepath.Join(workBeadsDir, "redirect")
	if err := os.WriteFile(redirectPath, []byte("../../../.beads\n"), 0644); err != nil {
		t.Fatalf("write stale redirect: %v", err)
	}

	ctx := RoleContext{
		Role:     RolePolecat,
		WorkDir:  workDir,
		TownRoot: townRoot,
	}

	ensureBeadsRedirect(ctx)

	content, err := os.ReadFile(redirectPath)
	if err != nil {
		t.Fatalf("read redirect: %v", err)
	}
	if got, want := string(content), "../../../mayor/rig/.beads\n"; got != want {
		t.Fatalf("redirect content = %q, want %q", got, want)
	}
}

func TestEnsureBeadsRedirect_CleansIdentityFilesWhenRedirectAlreadyCorrect(t *testing.T) {
	t.Parallel()
	runGit := func(t *testing.T, dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	townRoot := t.TempDir()
	rigRoot := filepath.Join(townRoot, "testrig")
	rigBeadsDir := filepath.Join(rigRoot, ".beads")
	mayorBeadsDir := filepath.Join(rigRoot, "mayor", "rig", ".beads")
	workDir := filepath.Join(rigRoot, "polecats", "worker1", "gastown")
	workBeadsDir := filepath.Join(workDir, ".beads")

	if err := os.MkdirAll(mayorBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir mayor beads dir: %v", err)
	}
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir rig beads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatalf("write rig redirect: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "metadata.json"), []byte(`{"dolt_database":"hq","backend":"dolt"}`), 0644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}
	if err := os.MkdirAll(workBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir work beads dir: %v", err)
	}
	runGit(t, workDir, "init")

	redirectPath := filepath.Join(workBeadsDir, "redirect")
	if err := os.WriteFile(redirectPath, []byte("../../../mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	for _, file := range []string{"metadata.json", "config.yaml"} {
		if err := os.WriteFile(filepath.Join(workBeadsDir, file), []byte("stale identity"), 0644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}

	ctx := RoleContext{
		Role:     RolePolecat,
		WorkDir:  workDir,
		TownRoot: townRoot,
	}

	ensureBeadsRedirect(ctx)

	content, err := os.ReadFile(redirectPath)
	if err != nil {
		t.Fatalf("read redirect: %v", err)
	}
	if got, want := string(content), "../../../mayor/rig/.beads\n"; got != want {
		t.Fatalf("redirect content = %q, want %q", got, want)
	}
	// metadata.json binds bd to the database it names, so it is removed;
	// config.yaml stays: bd reads config through the redirect (gt-y3pgh.8).
	if _, err := os.Stat(filepath.Join(workBeadsDir, "metadata.json")); !os.IsNotExist(err) {
		t.Fatalf("metadata.json should have been cleaned, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workBeadsDir, "config.yaml")); err != nil {
		t.Fatalf("config.yaml should be kept (bd follows the redirect), stat err=%v", err)
	}
}

func TestOutputRalphLoopDirective_PluginInstalled(t *testing.T) {
	t.Parallel()
	attachment := &beads.AttachmentFields{
		Mode:            "ralph",
		AttachedFormula: "mol-polecat-work",
		AttachedArgs:    "Run story audit, fix worst gap, commit, loop",
		FormulaVars:     "base_branch=main",
	}
	var gotErr error
	var outputBuf bytes.Buffer
	gotErr = outputRalphLoopDirectiveWithPluginCheck(&outputBuf, RoleContext{formulaRun: polecatChecklistRun()}, attachment, true, t.TempDir())
	output := outputBuf.String()
	if gotErr != nil {
		t.Fatalf("outputRalphLoopDirectiveWithPluginCheck: %v", gotErr)
	}

	if !strings.Contains(output, "/ralph-loop ") {
		t.Fatalf("expected /ralph-loop command, got:\n%s", output)
	}
	if !strings.Contains(output, "--completion-promise DONE") {
		t.Fatalf("expected completion promise flag, got:\n%s", output)
	}
	if !strings.Contains(output, "Formula Checklist") {
		t.Fatalf("expected rendered formula checklist in prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "story audit") {
		t.Fatalf("expected attached args in prompt, got:\n%s", output)
	}
	if !strings.Contains(output, `<promise>DONE</promise>`) {
		t.Fatalf("expected DONE promise instruction in prompt, got:\n%s", output)
	}
}

func TestOutputRalphLoopDirective_PluginMissing(t *testing.T) {
	t.Parallel()
	var gotErr error
	var outputBuf bytes.Buffer
	gotErr = outputRalphLoopDirectiveWithPluginCheck(&outputBuf, RoleContext{}, &beads.AttachmentFields{Mode: "ralph"}, false, "/tmp/claude-test")
	output := outputBuf.String()
	if gotErr == nil {
		t.Fatal("expected missing plugin error")
	}
	if !strings.Contains(gotErr.Error(), ralphLoopPluginID) || !strings.Contains(gotErr.Error(), "/plugin install") {
		t.Fatalf("missing plugin error not actionable: %v", gotErr)
	}
	if strings.Contains(output, "/ralph-loop") {
		t.Fatalf("should not emit /ralph-loop when plugin is missing, got:\n%s", output)
	}
}

func TestRalphLoopPluginInstalledIn(t *testing.T) {
	t.Parallel()
	pluginsDir := filepath.Join(t.TempDir(), "plugins")
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}
	manifestPath := filepath.Join(pluginsDir, "installed_plugins.json")
	if err := os.WriteFile(manifestPath, []byte(`{"plugins":{"ralph-loop@claude-plugins-official":{},"ralph-loop-evil@claude-plugins-official":{}}}`), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	installed, err := ralphLoopPluginInstalledIn(manifestPath)
	if err != nil {
		t.Fatalf("ralphLoopPluginInstalledIn: %v", err)
	}
	if !installed {
		t.Fatal("expected canonical ralph-loop plugin to be detected")
	}

	if err := os.WriteFile(manifestPath, []byte(`{"plugins":{"ralph-loop-evil@claude-plugins-official":{}}}`), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	installed, err = ralphLoopPluginInstalledIn(manifestPath)
	if err != nil {
		t.Fatalf("ralphLoopPluginInstalledIn malicious key: %v", err)
	}
	if installed {
		t.Fatal("must not accept prefix-like plugin IDs")
	}
}

func TestIsRalphLoopPluginInstalledUsesClaudeConfigDir(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	pluginsDir := filepath.Join(configDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginsDir, "installed_plugins.json"), []byte(`{"plugins":{"ralph-loop@claude-plugins-official":{}}}`), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	installed, gotConfigDir, err := isRalphLoopPluginInstalledIn(configDir, nil)
	if err != nil {
		t.Fatalf("isRalphLoopPluginInstalled: %v", err)
	}
	if !installed {
		t.Fatal("expected plugin installed in the Claude config dir")
	}
	if gotConfigDir != configDir {
		t.Fatalf("configDir = %q, want %q", gotConfigDir, configDir)
	}
}

func TestQuoteForRalphLoop(t *testing.T) {
	t.Parallel()
	quoted := quoteForRalphLoop("line1\r\nline2 \"quoted\" \\ $HOME `cmd`")
	if !strings.HasPrefix(quoted, `"`) || !strings.HasSuffix(quoted, `"`) {
		t.Fatalf("expected double-quoted prompt, got %q", quoted)
	}
	for _, forbidden := range []string{"\n", "\r"} {
		if strings.Contains(quoted, forbidden) {
			t.Fatalf("quoted prompt contains raw line break %q: %q", forbidden, quoted)
		}
	}
	for _, want := range []string{`\n`, `\"`, `\\`, `\$`, "\\`"} {
		if !strings.Contains(quoted, want) {
			t.Fatalf("quoted prompt missing escape %q: %q", want, quoted)
		}
	}
}

func TestIsBeadNotFound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"no issue found lowercase", errors.New("bd: no issue found"), true},
		{"No issue found cased", errors.New("ERROR: No issue found in DB"), true},
		{"not found", errors.New("error: not found in beads"), true},
		{"issue not found phrasing", errors.New("rpc: issue not found"), true},
		{"connection refused", errors.New("dial tcp: connection refused"), false},
		{"random error", errors.New("schema mismatch"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBeadNotFound(tt.err); got != tt.want {
				t.Errorf("isBeadNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrHookUnresolvable_IsErrors(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("%w: agent=foo hook_bead=hq-igrp", ErrHookUnresolvable)
	if !errors.Is(wrapped, ErrHookUnresolvable) {
		t.Fatalf("errors.Is should report wrapped err matches ErrHookUnresolvable")
	}
}

// TestShouldRenderMemories is the pure unit half of the memory role gate
// (gt-o51s, plan Task 8 C3), for the same reason TestShouldSkipStartupMailInject
// is the pure half of the mail gate: the subprocess-based tests that exercise
// this end to end tie their verdict to a spawn deadline, and this one does not.
// Every role is listed, so adding a role to the code without deciding whether
// it gets memories is a compile-visible omission rather than a silent default.
func TestShouldRenderMemories(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role string
		want bool
	}{
		{string(RoleMayor), true},
		{string(RoleCrew), true},
		{string(RolePolecat), false},
		{string(RoleUnknown), false},
		{"", false},
		{"MAYOR", true}, // role comparison is case-insensitive
	}
	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			if got := shouldRenderMemories(tt.role); got != tt.want {
				t.Errorf("shouldRenderMemories(%q) = %v, want %v", tt.role, got, tt.want)
			}
		})
	}
}
