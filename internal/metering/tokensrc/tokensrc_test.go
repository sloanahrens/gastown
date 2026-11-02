package tokensrc

import (
	"errors"
	"io/fs"
	"math"
	"path"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/steveyegge/gastown/internal/metering/export"
)

// october is the fixed month every case totals.
var october = export.CalendarMonth{Year: 2026, Month: time.October}

// usageTree builds an fs.FS whose beads directory holds files by base name.
func usageTree(files map[string]string) fstest.MapFS {
	tree := fstest.MapFS{}
	for name, body := range files {
		tree[path.Join(usageDir, name)] = &fstest.MapFile{Data: []byte(body)}
	}
	return tree
}

// record is a usage record's JSON with the fields Tokens reads and no others,
// so the fixture pins the file's shape rather than round-tripping through the
// dashboard struct. An empty end omits the field, which decodes as a zero time.
func record(workspace, end string, input, output, cacheCreation, cacheRead int64) string {
	body := `{"workspace":"` + workspace + `"`
	if end != "" {
		body += `,"end":"` + end + `"`
	}
	return body + `,"totals":{"input":` + strconv.FormatInt(input, 10) +
		`,"output":` + strconv.FormatInt(output, 10) +
		`,"cache_creation":` + strconv.FormatInt(cacheCreation, 10) +
		`,"cache_read":` + strconv.FormatInt(cacheRead, 10) + `}}`
}

func TestTokensSumsPerWorkspaceInMonth(t *testing.T) {
	tree := usageTree(map[string]string{
		"a-one.work.json":    record("ws-a", "2026-10-02T09:00:00Z", 100, 200, 3, 4),
		"a-two.work.json":    record("ws-a", "2026-10-31T23:59:59Z", 5, 6, 7, 8),
		"b-one.om.json":      record("ws-b", "2026-10-15T12:00:00Z", 10, 20, 1, 2),
		"notes.txt":          "not a record, ignored",
		"nested/x.work.json": record("ws-c", "2026-10-15T12:00:00Z", 99, 99, 99, 99),
	})
	got, err := Tokens(tree, october)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	want := []export.TokenCount{
		{Workspace: "ws-a", Input: 105, Output: 206, CacheCreation: 10, CacheRead: 12},
		{Workspace: "ws-b", Input: 10, Output: 20, CacheCreation: 1, CacheRead: 2},
	}
	if !reflect.DeepEqual(got.Workspaces, want) {
		t.Fatalf("workspaces = %+v, want %+v", got.Workspaces, want)
	}
	if got.Unattributed != (export.Tokens{}) || got.OutOfMonth != 0 || len(got.Unreadable) != 0 {
		t.Fatalf("excluded = %+v/%d/%v, want no exclusions", got.Unattributed, got.OutOfMonth, got.Unreadable)
	}
}

func TestTokensExcludesAndCounts(t *testing.T) {
	tree := usageTree(map[string]string{
		"in-month.json":     record("ws-a", "2026-10-15T12:00:00Z", 1, 2, 3, 4),
		"september.json":    record("ws-a", "2026-09-30T23:59:59Z", 1000, 0, 0, 0),
		"november.json":     record("ws-b", "2026-11-01T00:00:00Z", 2000, 0, 0, 0),
		"no-end.json":       record("ws-b", "", 3000, 0, 0, 0),
		"no-workspace.json": record("", "2026-10-20T12:00:00Z", 7, 8, 0, 0),
	})
	got, err := Tokens(tree, october)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	want := []export.TokenCount{
		{Workspace: "ws-a", Input: 1, Output: 2, CacheCreation: 3, CacheRead: 4},
	}
	if !reflect.DeepEqual(got.Workspaces, want) {
		t.Fatalf("workspaces = %+v, want %+v", got.Workspaces, want)
	}
	wantUnattributed := export.Tokens{Input: 7, Output: 8}
	if got.Unattributed != wantUnattributed {
		t.Fatalf("unattributed = %+v, want %+v", got.Unattributed, wantUnattributed)
	}
	if got.OutOfMonth != 3 {
		t.Fatalf("out of month = %d, want 3", got.OutOfMonth)
	}
}

