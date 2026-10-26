package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

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

// reportSpool puts the run's report file in a directory of the test's own, and
// reports the path it created so a case can prove the run removed that file
// rather than never making one.
func reportSpool(t *testing.T) (create func() (*spool, error), path func() string) {
	t.Helper()
	dir := t.TempDir()
	var made string
	create = func() (*spool, error) {
		s, err := createSpoolIn(dir)
		if err == nil {
			made = s.path
		}
		return s, err
	}
	return create, func() string { return made }
}

// assertReportRemoved fails if the run never created a report file, which
// would make the check pass on an absence rather than a removal, or left it
// behind.
func assertReportRemoved(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		t.Fatal("the run never created its report file")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("report file %s still exists (stat: %v)", path, err)
	}
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
	spooled, reportPath := reportSpool(t)
	var out bytes.Buffer

	code := runWith(context.Background(), []string{"-root", root, "-jobs", "2"}, &out, run, spooled)
	assertReportRemoved(t, reportPath())

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
	spooled, reportPath := reportSpool(t)
	var out bytes.Buffer

	code := runWith(context.Background(), []string{"-root", root}, &out, run, spooled)
	assertReportRemoved(t, reportPath())

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
	spooled, reportPath := reportSpool(t)
	var out bytes.Buffer

	code := runWith(context.Background(), []string{"-root", root}, &out, run, spooled)
	assertReportRemoved(t, reportPath())

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
	if code := runWith(context.Background(), []string{"-jobs", "0"}, &out, run, createSpool); code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
}

// TestRunVetsModulesAtOnce proves the modules are vetted concurrently: each
// fake vet blocks on a channel of its own, the test takes both entries before
// releasing either, and releases them in the opposite order of start. A
// serialized vetAll never delivers the second entry, so the test hangs instead
// of passing (gt-w3aby).
func TestRunVetsModulesAtOnce(t *testing.T) {
	t.Parallel()
	bound := awaitBound(t)
	root := fixtureTree(t)
	release := map[string]chan struct{}{
		root:                          make(chan struct{}),
		filepath.Join(root, "nested"): make(chan struct{}),
	}
	started := make(chan string)
	finished := make(chan string)
	run := func(_ context.Context, dir, _ string, _ ...string) (string, error) {
		if err := sendWithin(bound, started, dir); err != nil {
			return "", err
		}
		if err := awaitWithin(bound, release[dir]); err != nil {
			return "", err
		}
		return "", sendWithin(bound, finished, dir)
	}
	spooled, _ := reportSpool(t)

	var out bytes.Buffer
	code := make(chan int, 1)
	go func() { code <- runWith(context.Background(), []string{"-root", root}, &out, run, spooled) }()

	first, second := recvWithin(t, bound, started), recvWithin(t, bound, started)
	close(release[second])
	if got := recvWithin(t, bound, finished); got != second {
		t.Fatalf("the first release finished %q, want %q", got, second)
	}
	close(release[first])
	if got := recvWithin(t, bound, finished); got != first {
		t.Fatalf("the second release finished %q, want %q", got, first)
	}
	if got := recvWithin(t, bound, code); got != 0 {
		t.Fatalf("run = %d, want 0; output:\n%s", got, out.String())
	}
}

// TestRunRemovesTheReportOnInterrupt pins the interrupt path: the cancelled
// context stops the vets, run returns 130, and the report file is gone, which
// the gate's shell temp log used to leak (gt-w3aby).
func TestRunRemovesTheReportOnInterrupt(t *testing.T) {
	t.Parallel()
	root := fixtureTree(t)
	spooled, reportPath := reportSpool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := func(ctx context.Context, _, _ string, _ ...string) (string, error) {
		cancel()
		return "", ctx.Err()
	}
	var out bytes.Buffer

	code := runWith(ctx, []string{"-root", root}, &out, run, spooled)

	if code != 130 {
		t.Fatalf("run = %d, want 130; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "integration vet: interrupted") {
		t.Errorf("output does not report the interruption:\n%s", out.String())
	}
	assertReportRemoved(t, reportPath())
}

// TestRunRemovesTheReportWhenSelectionFails covers the other failure exit: a
// root the walk cannot read returns 2 and removes the report file.
func TestRunRemovesTheReportWhenSelectionFails(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	spooled, reportPath := reportSpool(t)
	run, calls := recordingRun(t, "")
	var out bytes.Buffer

	code := runWith(context.Background(), []string{"-root", missing}, &out, run, spooled)

	if code != 2 {
		t.Fatalf("run = %d, want 2; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "selecting packages") {
		t.Errorf("output does not name the selection failure:\n%s", out.String())
	}
	if len(calls()) != 0 {
		t.Errorf("a failed selection still vetted %+v", calls())
	}
	assertReportRemoved(t, reportPath())
}

// TestCommandKillsTheVetProcessGroup pins that cancellation reaches the vet
// tool go vet spawns: the command runs in a process group of its own and has a
// Cancel hook that signals the group (gt-w3aby).
func TestCommandKillsTheVetProcessGroup(t *testing.T) {
	t.Parallel()
	var got *exec.Cmd
	cancel := func(cmd *exec.Cmd) error { got = cmd; return nil }
	cmd := command(context.Background(), t.TempDir(), cancel, "go", "vet")
	if cmd.Cancel == nil {
		t.Fatal("command has no Cancel hook, so cancellation would stop only go vet")
	}
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
	if got != cmd {
		t.Errorf("Cancel did not reach the group killer with this command")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Errorf("command SysProcAttr = %+v, want Setpgid", cmd.SysProcAttr)
	}
	if cmd.WaitDelay == 0 {
		t.Error("command has no WaitDelay, so a descendant holding the output pipe can outlive cancellation")
	}
}

// awaitBound bounds every wait in the concurrency test: a regression that serializes the
// vets fails with a message instead of hanging the suite. The bound is the test's own
// deadline when there is one, else awaitFallback from now.
const awaitFallback = 30 * time.Second

func awaitBound(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		deadline = time.Now().Add(awaitFallback)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	t.Cleanup(cancel)
	return ctx
}

// recvWithin receives from ch or fails the test when ctx is done first.
func recvWithin[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatalf("nothing arrived before the test's bound: the vets did not run concurrently, or one hung")
		var zero T
		return zero
	}
}

// sendWithin sends v on ch, or returns an error when ctx is done first.
func sendWithin[T any](ctx context.Context, ch chan<- T, v T) error {
	select {
	case ch <- v:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the test did not take %v before its bound", v)
	}
}

// awaitWithin waits for ch to close, or returns an error when ctx is done first.
func awaitWithin[T any](ctx context.Context, ch <-chan T) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the test never released this vet before its bound")
	}
}
