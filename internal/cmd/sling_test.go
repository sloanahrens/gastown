package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
)

func assertNoRawReviewMetadata(t *testing.T, desc string) {
	t.Helper()
	if strings.Contains(desc, "no_merge: true") || strings.Contains(desc, "review_only: true") {
		t.Fatalf("stale raw review metadata remains in description:\n%s", desc)
	}
	fields := beads.ParseAttachmentFields(&beads.Issue{Description: desc})
	if fields != nil && (fields.NoMerge || fields.ReviewOnly) {
		t.Fatalf("parsed stale raw review metadata from description: %+v", fields)
	}
}

func assertHasRawReviewMetadata(t *testing.T, desc string) {
	t.Helper()
	fields := beads.ParseAttachmentFields(&beads.Issue{Description: desc})
	if fields == nil || !fields.NoMerge || !fields.ReviewOnly {
		t.Fatalf("raw review metadata missing from description:\n%s", desc)
	}
	if fields.AttachedAt == "" {
		t.Fatalf("raw review metadata missing attached_at:\n%s", desc)
	}
	if _, err := time.Parse(time.RFC3339Nano, fields.AttachedAt); err != nil {
		t.Fatalf("attached_at %q is not RFC3339Nano: %v", fields.AttachedAt, err)
	}
}

func containsVarArg(line, key, value string) bool {
	plain := "--var " + key + "=" + value
	if strings.Contains(line, plain) {
		return true
	}
	quoted := "--var \"" + key + "=" + value + "\""
	return strings.Contains(line, quoted)
}

func TestParseWispIDFromJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		json    string
		wantID  string
		wantErr bool
	}{
		{
			name:   "new_epic_id",
			json:   `{"new_epic_id":"gt-wisp-abc","created":7,"phase":"vapor"}`,
			wantID: "gt-wisp-abc",
		},
		{
			name:   "root_id legacy",
			json:   `{"root_id":"gt-wisp-legacy"}`,
			wantID: "gt-wisp-legacy",
		},
		{
			name:   "result_id forward compat",
			json:   `{"result_id":"gt-wisp-result"}`,
			wantID: "gt-wisp-result",
		},
		{
			name:   "precedence prefers new_epic_id",
			json:   `{"root_id":"gt-wisp-legacy","new_epic_id":"gt-wisp-new"}`,
			wantID: "gt-wisp-new",
		},
		{
			name:    "missing id keys",
			json:    `{"created":7,"phase":"vapor"}`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			json:    `{"new_epic_id":`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, err := parseWispIDFromJSON([]byte(tt.json))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseWispIDFromJSON() error = %v, wantErr %v", err, tt.wantErr)
			}
			if gotID != tt.wantID {
				t.Fatalf("parseWispIDFromJSON() id = %q, want %q", gotID, tt.wantID)
			}
		})
	}
}

