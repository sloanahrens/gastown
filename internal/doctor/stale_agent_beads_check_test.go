package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestNewStaleAgentBeadsCheck(t *testing.T) {
	t.Parallel()
	check := NewStaleAgentBeadsCheck()

	if check.Name() != "stale-agent-beads" {
		t.Errorf("expected name 'stale-agent-beads', got %q", check.Name())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}

	if check.Description() != "Detect agent beads for removed workers (crew and polecats)" {
		t.Errorf("unexpected description: %q", check.Description())
	}

	if check.Category() != CategoryRig {
		t.Errorf("expected category %q, got %q", CategoryRig, check.Category())
	}
}

func TestStaleAgentBeadsCheck_NoRoutes(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// No .beads dir at all — LoadRoutes returns empty, so check returns OK (no rigs)
	check := NewStaleAgentBeadsCheck()
	ctx := newDoctorTown().ctx(tmpDir, "")

	result := check.Run(ctx)

	// With no routes and a store that answers, there is nothing to report.
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for a town with no routes, got %v: %s", result.Status, result.Message)
	}
}

func TestStaleAgentBeadsCheck_NoRigs(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create .beads dir with empty routes.jsonl
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewStaleAgentBeadsCheck()
	ctx := newDoctorTown().ctx(tmpDir, "")

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for no rigs, got %v: %s", result.Status, result.Message)
	}
}

