package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const testTerms = `{
  "invoice_id": "INV-2026-10-0001",
  "factor_percent": 115,
  "floor_cents": 500,
  "seat_hour_cents": {"opus": 9, "sonnet": 3},
  "storage_gb_cents": 20
}`

// fixtureFS is one month of a town's files: two seat intervals, one tracker
// day, one Forgejo day, one usage record and a terms file.
func fixtureFS() fstest.MapFS {
	return fstest.MapFS{
		"metrics/sandbox-seats.jsonl": {Data: []byte(
			`{"workspace":"acme","profile":"opus","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-02T00:00:00Z"}
{"workspace":"acme","profile":"sonnet","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-01T12:00:00Z"}
`)},
		"metrics/tracker-sizes/2026-10-01.jsonl": {Data: []byte(
			`{"ts":"2026-10-01T00:00:00Z","workspace":"acme","prefix":"gt","bytes":1073741824}
`)},
		"metrics/forgejo-sizes/2026-10-01.jsonl": {Data: []byte(
			`{"ts":"2026-10-01T00:00:00Z","workspace":"acme","repo":"gastown","size_kb":1024}
`)},
		"usage/beads/gt-abc.json": {Data: []byte(
			`{"workspace":"acme","end":"2026-10-05T00:00:00Z","totals":{"input":1234,"output":567,"cache_creation":89,"cache_read":4321}}
`)},
		"terms.json": {Data: []byte(testTerms)},
	}
}

// fixtureWithTerms is fixtureFS with its terms file replaced.
func fixtureWithTerms(terms string) fstest.MapFS {
	m := fixtureFS()
	m["terms.json"] = &fstest.MapFile{Data: []byte(terms)}
	return m
}

// withFile is m with one more file at name.
func withFile(m fstest.MapFS, name, data string) fstest.MapFS {
	m[name] = &fstest.MapFile{Data: []byte(data)}
	return m
}

// termsWith is testTerms with edit applied to its keys.
func termsWith(t *testing.T, edit func(map[string]any)) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(testTerms), &m); err != nil {
		t.Fatalf("decoding the fixture terms: %v", err)
	}
	edit(m)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encoding the fixture terms: %v", err)
	}
	return string(data)
}

// baseArgs name the fixture's town, month, workspace and terms.
func baseArgs() []string {
	return []string{
		"-town", "/town",
		"-month", "2026-10",
		"-workspace", "acme",
		"-terms", "terms.json",
		"-overhead-usd", "10",
	}
}

// runFor runs the command in-process and returns what it wrote and its code.
func runFor(t *testing.T, fsys fs.FS, args []string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errw bytes.Buffer
	code = run(args, fsys, &out, &errw)
	return out.String(), errw.String(), code
}

func TestRootedRefusesAPathOutsideTheTree(t *testing.T) {
	t.Parallel()
	if _, err := rooted(fixtureFS(), "../escape"); err == nil {
		t.Error("rooted accepted a dir that escapes the tree")
	}
}

func TestRunPrintsInvoiceFromFixtures(t *testing.T) {
	t.Parallel()
	stdout, stderr, code := runFor(t, fixtureFS(), baseArgs())
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr)
	}
	for _, want := range []string{
		"Invoice INV-2026-10-0001",
		"Workspace: acme",
		"October 2026",
		"seat-hours opus: 24.00 h",
		"input: 1234",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not hold %q:\n%s", want, stdout)
		}
	}
}

