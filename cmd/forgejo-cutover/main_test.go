package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejocut"
	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// noEnv is the empty environment: every default these tests read is one no
// variable moved.
func noEnv(string) string { return "" }

// neverRuns is the cutover seam a parse-failure test hands runParse: reaching the
// run from a command line that did not parse is the bug under test.
func neverRuns(t *testing.T) cutoverRun {
	t.Helper()
	return func(context.Context, *forgejocut.Options, forgejocut.RunEnv) int {
		t.Error("the cutover ran from a command line that did not parse")
		return forgejocut.ExitFailure
	}
}

// homeEnv is the environment with only HOME set, so a command line reaches the
// town's default.
func homeEnv(key string) string {
	if key == "HOME" {
		return "/home/test"
	}
	return ""
}

// stubGit reports the GitHub origin every test rig names.
type stubGit struct{}

func (stubGit) OriginURL(string) (string, error) {
	return "https://github.com/acme/gastown.git", nil
}

// stubEnv is the seam set a valid command line parses against: every path is a
// directory and a file, so no test touches the real filesystem.
func stubEnv(getenv func(string) string) forgejocut.Env {
	return forgejocut.Env{
		Getenv:     getenv,
		IsDir:      func(string) bool { return true },
		IsFile:     func(string) bool { return true },
		Town:       func() string { return "" },
		Executable: func() (string, error) { return "/repo/forgejo-cutover", nil },
		Git:        stubGit{},
	}
}

// TestMainUsageExitsTwoAndHelpExitsZero holds the script's usage_die and its -h: a
// refused command line exits 2 with the problem named and the usage hint, and the
// help request exits 0 with the flag list on stdout and nothing on stderr.
func TestMainUsageExitsTwoAndHelpExitsZero(t *testing.T) {
	t.Parallel()
	refusals := map[string]struct {
		args   []string
		stderr string
	}{
		"unknown option":             {args: []string{"gastown", "--repo", "acme/gastown", "--nope"}, stderr: "unknown option: --nope"},
		"the dropped --mirror":       {args: []string{"gastown", "--repo", "acme/gastown", "--mirror"}, stderr: "unknown option: --mirror"},
		"no rig":                     {args: []string{"--repo", "acme/gastown"}, stderr: "<rig> is required"},
		"no repository":              {args: []string{"gastown"}, stderr: "--repo is required"},
		"repository with no /":       {args: []string{"gastown", "--repo", "gastown"}, stderr: "--repo 'gastown' must be OWNER/NAME"},
		"api-url without http":       {args: []string{"gastown", "--repo", "acme/gastown", "--api-url", "forgejo.test"}, stderr: "--api-url must be an http:// or https:// URL with a host and no credentials"},
		"repository with a dot":      {args: []string{"gastown", "--repo", "acme/.."}, stderr: "--repo 'acme/..' must be OWNER/NAME"},
		"main-branch with space":     {args: []string{"gastown", "--repo", "acme/gastown", "--main-branch", "ma in"}, stderr: "--main-branch 'ma in' must match"},
		"main-branch with a dash":    {args: []string{"gastown", "--repo", "acme/gastown", "--main-branch", "-x"}, stderr: "--main-branch '-x' is not a branch name"},
		"main-branch with a dot-dot": {args: []string{"gastown", "--repo", "acme/gastown", "--main-branch", "a..b"}, stderr: "--main-branch 'a..b' is not a branch name"},
		"api-url with a password":    {args: []string{"gastown", "--repo", "acme/gastown", "--api-url", "https://user:hunter2@forgejo.test/api/v1"}, stderr: "--api-url must be an http:// or https:// URL with a host and no credentials"},
	}
	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(context.Background(), tc.args, stubEnv(noEnv), &stdout, &stderr); got != forgejocut.ExitUsage {
				t.Fatalf("run(%v) = %d, want 2 (stderr: %s)", tc.args, got, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("run(%v) stderr = %q, want it to hold %q", tc.args, stderr.String(), tc.stderr)
			}
			if !strings.Contains(stderr.String(), "usage: forgejo-cutover --help") {
				t.Errorf("run(%v) stderr = %q, want the usage hint", tc.args, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("run(%v) stdout = %q, want nothing on a refusal", tc.args, stdout.String())
			}
		})
	}

	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(context.Background(), []string{flag}, stubEnv(noEnv), &stdout, &stderr); got != forgejocut.ExitOK {
				t.Fatalf("run(%s) = %d, want 0 (stderr: %s)", flag, got, stderr.String())
			}
			// Each of these is read from the constant that holds the default, so a
			// default the text no longer names fails here.
			for _, want := range []string{
				"Usage: forgejo-cutover <rig> --repo OWNER/NAME [options]",
				"--accept-diverged",
				"(default: " + forgejocut.DefaultMainBranch + ")",
				"else " + forgejocut.DefaultAPIURL + ")",
				"(default: " + forgejocut.DefaultProbe + ")",
			} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("run(%s) stdout = %q, want it to hold %q", flag, stdout.String(), want)
				}
			}
			if strings.Contains(stdout.String(), "--mirror") {
				t.Errorf("run(%s) stdout names --mirror, which the port dropped", flag)
			}
			if stderr.Len() != 0 {
				t.Errorf("run(%s) stderr = %q, want nothing on a help request", flag, stderr.String())
			}
		})
	}
}