func TestStaleAgentBeadsCheck_CrewOnDisk(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Set up routes pointing to a rig
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"myrig/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rig beads directory
	rigBeadsDir := filepath.Join(tmpDir, "myrig", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create crew on disk
	crewDir := filepath.Join(tmpDir, "myrig", "crew")
	for _, name := range []string{"alice", "bob"} {
		if err := os.MkdirAll(filepath.Join(crewDir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}

	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	// Without a running bd daemon, List() will fail gracefully
	// The check should handle this and not crash
	result := check.Run(ctx)
	t.Logf("Stale agent beads check: status=%v, message=%s", result.Status, result.Message)
}

// unlistablePath puts a regular file where a worker directory is expected, so
// os.ReadDir fails with an error that is not fs.ErrNotExist — the branch an
// existing but unreadable directory takes. A chmod 000 fixture would not do:
// under root, or on a filesystem without POSIX permissions, the directory stays
// readable, the branch is never taken, and the test passes without testing the
// refusal.
func unlistablePath(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestStaleAgentBeadsUnlistablePolecatsPath verifies that a rig whose polecats
// directory exists but cannot be read contributes no stale beads and closes
// none. Before gt-9wiz3, listPolecats folded that error into "no polecats", so
// the check and its fix closed the agent beads of live polecats.
func TestStaleAgentBeadsUnlistablePolecatsPath(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatal(err)
	}

	// A polecats path the check cannot read. The bead of a polecat that may
	// live there must not be closed.
	polecatsDir := filepath.Join(tmpDir, "gastown", "polecats")
	unlistablePath(t, polecatsDir)

	town := newDoctorTown()
	town.db(filepath.Join(tmpDir, "gastown", "mayor", "rig")).Seed(beads.Issue{
		ID:     "gt-gastown-polecat-nux",
		Title:  "nux",
		Type:   "agent",
		Labels: []string{"gt:agent"},
	})

	check := NewStaleAgentBeadsCheck()
	ctx := town.ctx(tmpDir, "")

	result := check.Run(ctx)
	for _, detail := range result.Details {
		if detail == "gt-gastown-polecat-nux" {
			t.Fatalf("unreadable polecats directory marked live polecat stale; details: %v", result.Details)
		}
	}
	if result.Status == StatusOK {
		t.Errorf("an unreadable polecats directory must be reported, got OK: %s", result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}
	if updates := town.updates(); len(updates) != 0 {
		t.Errorf("Fix() closed beads for a rig it could not read: %v", updates)
	}
}

// TestStaleAgentBeadsUnlistableCrewPath verifies that a rig whose crew
// directory exists but cannot be read contributes no stale beads and closes
// none. Before gt-ro1lc, listCrewWorkers folded that error into "no crew", so
// the check and its fix closed the agent beads of live crew members.
func TestStaleAgentBeadsUnlistableCrewPath(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatal(err)
	}

	// A crew path the check cannot read. The bead of a crew member that may
	// live there must not be closed.
	crewDir := filepath.Join(tmpDir, "gastown", "crew")
	unlistablePath(t, crewDir)

	town := newDoctorTown()
	town.db(filepath.Join(tmpDir, "gastown", "mayor", "rig")).Seed(beads.Issue{
		ID:     "gt-gastown-crew-sloan",
		Title:  "sloan",
		Type:   "agent",
		Labels: []string{"gt:agent"},
	})

	check := NewStaleAgentBeadsCheck()
	ctx := town.ctx(tmpDir, "")

	result := check.Run(ctx)
	for _, detail := range result.Details {
		if detail == "gt-gastown-crew-sloan" {
			t.Fatalf("unreadable crew directory marked live crew member stale; details: %v", result.Details)
		}
	}
	if result.Status == StatusOK {
		t.Errorf("an unreadable crew directory must be reported, got OK: %s", result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}
	if updates := town.updates(); len(updates) != 0 {
		t.Errorf("Fix() closed beads for a rig it could not read: %v", updates)
	}
}

// TestStaleAgentBeadsUnlistableRigCountsRigs verifies that the "not checked"
// count names rigs, not read errors. A rig whose crew and polecats directories
// are both unreadable is one skipped rig, and the message must say so.
func TestStaleAgentBeadsUnlistableRigCountsRigs(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n" +
		`{"prefix":"bd-","path":"beads/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	for _, rig := range []string{"gastown", "beads"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, rig, "mayor", "rig", ".beads"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// gastown: both worker directories unreadable, so it is one skipped rig.
	unlistablePath(t, filepath.Join(tmpDir, "gastown", "crew"))
	unlistablePath(t, filepath.Join(tmpDir, "gastown", "polecats"))

	// beads: readable and empty, with one agent bead for a removed crew
	// member, so the check reports a stale bead alongside the skipped rig.
	if err := os.MkdirAll(filepath.Join(tmpDir, "beads", "crew"), 0755); err != nil {
		t.Fatal(err)
	}

	town := newDoctorTown()
	town.db(filepath.Join(tmpDir, "beads", "mayor", "rig")).Seed(beads.Issue{
		ID:     "bd-beads-crew-ghost",
		Title:  "ghost",
		Type:   "agent",
		Labels: []string{"gt:agent"},
	})

	check := NewStaleAgentBeadsCheck()
	result := check.Run(town.ctx(tmpDir, ""))

	if len(result.Details) != 1 || result.Details[0] != "bd-beads-crew-ghost" {
		t.Fatalf("expected only the removed crew bead as stale, got details: %v", result.Details)
	}
	want := "1 stale agent bead(s) for removed workers; 1 rig(s) not checked"
	if !strings.HasPrefix(result.Message, want) {
		t.Errorf("message = %q, want prefix %q: two unreadable directories in one rig are one rig", result.Message, want)
	}
}

// failingBeads is a bead store where the selected agent bead read fails, as
// a store whose Dolt server is unreachable does.
type failingBeads struct {
	doctorBeads
	listErr       error
	wispsErr      error
	agentBeadsErr error
}

func (f failingBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if opts.Ephemeral && f.wispsErr != nil {
		return nil, f.wispsErr
	}
	return f.doctorBeads.List(opts)
}

func (f failingBeads) ListAgentBeads() (map[string]*beads.Issue, error) {
	if f.agentBeadsErr != nil {
		return nil, f.agentBeadsErr
	}
	return f.doctorBeads.ListAgentBeads()
}

// TestStaleAgentBeadsCheckUnreadableBeadListIsNotOK verifies that a bead list
// that fails to answer is reported as a warning naming the store, not read as
// an empty list. Before gt-szdhc the check swallowed every query error and
// reported StatusOK during a Dolt outage.
func TestStaleAgentBeadsCheckUnreadableBeadListIsNotOK(t *testing.T) {
	t.Parallel()
	errDown := errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")

	tests := []struct {
		name     string
		failRig  func(*failingBeads) // the rig store's failing call
		failTown func(*failingBeads) // the town store's failing call
		wantIn   string              // a phrase the message must name
	}{
		{
			name:    "rig bead list",
			failRig: func(f *failingBeads) { f.listErr = errDown },
			wantIn:  "gastown: " + errDown.Error(),
		},
		{
			name:    "rig wisp bead list",
			failRig: func(f *failingBeads) { f.wispsErr = errDown },
			wantIn:  "gastown: " + errDown.Error(),
		},
		{
			name:     "town bead list",
			failTown: func(f *failingBeads) { f.agentBeadsErr = errDown },
			wantIn:   "town beads: " + errDown.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()

			beadsDir := filepath.Join(tmpDir, ".beads")
			if err := os.MkdirAll(beadsDir, 0755); err != nil {
				t.Fatal(err)
			}
			routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
			if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
				t.Fatal(err)
			}
			rigDir := filepath.Join(tmpDir, "gastown", "mayor", "rig")
			for _, dir := range []string{filepath.Join(rigDir, ".beads"), filepath.Join(tmpDir, "gastown", "crew"), filepath.Join(tmpDir, "gastown", "polecats")} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}

			town := newDoctorTown()
			ctx := town.ctx(tmpDir, "")
			open := ctx.openBeads
			ctx.openBeads = func(site beadsSite) doctorBeads {
				db := failingBeads{doctorBeads: open(site)}
				switch site.workDir {
				case rigDir:
					if tt.failRig != nil {
						tt.failRig(&db)
					}
				case beadsDir:
					if tt.failTown != nil {
						tt.failTown(&db)
					}
				}
				return db
			}

			check := NewStaleAgentBeadsCheck()
			result := check.Run(ctx)

			if result.Status != StatusWarning {
				t.Errorf("status = %v, want StatusWarning when a bead list cannot be read: %s", result.Status, result.Message)
			}
			if !strings.Contains(result.Message, tt.wantIn) {
				t.Errorf("message = %q, want it to name %q", result.Message, tt.wantIn)
			}
			if len(result.Details) != 0 {
				t.Errorf("details = %v, want none: a failed query names no bead", result.Details)
			}

			if err := check.Fix(ctx); err != nil {
				t.Fatalf("Fix() returned error: %v", err)
			}
			if updates := town.updates(); len(updates) != 0 {
				t.Errorf("Fix() closed beads it could not see: %v", updates)
			}
		})
	}
}

// TestStaleAgentBeadsCheckWispErrorLeavesRigOut verifies that a rig whose wisp
// read fails contributes no stale beads, even the ones its bead list did return.
// The rig's list is partial, so the check must not act on part of it; the
// failed read is reported, and Fix closes nothing for that rig.
func TestStaleAgentBeadsCheckWispErrorLeavesRigOut(t *testing.T) {
	t.Parallel()
	errDown := errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(tmpDir, "gastown", "mayor", "rig")
	for _, dir := range []string{filepath.Join(rigDir, ".beads"), filepath.Join(tmpDir, "gastown", "crew"), filepath.Join(tmpDir, "gastown", "polecats")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// The crew member's bead is listed, but the wisp read that would have
	// completed the rig's list fails.
	town := newDoctorTown()
	town.db(rigDir).Seed(beads.Issue{
		ID:     "gt-gastown-crew-ghost",
		Title:  "ghost",
		Type:   "agent",
		Labels: []string{"gt:agent"},
	})
	ctx := town.ctx(tmpDir, "")
	open := ctx.openBeads
	ctx.openBeads = func(site beadsSite) doctorBeads {
		db := failingBeads{doctorBeads: open(site)}
		if site.workDir == rigDir {
			db.wispsErr = errDown
		}
		return db
	}

	check := NewStaleAgentBeadsCheck()
	result := check.Run(ctx)

	if len(result.Details) != 0 {
		t.Errorf("details = %v, want none: a rig with a failed wisp read is partial and must not be acted on", result.Details)
	}
	if result.Status != StatusWarning {
		t.Errorf("status = %v, want StatusWarning when a wisp read fails: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "gastown: "+errDown.Error()) {
		t.Errorf("message = %q, want it to name the failed wisp read", result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}
	if updates := town.updates(); len(updates) != 0 {
		t.Errorf("Fix() closed beads from a rig with a partial list: %v", updates)
	}
}

// TestStaleAgentBeadsCheckWispsThroughSeam covers the wisp read the check makes
// through the Client seam, the read a tracker rig answers. Each wisp is one
// classification case: the check must flag the stale agent wisps (labelled,
// unlabelled by ID shape, and typed as agent) and leave the live, closed and
// non-agent ones alone, with no query error recorded.
func TestStaleAgentBeadsCheckWispsThroughSeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		wisp      beads.Issue
		onDisk    bool // the worker has a directory, so its bead is not stale
		wantStale bool
	}{
		{name: "live labelled polecat wisp", wisp: beads.Issue{ID: "gt-gastown-polecat-toast", Status: "hooked", Labels: []string{"gt:agent"}, Ephemeral: true}, onDisk: true},
		{name: "stale labelled polecat wisp", wisp: beads.Issue{ID: "gt-gastown-polecat-ghost", Status: "open", Labels: []string{"gt:agent"}, Ephemeral: true}, wantStale: true},
		{name: "closed labelled polecat wisp", wisp: beads.Issue{ID: "gt-gastown-polecat-spent", Status: "closed", Labels: []string{"gt:agent"}, Ephemeral: true}},
		{name: "stale polecat wisp by ID shape only", wisp: beads.Issue{ID: "gt-gastown-polecat-bare", Status: "open", Ephemeral: true}, wantStale: true},
		{name: "stale polecat wisp by type only", wisp: beads.Issue{ID: "gt-gastown-polecat-typed", Status: "open", Type: "agent", Ephemeral: true}, wantStale: true},
		{name: "non-agent wisp", wisp: beads.Issue{ID: "gt-wisp-plain", Status: "open", Labels: []string{"gt:work"}, Type: "task", Ephemeral: true}},
	}

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(tmpDir, "gastown", "mayor", "rig")
	for _, dir := range []string{filepath.Join(rigDir, ".beads"), filepath.Join(tmpDir, "gastown", "crew"), filepath.Join(tmpDir, "gastown", "polecats")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	town := newDoctorTown()
	var wantStale []string
	for _, tt := range tests {
		town.db(rigDir).Seed(tt.wisp)
		if tt.onDisk {
			if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "polecats", strings.TrimPrefix(tt.wisp.ID, "gt-gastown-polecat-")), 0755); err != nil {
				t.Fatal(err)
			}
		}
		if tt.wantStale {
			wantStale = append(wantStale, tt.wisp.ID)
		}
	}
	ctx := town.ctx(tmpDir, "")

	check := NewStaleAgentBeadsCheck()
	result := check.Run(ctx)

	if strings.Contains(result.Message, "could not read") {
		t.Errorf("message = %q, want no query error when the wisp read answers", result.Message)
	}
	got := append([]string(nil), result.Details...)
	sort.Strings(got)
	sort.Strings(wantStale)
	if !reflect.DeepEqual(got, wantStale) {
		t.Errorf("stale details = %v, want %v", got, wantStale)
	}
}

// TestStaleAgentBeadsCheckStaleWithUnreadableBeadList verifies the message when
// a stale bead is found and the town bead list also fails: the stale count and
// the failed query are both reported, so neither hides the other.
func TestStaleAgentBeadsCheckStaleWithUnreadableBeadList(t *testing.T) {
	t.Parallel()
	errDown := errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(tmpDir, "gastown", "mayor", "rig")
	for _, dir := range []string{filepath.Join(rigDir, ".beads"), filepath.Join(tmpDir, "gastown", "crew"), filepath.Join(tmpDir, "gastown", "polecats")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// The rig's crew directory is readable and empty, so the listed crew bead
	// is stale. The town list fails, so Phase 2 cannot run.
	town := newDoctorTown()
	town.db(rigDir).Seed(beads.Issue{
		ID:     "gt-gastown-crew-ghost",
		Title:  "ghost",
		Type:   "agent",
		Labels: []string{"gt:agent"},
	})
	ctx := town.ctx(tmpDir, "")
	open := ctx.openBeads
	ctx.openBeads = func(site beadsSite) doctorBeads {
		db := failingBeads{doctorBeads: open(site)}
		if site.workDir == beadsDir {
			db.agentBeadsErr = errDown
		}
		return db
	}

	check := NewStaleAgentBeadsCheck()
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("status = %v, want StatusWarning: %s", result.Status, result.Message)
	}
	if len(result.Details) != 1 || result.Details[0] != "gt-gastown-crew-ghost" {
		t.Errorf("details = %v, want only the stale crew bead", result.Details)
	}
	for _, want := range []string{
		"1 stale agent bead(s) for removed workers",
		"could not read agent bead lists: town beads: " + errDown.Error(),
	} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message = %q, want it to contain %q", result.Message, want)
		}
	}
}

// === Tests for Phase 2: Deregistered rig orphan detection ===

func TestStaleAgentBeadsCheck_Phase2_NoTownBeadsDir(t *testing.T) {
	t.Parallel()
	// Phase 2 scans town beads for orphan agent beads. When the .beads
	// dir exists (for routes) but has no Dolt connection, the check should
	// handle ListAgentBeads failure gracefully and not crash.
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Routes with one known rig + town-level route
	routesContent := `{"prefix":"hq-","path":"."}` + "\n" +
		`{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create the rig beads directory so Phase 1 can attempt to scan
	rigBeadsDir := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	// Should not crash — Phase 2 ListAgentBeads will fail without Dolt. That
	// failure is a warning, not a clean result: the scan found nothing
	// because it could not run (gt-szdhc).
	result := check.Run(ctx)
	t.Logf("Phase 2 no-dolt check: status=%v, message=%s", result.Status, result.Message)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning when the town bead list cannot be read, got %v: %s", result.Status, result.Message)
	}
}

// TestStaleAgentBeadsCheck_Phase2_UnlistableWorkerPath verifies that a town
// agent bead whose rig has a worker directory the check cannot read is
// reported as a skipped rig, never closed. Phase 2 must not read the read
// error as an empty rig (gt-3zirf finding d9223200a1d2); the bead lives only
// in the town database, so only Phase 2 can mark it stale.
func TestStaleAgentBeadsCheck_Phase2_UnlistableWorkerPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		dir    string // worker directory that cannot be read, under the rig
		beadID string
		title  string
	}{
		{name: "crew", dir: "crew", beadID: "gt-gastown-crew-sloan", title: "sloan"},
		{name: "polecat", dir: "polecats", beadID: "gt-gastown-polecat-nux", title: "nux"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()

			beadsDir := filepath.Join(tmpDir, ".beads")
			if err := os.MkdirAll(beadsDir, 0755); err != nil {
				t.Fatal(err)
			}
			routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
			if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
				t.Fatal(err)
			}
			unlistablePath(t, filepath.Join(tmpDir, "gastown", tt.dir))

			// The worker's agent bead is in the town database, where Phase 2
			// looks, not in the rig's.
			town := newDoctorTown()
			town.db(beadsDir).Seed(beads.Issue{
				ID:     tt.beadID,
				Title:  tt.title,
				Type:   "agent",
				Labels: []string{"gt:agent"},
			})

			check := NewStaleAgentBeadsCheck()
			ctx := town.ctx(tmpDir, "")

			result := check.Run(ctx)
			for _, detail := range result.Details {
				if detail == tt.beadID {
					t.Fatalf("unreadable %s directory marked live agent stale; details: %v", tt.dir, result.Details)
				}
			}
			if result.Status == StatusOK {
				t.Errorf("an unreadable %s directory must be reported, got OK: %s", tt.dir, result.Message)
			}

			if err := check.Fix(ctx); err != nil {
				t.Fatalf("Fix() returned error: %v", err)
			}
			if updates := town.updates(); len(updates) != 0 {
				t.Errorf("Fix() closed beads for a rig it could not read: %v", updates)
			}
		})
	}
}

