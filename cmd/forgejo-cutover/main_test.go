package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

// recordingStart is the exec seam a test injects: it keeps every child the run
// built, with the capture mode it asked for, and answers with what the test
// staged, so no process starts.
type recordingStart struct {
	captures []bool
	cmds     []*exec.Cmd
	out      []byte
	err      error
}

func (s *recordingStart) start(cmd *exec.Cmd, capture bool) ([]byte, error) {
	s.captures = append(s.captures, capture)
	s.cmds = append(s.cmds, cmd)
	return s.out, s.err
}

// TestMainExecRunnerRefusesAnEmptyArgv: an argv with no program is refused on
// every method, rather than indexed out of range.
func TestMainExecRunnerRefusesAnEmptyArgv(t *testing.T) {
	t.Parallel()
	r := execRunner{}
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

// TestMainExecRunnerBuildsEachChildInProcess: every method hands its seam the
// child the run built: the argv it was given, the environment with the token
// variables removed and only the probe's child carrying the resolved token, the
// operator's streams on a running child, and the capture mode. Removing the
// scrub, the extra probe entry, or a stream assignment turns it red.
func TestMainExecRunnerBuildsEachChildInProcess(t *testing.T) {
	t.Parallel()
	const brokerLeak = "leak-broker-token"
	const standInLeak = "leak-standin-token" //nolint:gosec // G101: a fixture, not a credential
	const probeToken = "tok-probe-only-marker"
	tokenSecret, err := forgejoprov.ResolveAdminToken(func(string) string { return probeToken }, nil, "")
	if err != nil {
		t.Fatalf("resolve the fixture token: %v", err)
	}

	var stdout, stderr strings.Builder
	start := &recordingStart{out: []byte("child output")}
	r := execRunner{
		environ: func() []string {
			return []string{
				"GT_AUTH_FORGEJO_TOKEN=" + brokerLeak,
				"FORGEJO_ADMIN_TOKEN=" + standInLeak,
				"UNRELATED=kept",
			}
		},
		stdout: &stdout,
		stderr: &stderr,
		start:  start.start,
	}

	out, err := r.Output([]string{"bd", "list"})
	if err != nil || string(out) != "child output" {
		t.Fatalf("Output = %q, %v, want the seam's output", out, err)
	}
	if err := r.Run([]string{"bash", "probe.sh"}); err != nil {
		t.Fatalf("Run = %v, want the seam's answer", err)
	}
	if err := r.RunWithToken([]string{"bash", "probe.sh"}, tokenSecret); err != nil {
		t.Fatalf("RunWithToken = %v, want the seam's answer", err)
	}

	if len(start.cmds) != 3 {
		t.Fatalf("the seam was handed %d children, want 3", len(start.cmds))
	}
	wantArgv := [][]string{
		{"bd", "list"},
		{"bash", "probe.sh"},
		{"bash", "probe.sh"},
	}
	for i, want := range wantArgv {
		if got := start.cmds[i].Args; !slices.Equal(got, want) {
			t.Errorf("child %d argv = %q, want %q", i, got, want)
		}
	}
	if !slices.Equal(start.captures, []bool{true, false, false}) {
		t.Errorf("the capture modes = %v, want the read captured and the runs streamed", start.captures)
	}
	for i, cmd := range start.cmds {
		if slices.Contains(cmd.Env, "GT_AUTH_FORGEJO_TOKEN="+brokerLeak) || slices.Contains(cmd.Env, "FORGEJO_ADMIN_TOKEN="+standInLeak) {
			t.Errorf("child %d inherited a token variable: %q", i, cmd.Env)
		}
		if !slices.Contains(cmd.Env, "UNRELATED=kept") {
			t.Errorf("child %d lost an unrelated variable: %q", i, cmd.Env)
		}
	}
	probeEntry := probeTokenEnv + "=" + probeToken
	if !slices.Contains(start.cmds[2].Env, probeEntry) {
		t.Errorf("the probe's child did not receive the resolved token: %q", start.cmds[2].Env)
	}
	if slices.Contains(start.cmds[1].Env, probeEntry) {
		t.Errorf("a child other than the probe received the token: %q", start.cmds[1].Env)
	}

	if start.cmds[0].Stdout != nil || start.cmds[0].Stderr != nil {
		t.Error("the captured read was given the operator's streams")
	}
	for _, i := range []int{1, 2} {
		if start.cmds[i].Stdout != &stdout || start.cmds[i].Stderr != &stderr {
			t.Errorf("child %d did not get the operator's streams", i)
		}
	}
}

// TestMainExecRunnerHonorsACanceledContext: a canceled context stops a child
// before it starts, so an interrupt does not wait on it. The path is one that
// does not exist: the canceled context is what the error must name.
func TestMainExecRunnerHonorsACanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	argv := []string{filepath.Join(t.TempDir(), "child-that-is-never-started")}
	var stdout, stderr strings.Builder
	r := execRunner{ctx: ctx, stdout: &stdout, stderr: &stderr}
	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"Output", func() error { _, err := r.Output(argv); return err }},
		{"Run", func() error { return r.Run(argv) }},
		{"RunWithToken", func() error { return r.RunWithToken(argv, forgejoprov.Secret{}) }},
	} {
		if err := call.run(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s = %v, want the canceled context's error", call.name, err)
		}
	}
	if stdout.Len() != 0 {
		t.Error("a canceled run wrote to the operator's stream")
	}
}

