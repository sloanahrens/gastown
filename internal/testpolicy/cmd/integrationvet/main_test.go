package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// fixtureTree writes a module with two packages the integration tag adds a
// file to and one it does not, plus a nested module with one.
func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                 "module example.invalid/root\n\ngo 1.21\n",
		"tagged/a.go":            "//go:build integration\n\npackage tagged\n",
		"broken/b.go":            "//go:build integration\n\npackage broken\n",
		"plain/plain.go":         "package plain\n",
		"testdata/tag.go":        "//go:build integration\n\npackage tag\n",
		"nested/go.mod":          "module example.invalid/nested\n\ngo 1.21\n",
		"nested/integrated/i.go": "//go:build integration\n\npackage integrated\n",
		// The polecat image's seed tree is generated in place and holds
		// copies of other checkouts, so the command must not vet it.
		"deploy/polecat/seed/gastown/staged/s.go": "//go:build integration\n\npackage staged\n",
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// vetCall is one program the command ran: where, what, and with which argv.
type vetCall struct {
	dir  string
	name string
	args []string
}

// recordingRun is a runner that records its calls and fails the run in failDir
// with a compiler error.
func recordingRun(t *testing.T, failDir string) (runner, func() []vetCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []vetCall
	run := func(_ context.Context, dir, name string, args ...string) (string, error) {
		mu.Lock()
		calls = append(calls, vetCall{dir: dir, name: name, args: append([]string(nil), args...)})
		mu.Unlock()
		if dir == failDir {
			return "broken/b.go:2:2: undefined: missing\n", errors.New("exit status 1")
		}
		return "", nil
	}
	got := func() []vetCall {
		mu.Lock()
		defer mu.Unlock()
		out := append([]vetCall(nil), calls...)
		sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
		return out
	}
	return run, got
}

// TestRunVetsEachModuleInOneCall pins the whole invocation a unit test can see
// without the go tool: one call per module, the module's own directory as the
// working directory, the integration tag, the parallel cap, and a pattern for
// every selected package. A go vet run at the wrong directory sees the wrong
// module, so the directory is part of the contract.
func TestRunVetsEachModuleInOneCall(t *testing.T) {
	t.Parallel()
	root := fixtureTree(t)
	run, calls := recordingRun(t, root)
	var out bytes.Buffer

	code := runWith(context.Background(), []string{"-root", root, "-jobs", "2"}, &out, run)

	if code != 1 {
		t.Fatalf("run = %d, want 1; output:\n%s", code, out.String())
	}
	for _, want := range []string{"--- .", "undefined: missing", "FAILED in 1 of 2 modules"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	want := []vetCall{
		{dir: root, name: "go", args: []string{"vet", "-tags", "integration", "-p", "2", "./broken", "./tagged"}},
		{dir: filepath.Join(root, "nested"), name: "go", args: []string{"vet", "-tags", "integration", "-p", "2", "./integrated"}},
	}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("runs = %+v, want %+v", got, want)
	}
}

func TestRunPassesWhenEveryPackageTypeChecks(t *testing.T) {
	t.Parallel()
	root := fixtureTree(t)
	run, _ := recordingRun(t, "")
	var out bytes.Buffer

	code := runWith(context.Background(), []string{"-root", root}, &out, run)

	if code != 0 {
		t.Fatalf("run = %d, want 0; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "3 packages in 2 modules type-check with -tags integration") {
		t.Errorf("output does not report the selected packages:\n%s", out.String())
	}
}

// TestVetArgs covers a package at the module root, which is a bare "." to go
// rather than a "./" pattern the fixture trees do not contain.
func TestVetArgs(t *testing.T) {
	t.Parallel()
	got := vetArgs(module{dir: ".", pkgs: []string{".", "internal/git"}}, 3)
	want := []string{"vet", "-tags", "integration", "-p", "3", ".", "./internal/git"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("vetArgs = %v, want %v", got, want)
	}
}

func TestRunRefusesASelectionThatFindsNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run, calls := recordingRun(t, "")
	var out bytes.Buffer

	code := runWith(context.Background(), []string{"-root", root}, &out, run)

	if code != 2 {
		t.Fatalf("run = %d, want 2; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "no package under") {
		t.Errorf("output does not name the empty selection:\n%s", out.String())
	}
	if len(calls()) != 0 {
		t.Errorf("an empty selection still vetted %+v", calls())
	}
}

func TestRunRejectsZeroJobs(t *testing.T) {
	t.Parallel()
	run, _ := recordingRun(t, "")
	var out bytes.Buffer
	if code := runWith(context.Background(), []string{"-jobs", "0"}, &out, run); code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
}
