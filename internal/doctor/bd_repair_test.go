package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// repairCall is one bd repair verb a fixer asked for.
type repairCall struct {
	dir  string
	verb string
	id   string
}

// fakeRepairer records the bd repair verbs doctor fixers call
// (CheckContext.openRepair).
type fakeRepairer struct {
	mu    sync.Mutex
	calls []repairCall
	fail  map[string]error
}

type fakeRepairTarget struct {
	f   *fakeRepairer
	dir string
}

func (f *fakeRepairer) open(dir string) bdRepairer { return fakeRepairTarget{f: f, dir: filepath.Clean(dir)} }

func (t fakeRepairTarget) record(verb, id string) error {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.calls = append(t.f.calls, repairCall{dir: t.dir, verb: verb, id: id})
	return t.f.fail[id]
}

func (t fakeRepairTarget) DemoteToWisp(id string) error     { return t.record("demote", id) }
func (t fakeRepairTarget) ReopenUnassigned(id string) error { return t.record("reopen", id) }
func (t fakeRepairTarget) Update(id string, opts beads.UpdateOptions) error {
	return t.record("update:"+strings.Join(opts.AddLabels, ","), id)
}

func (f *fakeRepairer) sorted() []repairCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]repairCall(nil), f.calls...)
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// TestNullAssigneeFixReopensThroughBd (gt-fcxe9.12): the repair is one bd
// update per affected id, in that id's database, and no SQL write.
func TestNullAssigneeFixReopensThroughBd(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	bd := newFakeBD()
	rep := &fakeRepairer{}
	ctx := bd.ctx(town)
	ctx.openRepair = rep.open

	c := NewNullAssigneeCheck()
	c.affected = []nullAssigneeRow{{ID: "gt-a", RigDB: "gastown"}, {ID: "hq-b", RigDB: "hq"}}
	if err := c.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	want := []repairCall{
		{dir: filepath.Join(town, "gastown"), verb: "reopen", id: "gt-a"},
		{dir: filepath.Join(town, "hq"), verb: "reopen", id: "hq-b"},
	}
	if got := rep.sorted(); !reflect.DeepEqual(got, want) {
		t.Errorf("repairs = %+v, want %+v", got, want)
	}
	for dir, db := range bd.dbs {
		if stmts := db.SQLStatements(); len(stmts) != 0 {
			t.Errorf("Fix ran SQL in %s: %q", dir, stmts)
		}
	}
}

// TestNullAssigneeFixReportsEachFailure: a refused update is named in the
// error, and the other ids are still repaired.
func TestNullAssigneeFixReportsEachFailure(t *testing.T) {
	t.Parallel()
	bd := newFakeBD()
	rep := &fakeRepairer{fail: map[string]error{"gt-a": errors.New("refused")}}
	ctx := bd.ctx(t.TempDir())
	ctx.openRepair = rep.open

	c := NewNullAssigneeCheck()
	c.affected = []nullAssigneeRow{{ID: "gt-a", RigDB: "gastown"}, {ID: "gt-b", RigDB: "gastown"}}
	err := c.Fix(ctx)
	if err == nil || !strings.Contains(err.Error(), "gt-a") {
		t.Fatalf("Fix error = %v, want one naming gt-a", err)
	}
	if n := len(rep.sorted()); n != 2 {
		t.Errorf("Fix attempted %d repairs, want 2", n)
	}
}

// TestMisclassifiedWispFixDemotesThroughBd (gt-fcxe9.12): moving a row from
// issues to wisps is bd's DemoteToWisp (bd update --ephemeral), which carries
// labels, comments, events and dependencies; doctor issues no SQL.
func TestMisclassifiedWispFixDemotesThroughBd(t *testing.T) {
	t.Parallel()
	bd := newFakeBD()
	rep := &fakeRepairer{}
	ctx := bd.ctx(t.TempDir())
	ctx.openRepair = rep.open
	rigDir := t.TempDir()

	c := NewCheckMisclassifiedWisps()
	c.misclassified = []misclassifiedWisp{
		{rigName: "gt", workDir: rigDir, id: "gt-wisp-a"},
		{rigName: "gt", workDir: rigDir, id: "gt-wisp-b"},
	}
	if err := c.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	want := []repairCall{
		{dir: filepath.Clean(rigDir), verb: "demote", id: "gt-wisp-a"},
		{dir: filepath.Clean(rigDir), verb: "demote", id: "gt-wisp-b"},
	}
	if got := rep.sorted(); !reflect.DeepEqual(got, want) {
		t.Errorf("repairs = %+v, want %+v", got, want)
	}
	for dir, db := range bd.dbs {
		if stmts := db.SQLStatements(); len(stmts) != 0 {
			t.Errorf("Fix ran SQL in %s: %q", dir, stmts)
		}
	}
}

// TestDoctorSourceIssuesNoSQLWrites: no doctor check writes a bd table with
// SQL (gt-fcxe9.12, ADR 0001). Reads through bd sql stay allowed.
func TestDoctorSourceIssuesNoSQLWrites(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	write := regexp.MustCompile(`(?i)\b(UPDATE\s+\w+\s+SET|DELETE\s+FROM|INSERT\s+(IGNORE\s+)?INTO|REPLACE\s+INTO|CREATE\s+TABLE|DROP\s+TABLE|DOLT_COMMIT|DOLT_ADD|CommitServerWorkingSet)\b`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if m := write.FindString(line); m != "" {
				t.Errorf("%s:%d writes through SQL (%s): %s", f, i+1, m, strings.TrimSpace(line))
			}
		}
	}
}

// TestEnsureAgentLabelRetriesPinnedNeverSQL: when the routed bd update fails
// (a legacy prefix that does not route, GH#2127), the label is retried
// through bd pinned to the rig database; doctor never falls back to an SQL
// INSERT (gt-fcxe9.12), and a label bd cannot add is an error.
func TestEnsureAgentLabelRetriesPinnedNeverSQL(t *testing.T) {
	t.Parallel()
	fbd := newFakeBD()
	workDir := t.TempDir()
	labeled := false
	fbd.db(workDir).OnSQL(func(string) ([][]string, error) {
		if labeled {
			return [][]string{{"1"}, {"1"}}, nil
		}
		return [][]string{{"1"}}, nil
	})
	rep := &fakeRepairer{}
	ctx := fbd.ctx(t.TempDir())
	ctx.openRepair = func(dir string) bdRepairer {
		labeled = true
		return rep.open(dir)
	}
	routed := beads.NewWithBeadsDirAndRunner(workDir, filepath.Join(workDir, ".beads"),
		func(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
			return nil, []byte("Error: issue not found"), errors.New("exit status 1")
		})

	if err := ensureAgentLabel(ctx, routed, workDir, "legacy-x"); err != nil {
		t.Fatalf("ensureAgentLabel: %v", err)
	}
	if got := rep.sorted(); len(got) != 1 || got[0].verb != "update:gt:agent" || got[0].dir != filepath.Clean(workDir) {
		t.Errorf("pinned retries = %+v, want one gt:agent update in %s", got, workDir)
	}
	for _, stmt := range fbd.db(workDir).SQLStatements() {
		if !strings.HasPrefix(strings.TrimSpace(stmt), "SELECT") {
			t.Errorf("ensureAgentLabel wrote SQL: %s", stmt)
		}
	}

	rep.fail = map[string]error{"legacy-y": errors.New("refused")}
	if err := ensureAgentLabel(ctx, routed, workDir, "legacy-y"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("ensureAgentLabel with both bd paths failing = %v, want the pinned refusal", err)
	}
}
