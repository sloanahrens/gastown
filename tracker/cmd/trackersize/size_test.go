package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testDSN is a connection string the fakes never read.
const testDSN = "postgres://tracker:s3cr3t-Pa55w0rd@db.internal:5433/tracker"

// testNow is the 03:00 report on 2026-01-09, in UTC.
var testNow = time.Date(2026, 1, 9, 3, 0, 0, 0, time.UTC)

// fakeQuerier is a Querier the test answers: the sizes it returns, or the error
// it fails with.
type fakeQuerier struct {
	sizes []PrefixSize
	err   error
}

func (f fakeQuerier) Sizes(context.Context) ([]PrefixSize, error) { return f.sizes, f.err }

// fixedClock returns one instant.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// dayFile reads the day file at path as its lines, each without the newline.
func dayFile(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	text := string(data)
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if last := lines[len(lines)-1]; last == "" {
		t.Fatalf("%s ends with a blank line", path)
	}
	return lines
}

func TestWritesOneLinePerPrefix(t *testing.T) {
	out := t.TempDir()
	// Returned out of order: the file's order is the sort's, not the query's.
	q := fakeQuerier{sizes: []PrefixSize{
		{Prefix: "hq", Rows: 3, Bytes: 120},
		{Prefix: "gt", Rows: 12, Bytes: 3456},
	}}
	var stdout bytes.Buffer
	opts := options{dsn: testDSN, workspace: "customer-zero", out: out}
	if err := report(context.Background(), opts, q, fixedClock{testNow}, &stdout); err != nil {
		t.Fatalf("report: %v", err)
	}

	path := filepath.Join(out, "2026-01-09.jsonl")
	want := []string{
		`{"ts":"2026-01-09T03:00:00Z","workspace":"customer-zero","prefix":"gt","rows":12,"bytes":3456}`,
		`{"ts":"2026-01-09T03:00:00Z","workspace":"customer-zero","prefix":"hq","rows":3,"bytes":120}`,
	}
	got := dayFile(t, path)
	if len(got) != len(want) {
		t.Fatalf("%s holds %d lines, want %d: %q", path, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d of %s:\n got %s\nwant %s", i+1, path, got[i], want[i])
		}
	}
	// The partial name is not left beside the published file.
	if entries, err := os.ReadDir(out); err != nil {
		t.Fatalf("reading %s: %v", out, err)
	} else if len(entries) != 1 {
		t.Fatalf("%s holds %d files, want just the day file", out, len(entries))
	}
}

func TestOutputFileNamedByDate(t *testing.T) {
	out := t.TempDir()
	// 02:30 on the 16th at +03:00 is the 15th in UTC, the day the file is named
	// for.
	clock := fixedClock{time.Date(2026, 6, 16, 2, 30, 0, 0, time.FixedZone("EAT", 3*60*60))}
	q := fakeQuerier{sizes: []PrefixSize{{Prefix: "gt", Rows: 1, Bytes: 10}}}
	if err := report(context.Background(), options{workspace: "customer-zero", out: out}, q, clock, &bytes.Buffer{}); err != nil {
		t.Fatalf("report: %v", err)
	}
	path := filepath.Join(out, "2026-06-15.jsonl")
	if got := dayFile(t, path); len(got) != 1 {
		t.Fatalf("%s holds %d lines, want 1", path, len(got))
	}
}

func TestQueryErrorWritesNothingAndFails(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sizes")
	q := fakeQuerier{err: errors.New("connection reset by peer")}
	err := report(context.Background(), options{workspace: "customer-zero", out: out}, q, fixedClock{testNow}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("report succeeded on a failed query")
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a failed query touched %s: %v", out, statErr)
	}
}

func TestRerunReplacesTheDayFile(t *testing.T) {
	out := t.TempDir()
	q := fakeQuerier{sizes: []PrefixSize{{Prefix: "gt", Rows: 1, Bytes: 10}}}
	for run := 0; run < 2; run++ {
		if err := report(context.Background(), options{workspace: "customer-zero", out: out}, q, fixedClock{testNow}, &bytes.Buffer{}); err != nil {
			t.Fatalf("run %d: %v", run+1, err)
		}
	}
	path := filepath.Join(out, "2026-01-09.jsonl")
	if got := dayFile(t, path); len(got) != 1 {
		t.Fatalf("a second run left %d lines in %s, want the one line of its own report", len(got), path)
	}
}

func TestParseTakesOneDsnWorkspaceAndOutDir(t *testing.T) {
	opts, err := parse([]string{"-out", "/reports", "-workspace", "customer-zero", testDSN}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := options{dsn: testDSN, workspace: "customer-zero", out: "/reports"}
	if opts != want {
		t.Fatalf("parse returned %+v, want %+v", opts, want)
	}
}

func TestParseRejectsInvocationsThatNameNoDatabaseOrHome(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no dsn", []string{"-out", "/reports", "-workspace", "w"}, "want exactly one DSN"},
		{"two dsns", []string{"-out", "/reports", "-workspace", "w", testDSN, testDSN}, "want exactly one DSN"},
		{"no workspace", []string{"-out", "/reports", testDSN}, "-workspace is required"},
		{"no out", []string{"-workspace", "w", testDSN}, "-out is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parse(tc.args, &bytes.Buffer{}); err == nil {
				t.Fatalf("parse accepted %q", tc.args)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestPublishFailureLeavesNoPartial(t *testing.T) {
	out := t.TempDir()
	// A directory at the day file's name makes the rename fail, the way a
	// planted path would.
	if err := os.Mkdir(filepath.Join(out, "2026-01-09.jsonl"), 0o700); err != nil {
		t.Fatalf("making the blocker: %v", err)
	}
	q := fakeQuerier{sizes: []PrefixSize{{Prefix: "gt", Rows: 1, Bytes: 10}}}
	err := report(context.Background(), options{workspace: "customer-zero", out: out}, q, fixedClock{testNow}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("report published over a directory")
	}
	entries, readErr := os.ReadDir(out)
	if readErr != nil {
		t.Fatalf("reading %s: %v", out, readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "2026-01-09.jsonl" {
		t.Fatalf("a failed publish left %v in %s, want just the blocker", entries, out)
	}
}

func TestOutputDirectoryThatIsAFileFails(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "sizes")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("writing %s: %v", blocker, err)
	}
	q := fakeQuerier{sizes: []PrefixSize{{Prefix: "gt", Rows: 1, Bytes: 10}}}
	if err := report(context.Background(), options{workspace: "customer-zero", out: blocker}, q, fixedClock{testNow}, &bytes.Buffer{}); err == nil {
		t.Fatalf("report wrote into %s, which is a file", blocker)
	}
}
