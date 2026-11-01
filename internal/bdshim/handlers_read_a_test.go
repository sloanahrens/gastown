package bdshim

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdshim/golden"
	"github.com/steveyegge/gastown/internal/beads"
)

// The read handlers' tests answer each verb through Main, against the fixture
// the goldens were recorded from, and compare what bd printed with what the
// handler prints after the same normalization the differential test applies.
// TestDifferentialGoldens replays every golden; these tests name each verb's
// rows, so a verb whose handler is removed fails here by name.

// TestHandlersShow answers every show golden row.
func TestHandlersShow(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "show")
}

// TestHandlersList answers every list golden row.
func TestHandlersList(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "list")
}

// TestHandlersChildren answers every children golden row.
func TestHandlersChildren(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "children")
}

// testHandlerRows runs every golden row of verb through the shim and compares
// its answer and exit code to the golden. One case per row, named by the
// golden's file stem, so a drifting row is named.
func testHandlerRows(t *testing.T, verb string) {
	t.Helper()
	fixture, keep := readFixture(t)
	town := newTown(t, routeFixture)
	rows := 0
	for _, row := range differentialRows {
		if verbOf(row.argv) != verb {
			continue
		}
		rows++
		t.Run(row.stem, func(t *testing.T) {
			want, wantExit, err := readGolden(goldenDirectory, row.stem)
			if err != nil {
				t.Fatalf("reading the golden: %v", err)
			}
			got, exit, err := runFixtureRow(t, town, fixture, row.argv)
			if err != nil {
				t.Fatalf("%v: %v", row.argv, err)
			}
			got = golden.Normalize(got, keep)
			if !bytes.Equal(got, want) {
				t.Errorf("%v: stdout differs from bd's answer\n%s", row.argv, lineDiff(want, got))
			}
			if exit != wantExit {
				t.Errorf("%v: exit code is %d, want bd's %d", row.argv, exit, wantExit)
			}
		})
	}
	if rows == 0 {
		t.Fatalf("no %s golden row to answer", verb)
	}
}

// TestHandlersRefuseAnUnknownFlag: a flag a golden row does not cover is
// refused by name, never ignored, so a caller cannot read an unfiltered answer
// as a filtered one.
func TestHandlersRefuseAnUnknownFlag(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		flag string
	}{
		{name: "show refuses --children", argv: []string{"show", "--json", "--children", "fx-parent"}, flag: "--children"},
		{name: "list refuses --assignee", argv: []string{"list", "--json", "--assignee", "sloan"}, flag: "--assignee"},
		{name: "children refuses --format", argv: []string{"children", "--json", "--format", "tree", "fx-parent"}, flag: "--format"},
		{name: "list refuses a global flag before the verb", argv: []string{"--allow-stale", "list", "--json"}, flag: "--allow-stale"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.flag) {
				t.Errorf("%v: stderr = %q, want it to name %s", tc.argv, stderr.String(), tc.flag)
			}
		})
	}
}

// TestHandlersRefuseAnAnswerTheyDoNotPrint: a read the goldens record as JSON
// is refused without --json rather than printed in a shape bd did not record.
func TestHandlersRefuseAnAnswerTheyDoNotPrint(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "show", argv: []string{"show", "fx-1"}, want: "show without --json"},
		{name: "list", argv: []string{"list"}, want: "list without --json"},
		{name: "children", argv: []string{"children", "fx-parent"}, want: "children without --json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to hold %q", tc.argv, stderr.String(), tc.want)
			}
		})
	}
}

// TestHandlersRefuseOperandsTheyDoNotRead: each verb's usage names its
// operands, and one the handler cannot answer is refused rather than ignored.
func TestHandlersRefuseOperandsTheyDoNotRead(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "show with no id", argv: []string{"show", "--json"}, want: "show needs an issue id"},
		{name: "list with an operand", argv: []string{"list", "--json", "fx-1"}, want: "list takes no operands"},
		{name: "children with no parent", argv: []string{"children", "--json"}, want: "children needs one parent id"},
		{name: "children with two parents", argv: []string{"children", "--json", "fx-parent", "fx-2"}, want: "children needs one parent id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to hold %q", tc.argv, stderr.String(), tc.want)
			}
		})
	}
}

