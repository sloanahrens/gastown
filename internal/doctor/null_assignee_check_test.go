package doctor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// The check reads registered rigs only. The Dolt server also hosts databases
// that are no rig: the operator's own tracker (claude) keeps human-owned epics
// in_progress with no assignee, and Fix would reopen them.
func TestNullAssigneeCheck_ScansRegisteredRigsOnly(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, d := range []string{"mayor", "gastown", "claude"} {
		if err := os.MkdirAll(filepath.Join(townRoot, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), `{"rigs":{"gastown":{}}}`)

	bd := newFakeBD()
	stuck := csvAnswer([]string{"id", "title", "updated_at"}, []string{"x-1", "step", "2026-09-30"})
	bd.db(filepath.Join(townRoot, "gastown")).OnSQL(stuck)
	bd.db(filepath.Join(townRoot, "claude")).OnSQL(stuck)

	result := NewNullAssigneeCheck().Run(bd.ctx(townRoot))

	if result.Status != StatusWarning || len(result.Details) != 1 ||
		!strings.HasPrefix(result.Details[0], "[gastown] x-1") {
		t.Errorf("want one gastown finding, got %v %q", result.Status, result.Details)
	}
	for _, o := range bd.opened() {
		if o.dir == filepath.Join(townRoot, "claude") {
			t.Errorf("check queried the unregistered claude database")
		}
	}
}

// routesTown writes a town with the given route lines and registered rigs.
func routesTown(t *testing.T, routes, rigsJSON string) string {
	t.Helper()
	townRoot := writeTownRoutes(t, routes)
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), rigsJSON)
	return townRoot
}

const skippedTrackerLine = "skipped tracker rig trackerrig: the tracker stores no NULL assignee"

// A rig on the tracker is not read: the backend refuses sql and stores no NULL
// assignee, so the run says it skipped the rig rather than reporting a bare OK.
// The fake has a row ready, so a run that read the rig would report it.
func TestNullAssigneeCheck_SkipsTrackerRig(t *testing.T) {
	t.Parallel()
	townRoot := routesTown(t,
		`{"prefix":"tr-","path":"trackerrig","backend":"tracker"}`+"\n",
		`{"rigs":{"trackerrig":{}}}`)

	bd := newFakeBD()
	bd.db(filepath.Join(townRoot, "trackerrig")).OnSQL(csvAnswer(
		[]string{"id", "title", "updated_at"}, []string{"tr-1", "stuck", "2026-09-30"}))

	result := NewNullAssigneeCheck().Run(bd.ctx(townRoot))

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for a town whose only rig is on the tracker", result.Status)
	}
	if !slices.Equal(result.Details, []string{skippedTrackerLine}) {
		t.Errorf("details = %q, want [%q]", result.Details, skippedTrackerLine)
	}
	if !strings.Contains(result.Message, "skipped 1 tracker rig") {
		t.Errorf("message = %q, want it to name the skipped rigs", result.Message)
	}
	for _, o := range bd.opened() {
		if o.dir == filepath.Join(townRoot, "trackerrig") {
			t.Errorf("check read the tracker rig through bd")
		}
	}
}

// A Dolt rig's NULL assignee is still reported next to the skipped tracker rig.
func TestNullAssigneeCheck_ReportsDoltRigNextToSkippedTrackerRig(t *testing.T) {
	t.Parallel()
	townRoot := routesTown(t,
		`{"prefix":"dl-","path":"doltrig"}`+"\n"+
			`{"prefix":"tr-","path":"trackerrig","backend":"tracker"}`+"\n",
		`{"rigs":{"doltrig":{},"trackerrig":{}}}`)

	bd := newFakeBD()
	bd.db(filepath.Join(townRoot, "doltrig")).OnSQL(csvAnswer(
		[]string{"id", "title", "updated_at"}, []string{"dl-1", "step", "2026-09-30"}))
	bd.db(filepath.Join(townRoot, "trackerrig")).OnSQL(csvAnswer(
		[]string{"id", "title", "updated_at"}, []string{"tr-1", "stuck", "2026-09-30"}))

	result := NewNullAssigneeCheck().Run(bd.ctx(townRoot))

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want a warning for the Dolt rig's NULL assignee", result.Status)
	}
	if !slices.Contains(result.Details, "[doltrig] dl-1 — step (updated: 2026-09-30)") {
		t.Errorf("details = %q, want the Dolt rig's finding", result.Details)
	}
	if !slices.Contains(result.Details, skippedTrackerLine) {
		t.Errorf("details = %q, want the skipped tracker line", result.Details)
	}
	if !strings.Contains(result.Message, "skipped 1 tracker rig") {
		t.Errorf("message = %q, want it to name the skipped rigs beside the finding", result.Message)
	}
	for _, o := range bd.opened() {
		if o.dir == filepath.Join(townRoot, "trackerrig") {
			t.Errorf("check read the tracker rig through bd")
		}
	}
}