// execHelperMarker separates the helper's arguments from the test binary's.
const execHelperMarker = "forgejo-cutover-exec-helper"

// execHelperArgv is the argv that starts the test binary as a child process in
// the mode named: it prints its environment or exits 1.
func execHelperArgv(t *testing.T, mode string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	return []string{exe, "-test.run=^TestExecHelperProcess$", execHelperMarker, mode}
}

// TestExecHelperProcess is not a test: started with the helper marker, it stands
// in for a child process, so the exec seam is exercised against a real one.
func TestExecHelperProcess(t *testing.T) {
	// Parallel because the tree-wide policy asks every test for it; the body
	// below runs only in the child process, where this is the one test selected.
	t.Parallel()
	i := slices.Index(os.Args, execHelperMarker)
	if i < 0 || i+1 >= len(os.Args) {
		return
	}
	switch os.Args[i+1] {
	case "environment":
		for _, entry := range os.Environ() {
			fmt.Println(entry)
		}
	case "exit-1":
		os.Exit(1)
	}
}

// realExec is the exec seam under test, fed the environment a test stages and
// writing a child's stdout into stdout.
func realExec(ctx context.Context, environ []string, stdout, stderr io.Writer) execRunner {
	return execRunner{ctx: ctx, environ: func() []string { return environ }, stdout: stdout, stderr: stderr}
}

// TestMainExecRunnerRefusesAnEmptyArgv: an argv with no program is refused on
// every method, rather than indexed out of range.
func TestMainExecRunnerRefusesAnEmptyArgv(t *testing.T) {
	t.Parallel()
	r := execRunner{stdout: io.Discard, stderr: io.Discard}
	if _, err := r.Output(nil); err == nil || !strings.Contains(err.Error(), "no command to run") {
		t.Errorf("Output(nil) = %v, want the empty argv refused", err)
	}
	if err := r.Run(nil); err == nil || !strings.Contains(err.Error(), "no command to run") {
		t.Errorf("Run(nil) = %v, want the empty argv refused", err)
	}
	if err := r.RunWithToken(nil, forgejoprov.Secret{}); err == nil || !strings.Contains(err.Error(), "no command to run") {
		t.Errorf("RunWithToken(nil) = %v, want the empty argv refused", err)
	}
}