// TestMainWatchInterrupts: the first signal cancels the run and the second ends
// the process with 128 plus the signal number; a run that returns ends the watch
// without waiting for a signal. The signals are already buffered, so the watch
// runs to its decision in the test's own goroutine.
func TestMainWatchInterrupts(t *testing.T) {
	t.Parallel()
	t.Run("the second signal ends the process", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		signals := make(chan os.Signal, 2)
		signals <- os.Interrupt
		signals <- syscall.SIGTERM
		var exited []int
		watchInterrupts(make(chan struct{}), signals, cancel, func(code int) { exited = append(exited, code) })
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Error("the first signal did not cancel the run")
		}
		if !slices.Equal(exited, []int{128 + int(syscall.SIGTERM)}) {
			t.Errorf("the second signal exited %v, want one exit with 128+SIGTERM", exited)
		}
	})

	t.Run("a finished run ends the watch", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		close(done)
		var exited []int
		watchInterrupts(done, make(chan os.Signal), cancel, func(code int) { exited = append(exited, code) })
		if ctx.Err() != nil {
			t.Errorf("the run's context was canceled after it returned: %v", ctx.Err())
		}
		if len(exited) != 0 {
			t.Errorf("a finished run exited %v", exited)
		}
	})

	t.Run("a run that returns after the first signal ends the watch", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		signals := make(chan os.Signal, 1)
		signals <- os.Interrupt
		done := make(chan struct{})
		var exited []int
		// The cancel is also the run returning: the watch has taken its first
		// signal (done is still open when it looks) and finds done closed when it
		// waits for the second.
		cancelAndFinish := func() {
			cancel()
			close(done)
		}
		watchInterrupts(done, signals, cancelAndFinish, func(code int) { exited = append(exited, code) })
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Error("the first signal did not cancel the run")
		}
		if len(exited) != 0 {
			t.Errorf("the watch exited %v after the run returned", exited)
		}
	})
}

// interruptSignal is an os.Signal that is not a syscall.Signal, the shape
// interruptExitCode falls back on.
type interruptSignal string

func (s interruptSignal) String() string { return string(s) }
func (interruptSignal) Signal()          {}

// TestMainInterruptExitCode: the shell's 128+signum, so a second Ctrl-C reads as
// an interrupt rather than as the run's own failure; a signal the syscall
// package does not number is the plain interrupt status.
func TestMainInterruptExitCode(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		sig  os.Signal
		code int
	}{
		{os.Interrupt, 130},
		{syscall.SIGTERM, 143},
		{interruptSignal("SIGOTHER"), 130},
	} {
		if got := interruptExitCode(tt.sig); got != tt.code {
			t.Errorf("interruptExitCode(%v) = %d, want %d", tt.sig, got, tt.code)
		}
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