func TestStaleAgentBeadsCheck_KnownPrefixTracking(t *testing.T) {
	t.Parallel()
	// Verify that both town-level routes (path ".") and rig routes are
	// tracked in the knownPrefixes map. Phase 2 uses this to identify
	// orphan beads from deregistered rigs.
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Mix of town-level and rig-level routes
	routesContent := `{"prefix":"hq-","path":"."}` + "\n" +
		`{"prefix":"hq-cv-","path":"."}` + "\n" +
		`{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n" +
		`{"prefix":"bd-","path":"beads/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rig directories
	for _, rigPath := range []string{"gastown/mayor/rig/.beads", "beads/mayor/rig/.beads"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, rigPath), 0755); err != nil {
			t.Fatal(err)
		}
	}

	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	// Run should complete without crash — verifies knownPrefixes includes
	// both town-level ("hq", "hq-cv") and rig-level ("gt", "bd") prefixes
	result := check.Run(ctx)
	t.Logf("Known prefix tracking: status=%v, message=%s", result.Status, result.Message)
}

// === Tests for parseCrewOrPolecatFromID helper ===

func TestParseCrewOrPolecatFromID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		id         string
		prefix     string
		rigName    string
		role       string
		wantWorker string
	}{
		{
			name:       "standard crew ID",
			id:         "gt-gastown-crew-alice",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "crew",
			wantWorker: "alice",
		},
		{
			name:       "standard polecat ID",
			id:         "gt-gastown-polecat-nux",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "polecat",
			wantWorker: "nux",
		},
		{
			name:       "collapsed form (prefix == rigName)",
			id:         "ff-crew-joe",
			prefix:     "ff",
			rigName:    "ff",
			role:       "crew",
			wantWorker: "joe",
		},
		{
			name:       "collapsed polecat (prefix == rigName)",
			id:         "ff-polecat-toast",
			prefix:     "ff",
			rigName:    "ff",
			role:       "polecat",
			wantWorker: "toast",
		},
		{
			name:       "different prefix and rigName",
			id:         "sh-shippercrm-crew-controllers",
			prefix:     "sh",
			rigName:    "shippercrm",
			role:       "crew",
			wantWorker: "controllers",
		},
		{
			name:       "ID does not match pattern",
			id:         "gt-gastown-polecat-toast",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "crew",
			wantWorker: "",
		},
		{
			name:       "wrong prefix",
			id:         "bd-beads-crew-human",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "crew",
			wantWorker: "",
		},
		{
			name:       "empty worker name after prefix strip",
			id:         "gt-gastown-crew-",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "crew",
			wantWorker: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := parseCrewOrPolecatFromID(tt.id, tt.prefix, tt.rigName, tt.role)
			if worker != tt.wantWorker {
				t.Errorf("worker = %q, want %q", worker, tt.wantWorker)
			}
		})
	}
}

