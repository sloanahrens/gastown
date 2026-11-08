package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/gatemodel"
)

// TestGateNestedModules pins the walk that tells the model which directories
// belong to their own module, and which the go tool never descends into.
func TestGateNestedModules(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, dir := range []string{"sub", "sub/deeper", ".hidden", "vendor", "testdata", "plain"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	for _, dir := range []string{".", "sub", "sub/deeper", ".hidden", "vendor", "testdata"} {
		if err := os.WriteFile(filepath.Join(root, dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatalf("writing %s/go.mod: %v", dir, err)
		}
	}

	got, err := gateNestedModules(root)
	if err != nil {
		t.Fatalf("gateNestedModules: %v", err)
	}
	// sub/deeper is inside sub, so sub covers it; the go tool skips the
	// hidden, vendor and testdata trees.
	if want := []string{"sub"}; !slices.Equal(got, want) {
		t.Errorf("nested modules = %v, want %v", got, want)
	}
}

// TestParseLandingRefs pins the git log record parser and its refusal of a
// cut-short stream.
func TestParseLandingRefs(t *testing.T) {
	t.Parallel()
	sha1, sha2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	tests := []struct {
		name     string
		out      string
		wantSHAs []string
		wantErr  bool
	}{
		{
			name:     "two records",
			out:      sha1 + "\x00s1\x00land: one\x00\n" + sha2 + "\x00s2\x00land: two\x00\n",
			wantSHAs: []string{sha1, sha2},
		},
		{name: "no landings", out: ""},
		{name: "cut-short record", out: sha1 + "\x00s1\x00", wantErr: true},
		{name: "not a sha", out: "--all\x00s1\x00land: one\x00\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			landings, err := parseLandingRefs(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseLandingRefs(%q) = %v, want an error", tt.out, landings)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLandingRefs: %v", err)
			}
			var shas []string
			for _, l := range landings {
				shas = append(shas, l.SHA)
			}
			if !slices.Equal(shas, tt.wantSHAs) {
				t.Errorf("shas = %v, want %v", shas, tt.wantSHAs)
			}
		})
	}
}

// TestParseGateGraph pins the refusal of a go list run that did not finish: a
// partial listing would miss the dependents of the packages it did not print,
// so the graph is unreadable and the model runs everything.
func TestParseGateGraph(t *testing.T) {
	t.Parallel()
	stream := []byte(`{"ImportPath":"example.com/fx/internal/a","Module":{"Path":"example.com/fx","Main":true}}` + "\n")

	tests := []struct {
		name    string
		out     []byte
		runErr  error
		wantNil bool
	}{
		{name: "complete listing", out: stream},
		{name: "listing exited non-zero", out: stream, runErr: errors.New("exit status 1"), wantNil: true},
		{name: "no output", out: nil, wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := parseGateGraph(tt.out, tt.runErr)
			if (g == nil) != tt.wantNil {
				t.Fatalf("parseGateGraph graph nil = %v, want %v", g == nil, tt.wantNil)
			}
		})
	}
}

// TestGateModelRefusesBadFlags drives the command's own refusals before it
// reads any repository.
func TestGateModelRefusesBadFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts gateModelOptions
		want string
	}{
		{name: "no logs", opts: gateModelOptions{Last: 1, CPUs: 1}, want: "--logs"},
		{name: "last below one", opts: gateModelOptions{Logs: "logs", Last: 0, CPUs: 1}, want: "--last"},
		{name: "cpus below one", opts: gateModelOptions{Logs: "logs", Last: 1, CPUs: -1}, want: "--cpus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := gateModel(&cobra.Command{}, tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("gateModel = %v, want a refusal naming %s", err, tt.want)
			}
		})
	}
}

// gateFakeGit answers the git calls gateLandings and gateRepoRoot make, keyed
// by the subcommand. A key with no answer fails the test: an unexpected call
// would otherwise read as a clean repository.
func gateFakeGit(t *testing.T, answers map[string]gateFakeAnswer) gateGitRunner {
	t.Helper()
	return func(_ context.Context, _ string, args ...string) (string, error) {
		a, ok := answers[args[0]]
		if !ok {
			t.Fatalf("unexpected git %v", args)
		}
		return a.out, a.err
	}
}

// gateFakeAnswer is one canned git reply.
type gateFakeAnswer struct {
	out string
	err error
}

// gateTestGraph is a one-package graph, for the reads that come after it.
func gateTestGraph(t *testing.T) *gatemodel.Graph {
	t.Helper()
	g, err := gatemodel.ParseGraph(strings.NewReader(`{"ImportPath":"example.com/fx/internal/a","Module":{"Path":"example.com/fx","Main":true}}` + "\n"))
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	return g
}

// gateTestLogs is a log directory holding one saved gate log.
func gateTestLogs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "baseline.log"), []byte("tierwall: example.com/fx/internal/a 2.0s\n"), 0o644); err != nil {
		t.Fatalf("writing the log: %v", err)
	}
	return dir
}