func TestRunBadTermsAndArguments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fsys fstest.MapFS
		args []string
		want string
	}{
		{
			"unknown terms key",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["colour"] = 1 })),
			baseArgs(),
			`unknown key "colour"`,
		},
		{
			"missing invoice id",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { delete(m, "invoice_id") })),
			baseArgs(),
			`missing key "invoice_id"`,
		},
		{
			"missing floor",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { delete(m, "floor_cents") })),
			baseArgs(),
			`missing key "floor_cents"`,
		},
		{
			"missing seat prices",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { delete(m, "seat_hour_cents") })),
			baseArgs(),
			`missing key "seat_hour_cents"`,
		},
		{
			"missing storage price",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { delete(m, "storage_gb_cents") })),
			baseArgs(),
			`missing key "storage_gb_cents"`,
		},
		{
			"fractional cents",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["floor_cents"] = 1.5 })),
			baseArgs(),
			`key "floor_cents" is not an integer`,
		},
		{
			"number written as a string",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["factor_percent"] = "115" })),
			baseArgs(),
			`key "factor_percent" is not an integer`,
		},
		{
			"invoice id not a string",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["invoice_id"] = 1 })),
			baseArgs(),
			`key "invoice_id" is not a string`,
		},
		{
			"empty invoice id",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["invoice_id"] = " " })),
			baseArgs(),
			`key "invoice_id" is empty`,
		},
		{
			"zero factor",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["factor_percent"] = 0 })),
			baseArgs(),
			`key "factor_percent" is 0, want a positive percent`,
		},
		{
			"negative floor",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["floor_cents"] = -1 })),
			baseArgs(),
			`key "floor_cents" is -1`,
		},
		{
			"zero storage price",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["storage_gb_cents"] = 0 })),
			baseArgs(),
			`key "storage_gb_cents" is 0`,
		},
		{
			"seat prices not an object",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["seat_hour_cents"] = "cheap" })),
			baseArgs(),
			`key "seat_hour_cents" is not an object`,
		},
		{
			"null seat prices",
			fixtureWithTerms(termsWith(t, func(m map[string]any) { m["seat_hour_cents"] = nil })),
			baseArgs(),
			`key "seat_hour_cents" is not an object`,
		},
		{
			"seat price not an integer",
			fixtureWithTerms(termsWith(t, func(m map[string]any) {
				m["seat_hour_cents"] = map[string]any{"opus": "nine"}
			})),
			baseArgs(),
			`non-integer price for profile "opus"`,
		},
		{
			"empty profile name",
			fixtureWithTerms(termsWith(t, func(m map[string]any) {
				m["seat_hour_cents"] = map[string]any{"": 9}
			})),
			baseArgs(),
			"empty profile name",
		},
		{
			"negative seat price",
			fixtureWithTerms(termsWith(t, func(m map[string]any) {
				m["seat_hour_cents"] = map[string]any{"opus": -9}
			})),
			baseArgs(),
			`key "seat_hour_cents" is -9 for profile "opus"`,
		},
		{
			"terms file absent",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "2026-10", "-workspace", "acme", "-terms", "missing.json"},
			"terms missing.json",
		},
		{
			"terms file not JSON",
			fixtureWithTerms("not json"),
			baseArgs(),
			"terms.json:",
		},
		{
			"terms file not an object",
			fixtureWithTerms("[]"),
			baseArgs(),
			"terms.json:",
		},
		{
			"terms file null",
			fixtureWithTerms("null"),
			baseArgs(),
			"not a JSON object",
		},
		{
			"bad month",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "2026-13", "-workspace", "acme", "-terms", "terms.json"},
			`-month "2026-13" is not YYYY-MM`,
		},
		{
			"one-digit month",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "2026-1", "-workspace", "acme", "-terms", "terms.json"},
			"-month",
		},
		{
			"month with a day",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "2026-10-05", "-workspace", "acme", "-terms", "terms.json"},
			"-month",
		},
		{
			"year zero",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "0000-01", "-workspace", "acme", "-terms", "terms.json"},
			`-month "0000-01" is not YYYY-MM`,
		},
		{
			"missing month",
			fixtureFS(),
			[]string{"-town", "/town", "-workspace", "acme", "-terms", "terms.json"},
			"-month is required",
		},
		{
			"missing workspace",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "2026-10", "-terms", "terms.json"},
			"-workspace is required",
		},
		{
			"missing town",
			fixtureFS(),
			[]string{"-month", "2026-10", "-workspace", "acme", "-terms", "terms.json"},
			"-town is required",
		},
		{
			"missing terms flag",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "2026-10", "-workspace", "acme"},
			"-terms is required",
		},
		{
			"negative overhead",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "2026-10", "-workspace", "acme", "-terms", "terms.json", "-overhead-usd", "-5"},
			"-overhead-usd -5 is negative",
		},
		{
			"non-finite overhead",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "2026-10", "-workspace", "acme", "-terms", "terms.json", "-overhead-usd", "NaN"},
			"not a finite number",
		},
		{
			"unknown flag",
			fixtureFS(),
			append(baseArgs(), "-nope"),
			"-nope",
		},
		{
			"stray argument",
			fixtureFS(),
			append(baseArgs(), "orphan.json"),
			`unexpected argument "orphan.json"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := runFor(t, tc.fsys, tc.args)
			if code != 2 {
				t.Errorf("exit %d, want 2 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not name %s: %q", tc.want, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout is not empty: %q", stdout)
			}
		})
	}
}

// errorOnPath makes Open fail for one path, as an unreadable file on disk
// does.
type errorOnPath struct {
	fs.FS
	path string
	err  error
}

func (e errorOnPath) Open(name string) (fs.File, error) {
	if name == e.path {
		return nil, e.err
	}
	return e.FS.Open(name)
}

// readErrorOnPath hands back a file whose first read fails, as a file on a
// broken disk does.
type readErrorOnPath struct {
	fs.FS
	path string
	err  error
}

func (e readErrorOnPath) Open(name string) (fs.File, error) {
	if name == e.path {
		return readErrorFile{err: e.err}, nil
	}
	return e.FS.Open(name)
}

type readErrorFile struct {
	err error
}

func (readErrorFile) Stat() (fs.FileInfo, error) { return nil, errors.New("no file info") }
func (f readErrorFile) Read([]byte) (int, error) { return 0, f.err }
func (readErrorFile) Close() error               { return nil }

func TestRunSourceFailureWritesNoInvoice(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fsys fs.FS
		args []string
		want string
	}{
		{
			"seat ledger cannot be opened",
			errorOnPath{FS: fixtureFS(), path: "metrics/sandbox-seats.jsonl", err: errors.New("the ledger is unreadable")},
			baseArgs(),
			"seat ledger",
		},
		{
			"seat ledger fails mid-read",
			readErrorOnPath{FS: fixtureFS(), path: "metrics/sandbox-seats.jsonl", err: errors.New("the disk failed")},
			baseArgs(),
			"seat ledger",
		},
		{
			"size files cannot be listed",
			errorOnPath{FS: fixtureFS(), path: "metrics/tracker-sizes", err: errors.New("the directory is unreadable")},
			baseArgs(),
			"storagesrc",
		},
		{
			"usage tree is missing",
			noUsage(),
			baseArgs(),
			"tokensrc",
		},
		{
			"usage record cannot be read",
			errorOnPath{FS: fixtureFS(), path: "usage/beads/gt-abc.json", err: errors.New("the record is unreadable")},
			baseArgs(),
			`usage records unreadable: "gt-abc.json"`,
		},
		{
			"usage record cannot be decoded",
			withFile(fixtureFS(), "usage/beads/bad.json", "not json"),
			baseArgs(),
			`usage records unreadable: "bad.json"`,
		},
		{
			"workspace absent from the export",
			fixtureFS(),
			[]string{"-town", "/town", "-month", "2026-10", "-workspace", "nobody", "-terms", "terms.json"},
			`workspace "nobody" is not in the export`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := runFor(t, tc.fsys, tc.args)
			if code != 1 {
				t.Errorf("exit %d, want 1 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not name %s: %q", tc.want, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout is not empty: %q", stdout)
			}
		})
	}
}

// noUsage is fixtureFS without the usage tree.
func noUsage() fstest.MapFS {
	m := fixtureFS()
	delete(m, "usage/beads/gt-abc.json")
	return m
}

func TestRunNotesGoToStderr(t *testing.T) {
	t.Parallel()
	stdout, stderr, code := runFor(t, notesFixture(), baseArgs())
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "Invoice INV-2026-10-0001") {
		t.Errorf("stdout does not hold the invoice:\n%s", stdout)
	}
	notes := []string{
		"invoice: notes",
		"seat ledger skipped: empty workspace: 1",
		"size records skipped: invalid JSON: 1",
		"usage records skipped: out of month: 1",
		"records naming no workspace are not billed: input 7",
		"seats still running are not in the seat ledger until they end",
		"token counts are printed unpriced",
	}
	for _, note := range notes {
		if !strings.Contains(stderr, note) {
			t.Errorf("stderr does not hold %q:\n%s", note, stderr)
		}
		if strings.Contains(stdout, note) {
			t.Errorf("stdout also holds the note %q:\n%s", note, stdout)
		}
	}
}

// notesFixture is fixtureFS with one skipped line per source, an out-of-month
// usage record and an unattributed usage record.
func notesFixture() fs.FS {
	m := fixtureFS()
	m["metrics/sandbox-seats.jsonl"] = &fstest.MapFile{Data: []byte(
		`{"workspace":"acme","profile":"opus","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-02T00:00:00Z"}
{"workspace":"","profile":"opus","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-01T01:00:00Z"}
`)}
	m["metrics/tracker-sizes/2026-10-01.jsonl"] = &fstest.MapFile{Data: []byte(
		`{"ts":"2026-10-01T00:00:00Z","workspace":"acme","prefix":"gt","bytes":1073741824}
not json
`)}
	m["usage/beads/gt-old.json"] = &fstest.MapFile{Data: []byte(
		`{"workspace":"acme","end":"2026-09-15T00:00:00Z","totals":{"input":5}}
`)}
	m["usage/beads/unattributed.json"] = &fstest.MapFile{Data: []byte(
		`{"end":"2026-10-05T00:00:00Z","totals":{"input":7}}
`)}
	return m
}

func TestRunNotesAbsentSources(t *testing.T) {
	t.Parallel()
	m := fixtureFS()
	delete(m, "metrics/sandbox-seats.jsonl")
	delete(m, "metrics/tracker-sizes/2026-10-01.jsonl")
	stdout, stderr, code := runFor(t, m, baseArgs())
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr)
	}
	for _, note := range []string{
		"seat ledger skipped: 0",
		"usage records skipped: 0",
		"seat ledger absent: no seat has ended yet",
		`"tracker-sizes" absent: its days count as zero`,
	} {
		if !strings.Contains(stderr, note) {
			t.Errorf("stderr does not hold %q:\n%s", note, stderr)
		}
	}
	if !strings.Contains(stdout, "Invoice INV-2026-10-0001") {
		t.Errorf("stdout does not hold the invoice:\n%s", stdout)
	}
}

// failingWriter refuses every write, as a closed pipe does.
type failingWriter struct {
	err error
}

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestRunReportsWriteFailures(t *testing.T) {
	t.Parallel()
	t.Run("stdout", func(t *testing.T) {
		t.Parallel()
		var errw bytes.Buffer
		code := run(baseArgs(), fixtureFS(), failingWriter{errors.New("the pipe is closed")}, &errw)
		if code != 1 {
			t.Errorf("exit %d, want 1 (stderr %q)", code, errw.String())
		}
		if !strings.Contains(errw.String(), "writing the invoice") {
			t.Errorf("stderr does not report the write failure: %q", errw.String())
		}
	})
	t.Run("stderr notes", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		code := run(baseArgs(), fixtureFS(), &out, failingWriter{errors.New("the pipe is closed")})
		if code != 0 {
			t.Errorf("exit %d, want 0: the invoice printed and the notes are advisory", code)
		}
		if !strings.Contains(out.String(), "Invoice INV-2026-10-0001") {
			t.Errorf("stdout does not hold the invoice:\n%s", out.String())
		}
	})
}

// writeTree writes a MapFS under root, making the directories it needs.
func writeTree(t *testing.T, root string, tree fstest.MapFS) {
	t.Helper()
	for name, file := range tree {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("making %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, file.Data, 0o644); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
}

func TestRealMainReadsTheTownRuntimeTree(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTree(t, filepath.Join(town, ".runtime"), fixtureFS())

	var out, errw bytes.Buffer
	code := realMain([]string{
		"-town", town,
		"-month", "2026-10",
		"-workspace", "acme",
		"-terms", "terms.json",
	}, &out, &errw)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, errw.String())
	}
	if !strings.Contains(out.String(), "Invoice INV-2026-10-0001") {
		t.Errorf("stdout does not hold the invoice:\n%s", out.String())
	}
}

func TestRealMainRequiresTown(t *testing.T) {
	t.Parallel()
	var out, errw bytes.Buffer
	code := realMain([]string{"-month", "2026-10"}, &out, &errw)
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "-town is required") {
		t.Errorf("stderr does not name -town: %q", errw.String())
	}
}

func TestRealMainRefusesAnUnknownFlag(t *testing.T) {
	t.Parallel()
	var out, errw bytes.Buffer
	code := realMain([]string{"-town", t.TempDir(), "-nope"}, &out, &errw)
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "-nope") {
		t.Errorf("stderr does not name -nope: %q", errw.String())
	}
	if out.String() != "" {
		t.Errorf("stdout is not empty: %q", out.String())
	}
}