// TestMainExecRunnerRunsARealChild: Output returns a real child's stdout, Run
// lets it write to the run's streams, and a non-zero status is an error.
func TestMainExecRunnerRunsARealChild(t *testing.T) {
	t.Parallel()
	environ := []string{"EXEC_HELPER=1"}

	var stdout, stderr strings.Builder
	r := realExec(context.Background(), environ, &stdout, &stderr)
	out, err := r.Output(execHelperArgv(t, "environment"))
	if err != nil {
		t.Fatalf("Output = %v, want the child's stdout", err)
	}
	if !strings.Contains(string(out), "EXEC_HELPER=1") {
		t.Error("Output did not return the child's environment")
	}

	stdout.Reset()
	if err := r.Run(execHelperArgv(t, "environment")); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if !strings.Contains(stdout.String(), "EXEC_HELPER=1") {
		t.Error("Run did not let the child's output reach the run's stream")
	}
	if stderr.Len() != 0 {
		t.Error("the child wrote to stderr, want nothing there")
	}

	if err := r.Run(execHelperArgv(t, "exit-1")); err == nil {
		t.Error("Run = nil for a child that exits 1, want its failure")
	}
}

// TestMainExecRunnerScrubsTheTokenFromEveryChild: no child inherits either
// variable an admin token arrives in, and every other variable is kept.
func TestMainExecRunnerScrubsTheTokenFromEveryChild(t *testing.T) {
	t.Parallel()
	const brokerLeak = "leak-broker-token"
	const standInLeak = "leak-standin-token" //nolint:gosec // G101: a fixture, not a credential
	environ := []string{
		"GT_AUTH_FORGEJO_TOKEN=" + brokerLeak,
		"FORGEJO_ADMIN_TOKEN=" + standInLeak,
		"UNRELATED=kept",
	}

	for _, method := range []string{"Output", "Run"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			r := realExec(context.Background(), environ, &stdout, &stderr)
			var childEnv string
			if method == "Output" {
				out, err := r.Output(execHelperArgv(t, "environment"))
				if err != nil {
					t.Fatalf("Output = %v, want the child's stdout", err)
				}
				childEnv = string(out)
			} else {
				if err := r.Run(execHelperArgv(t, "environment")); err != nil {
					t.Fatalf("Run = %v, want nil", err)
				}
				childEnv = stdout.String()
			}
			if strings.Contains(childEnv, brokerLeak) || strings.Contains(childEnv, standInLeak) {
				t.Error("a token reached the child's environment")
			}
			if strings.Contains(childEnv, "GT_AUTH_FORGEJO_TOKEN=") || strings.Contains(childEnv, "FORGEJO_ADMIN_TOKEN=") {
				t.Error("a token variable survived into the child's environment")
			}
			if !strings.Contains(childEnv, "UNRELATED=kept") {
				t.Error("an unrelated variable was dropped from the child's environment")
			}
		})
	}
}

// TestMainExecRunnerHandsTheTokenOnlyToTheRunningProbe: RunWithToken gives its
// child the resolved token as the probe's variable and nothing else, and the
// stand-in variable a child might have inherited is still removed.
func TestMainExecRunnerHandsTheTokenOnlyToTheRunningProbe(t *testing.T) {
	t.Parallel()
	const token = "tok-probe-only-marker"
	const standInLeak = "leak-standin-token" //nolint:gosec // G101: a fixture, not a credential
	tokenSecret, err := forgejoprov.ResolveAdminToken(func(string) string { return token }, nil, "")
	if err != nil {
		t.Fatalf("resolve the fixture token: %v", err)
	}
	environ := []string{"FORGEJO_ADMIN_TOKEN=" + standInLeak, "UNRELATED=kept"}

	var stdout, stderr strings.Builder
	r := realExec(context.Background(), environ, &stdout, &stderr)
	if err := r.RunWithToken(execHelperArgv(t, "environment"), tokenSecret); err != nil {
		t.Fatalf("RunWithToken = %v, want nil", err)
	}
	childEnv := stdout.String()
	if !strings.Contains(childEnv, "GT_AUTH_FORGEJO_TOKEN="+token) {
		t.Error("the probe's child did not receive the resolved token")
	}
	if strings.Contains(childEnv, standInLeak) || strings.Contains(childEnv, "FORGEJO_ADMIN_TOKEN=") {
		t.Error("the stand-in token variable reached the child")
	}
	if !strings.Contains(childEnv, "UNRELATED=kept") {
		t.Error("an unrelated variable was dropped from the child's environment")
	}
}