// TestGateRepoRootReportsAFailedGit pins the refusal of a directory git cannot
// name a work tree for.
func TestGateRepoRootReportsAFailedGit(t *testing.T) {
	t.Parallel()
	git := gateFakeGit(t, map[string]gateFakeAnswer{"rev-parse": {err: errors.New("exit status 128")}})
	if _, err := gateRepoRoot(context.Background(), git); err == nil {
		t.Fatal("gateRepoRoot with git failing = nil error, want a refusal")
	}
}

// TestGateLandingsReportsEachFailure drives the landing reader's refusals and
// its error returns, and checks that an unreadable diff is marked, not read as
// no change.
func TestGateLandingsReportsEachFailure(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	logRecord := sha + "\x00aaaa\x00land: x\x00\n"

	tests := []struct {
		name    string
		base    string
		answers map[string]gateFakeAnswer
	}{
		{name: "option-shaped base", base: "--all"},
		{name: "empty base", base: ""},
		{name: "git log fails", base: "HEAD", answers: map[string]gateFakeAnswer{"log": {err: errors.New("exit status 128")}}},
		{name: "git log cut short", base: "HEAD", answers: map[string]gateFakeAnswer{"log": {out: sha + "\x00aaaa\x00"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			git := gateFakeGit(t, tt.answers)
			if tt.answers == nil {
				git = func(context.Context, string, ...string) (string, error) {
					t.Fatal("git ran for a base the reader must refuse")
					return "", nil
				}
			}
			if _, err := gateLandings(context.Background(), git, t.TempDir(), tt.base, 1); err == nil {
				t.Fatal("gateLandings = nil error, want a refusal")
			}
		})
	}

	t.Run("failed diff is unreadable, not no change", func(t *testing.T) {
		t.Parallel()
		git := gateFakeGit(t, map[string]gateFakeAnswer{
			"log":  {out: logRecord},
			"diff": {err: errors.New("exit status 128")},
		})
		landings, err := gateLandings(context.Background(), git, t.TempDir(), "HEAD", 1)
		if err != nil {
			t.Fatalf("gateLandings: %v", err)
		}
		if len(landings) != 1 || landings[0].Unreadable == "" || landings[0].NameStatus != "" {
			t.Errorf("landings = %+v, want one landing whose diff is marked unreadable", landings)
		}
	})
}

// TestGateGitRefusesACancelledContext pins gateGit's error return. The context is
// already done, so the command never starts.
func TestGateGitRefusesACancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gateGit(ctx, t.TempDir(), "status"); err == nil {
		t.Fatal("gateGit with a cancelled context = nil error, want a refusal")
	}
}

// TestNestedModulesReportsAWalkOrStatFailure pins the two error returns of the
// nested-module scan: a tree that cannot be read at all, and a go.mod whose
// stat fails for a reason other than its absence.
func TestNestedModulesReportsAWalkOrStatFailure(t *testing.T) {
	t.Parallel()
	tree := fstest.MapFS{"sub/x.go": {Data: []byte("package sub")}}

	t.Run("root cannot be read", func(t *testing.T) {
		t.Parallel()
		if _, err := nestedModules(unreadableFS{err: fs.ErrPermission}); err == nil {
			t.Fatal("nestedModules over an unreadable root = nil error, want a refusal")
		}
	})

	t.Run("go.mod stat fails for a reason other than absence", func(t *testing.T) {
		t.Parallel()
		_, err := nestedModules(statFailFS{FS: tree, name: "sub/go.mod", err: fs.ErrPermission})
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("nestedModules = %v, want the permission error reported, not read as no go.mod", err)
		}
	})
}

// unreadableFS is a tree whose every open fails.
type unreadableFS struct{ err error }

func (u unreadableFS) Open(string) (fs.File, error) { return nil, u.err }

// statFailFS is a tree whose one file fails to open with err.
type statFailFS struct {
	fs.FS
	name string
	err  error
}

func (s statFailFS) Open(name string) (fs.File, error) {
	if name == s.name {
		return nil, s.err
	}
	return s.FS.Open(name)
}