// TestHandlersRefuseAListFilterTheyCannotBuild: a flag combination or value the
// handler cannot turn into the store's filter is refused, not given one flag's
// meaning.
func TestHandlersRefuseAListFilterTheyCannotBuild(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "--all with --status", argv: []string{"list", "--json", "--all", "--status", "open"}, want: "name one of them"},
		{name: "--limit that is not a number", argv: []string{"list", "--json", "--limit", "many"}, want: "--limit"},
		{name: "--limit below zero", argv: []string{"list", "--json", "--limit=-1"}, want: "--limit"},
		{name: "a value flag with no value", argv: []string{"list", "--json", "--label"}, want: "--label needs a value"},
		{name: "a value on a boolean flag", argv: []string{"list", "--json=true"}, want: "--json takes no value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to hold %q", tc.argv, stderr.String(), tc.want)
			}
		})
	}
}

// TestHandlersRefuseAStoreTheyCannotOpen: a tracker rig whose connection
// settings are missing fails loudly instead of answering from nowhere.
func TestHandlersRefuseAStoreTheyCannotOpen(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	env, stdout, stderr := fixtureEnv(t, town, fixture, []string{"show", "--json", "fx-1"})
	// The seat has no tracker endpoint: backend.Open must refuse rather than
	// fall back to another store.
	env.Getenv = func(string) string { return "" }
	if code := Main(env); code != 1 {
		t.Errorf("show with no TRACKER_URL: exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "TRACKER_URL") {
		t.Errorf("show with no TRACKER_URL: stderr = %q, want it to name TRACKER_URL", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("show with no TRACKER_URL: wrote %q, want nothing", stdout.String())
	}
}

// TestHandlersRefuseATownWithNoRouteTable drives the store opener from a
// handler: a working directory with no route table above it cannot name the
// rig's beads directory, so the handler refuses.
func TestHandlersRefuseATownWithNoRouteTable(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	env := Env{
		Args:   []string{"show", "--json", "fx-1"},
		Getenv: func(string) string { return "" },
		Getwd:  func() (string, error) { return t.TempDir(), nil },
		Stdout: &bytes.Buffer{}, Stderr: stderr,
	}
	code := showHandler(env, Decision{Route: beads.Route{Prefix: "fx-"}})
	if code != 1 {
		t.Errorf("show with no route table: exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no route table") {
		t.Errorf("stderr = %q, want it to say no route table was found", stderr.String())
	}
}

// TestHandlersRefuseAWorkingDirectoryTheyCannotRead: a handler that cannot
// learn where it is refuses, rather than opening a store the working directory
// did not name.
func TestHandlersRefuseAWorkingDirectoryTheyCannotRead(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	env := Env{
		Args:   []string{"list", "--json"},
		Getenv: func(string) string { return "" },
		Getwd:  func() (string, error) { return "", errors.New("no working directory") },
		Stdout: &bytes.Buffer{}, Stderr: stderr,
	}
	if code := listHandler(env, Decision{Route: beads.Route{Prefix: "fx-"}}); code != 1 {
		t.Errorf("list without a working directory: exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "working directory") {
		t.Errorf("stderr = %q, want it to name the working directory", stderr.String())
	}
}

// TestBDShowRowRefusesADependencyWithNoTarget: bd prints a dependency's target
// fields, so an answer without the target would print a bead the store never
// described. The branch is defensive — every dependency the tracker returns
// names a bead it holds — so the row builder is driven directly.
func TestBDShowRowRefusesADependencyWithNoTarget(t *testing.T) {
	t.Parallel()
	is := &beads.Issue{ID: "fx-1", Dependencies: []beads.IssueDep{{ID: "fx-2", DependencyType: "blocks"}}}
	if _, err := bdShowRow(is, map[string]*beads.Issue{}, 0); err == nil {
		t.Fatal("bdShowRow passed a dependency whose target the store did not answer")
	}
}

// TestPrintBDAnswerReportsAWriteFailure: a failed write is the shim's error,
// not a silent success.
func TestPrintBDAnswerReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	if err := printBDAnswer(failingWriter{}, bdListAnswer{}); err == nil {
		t.Fatal("printBDAnswer passed a write that failed")
	}
}

// failingWriter is an output stream that refuses every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

// TestPrintBDAnswerReportsAValueItCannotRender: an answer the printer cannot
// marshal is the shim's error, not a half-written answer.
func TestPrintBDAnswerReportsAValueItCannotRender(t *testing.T) {
	t.Parallel()
	if err := printBDAnswer(failingWriter{}, make(chan int)); err == nil {
		t.Fatal("printBDAnswer passed a value it could not render")
	}
}

// TestShowRowReportsEachStoreFailure: every read a show row makes can fail, and
// each failure is reported with the issue it was reading.
func TestShowRowReportsEachStoreFailure(t *testing.T) {
	t.Parallel()
	withDep := &beads.Issue{ID: "fx-1", Dependencies: []beads.IssueDep{{ID: "fx-2", DependencyType: "blocks"}}}
	tests := []struct {
		name  string
		store stubReadStore
		want  string
	}{
		{
			name:  "the issue cannot be read",
			store: stubReadStore{show: func(string) (*beads.Issue, error) { return nil, errors.New("read refused") }},
			want:  "show fx-1: read refused",
		},
		{
			name: "the beads it depends on cannot be read",
			store: stubReadStore{
				show:         func(string) (*beads.Issue, error) { return withDep, nil },
				showMultiple: func([]string) (map[string]*beads.Issue, error) { return nil, errors.New("targets refused") },
			},
			want: "reading the beads it depends on: targets refused",
		},
		{
			name: "its comments cannot be read",
			store: stubReadStore{
				show:         func(string) (*beads.Issue, error) { return withDep, nil },
				showMultiple: func([]string) (map[string]*beads.Issue, error) { return map[string]*beads.Issue{}, nil },
				comments:     func(string) ([]beads.Comment, error) { return nil, errors.New("comments refused") },
			},
			want: "reading its comments: comments refused",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := showRow(tc.store, "fx-1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("showRow = %v, want it to hold %q", err, tc.want)
			}
		})
	}
}

// TestListRowsReportsEachStoreFailure: the re-read and the comment count a list
// row makes can each fail, and each failure names the issue.
func TestListRowsReportsEachStoreFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		store stubReadStore
		want  string
	}{
		{
			name:  "the issue cannot be read again",
			store: stubReadStore{show: func(string) (*beads.Issue, error) { return nil, errors.New("read refused") }},
			want:  "reading fx-1: read refused",
		},
		{
			name: "its comments cannot be read",
			store: stubReadStore{
				show:     func(id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil },
				comments: func(string) ([]beads.Comment, error) { return nil, errors.New("comments refused") },
			},
			want: "reading fx-1's comments: comments refused",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := listRows(tc.store, []*beads.Issue{{ID: "fx-1"}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("listRows = %v, want it to hold %q", err, tc.want)
			}
		})
	}
}