// With no routes file every registered rig is a Dolt rig, the common town:
// it is read, and a clean answer is OK with no detail.
func TestNullAssigneeCheck_NoRoutesFileReadsDoltRigs(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), `{"rigs":{"gastown":{}}}`)

	bd := newFakeBD()
	bd.db(filepath.Join(townRoot, "gastown")).OnSQL(csvAnswer([]string{"id", "title", "updated_at"}))

	result := NewNullAssigneeCheck().Run(bd.ctx(townRoot))

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for a clean Dolt town", result.Status)
	}
	if len(result.Details) != 0 {
		t.Errorf("details = %q, want none", result.Details)
	}
	if got := bd.db(filepath.Join(townRoot, "gastown")).SQLStatements(); len(got) != 1 {
		t.Errorf("sql statements = %d, want the Dolt rig read once", len(got))
	}
}

// unreadableRoutesTown writes a town whose routes file is a directory, which
// cannot be read as one.
func unreadableRoutesTown(t *testing.T, rigsJSON string) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads", beads.RoutesFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), rigsJSON)
	return townRoot
}

// An unreadable route table hides which rigs the tracker holds, so it must not
// suppress the Dolt scan: every rig is read as before and the result warns.
func TestNullAssigneeCheck_UnreadableRoutesStillReadsDoltRigs(t *testing.T) {
	t.Parallel()
	townRoot := unreadableRoutesTown(t, `{"rigs":{"gastown":{}}}`)

	bd := newFakeBD()
	bd.db(filepath.Join(townRoot, "gastown")).OnSQL(csvAnswer(
		[]string{"id", "title", "updated_at"}, []string{"gt-1", "step", "2026-09-30"}))

	result := NewNullAssigneeCheck().Run(bd.ctx(townRoot))

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want a warning for unreadable routes", result.Status)
	}
	if !slices.Contains(result.Details, "[gastown] gt-1 — step (updated: 2026-09-30)") {
		t.Errorf("details = %q, want the Dolt rig finding", result.Details)
	}
	if !strings.Contains(result.Message, "the town routes could not be read") {
		t.Errorf("message = %q, want it to name the routes problem", result.Message)
	}
	if !slices.ContainsFunc(result.Details, func(d string) bool {
		return strings.HasPrefix(d, "cannot read the town routes")
	}) {
		t.Errorf("details = %q, want a line naming the routes problem", result.Details)
	}
}

// A clean scan over unreadable routes is still not OK: the tracker rigs are
// unverified, and a bare OK would hide that.
func TestNullAssigneeCheck_UnreadableRoutesNeverReportsOK(t *testing.T) {
	t.Parallel()
	townRoot := unreadableRoutesTown(t, `{"rigs":{"gastown":{}}}`)

	bd := newFakeBD()

	result := NewNullAssigneeCheck().Run(bd.ctx(townRoot))

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want a warning for unreadable routes", result.Status)
	}
	if !strings.Contains(result.Message, "the town routes could not be read") {
		t.Errorf("message = %q, want it to name the routes problem", result.Message)
	}
	if opened := bd.opened(); len(opened) != 1 || opened[0].dir != filepath.Join(townRoot, "gastown") {
		t.Errorf("opened = %v, want the Dolt rig read", opened)
	}
}
