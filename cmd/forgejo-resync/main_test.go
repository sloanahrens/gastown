package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoresync"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// releaseBound is the most a test waits for the handler release. A run that holds it
// fails the test at this bound instead of hanging.
const releaseBound = 10 * time.Second

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// noEnv is the empty environment: every default these tests read is one no
// variable moved.
func noEnv(string) string { return "" }

// homeEnv is the environment with only HOME set, for the command lines that must
// reach the backup directory's default.
func homeEnv(key string) string {
	if key == "HOME" {
		return "/home/test"
	}
	return ""
}

// noFiles is the file seam for a command line that must read no file: any read
// fails the test, so a token file that no flag or variable named is never looked
// for on the real filesystem.
func noFiles(t *testing.T) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		t.Errorf("read %s: the command read a file no flag or variable named", path)
		return nil, fs.ErrNotExist
	}
}

// TestMainUsageExitsTwoAndHelpExitsZero holds the script's usage_die and its
// -h: a refused command line exits 2 with the problem named and the usage hint,
// and the help request exits 0 with the flag list on stdout and nothing on
// stderr.
func TestMainUsageExitsTwoAndHelpExitsZero(t *testing.T) {
	t.Parallel()
	refusals := map[string]struct {
		args   []string
		stderr string
	}{
		"unknown option":         {args: []string{"--nope", "acme/gastown"}, stderr: "unknown option: --nope"},
		"no repository":          {args: []string{"--dry-run"}, stderr: "OWNER/NAME is required"},
		"repository with slash":  {args: []string{"gastown"}, stderr: "'gastown' must be OWNER/NAME"},
		"api-url without http":   {args: []string{"--api-url", "forgejo.test", "acme/gastown"}, stderr: "--api-url must be http:// or https://"},
		"main-branch with space": {args: []string{"--main-branch", "ma in", "acme/gastown"}, stderr: "--main-branch 'ma in' must match"},
		"repository with space":  {args: []string{"acme/git town"}, stderr: "'acme/git town' must be OWNER/NAME"},
		"three-part repository":  {args: []string{"a/b/c"}, stderr: "'a/b/c' must be OWNER/NAME"},
		"web-url without http":   {args: []string{"--web-url", "web.test", "acme/gastown"}, stderr: "the web root (--web-url or FORGEJO_URL) must be http:// or https://"},
		"no HOME, no backup dir": {args: []string{"acme/gastown"}, stderr: "--backup-dir is required"},
	}
	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(tc.args, noEnv, noFiles(t), &stdout, &stderr); got != forgejoresync.ExitUsage {
				t.Fatalf("run(%v) = %d, want 2 (stderr: %s)", tc.args, got, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("run(%v) stderr = %q, want it to hold %q", tc.args, stderr.String(), tc.stderr)
			}
			if !strings.Contains(stderr.String(), "usage: forgejo-resync --help") {
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
			if got := run([]string{flag}, noEnv, noFiles(t), &stdout, &stderr); got != forgejoresync.ExitOK {
				t.Fatalf("run(%s) = %d, want 0 (stderr: %s)", flag, got, stderr.String())
			}
			// Each of these is read from the constant that holds the default, so a
			// default the text no longer names fails here.
			for _, want := range []string{
				"Usage: forgejo-resync [options] OWNER/NAME",
				"--dry-run",
				"else " + forgejoresync.DefaultAPIURL + ")",
				"(default: " + forgejoresync.DefaultMainBranch + ")",
				"$HOME/forgejo/backups)",
			} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("run(%s) stdout = %q, want it to hold %q", flag, stdout.String(), want)
				}
			}
			if stderr.Len() != 0 {
				t.Errorf("run(%s) stderr = %q, want nothing on a help request", flag, stderr.String())
			}
		})
	}
}