// TestMainExecRunnerChildHonorsACanceledContext: a canceled context stops a
// child before it starts, so an interrupt does not wait on it.
func TestMainExecRunnerChildHonorsACanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr strings.Builder
	r := realExec(ctx, nil, &stdout, &stderr)
	var err error
	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"Output", func() error { _, err := r.Output(execHelperArgv(t, "environment")); return err }},
		{"Run", func() error { return r.Run(execHelperArgv(t, "environment")) }},
		{"RunWithToken", func() error { return r.RunWithToken(execHelperArgv(t, "environment"), forgejoprov.Secret{}) }},
	} {
		err = call.run()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s = %v, want the canceled context's error", call.name, err)
		}
	}
	if stdout.Len() != 0 {
		t.Error("a canceled run started the child")
	}
}

// TestRunParsePassesItsContextToTheCutover: the run receives the command's own
// context, so an interrupt reaches the run and its children.
func TestRunParsePassesItsContextToTheCutover(t *testing.T) {
	t.Parallel()
	type ctxKey struct{}
	var got context.Context
	cutover := func(ctx context.Context, _ *forgejocut.Options, _ forgejocut.RunEnv) int {
		got = ctx
		return forgejocut.ExitOK
	}
	ctx := context.WithValue(context.Background(), ctxKey{}, "the run's context")
	var stdout, stderr strings.Builder
	if code := runParse(ctx, []string{"gastown", "--repo", "acme/gastown"}, stubEnv(homeEnv), &stdout, &stderr, forgejocut.Parse, cutover); code != forgejocut.ExitOK {
		t.Fatalf("runParse = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if got == nil || got.Value(ctxKey{}) != "the run's context" {
		t.Error("the cutover ran with a context the command did not pass")
	}
}

// TestMainValidCommandLineRunsTheCutover: a command line that parses reaches the
// cutover run, with the parsed options and the process's streams. The run's own
// exit code is the command's.
func TestMainValidCommandLineRunsTheCutover(t *testing.T) {
	t.Parallel()
	var gotOpts *forgejocut.Options
	var gotEnv forgejocut.RunEnv
	cutover := func(_ context.Context, opts *forgejocut.Options, env forgejocut.RunEnv) int {
		gotOpts, gotEnv = opts, env
		fmt.Fprintln(env.Stdout, "the cutover ran")
		return forgejocut.ExitOK
	}
	var parseOut, parseErr strings.Builder
	got := runParse(context.Background(), []string{"gastown", "--repo", "acme/gastown"}, stubEnv(homeEnv), &parseOut, &parseErr, forgejocut.Parse, cutover)
	if got != forgejocut.ExitOK {
		t.Fatalf("runParse = %d, want the run's 0 (stderr: %s)", got, parseErr.String())
	}
	if gotOpts == nil || gotOpts.Rig != "gastown" || gotOpts.Repo != "acme/gastown" {
		t.Fatalf("the run received %+v, want the parsed gastown options", gotOpts)
	}
	if gotEnv.Stdout != &parseOut || gotEnv.Stderr != &parseErr {
		t.Errorf("the run received the streams %v/%v, want the command's own", gotEnv.Stdout, gotEnv.Stderr)
	}
	if gotEnv.Runner == nil {
		t.Error("the run received no exec seam")
	}
	if strings.Contains(parseErr.String(), "usage: forgejo-cutover --help") {
		t.Errorf("stderr = %q, want no usage hint: the command line was valid", parseErr.String())
	}
	if !strings.Contains(parseOut.String(), "the cutover ran") {
		t.Errorf("stdout = %q, want the run's own output", parseOut.String())
	}
}

// TestRunExitCodes holds the exit contract the command owns: a refused command
// line is 2 and never reaches the run, the help request is 0, and a run that
// reports failure is 1. The no-admin-token refusal, which is also 1, is pinned in
// internal/forgejocut (TestRunExitCodes there).
func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		code    int
		ran     bool
		parse   parseOptions
		cutover cutoverRun
	}{
		{
			name: "a refused command line exits 2 without running",
			args: []string{"gastown", "--nope"},
			code: forgejocut.ExitUsage,
		},
		{
			name: "help exits 0 without running",
			args: []string{"--help"},
			code: forgejocut.ExitOK,
		},
		{
			name: "a failed run exits 1",
			args: []string{"gastown", "--repo", "acme/gastown"},
			code: forgejocut.ExitFailure,
			ran:  true,
			cutover: func(context.Context, *forgejocut.Options, forgejocut.RunEnv) int {
				return forgejocut.ExitFailure
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ran := false
			cutover := tt.cutover
			if cutover == nil {
				cutover = func(context.Context, *forgejocut.Options, forgejocut.RunEnv) int {
					ran = true
					return forgejocut.ExitOK
				}
			} else {
				inner := cutover
				cutover = func(ctx context.Context, opts *forgejocut.Options, env forgejocut.RunEnv) int {
					ran = true
					return inner(ctx, opts, env)
				}
			}
			var stdout, stderr strings.Builder
			got := runParse(context.Background(), tt.args, stubEnv(homeEnv), &stdout, &stderr, forgejocut.Parse, cutover)
			if got != tt.code {
				t.Fatalf("runParse(%v) = %d, want %d (stderr: %s)", tt.args, got, tt.code, stderr.String())
			}
			if ran != tt.ran {
				t.Errorf("runParse(%v) ran the cutover = %v, want %v", tt.args, ran, tt.ran)
			}
		})
	}
}