// === Tests for dedup helper ===

func TestDedup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "empty",
			input: []string{},
			want:  []string{},
		},
		{
			name:  "nil",
			input: nil,
			want:  nil,
		},
		{
			name:  "no duplicates",
			input: []string{"a", "b", "c"},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "all duplicates",
			input: []string{"a", "a", "a"},
			want:  []string{"a"},
		},
		{
			name:  "mixed duplicates preserving order",
			input: []string{"c", "a", "b", "a", "c", "d"},
			want:  []string{"c", "a", "b", "d"},
		},
		{
			name:  "adjacent duplicates",
			input: []string{"x", "x", "y", "y", "z"},
			want:  []string{"x", "y", "z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedup(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("dedup(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// === Test for Fix() fallback to town beads ===

func TestStaleAgentBeadsCheck_FixFallbackToTownBeads(t *testing.T) {
	t.Parallel()
	// When Fix() encounters an orphan bead with unknown prefix, it should
	// attempt to close via town beads client instead of silently skipping.
	// Without Dolt, this verifies the code path doesn't crash.
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"hq-","path":"."}` + "\n" +
		`{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	rigBeadsDir := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	// Fix should not crash even when Run() reports no stale beads
	err := check.Fix(ctx)
	if err != nil {
		t.Errorf("Fix() returned unexpected error: %v", err)
	}
	t.Logf("Fix() with no stale beads completed without error")
}