// TestMainValidCommandLineWithoutTokenExitsOne: a command line that parses and
// reaches the run, with no admin token, is a failed run: exit 1, the broker
// command named, and no usage hint, since the command line itself was valid.
// No token file is named, so the run reads none.
func TestMainValidCommandLineWithoutTokenExitsOne(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	got := run([]string{"--dry-run", "acme/gastown"}, homeEnv, noFiles(t), &stdout, &stderr)
	if got != forgejoresync.ExitFailure {
		t.Fatalf("run = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no admin token") {
		t.Errorf("stderr = %q, want the missing token named", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-resync --help") {
		t.Errorf("stderr = %q, want no usage hint: the command line was valid", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing from a failed run", stdout.String())
	}
}

// TestMainNamedTokenFileIsReadThroughTheSeam: a token file the environment names is
// read through the file seam the command was given, not the process's own reader.
// The fake returns a file with no token, so the run refuses for want of one and
// names the file as the stand-in.
func TestMainNamedTokenFileIsReadThroughTheSeam(t *testing.T) {
	t.Parallel()
	const tokenFile = "/fake/forgejo/admin.env"
	getenv := func(key string) string {
		switch key {
		case "HOME":
			return "/home/test"
		case "FORGEJO_ADMIN_ENV":
			return tokenFile
		}
		return ""
	}
	var reads []string
	readFile := func(path string) ([]byte, error) {
		reads = append(reads, path)
		return []byte("# no token in this file\n"), nil
	}
	var stdout, stderr strings.Builder
	got := run([]string{"--dry-run", "acme/gastown"}, getenv, readFile, &stdout, &stderr)
	if got != forgejoresync.ExitFailure {
		t.Fatalf("run = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if len(reads) != 1 || reads[0] != tokenFile {
		t.Errorf("reads = %v, want exactly [%s]", reads, tokenFile)
	}
	if !strings.Contains(stderr.String(), "FORGEJO_ADMIN_TOKEN= in "+tokenFile) {
		t.Errorf("stderr = %q, want the token file named as the stand-in", stderr.String())
	}
}

// TestRunExitCodes pins the exit codes the command reports: help is 0, a refused
// command line is 2, and a run that cannot start for want of an admin token is 1.
// The token case stops before any git or HTTP call, and reads no file, so it is
// hermetic.
func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		args   []string
		getenv func(string) string
		want   int
	}{
		{name: "help", args: []string{"--help"}, getenv: noEnv, want: forgejoresync.ExitOK},
		{name: "bad flag", args: []string{"--nope", "acme/gastown"}, getenv: homeEnv, want: forgejoresync.ExitUsage},
		{name: "no admin token", args: []string{"--dry-run", "acme/gastown"}, getenv: homeEnv, want: forgejoresync.ExitFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			if got := run(tc.args, tc.getenv, noFiles(t), &stdout, &stderr); got != tc.want {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", tc.args, got, tc.want, stderr.String())
			}
		})
	}
}

// fakeNotify stands in for signal.NotifyContext: it reports the context the test
// holds and records that the handler was released. released is closed once, so the
// run's own release and the deferred one can both call it.
type fakeNotify struct {
	ctx      context.Context
	caught   []os.Signal
	released chan struct{}
	once     sync.Once
	calls    atomic.Int32 // how many times the handler was released
}

func (f *fakeNotify) notify(_ context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	f.caught = append(f.caught, signals...)
	return f.ctx, func() {
		f.calls.Add(1)
		f.once.Do(func() { close(f.released) })
	}
}

// TestMainCatchesTheSignalsTheScriptCaught: the handler covers the three signals
// bash ran the script's EXIT trap for, the run it starts is handed their context
// (a canceled one stops the run before any token or git call), and the handler is
// released. Mutation: drop a signal from the list and the signal check goes red;
// drop the wiring and no call is recorded.
func TestMainCatchesTheSignalsTheScriptCaught(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeNotify{ctx: ctx, released: make(chan struct{})}
	opts := &forgejoresync.Options{Repo: "acme/gastown", MainBranch: "main"}
	parse := func([]string, func(string) string) (*forgejoresync.Options, error) { return opts, nil }
	var stdout, stderr strings.Builder
	if got := runNotify(nil, noEnv, noFiles(t), &stdout, &stderr, parse, fake.notify); got != forgejoresync.ExitFailure {
		t.Fatalf("runNotify = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	want := []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
	if !slices.Equal(fake.caught, want) {
		t.Fatalf("handler caught %v, want %v", fake.caught, want)
	}
	select {
	case <-fake.released:
	default:
		t.Fatal("the signal handler was not released")
	}
	if !strings.Contains(stderr.String(), "run canceled") {
		t.Errorf("stderr = %q, want the canceled run named", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing on a failed run", stdout.String())
	}
}

// settle is how long the held-handler test waits for an asynchronous release that
// must not come.
const settle = 20 * time.Millisecond

// TestMainKeepsTheHandlerThroughTheRestore: the restore can run for minutes after
// the first signal, and a second signal during it must not kill the process and
// skip the restore: the handler is held until the run returns, and released once,
// by the deferred release. Releasing it at the first signal as well (a second
// release call) is the defect. Mutation: add context.AfterFunc(ctx, stop) after the
// notify call and the release count reads 2.
func TestMainKeepsTheHandlerThroughTheRestore(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeNotify{ctx: ctx, released: make(chan struct{})}
	var once sync.Once
	getenv := func(string) string {
		once.Do(func() {
			cancel() // the first signal lands while the run is resolving its token
			// A release made at the first signal would run on a goroutine
			// context.AfterFunc starts; the settle gives it the time to run. The
			// absence of a release cannot be observed without a bound, so the
			// bound is short, and the count below is the second check.
			select {
			case <-fake.released:
				t.Error("the signal handler was released at the first signal: a second signal would kill the process and skip the restore")
			case <-time.After(settle): //testpolicy:allow no-sleep — a negative proof: nothing arrives on the release channel while the run is held; the bound is the settle for an asynchronous release, and the test does not depend on it for a pass of correct code
			}
		})
		return ""
	}
	opts := &forgejoresync.Options{Repo: "acme/gastown", MainBranch: "main"}
	parse := func([]string, func(string) string) (*forgejoresync.Options, error) { return opts, nil }
	var stdout, stderr strings.Builder
	if got := runNotify(nil, getenv, noFiles(t), &stdout, &stderr, parse, fake.notify); got != forgejoresync.ExitFailure {
		t.Fatalf("runNotify = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	select {
	case <-fake.released:
	default:
		t.Fatal("the signal handler was not released when the run returned")
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("the handler was released %d times, want once at the end of the run: an earlier release lets a second signal skip the restore", got)
	}
}

// TestMainParseWithoutOptionsExitsOne: a parse seam that returns neither options
// nor an error has no command line to run. It exits 1 and says so; reading the
// missing options would panic instead of reporting the failed run.
func TestMainParseWithoutOptionsExitsOne(t *testing.T) {
	t.Parallel()
	empty := func([]string, func(string) string) (*forgejoresync.Options, error) {
		return nil, nil
	}
	var stdout, stderr strings.Builder
	if got := runParse(nil, noEnv, noFiles(t), &stdout, &stderr, empty); got != forgejoresync.ExitFailure {
		t.Fatalf("runParse = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "the command line parsed to no options") {
		t.Errorf("stderr = %q, want the missing options named", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing on a failed run", stdout.String())
	}
}

// TestMainParseFailureExitsOne: a parse failure that is not a usage error is a
// failed run (1) with its own line, not a refused command line (2) with the
// usage hint. Parse reports no such failure today; the seam stands so a later
// bead reading the admin token file at parse time cannot turn its failure into
// a usage refusal.
func TestMainParseFailureExitsOne(t *testing.T) {
	t.Parallel()
	unreadable := func([]string, func(string) string) (*forgejoresync.Options, error) {
		return nil, errors.New("the admin token file is unreadable")
	}
	var stdout, stderr strings.Builder
	if got := runParse(nil, noEnv, noFiles(t), &stdout, &stderr, unreadable); got != forgejoresync.ExitFailure {
		t.Fatalf("runParse = %d, want 1 (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "the admin token file is unreadable") {
		t.Errorf("stderr = %q, want the parse failure named", stderr.String())
	}
	if strings.Contains(stderr.String(), "usage: forgejo-resync --help") {
		t.Errorf("stderr = %q, want no usage hint: this is not a refused command line", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing on a failed run", stdout.String())
	}
}
