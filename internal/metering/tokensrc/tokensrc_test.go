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

// countsWith is counts with one named token field at v and the rest zero.
func countsWith(field string, v int64) export.Tokens {
	var counts export.Tokens
	switch field {
	case "input":
		counts.Input = v
	case "output":
		counts.Output = v
	case "cache creation":
		counts.CacheCreation = v
	case "cache read":
		counts.CacheRead = v
	}
	return counts
}

// recordOf is record with the four counter arguments taken from counts.
func recordOf(workspace, end string, counts export.Tokens) string {
	return record(workspace, end, counts.Input, counts.Output, counts.CacheCreation, counts.CacheRead)
}

// twoRecords builds a tree of two in-month records for workspace, read in the
// order given. An empty workspace leaves both unattributed.
func twoRecords(workspace string, first, second export.Tokens) fstest.MapFS {
	return usageTree(map[string]string{
		"a.json": recordOf(workspace, "2026-10-01T00:00:00Z", first),
		"b.json": recordOf(workspace, "2026-10-01T00:00:00Z", second),
	})
}

// paddedRecord is a valid in-month record of exactly size bytes: JSON permits
// trailing whitespace, so the padding leaves it decodable.
func paddedRecord(t *testing.T, workspace string, size int) string {
	t.Helper()
	base := record(workspace, "2026-10-15T12:00:00Z", 1, 2, 3, 4)
	if len(base) > size {
		t.Fatalf("record is %d bytes, larger than the %d it is padded to", len(base), size)
	}
	return base + strings.Repeat(" ", size-len(base))
}

func TestTokensSumsPerWorkspaceInMonth(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// failingOpenFS fails Open for one name and serves the rest.
type failingOpenFS struct {
	fstest.MapFS
	bad string
}

func (f failingOpenFS) Open(name string) (fs.File, error) {
	if name == f.bad {
		return nil, errors.New("permission denied")
	}
	return f.MapFS.Open(name)
}

func TestTokensReadFailureIsUnreadable(t *testing.T) {
	t.Parallel()
	tree := failingOpenFS{
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

// unreadableFile serves body and then fails Read, so a caller that ignored the
// error would decode and count the record anyway.
type unreadableFile struct {
	body []byte
}

func (f *unreadableFile) Stat() (fs.FileInfo, error) { return nil, errors.New("no stat") }
func (f *unreadableFile) Read(p []byte) (int, error) {
	if len(f.body) == 0 {
		return 0, errors.New("read failed")
	}
	n := copy(p, f.body)
	f.body = f.body[n:]
	return n, nil
}
func (f *unreadableFile) Close() error { return nil }

// failingReadFS opens one name as a file that serves body and then fails Read,
// and serves the rest from the tree, so a read error after a successful open
// is driven.
type failingReadFS struct {
	fstest.MapFS
	bad  string
	body string
}

func (f failingReadFS) Open(name string) (fs.File, error) {
	if name == f.bad {
		return &unreadableFile{body: []byte(f.body)}, nil
	}
	return f.MapFS.Open(name)
}

func TestTokensReadErrorIsUnreadable(t *testing.T) {
	t.Parallel()
	broken := record("ws-a", "2026-10-15T12:00:00Z", 1, 2, 3, 4)
	tree := failingReadFS{
		MapFS: usageTree(map[string]string{
			"broken-read.json": broken,
			"good.json":        record("ws-a", "2026-10-15T12:00:00Z", 5, 6, 7, 8),
		}),
		bad:  path.Join(usageDir, "broken-read.json"),
		body: broken,
	}
	got, err := Tokens(tree, october)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if !reflect.DeepEqual(got.Unreadable, []string{"broken-read.json"}) {
		t.Fatalf("unreadable = %v, want [broken-read.json]", got.Unreadable)
	}
	want := []export.TokenCount{
		{Workspace: "ws-a", Input: 5, Output: 6, CacheCreation: 7, CacheRead: 8},
	}
	if !reflect.DeepEqual(got.Workspaces, want) {
		t.Fatalf("workspaces = %+v, want %+v", got.Workspaces, want)
	}
}

func TestTokensRefusesMissingRecordsDir(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	destinations := []struct {
		label     string
		workspace string
		where     string
	}{
		{label: "workspace", workspace: "ws-a", where: `workspace "ws-a"`},
		{label: "unattributed", workspace: "", where: "unattributed"},
	}
	fields := []string{"input", "output", "cache creation", "cache read"}
	for _, dest := range destinations {
		for _, field := range fields {
			t.Run(dest.label+"/"+field, func(t *testing.T) {
				t.Parallel()
				tree := twoRecords(dest.workspace, countsWith(field, math.MaxInt64), countsWith(field, 1))
				_, err := Tokens(tree, october)
				if err == nil {
					t.Fatalf("Tokens accepted a %s sum that overflows int64", field)
				}
				if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), dest.where) || !strings.Contains(err.Error(), "overflow") {
					t.Fatalf("error = %q, want it to name %q, %q and the overflow", err, field, dest.where)
				}
			})
		}
	}
}

func TestTokensRefusesNegativeCounts(t *testing.T) {
	t.Parallel()
	destinations := []struct {
		label     string
		workspace string
		where     string
	}{
		{label: "workspace", workspace: "ws-a", where: `workspace "ws-a"`},
		{label: "unattributed", workspace: "", where: "unattributed"},
	}
	fields := []string{"input", "output", "cache creation", "cache read"}
	for _, dest := range destinations {
		for _, field := range fields {
			t.Run(dest.label+"/"+field, func(t *testing.T) {
				t.Parallel()
				tree := usageTree(map[string]string{
					"a.json": recordOf(dest.workspace, "2026-10-15T12:00:00Z", countsWith(field, -1)),
				})
				_, err := Tokens(tree, october)
				if err == nil {
					t.Fatalf("Tokens accepted a negative %s count", field)
				}
				if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), dest.where) || !strings.Contains(err.Error(), "negative") {
					t.Fatalf("error = %q, want it to name %q, %q and the negative count", err, field, dest.where)
				}
			})
		}
	}
}

func TestTokensBoundsRecordSize(t *testing.T) {
	t.Parallel()
	tree := usageTree(map[string]string{
		"at-bound.json":   paddedRecord(t, "ws-a", maxRecordSize),
		"over-bound.json": paddedRecord(t, "ws-a", maxRecordSize+1),
	})
	got, err := Tokens(tree, october)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if !reflect.DeepEqual(got.Unreadable, []string{"over-bound.json"}) {
		t.Fatalf("unreadable = %v, want [over-bound.json]", got.Unreadable)
	}
	want := []export.TokenCount{
		{Workspace: "ws-a", Input: 1, Output: 2, CacheCreation: 3, CacheRead: 4},
	}
	if !reflect.DeepEqual(got.Workspaces, want) {
		t.Fatalf("workspaces = %+v, want %+v", got.Workspaces, want)
	}
}