// TestHandlersRefuseAWorkingDirectoryBelowANonDirectoryBeads drives the town
// walk: a .beads that is a file names no store, so the handler refuses rather
// than walking past a directory that could hold the route table.
func TestHandlersRefuseAWorkingDirectoryBelowANonDirectoryBeads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".beads"), "not a store")
	stderr := &bytes.Buffer{}
	env := Env{
		Args:   []string{"show", "--json", "fx-1"},
		Getenv: func(string) string { return "" },
		Getwd:  func() (string, error) { return dir, nil },
		Stdout: &bytes.Buffer{}, Stderr: stderr,
	}
	if code := showHandler(env, Decision{Route: beads.Route{Prefix: "fx-"}}); code != 1 {
		t.Errorf("show below a .beads file: exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not a directory") {
		t.Errorf("stderr = %q, want it to say .beads is not a directory", stderr.String())
	}
}

// stubReadStore is a store whose reads a test scripts. A read the test did not
// script refuses, except ShowMultiple, which answers no beads; that is enough
// for the failure paths a test drives.
type stubReadStore struct {
	show         func(string) (*beads.Issue, error)
	showMultiple func([]string) (map[string]*beads.Issue, error)
	list         func(beads.ListOptions) ([]*beads.Issue, error)
	children     func(string) ([]*beads.Issue, error)
	comments     func(string) ([]beads.Comment, error)
}

func (s stubReadStore) Show(id string) (*beads.Issue, error) {
	if s.show == nil {
		return nil, errors.New("stubReadStore: Show was not scripted")
	}
	return s.show(id)
}

func (s stubReadStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.showMultiple == nil {
		return map[string]*beads.Issue{}, nil
	}
	return s.showMultiple(ids)
}

func (s stubReadStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if s.list == nil {
		return nil, errors.New("stubReadStore: List was not scripted")
	}
	return s.list(opts)
}

func (s stubReadStore) Children(parentID string) ([]*beads.Issue, error) {
	if s.children == nil {
		return nil, errors.New("stubReadStore: Children was not scripted")
	}
	return s.children(parentID)
}

func (s stubReadStore) Comments(id string) ([]beads.Comment, error) {
	if s.comments == nil {
		return nil, errors.New("stubReadStore: Comments was not scripted")
	}
	return s.comments(id)
}