// TestMainFailedRigExitsOne: a rig whose directory is missing is a failed run, not
// a refused command line, so it exits 1 with no usage hint.
func TestMainFailedRigExitsOne(t *testing.T) {
	t.Parallel()
	env := stubEnv(homeEnv)
	env.IsDir = func(path string) bool { return path == "/home/test/gt" }
	var stdout, stderr strings.Builder
	got := run(context.Background(), []string{"absent", "--repo", "acme/gastown"}, env, &stdout, &stderr)
	if got != forgejocut.ExitFailure {
		t.Fatalf("run = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no rig directory") {
		t.Errorf("stderr = %q, want the missing rig named", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-cutover --help") {
		t.Errorf("stderr = %q, want no usage hint for a failed run", stderr.String())
	}
}

// TestRunParseWithoutOptionsExitsOne: a parse seam that returns neither options nor
// an error has no command line to run. It exits 1 and says so; reading the missing
// options would panic instead of reporting the failed run.
func TestRunParseWithoutOptionsExitsOne(t *testing.T) {
	t.Parallel()
	empty := func([]string, forgejocut.Env) (*forgejocut.Options, error) {
		return nil, nil
	}
	var stdout, stderr strings.Builder
	if got := runParse(context.Background(), nil, stubEnv(noEnv), &stdout, &stderr, empty, neverRuns(t)); got != forgejocut.ExitFailure {
		t.Fatalf("runParse = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "parsed to no options") {
		t.Errorf("stderr = %q, want the missing options named", stderr.String())
	}
}

// TestRunParseFailedParseIsAFailedRun: a parse error that is not a usage error
// exits 1 and names the error, with no usage hint.
func TestRunParseFailedParseIsAFailedRun(t *testing.T) {
	t.Parallel()
	failing := func([]string, forgejocut.Env) (*forgejocut.Options, error) {
		return nil, errors.New("no origin URL")
	}
	var stdout, stderr strings.Builder
	if got := runParse(context.Background(), nil, stubEnv(noEnv), &stdout, &stderr, failing, neverRuns(t)); got != forgejocut.ExitFailure {
		t.Fatalf("runParse = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "no origin URL") || strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q, want the error named and no usage hint", stderr.String())
	}
}