// TestGateModelRunReportsEachFailure drives every error return of the model
// pipeline with fake reads, and checks the report a clean run prints.
func TestGateModelRunReportsEachFailure(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	// The work tree holds the package the diff changes, so the selection is a
	// package selection and not the full run.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "a"), 0o755); err != nil {
		t.Fatalf("creating the package: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "a", "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("writing the package: %v", err)
	}
	okGit := gateFakeGit(t, map[string]gateFakeAnswer{
		"rev-parse": {out: root + "\n"},
		"log":       {out: sha + "\x00aaaa\x00land: x\x00\n"},
		"diff":      {out: "M\x00internal/a/a.go\x00"},
	})
	g := gateTestGraph(t)
	graph := func(context.Context, string) *gatemodel.Graph { return g }
	nested := func(string) ([]string, error) { return nil, nil }
	failGit := gateFakeGit(t, map[string]gateFakeAnswer{"rev-parse": {err: errors.New("exit status 128")}})

	tests := []struct {
		name    string
		src     gateSources
		logs    string
		cpus    int
		wantErr bool
	}{
		{name: "not in a work tree", src: gateSources{git: failGit, graph: graph, nested: nested}, logs: "x", cpus: 1, wantErr: true},
		{name: "landings unreadable", src: gateSources{git: gateFakeGit(t, map[string]gateFakeAnswer{"rev-parse": {out: "/r\n"}, "log": {err: errors.New("exit status 128")}}), graph: graph, nested: nested}, logs: "x", cpus: 1, wantErr: true},
		{name: "log directory missing", src: gateSources{git: okGit, graph: graph, nested: nested}, logs: filepath.Join(t.TempDir(), "absent"), cpus: 1, wantErr: true},
		{name: "nested modules unreadable", src: gateSources{git: okGit, graph: graph, nested: func(string) ([]string, error) { return nil, errors.New("walk failed") }}, logs: "LOGS", cpus: 1, wantErr: true},
		{name: "cpus below one reaches the model", src: gateSources{git: okGit, graph: graph, nested: nested}, logs: "LOGS", cpus: 0, wantErr: true},
		{name: "clean run", src: gateSources{git: okGit, graph: graph, nested: nested}, logs: "LOGS", cpus: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logs := tt.logs
			if logs == "LOGS" {
				logs = gateTestLogs(t)
			}
			var out bytes.Buffer
			opts := gateModelOptions{Logs: logs, Base: "HEAD", Last: 1}
			err := gateModelRun(context.Background(), tt.src, opts, tt.cpus, &out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("gateModelRun = nil error, want a refusal; output:\n%s", out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("gateModelRun: %v", err)
			}
			for _, want := range []string{"internal/a", "summary: 1 landings"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("report does not hold %q:\n%s", want, out.String())
				}
			}
		})
	}

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		opts := gateModelOptions{Logs: gateTestLogs(t), Base: "HEAD", Last: 1, JSON: true}
		if err := gateModelRun(context.Background(), gateSources{git: okGit, graph: graph, nested: nested}, opts, 2, &out); err != nil {
			t.Fatalf("gateModelRun --json: %v", err)
		}
		var decoded struct {
			Landings []struct {
				Changed []string `json:"changed"`
			} `json:"landings"`
		}
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatalf("decoding the JSON report: %v\n%s", err, out.String())
		}
		if len(decoded.Landings) != 1 || !slices.Equal(decoded.Landings[0].Changed, []string{"internal/a"}) {
			t.Errorf("JSON landings = %+v, want one landing changing internal/a", decoded.Landings)
		}
	})
}

// TestGateModelCommandPrintsTheTable drives the command's text report: a
// header row naming the Output line's columns, then one row per landing with
// them filled. The table pads columns with two or more spaces; a list inside
// one cell separates with a comma and one space.
func TestGateModelCommandPrintsTheTable(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	root := t.TempDir()
	pkgDir := filepath.Join(root, "internal", "a")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("creating the package: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("writing the package: %v", err)
	}
	git := gateFakeGit(t, map[string]gateFakeAnswer{
		"rev-parse": {out: root + "\n"},
		"log":       {out: sha + "\x00aaaaaaaaaaaa\x00land: x\x00\n"},
		"diff":      {out: "M\x00internal/a/a.go\x00"},
	})
	g := gateTestGraph(t)
	src := gateSources{
		git:    git,
		graph:  func(context.Context, string) *gatemodel.Graph { return g },
		nested: func(string) ([]string, error) { return nil, nil },
	}
	var out bytes.Buffer
	opts := gateModelOptions{Logs: gateTestLogs(t), Base: "HEAD", Last: 1}
	if err := gateModelRun(context.Background(), src, opts, 2, &out); err != nil {
		t.Fatalf("gateModelRun: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("report has %d lines, want a header and one landing row:\n%s", len(lines), out.String())
	}
	wantHeader := []string{"SHORT SHA", "CHANGED", "DEPENDENTS", "ALWAYS-RUN", "FALLBACK", "MODELED(2 cpus)", "LOGGED"}
	if got := gateTableCells(lines[0]); !slices.Equal(got, wantHeader) {
		t.Errorf("header cells = %q, want %q", got, wantHeader)
	}
	wantRow := []string{
		"aaaaaaaaaaaa",
		"internal/a",
		"-",
		"internal/guardlint, internal/makegate, internal/testpolicy",
		"-",
		"2.0s",
		"-",
	}
	if got := gateTableCells(lines[1]); !slices.Equal(got, wantRow) {
		t.Errorf("landing row = %q, want %q", got, wantRow)
	}
}

// gateTableCells splits one table line into its cells.
func gateTableCells(line string) []string {
	return gateTableCellGap.Split(strings.TrimSpace(line), -1)
}

var gateTableCellGap = regexp.MustCompile(`\s{2,}`)