func TestExtractIssueID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"unwraps external format", "external:gt-mol:gt-mol-abc123", "gt-mol-abc123"},
		{"unwraps beads external", "external:beads-task:beads-task-xyz", "beads-task-xyz"},
		{"passes through hq IDs", "hq-abc123", "hq-abc123"},
		{"passes through plain IDs", "gt-abc123", "gt-abc123"},
		{"handles malformed external (only 2 parts)", "external:gt-mol", "external:gt-mol"},
		{"handles empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := beads.ExtractIssueID(tt.id)
			if got != tt.want {
				t.Errorf("ExtractIssueID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

// TestGetBeadInfoViaReadsRoutedBeadFromRigDatabase: a rig-prefixed bead is
// read from the rig's database its route names, not the town's, and the
// issue fields survive the parse.
func TestGetBeadInfoViaReadsRoutedBeadFromRigDatabase(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadID := "gt-new123"
	rigBeadsDir := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	for _, dir := range []string{filepath.Join(townRoot, ".beads"), rigBeadsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTestRoutes(t, townRoot, []beads.Route{{Prefix: "gt-", Path: "gastown/mayor/rig"}, {Prefix: "hq-", Path: "."}})

	var mu sync.Mutex
	var showDirs []string
	run := func(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
		beadsDir := envSlice(c.Env)["BEADS_DIR"]
		mu.Lock()
		showDirs = append(showDirs, beadsDir)
		mu.Unlock()
		if beadsDir != rigBeadsDir {
			return nil, []byte("wrong database: " + beadsDir), inprocBDExit(1)
		}
		return []byte(`[{"id":"gt-new123","title":"Routed bead","status":"open","assignee":"","description":"body","issue_type":"bug","labels":["x"],"dependencies":[{"id":"gt-wisp-old","status":"open"}]}]`), nil, nil
	}

	info, err := getBeadInfoVia(run, townRoot, beadID)
	if err != nil {
		t.Fatalf("getBeadInfoVia: %v (show BEADS_DIRs %q)", err, showDirs)
	}
	if info.Title != "Routed bead" || info.IssueType != "bug" || len(info.Labels) != 1 || len(info.Dependencies) != 1 {
		t.Fatalf("info = %+v, want routed issue fields preserved", info)
	}
	if len(showDirs) != 1 || showDirs[0] != rigBeadsDir {
		t.Fatalf("bd show BEADS_DIRs = %q, want one show against %q", showDirs, rigBeadsDir)
	}
}

// TestSlingRejectsBeadMissingFromTargetRigBeforeSpawn: a bead that resolves
// from HQ but is absent from the target rig's own database is refused before
// a polecat is spawned for it.
func TestSlingRejectsBeadMissingFromTargetRigBeforeSpawn(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead("gt-r2405", beadInfo{Title: "HQ-owned issue"})
	h.run.opts.noConvoy = true
	h.run.resolveTarget = h.run.resolveSlingTarget
	h.run.verifyInTargetRig = func(id, rig, _ string) error {
		return fmt.Errorf("bead %s is not present in target rig %q", id, rig)
	}

	wantSlingErr(t, h.sling("gt-r2405", "gastown"), "not present in target rig")
	h.wantNo("spawn")
	h.wantNo("hook")
}

// TestTargetRigDatabaseAllowsRouteResolvedGtBead: a gt- bead whose id also
// reads like an hq one is checked in the target rig's own database, pinned to
// that rig's Dolt database name, and found there.
func TestTargetRigDatabaseAllowsRouteResolvedGtBead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	for _, dir := range []string{filepath.Join(townRoot, ".beads"), filepath.Join(townRoot, "mayor", "rig"), filepath.Join(rigDir, ".beads")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTestRoutes(t, townRoot, []beads.Route{{Prefix: "gt-", Path: "gastown/mayor/rig"}, {Prefix: "hq-", Path: "."}})
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"dolt_database":"gastown","dolt_server_host":"127.0.0.1","dolt_server_port":3307}`), 0644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}

	var mu sync.Mutex
	var calls []beads.BDCall
	run := func(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		return []byte(`[{"title":"Route issue","status":"open","assignee":"","description":""}]`), nil, nil
	}

	if err := verifyBeadExistsInTargetRigDatabaseVia(run, "gt-hq-oy83-cleanup", "gastown", townRoot); err != nil {
		t.Fatalf("verifyBeadExistsInTargetRigDatabase: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("bd calls = %d, want one direct show", len(calls))
	}
	c := calls[0]
	if !strings.Contains(strings.Join(c.Args, " "), "show gt-hq-oy83-cleanup --json") {
		t.Fatalf("bd args = %q, want route-resolved show", c.Args)
	}
	if c.Dir != rigDir {
		t.Fatalf("bd cwd = %q, want %q", c.Dir, rigDir)
	}
	env := envSlice(c.Env)
	if want := filepath.Join(rigDir, ".beads"); env["BEADS_DIR"] != want {
		t.Fatalf("BEADS_DIR = %q, want %q", env["BEADS_DIR"], want)
	}
	if env["BEADS_DOLT_SERVER_DATABASE"] != "gastown" {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want gastown", env["BEADS_DOLT_SERVER_DATABASE"])
	}
}

// TestScheduleBeadRejectsMissingTargetRigDatabaseBeforeContext: scheduling
// a bead absent from the target rig's database is refused before any sling
// context, cook, convoy or feed write.
func TestScheduleBeadRejectsMissingTargetRigDatabaseBeforeContext(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead("gt-r2405", beadInfo{Title: "HQ-owned issue"})
	h.run.verifyInTargetRig = func(id, rig, _ string) error {
		return fmt.Errorf("bead %s is not present in target rig %q", id, rig)
	}

	err := h.run.scheduleSlingBead("gt-r2405", "gastown", ScheduleOptions{Formula: "mol-polecat-work"})
	wantSlingErr(t, err, "not present in target rig")
	for _, sideEffect := range []string{"find context", "create context", "update context", "cook", "create convoy", "feed"} {
		h.wantNo(sideEffect)
	}
}

// TestBatchSlingRejectsMissingTargetRigDatabaseBeforeSpawn: a bead the
// target rig's database does not hold stops the whole batch before any
// formula is cooked or any polecat is spawned.
func TestBatchSlingRejectsMissingTargetRigDatabaseBeforeSpawn(t *testing.T) {
	t.Parallel()
	f := newBatchSlingFake(t)
	f.b.resolveFormula = func(string, bool, string, string) string { return "mol-polecat-work" }
	f.b.verifyInTargetRig = func(beadID, targetRig, townRoot string) error {
		if beadID == "gt-r2405" {
			return errors.New("bead " + beadID + " is not present in target rig " + targetRig + " beads database")
		}
		return nil
	}

	err := f.b.run([]string{"gt-ok", "gt-r2405"}, "gastown", filepath.Join(t.TempDir(), ".beads"))
	if err == nil {
		t.Fatal("expected target-rig database validation error")
	}
	if !strings.Contains(err.Error(), "not present in target rig") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.executed) != 0 || f.cooked != 0 {
		t.Fatalf("slung %v and cooked %d times before target-rig validation rejected the batch", f.executed, f.cooked)
	}
}

func TestSchedulerRejectsReviewOnlyForEpicConvoy(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.Flags().Bool("review-only", false, "")
	if err := cmd.Flags().Set("review-only", "true"); err != nil {
		t.Fatalf("set review-only flag: %v", err)
	}

	for _, mode := range []string{"epic", "convoy"} {
		err := validateNoTaskOnlySchedulerFlags(cmd, mode)
		if err == nil {
			t.Fatalf("validateNoTaskOnlySchedulerFlags(%s) accepted --review-only", mode)
		}
		if !strings.Contains(err.Error(), "--review-only") {
			t.Fatalf("validateNoTaskOnlySchedulerFlags(%s) error = %v, want --review-only", mode, err)
		}
	}
}

// TestResolveTargetRejectsLivePolecatMissingTargetRigDatabase: a live
// polecat target, in every address form, is refused when the bead is absent
// from that polecat's rig database.
func TestResolveTargetRejectsLivePolecatMissingTargetRigDatabase(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"gastown/polecats/toast", "gastown/toast", "gt-gastown-polecat-toast"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.run.resolveAgent = func(string) (string, string, string, error) {
				return "gastown/polecats/toast", "%1", slingTestTown + "/gastown/polecats/toast", nil
			}
			h.run.verifyInTargetRig = func(id, rig, _ string) error {
				h.record("verify %s in %s", id, rig)
				return fmt.Errorf("bead %s is not present in target rig %q", id, rig)
			}

			_, err := h.run.resolveSlingTarget(target, ResolveTargetOptions{BeadID: "gt-r2405", TownRoot: slingTestTown})
			wantSlingErr(t, err, "not present in target rig")
			h.wantCalls("verify", "verify gt-r2405 in gastown")
		})
	}
}

// TestResolveTargetCreateSpawnsPolecatShorthandWhenPaneMissing: under
// --create, a <rig>/<name> shorthand with no session and no crew member of
// that name spawns the named polecat, keeping --create.
func TestResolveTargetCreateSpawnsPolecatShorthandWhenPaneMissing(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	var gotRig string
	var got SlingSpawnOptions
	h.run.spawnPolecat = func(rig string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		gotRig, got = rig, opts
		return &SpawnedPolecatInfo{RigName: rig, PolecatName: opts.Name}, nil
	}

	res, err := h.run.resolveSlingTarget("gastown/toast", ResolveTargetOptions{Create: true, NoBoot: true})
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if gotRig != "gastown" || got.Name != "toast" || !got.Create {
		t.Fatalf("spawn(%q, Name=%q Create=%v), want (gastown, toast, true)", gotRig, got.Name, got.Create)
	}
	if res.Agent != "gastown/polecats/toast" {
		t.Fatalf("Agent = %q, want gastown/polecats/toast", res.Agent)
	}
}

// TestResolveTargetCreateDoesNotSpawnCrewShorthandWhenPaneMissing: a
// <rig>/<name> shorthand that names a crew member with no session stays a
// resolve error even under --create; it never becomes a polecat.
func TestResolveTargetCreateDoesNotSpawnCrewShorthandWhenPaneMissing(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.crew["gastown/toast"] = true

	_, err := h.run.resolveSlingTarget("gastown/toast", ResolveTargetOptions{Create: true, NoBoot: true})
	wantSlingErr(t, err, "resolving target")
	h.wantNo("spawn")
}

func TestTargetRigDatabaseLookupFailsClosedWithoutTownRoot(t *testing.T) {
	t.Parallel()
	err := verifyBeadExistsInTargetRigDatabase("gt-r2405", "gastown", "")
	if err == nil {
		t.Fatal("expected fail-closed error without town root")
	}
	if !strings.Contains(err.Error(), "town root is unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRestoreRollbackRawWorkflowFieldsRestoresOriginalValues: a rollback puts
// the raw workflow fields back to their pre-sling values and keeps the
// current metadata and body.
func TestRestoreRollbackRawWorkflowFieldsRestoresOriginalValues(t *testing.T) {
	t.Parallel()
	current := strings.Join([]string{
		"no_merge: true",
		"review_only: true",
		"dispatched_by: mayor/",
		"",
		"Keep this body.",
	}, "\n")
	bead := &mutableBead{id: "gt-rawrollback", status: "hooked", desc: current}
	townRoot := t.TempDir()
	original := &beadInfo{Description: strings.Join([]string{
		"no_merge: true",
		"",
		"Original body.",
	}, "\n")}

	restored, err := restoreRollbackRawWorkflowFieldsVia(mutableBD(bead).run, "gt-rawrollback", townRoot, filepath.Join(townRoot, "gastown", "polecats", "toast"), &beadInfo{Description: current}, original)
	if err != nil || !restored {
		t.Fatalf("restoreRollbackRawWorkflowFields = %v, %v; want restored", restored, err)
	}

	desc := bead.description()
	fields := beads.ParseAttachmentFields(&beads.Issue{Description: desc})
	if fields == nil || !fields.NoMerge || fields.ReviewOnly {
		t.Fatalf("rollback did not restore original workflow values: %+v\n%s", fields, desc)
	}
	if !strings.Contains(desc, "dispatched_by: mayor/") || !strings.Contains(desc, "Keep this body.") {
		t.Fatalf("rollback did not preserve current metadata/body:\n%s", desc)
	}
}

// TestSlingFormulaRollsBackSpawnedPolecatOnWispFailure: a formula slung to a
// rig spawns its polecat before the wisp exists, so a failed wisp create
// rolls the polecat back, naming no bead and using the polecat's clone as the
// working directory.
func TestSlingFormulaRollsBackSpawnedPolecatOnWispFailure(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.run.createWisp = func(string, string, string, []string) ([]byte, error) {
		return nil, errors.New("missing required vars")
	}
	h.run.rollbackArtifacts = func(s *SpawnedPolecatInfo, id, dir, _ string) {
		h.record("rollback %s bead=%q dir=%s", s.PolecatName, id, dir)
	}

	err := h.run.runFormula(context.Background(), []string{"mol-anything", "gastown"})
	wantSlingErr(t, err, "creating wisp")
	h.wantCalls("rollback", `rollback Toast bead="" dir=`+slingTestTown+"/gastown/polecats/Toast")
}

// TestRunSlingFormulaPersistsVarContext: a standalone formula sling passes
// its --var values to the wisp and stores the formula, the vars and ralph
// mode on the wisp it hooks, so they survive the session.
func TestRunSlingFormulaPersistsVarContext(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.run.resolveSelf = func() (string, string, string, error) { return "mayor/", "", slingTestTown, nil }
	h.run.opts.vars = []string{"version=1.2.3", "channel=stable"}
	h.run.opts.ralph = true

	if err := h.run.runFormula(context.Background(), []string{"mol-anything"}); err != nil {
		t.Fatalf("runFormula: %v", err)
	}
	h.wantCalls("create wisp", "create wisp mol-anything vars=version=1.2.3,channel=stable")
	h.wantCalls("hook", "hook gt-wisp-new mayor/")
	h.wantCalls("agent mode", "agent mode mayor/ ralph")

	stored := h.stored["gt-wisp-new"]
	if len(stored) != 1 {
		t.Fatalf("stored field updates = %+v, want one", stored)
	}
	u := stored[0]
	if u.AttachedFormula != "mol-anything" {
		t.Errorf("AttachedFormula = %q, want mol-anything", u.AttachedFormula)
	}
	if u.FormulaVars != "version=1.2.3\nchannel=stable" || strings.Join(u.Vars, ",") != "version=1.2.3,channel=stable" {
		t.Errorf("vars = %q / %q, want both --var values", u.FormulaVars, u.Vars)
	}
	if u.Mode == nil || *u.Mode != "ralph" {
		t.Errorf("Mode = %v, want ralph", u.Mode)
	}
}

// TestRunSlingFormulaNoOpWhenSameFormulaAlreadyHooked: slinging a formula
// already hooked to the target, in the same mode, writes nothing: no cook, no
// new wisp, no hook and no field update.
func TestRunSlingFormulaNoOpWhenSameFormulaAlreadyHooked(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.run.resolveSelf = func() (string, string, string, error) { return "mayor/", "", slingTestTown, nil }
	h.hookedFormulas["mayor/"] = &beads.Issue{ID: "gt-wisp-existing"}

	if err := h.run.runFormula(context.Background(), []string{"mol-anything"}); err != nil {
		t.Fatalf("runFormula: %v", err)
	}
	for _, write := range []string{"cook", "create wisp", "hook", "store fields", "agent mode"} {
		h.wantNo(write)
	}
}

// TestRunSlingFormulaUpdatesModeWhenSameFormulaAlreadyHooked: re-slinging
// the hooked formula without --ralph clears the stale ralph mode on the
// existing wisp and the agent, and still creates no new wisp.
func TestRunSlingFormulaUpdatesModeWhenSameFormulaAlreadyHooked(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.run.resolveSelf = func() (string, string, string, error) { return "mayor/", "", slingTestTown, nil }
	h.hookedFormulas["mayor/"] = &beads.Issue{ID: "gt-wisp-existing", Description: "attached_formula: mol-anything\nmode: ralph"}

	if err := h.run.runFormula(context.Background(), []string{"mol-anything"}); err != nil {
		t.Fatalf("runFormula: %v", err)
	}
	stored := h.stored["gt-wisp-existing"]
	if len(stored) != 1 || stored[0].Mode == nil || *stored[0].Mode != "" {
		t.Fatalf("stored field updates = %+v, want one clearing the mode", stored)
	}
	h.wantCalls("agent mode", "agent mode mayor/ ")
	h.wantNo("create wisp")
}

// TestFormulaVarsForBeadPassesFeatureAndIssueVars verifies that gt sling
// <formula> --on <bead> bonds with --var feature=<title> and --var
// issue=<beadID> first, then the caller's vars, for a formula declaring none.
func TestFormulaVarsForBeadPassesFeatureAndIssueVars(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := &fakeCook{out: cookTreeJSON(`{"formula": "mol-review", "vars": [], "steps": []}`)}
	vars, err := formulaBDVia(fake.run).varsForBead("mol-review", "gt-abc123", "My Test Feature", town, town, []string{"k=v"})
	if err != nil {
		t.Fatalf("varsForBead: %v", err)
	}
	want := []string{"feature=My Test Feature", "issue=gt-abc123", "k=v"}
	if strings.Join(vars, "|") != strings.Join(want, "|") {
		t.Fatalf("vars = %q, want %q", vars, want)
	}
}

// TestLooksLikeBeadID tests the bead ID pattern recognition function.
// This ensures gt sling accepts bead IDs even when routing-based verification fails.
// Fixes: gt sling bd-ka761 failing with 'not a valid bead or formula'
//
// Note: looksLikeBeadID is a fallback check in sling. The actual sling flow is:
// 1. Try verifyBeadExists (routing-based lookup)
// 2. Try verifyFormulaExists (formula check)
// 3. Fall back to looksLikeBeadID pattern match
// So "mol-release" matches the pattern but won't be treated as bead in practice
// because it would be caught by formula verification first.
func TestLooksLikeBeadID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		// Valid bead IDs - should return true
		{"gt-abc123", true},
		{"bd-ka761", true},
		{"hq-cv-abc", true},
		{"ap-qtsup.16", true},
		{"beads-xyz", true},
		{"jv-v599", true},
		{"gt-9e8s5", true},
		{"hq-00gyg", true},

		// Short prefixes that match pattern (but may be formulas in practice)
		{"mol-release", true}, // 3-char prefix matches pattern (formula check runs first in sling)
		{"mol-abc123", true},  // 3-char prefix matches pattern

		// Non-bead strings - should return false
		{"formula-name", false}, // "formula" is 7 chars (> 5)
		{"mayor", false},        // no hyphen
		{"gastown", false},      // no hyphen
		{"deacon/dogs", false},  // contains slash
		{"", false},             // empty
		{"-abc", false},         // starts with hyphen
		{"GT-abc", false},       // uppercase prefix
		{"123-abc", false},      // numeric prefix
		{"a-", false},           // nothing after hyphen
		{"aaaaaa-b", false},     // prefix too long (6 chars)

		// Injection / invalid suffix characters - should return false
		{"gt-abc;rm -rf /", false}, // shell injection in suffix
		{"gt-abc$(cmd)", false},    // command substitution in suffix
		{"gt-abc&bg", false},       // ampersand in suffix
		{"gt-abc|pipe", false},     // pipe in suffix
		{"gt-abc`tick`", false},    // backtick in suffix
		{"gt-abc>redir", false},    // redirect in suffix
		{"gt-abc<redir", false},    // redirect in suffix
		{"gt-abc'quote", false},    // single quote in suffix
		{"gt-abc\"dquote", false},  // double quote in suffix
		{"gt-abc\\slash", false},   // backslash in suffix
		{"gt-abc xyz", false},      // space in suffix
		{"gt-ABC", false},          // uppercase in suffix
		{"gt-abc/path", false},     // slash in suffix
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := looksLikeBeadID(tt.input)
			if got != tt.want {
				t.Errorf("looksLikeBeadID(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestSlingSetsDoltAutoCommitOff verifies that gt sling sets BD_DOLT_AUTO_COMMIT=off
// for all child bd processes. Under concurrent load (batch slinging), auto-commits
// from individual bd writes cause manifest contention and 'database is read only'
// errors. The Dolt server handles commits — individual auto-commits are unnecessary.
// Fixes: gt-u6n6a

// TestCheckCrossRigGuard verifies that cross-rig sling is rejected when a bead's
// prefix doesn't match the target rig. This prevents slinging beads-codebase issues
// to gastown polecats, which cannot fix code in a different rig's repo.
// Fixes: gt-myecw
func TestCheckCrossRigGuard(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}
{"prefix":"bd-","path":"beads/mayor/rig"}
{"prefix":"hq-","path":"."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		beadID      string
		targetAgent string
		wantErr     bool
	}{
		{
			name:        "same rig: gt bead to gastown polecat",
			beadID:      "gt-abc123",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     false,
		},
		{
			name:        "same rig: bd bead to beads polecat",
			beadID:      "bd-ka761",
			targetAgent: "beads/polecats/obsidian",
			wantErr:     false,
		},
		{
			name:        "cross-rig: bd bead to gastown polecat",
			beadID:      "bd-ka761",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     true,
		},
		{
			name:        "cross-rig: gt bead to beads polecat",
			beadID:      "gt-abc123",
			targetAgent: "beads/polecats/obsidian",
			wantErr:     true,
		},
		{
			// Known town-root prefix: warn but allow. A crew member with a broken
			// redirect chain may create hq-* beads that legitimately target a rig
			// polecat (gt-gbu). Hard-rejecting silently drops all their polecat work.
			name:        "town-level: hq bead to rig (warns but allows — gt-gbu)",
			beadID:      "hq-abc123",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     false,
		},
		{
			// Truly unknown prefix (not in routes.jsonl): hard reject.
			name:        "unknown prefix: rejected (no route exists at all)",
			beadID:      "xx-unknown",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     true,
		},
		{
			name:        "empty bead prefix: allowed",
			beadID:      "nohyphen",
			targetAgent: "gastown/polecats/Toast",
			wantErr:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCrossRigGuard(tc.beadID, tc.targetAgent, tmpDir)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkCrossRigGuard(%q, %q) error = %v, wantErr %v", tc.beadID, tc.targetAgent, err, tc.wantErr)
			}
			if err != nil && tc.wantErr {
				errMsg := err.Error()
				if !strings.Contains(errMsg, "cross-rig mismatch") && !strings.Contains(errMsg, "not in routes") {
					t.Errorf("expected cross-rig mismatch or unknown-prefix error, got: %v", err)
				}
				if !strings.Contains(errMsg, "--force") {
					t.Errorf("error should mention --force override, got: %v", err)
				}
				if !strings.Contains(errMsg, "bd create") {
					t.Errorf("error should mention bd create, got: %v", err)
				}
			}
		})
	}
}

func TestIsHookedAgentDead_UnknownFormat(t *testing.T) {
	t.Parallel()
	// Unknown assignee formats should return false (conservative)
	tests := []struct {
		name     string
		assignee string
	}{
		{"empty", ""},
		{"unknown_single", "foobar"},
		{"four_parts", "a/b/c/d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if isHookedAgentDead(tt.assignee) {
				t.Errorf("isHookedAgentDead(%q) = true, want false (unknown format)", tt.assignee)
			}
		})
	}
}

func TestIsHookedAgentDead_NoTmuxSession(t *testing.T) {
	t.Parallel()
	// For a known assignee format where no tmux session exists,
	// isHookedAgentDead should return true (session is dead).
	// Use a highly unlikely polecat name to ensure no collision with real sessions.
	result := isHookedAgentDead("nonexistent_rig_xyz/polecats/ghost_polecat_999")
	// This might return true (no session) or false (tmux not available).
	// We just verify it doesn't panic.
	_ = result
}

// TestHookBeadWithRetryForcesAutoCommit: the hook write commits on its own,
// so the read-back and every later bd call see it.
func TestHookBeadWithRetryForcesAutoCommit(t *testing.T) {
	t.Parallel()
	var got beads.BDCall
	run := func(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
		got = c
		return nil, nil, nil
	}

	if err := hookBeadWithRetryVia(run, nil, "gt-test123", "gastown/polecats/toast", t.TempDir()); err != nil {
		t.Fatalf("hookBeadWithRetry: %v", err)
	}
	if strings.Join(got.Args, " ") != "update gt-test123 --status=hooked --assignee=gastown/polecats/toast" {
		t.Fatalf("hook argv = %q", got.Args)
	}
	if v := envSlice(got.Env)["BD_DOLT_AUTO_COMMIT"]; v != "on" {
		t.Fatalf("hook update BD_DOLT_AUTO_COMMIT = %q, want on", v)
	}
}

func TestBuildSlingFieldUpdatesIncludesConvoyFields(t *testing.T) {
	t.Parallel()
	got := buildSlingFieldUpdates(
		"mayor",
		"review this",
		[]string{"feature=test"},
		"gt-wisp-test",
		"mol-polecat-work",
		false,
		false,
		"ralph",
		"feature=test",
		"hq-cv-test1",
		"local",
		true,
	)

	if got.ConvoyID != "hq-cv-test1" {
		t.Fatalf("ConvoyID = %q, want %q", got.ConvoyID, "hq-cv-test1")
	}
	if got.MergeStrategy != "local" {
		t.Fatalf("MergeStrategy = %q, want %q", got.MergeStrategy, "local")
	}
	if !got.ConvoyOwned {
		t.Fatal("ConvoyOwned = false, want true")
	}
	if got.Mode == nil || *got.Mode != "ralph" {
		t.Fatalf("Mode = %v, want ralph", got.Mode)
	}
}

// TestStoreFieldsInBeadConvoyFields: convoy membership lands in the bead's
// attachment fields.
func TestStoreFieldsInBeadConvoyFields(t *testing.T) {
	t.Parallel()
	text := applyBeadFieldUpdates(&beads.Issue{}, beadFieldUpdates{
		ConvoyID:      "hq-cv-test1",
		MergeStrategy: "local",
		ConvoyOwned:   true,
	})

	if !strings.Contains(text, "convoy_id: hq-cv-test1") {
		t.Fatalf("missing convoy_id in description:\n%s", text)
	}
	if !strings.Contains(text, "merge_strategy: local") {
		t.Fatalf("missing merge_strategy in description:\n%s", text)
	}
	if !strings.Contains(text, "convoy_owned: true") {
		t.Fatalf("missing convoy_owned in description:\n%s", text)
	}
}

func TestBeadFieldModeUpdateCanClearStaleRalphMode(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{Description: "attached_formula: mol-polecat-work\nmode: ralph"}
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		t.Fatal("expected attachment fields")
	}
	mode := ""
	updates := beadFieldUpdates{Mode: &mode}
	if updates.Mode != nil {
		fields.Mode = *updates.Mode
	}
	desc := beads.SetAttachmentFields(issue, fields)
	if strings.Contains(desc, "mode: ralph") || strings.Contains(desc, "mode:") {
		t.Fatalf("expected stale ralph mode to be cleared, got:\n%s", desc)
	}
	if !strings.Contains(desc, "attached_formula: mol-polecat-work") {
		t.Fatalf("expected unrelated attachment fields preserved, got:\n%s", desc)
	}
}

// TestStoreFieldsInBeadFormulaSetsAttachedAt: attaching a formula stamps
// attached_at.
func TestStoreFieldsInBeadFormulaSetsAttachedAt(t *testing.T) {
	t.Parallel()
	body := applyBeadFieldUpdates(&beads.Issue{}, beadFieldUpdates{
		AttachedFormula: "mol-dog-reaper",
	})

	fields := beads.ParseAttachmentFields(&beads.Issue{Description: body})
	if fields == nil || fields.AttachedFormula != "mol-dog-reaper" || fields.AttachedAt == "" {
		t.Fatalf("formula attachment fields = %#v, want formula and attached_at", fields)
	}
	if _, err := time.Parse(time.RFC3339Nano, fields.AttachedAt); err != nil {
		t.Fatalf("attached_at %q is not RFC3339Nano: %v", fields.AttachedAt, err)
	}
}

// TestStoreFieldsInBeadRawReviewRefreshesAttachedAt: re-slinging raw review
// work refreshes a stale attached_at.
func TestStoreFieldsInBeadRawReviewRefreshesAttachedAt(t *testing.T) {
	t.Parallel()
	stale := "2026-06-30T12:00:00Z"
	issue := &beads.Issue{Description: "attached_at: " + stale + "\nno_merge: true\nreview_only: true\n"}

	body := applyBeadFieldUpdates(issue, beadFieldUpdates{
		NoMerge:    true,
		ReviewOnly: true,
	})

	fields := beads.ParseAttachmentFields(&beads.Issue{Description: body})
	if fields == nil || !fields.NoMerge || !fields.ReviewOnly {
		t.Fatalf("raw review fields = %#v", fields)
	}
	if fields.AttachedAt == "" || fields.AttachedAt == stale {
		t.Fatalf("attached_at = %q, want refreshed from %q", fields.AttachedAt, stale)
	}
	if _, err := time.Parse(time.RFC3339Nano, fields.AttachedAt); err != nil {
		t.Fatalf("attached_at %q is not RFC3339Nano: %v", fields.AttachedAt, err)
	}
}

// TestResolveTargetSelfSlingByPane verifies that a named target resolving to the
// caller's own tmux pane sets IsSelfSling=true (GH#3839). Without this, gt sling
// deacon (from the deacon itself) injects the ack prompt into the running agent's
// pane, wedging it mid-command.
func TestResolveTargetSelfSlingByPane(t *testing.T) {
	t.Parallel()
	const callerPane = "%42"
	for _, tt := range []struct {
		name       string
		targetPane string
		want       bool
	}{
		{"named_target_same_pane_is_self_sling", callerPane, true},
		{"named_target_different_pane_is_not_self_sling", "%99", false},
		{"empty_pane_is_not_self_sling", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.env["TMUX_PANE"] = callerPane
			h.run.resolveAgent = func(string) (string, string, string, error) {
				return "deacon/", tt.targetPane, "/home/deacon", nil
			}

			result, err := h.run.resolveSlingTarget("deacon", ResolveTargetOptions{})
			if err != nil {
				t.Fatalf("resolveTarget: %v", err)
			}
			if result.IsSelfSling != tt.want {
				t.Errorf("IsSelfSling = %v, want %v (target pane %q, caller pane %q)", result.IsSelfSling, tt.want, tt.targetPane, callerPane)
			}
		})
	}
}