func TestTokensUnreadableFileIsCountedNotFatal(t *testing.T) {
	tree := usageTree(map[string]string{
		"broken.json": "this is not json",
		"good.json":   record("ws-a", "2026-10-15T12:00:00Z", 1, 2, 3, 4),
	})
	got, err := Tokens(tree, october)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if !reflect.DeepEqual(got.Unreadable, []string{"broken.json"}) {
		t.Fatalf("unreadable = %v, want [broken.json]", got.Unreadable)
	}
	want := []export.TokenCount{
		{Workspace: "ws-a", Input: 1, Output: 2, CacheCreation: 3, CacheRead: 4},
	}
	if !reflect.DeepEqual(got.Workspaces, want) {
		t.Fatalf("workspaces = %+v, want %+v", got.Workspaces, want)
	}
}

// failingReadFS fails ReadFile for one name and serves the rest.
type failingReadFS struct {
	fstest.MapFS
	bad string
}

func (f failingReadFS) ReadFile(name string) ([]byte, error) {
	if name == f.bad {
		return nil, errors.New("permission denied")
	}
	return f.MapFS.ReadFile(name)
}

func TestTokensReadFailureIsUnreadable(t *testing.T) {
	tree := failingReadFS{
		MapFS: usageTree(map[string]string{
			"denied.json": record("ws-a", "2026-10-15T12:00:00Z", 1, 2, 3, 4),
			"good.json":   record("ws-a", "2026-10-15T12:00:00Z", 5, 6, 7, 8),
		}),
		bad: path.Join(usageDir, "denied.json"),
	}
	got, err := Tokens(tree, october)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if !reflect.DeepEqual(got.Unreadable, []string{"denied.json"}) {
		t.Fatalf("unreadable = %v, want [denied.json]", got.Unreadable)
	}
	want := []export.TokenCount{
		{Workspace: "ws-a", Input: 5, Output: 6, CacheCreation: 7, CacheRead: 8},
	}
	if !reflect.DeepEqual(got.Workspaces, want) {
		t.Fatalf("workspaces = %+v, want %+v", got.Workspaces, want)
	}
}

func TestTokensRefusesUnreadableRecordsDir(t *testing.T) {
	_, err := Tokens(fstest.MapFS{}, october)
	if err == nil {
		t.Fatal("Tokens accepted a tree with no beads directory")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want a not-found error naming the missing directory", err)
	}
	if !strings.Contains(err.Error(), usageDir) {
		t.Fatalf("error = %q, want it to name %q", err, usageDir)
	}
}

func TestTokensRefusesOverflow(t *testing.T) {
	cases := []struct {
		name  string
		tree  fstest.MapFS
		where string
	}{
		{
			name: "one workspace",
			tree: usageTree(map[string]string{
				"a.json": record("ws-a", "2026-10-01T00:00:00Z", math.MaxInt64, 0, 0, 0),
				"b.json": record("ws-a", "2026-10-01T00:00:00Z", 1, 0, 0, 0),
			}),
			where: `workspace "ws-a"`,
		},
		{
			name: "unattributed",
			tree: usageTree(map[string]string{
				"a.json": record("", "2026-10-01T00:00:00Z", math.MaxInt64, 0, 0, 0),
				"b.json": record("", "2026-10-01T00:00:00Z", 1, 0, 0, 0),
			}),
			where: "unattributed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Tokens(tc.tree, october)
			if err == nil {
				t.Fatal("Tokens accepted a token sum that overflows int64")
			}
			if !strings.Contains(err.Error(), tc.where) || !strings.Contains(err.Error(), "overflow") {
				t.Fatalf("error = %q, want it to name %q and the overflow", err, tc.where)
			}
		})
	}
}
