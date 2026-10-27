package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/repoguard"
)

// TestRepoGuardRegistered pins the help surface an agent finding the command
// relies on: a registered name with Short and Long text and both flags.
func TestRepoGuardRegistered(t *testing.T) {
	t.Parallel()
	c := lookupCommand(rootCmd, []string{"repoguard"})
	if c == nil {
		t.Fatal("gt repoguard is not registered")
	}
	if c.Short == "" || c.Long == "" {
		t.Errorf("repoguard Short = %q, Long = %q; both must carry help text", c.Short, c.Long)
	}
	for _, flag := range []string{"base", "init"} {
		if c.Flags().Lookup(flag) == nil {
			t.Errorf("repoguard has no --%s flag", flag)
		}
	}
}

// TestRunRepoGuardClean: a tree whose only script is listed with a reason
// exits 0 and prints nothing.
func TestRunRepoGuardClean(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runRepoGuard(&out, io.Discard, repoGuardInputs{
		listFiles:         func() ([]string, error) { return []string{"main.go", "scripts/probe.sh"}, nil },
		readAllowlist:     entriesReader([]repoguard.Entry{{Path: "scripts/probe.sh", Reason: "needs the real shell"}}),
		readBaseAllowlist: entriesReader([]repoguard.Entry{{Path: "scripts/probe.sh", Reason: "needs the real shell"}}),
	})
	if err != nil {
		t.Fatalf("runRepoGuard on a clean tree = %v, want nil", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("output = %q, want empty", got)
	}
}

// TestRunRepoGuardDirty: an unlisted script exits 1 with the path named, the
// contract a caller gates on.
func TestRunRepoGuardDirty(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runRepoGuard(&out, io.Discard, repoGuardInputs{
		listFiles:         func() ([]string, error) { return []string{"scripts/a.sh", "scripts/new.sh"}, nil },
		readAllowlist:     entriesReader([]repoguard.Entry{{Path: "scripts/a.sh", Reason: "fleet script"}}),
		readBaseAllowlist: entriesReader([]repoguard.Entry{{Path: "scripts/a.sh", Reason: "fleet script"}}),
	})
	if err == nil {
		t.Fatal("runRepoGuard on a dirty tree = nil, want a violation error")
	}
	if code := exitCodeForError(err); code != 1 {
		t.Fatalf("exit code for %v = %d, want 1", err, code)
	}
	if !strings.Contains(out.String(), "scripts/new.sh") {
		t.Errorf("output = %q, want the violation named", out.String())
	}
}

// TestRunRepoGuardInitNotice: a run that skipped the growth check says so on
// warn, while findings stay on out, so the notice cannot pass for a violation
// and a skip cannot pass silently.
func TestRunRepoGuardInitNotice(t *testing.T) {
	t.Parallel()
	const notice = "repoguard: no .repoguard-allowlist at origin/main: --init skips the growth check for this run"
	var out, warn bytes.Buffer
	err := runRepoGuard(&out, &warn, repoGuardInputs{
		listFiles:     func() ([]string, error) { return []string{"main.go"}, nil },
		readAllowlist: entriesReader(nil),
		notice:        notice,
	})
	if err != nil {
		t.Fatalf("runRepoGuard with only a notice = %v, want nil", err)
	}
	if !strings.Contains(warn.String(), "--init skips the growth check") {
		t.Errorf("warn = %q, want the skip notice", warn.String())
	}
	if out.String() != "" {
		t.Errorf("out = %q, want no findings beside the notice", out.String())
	}
}

// TestRepoGuardCommandPath drives the command the registered node is built
// from: --base and --init parse into its flags, the body reads them, and the
// body's error is what the run returns, so deleting the RunE wiring turns this
// red.
func TestRepoGuardCommandPath(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	wantErr := errors.New("repoguard: 1 violation, listed above")
	var gotBase string
	var gotInit bool
	c := newRepoGuardCommand(func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			t.Errorf("body got args %q, want none", args)
		}
		var err error
		if gotBase, err = cmd.Flags().GetString("base"); err != nil {
			t.Fatalf("reading --base = %v", err)
		}
		if gotInit, err = cmd.Flags().GetBool("init"); err != nil {
			t.Fatalf("reading --init = %v", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "scripts/new.sh: not in .repoguard-allowlist")
		return wantErr
	})
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.ParseFlags([]string{"--base", "upstream/main", "--init"}); err != nil {
		t.Fatalf("parsing --base and --init = %v", err)
	}
	if err := c.RunE(c, nil); !errors.Is(err, wantErr) {
		t.Fatalf("RunE = %v, want the body's %v", err, wantErr)
	}
	if gotBase != "upstream/main" || !gotInit {
		t.Fatalf("body read base=%q init=%v, want the values passed", gotBase, gotInit)
	}
	if !strings.Contains(out.String(), "scripts/new.sh") {
		t.Errorf("output = %q, want the body's finding", out.String())
	}
}

// TestRepoGuardCommandFlagsDriveTheRun drives the real flag-reading body over
// a fake git: the --base value is the revision git is asked about, and --init
// decides whether a base with no allowlist skips the growth check or fails.
func TestRepoGuardCommandFlagsDriveTheRun(t *testing.T) {
	t.Parallel()
	const base = "upstream/main"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, repoguardAllowlist), []byte(repoAllowlistBody), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name             string
		args             []string
		baseHasAllowlist bool
		wantErr          string
		wantNotice       bool
	}{
		{name: "base flag reaches git", args: []string{"--base", base}, baseHasAllowlist: true},
		{name: "init skips a base with no allowlist and says so", args: []string{"--base", base, "--init"}, wantNotice: true},
		{name: "base with no allowlist fails without init", args: []string{"--base", base}, wantErr: "pass --init"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			run := fakeGit(&calls, guardGit(root, base, tc.baseHasAllowlist))
			c := newRepoGuardCommand(func(cmd *cobra.Command, args []string) error {
				return repoGuardCommandWith(cmd, args, run)
			})
			var out, warn bytes.Buffer
			c.SetOut(&out)
			c.SetErr(&warn)
			if err := c.ParseFlags(tc.args); err != nil {
				t.Fatalf("parsing %q = %v", tc.args, err)
			}
			err := c.RunE(c, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("RunE = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RunE = %v, want an error containing %q", err, tc.wantErr)
			}
			if gotNotice := warn.String() != ""; gotNotice != tc.wantNotice {
				t.Errorf("warn = %q, want a notice: %v", warn.String(), tc.wantNotice)
			}
			// The fake answers only base's calls, so a run that got past it
			// asked git about the revision --base named.
			if len(calls) == 0 {
				t.Error("git was never asked anything")
			}
		})
	}
}

// TestRepoGuardCheckBase: a base git would read as an option is refused before
// it reaches any git command, and a plain revision passes.
func TestRepoGuardCheckBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		base    string
		wantErr bool
	}{
		{base: "origin/main"},
		{base: "HEAD~1"},
		{base: "", wantErr: true},
		{base: "-n", wantErr: true},
		{base: "--exec-path", wantErr: true},
	}
	for _, tc := range tests {
		t.Run("base="+tc.base, func(t *testing.T) {
			t.Parallel()
			err := repoGuardCheckBase(tc.base)
			if tc.wantErr && err == nil {
				t.Fatalf("repoGuardCheckBase(%q) = nil, want a refusal", tc.base)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("repoGuardCheckBase(%q) = %v, want nil", tc.base, err)
			}
		})
	}
}

// TestRepoGuardBasePlan: a base allowlist is compared, --init on a base with
// none skips the growth check with a notice that says so, and without --init
// the run fails rather than passing uncompared.
func TestRepoGuardBasePlan(t *testing.T) {
	t.Parallel()
	const base = "origin/main"
	tests := []struct {
		name             string
		baseHasAllowlist bool
		init             bool
		wantCompare      bool
		wantNotice       bool
		wantErr          bool
	}{
		{name: "base allowlist is compared", baseHasAllowlist: true, wantCompare: true},
		{name: "init with no base allowlist skips and says so", init: true, wantNotice: true},
		{name: "no base allowlist without init fails", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			compare, notice, err := repoGuardBasePlan(tc.baseHasAllowlist, tc.init, base)
			if tc.wantErr {
				if err == nil {
					t.Fatal("repoGuardBasePlan = nil error, want a refusal naming --init")
				}
				if !strings.Contains(err.Error(), "--init") {
					t.Errorf("error = %q, want it to name --init", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("repoGuardBasePlan = %v, want nil", err)
			}
			if compare != tc.wantCompare {
				t.Errorf("compare = %v, want %v", compare, tc.wantCompare)
			}
			if gotNotice := notice != ""; gotNotice != tc.wantNotice {
				t.Fatalf("notice = %q, want a notice: %v", notice, tc.wantNotice)
			}
			if tc.wantNotice && (!strings.Contains(notice, "--init") || !strings.Contains(notice, repoguardAllowlist)) {
				t.Errorf("notice = %q, want it to name --init and %s", notice, repoguardAllowlist)
			}
		})
	}
}

// fakeGit is a gitRunner over a handler, recording each command it answered so
// a wiring test can assert the guard asked for what it needed.
func fakeGit(calls *[]string, handle func(args []string) ([]byte, error)) gitRunner {
	return func(_ string, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		return handle(args)
	}
}

// guardGit answers the calls a run against base makes, with the base allowlist
// holding scripts/a.sh. Any other base or call is an error, so a test proves
// the revision it asked for reached git.
func guardGit(root, base string, baseHasAllowlist bool) func(args []string) ([]byte, error) {
	return func(args []string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "rev-parse --show-toplevel":
			return []byte(root + "\n"), nil
		case "rev-parse --verify --quiet --end-of-options " + base + "^{commit}":
			return nil, nil
		case "ls-tree -z --name-only " + base + " -- .repoguard-allowlist":
			if !baseHasAllowlist {
				return nil, nil
			}
			return []byte(".repoguard-allowlist\x00"), nil
		case "show " + base + ":.repoguard-allowlist":
			return []byte(repoAllowlistBody), nil
		case "ls-files -z":
			return []byte("scripts/a.sh\x00"), nil
		}
		return nil, fmt.Errorf("unexpected git call %q", args)
	}
}

// cleanGit answers the calls a clean run against origin/main makes.
func cleanGit(root string) func(args []string) ([]byte, error) {
	return guardGit(root, "origin/main", true)
}

// repoAllowlistBody lists the one script the fixtures track.
const repoAllowlistBody = "scripts/a.sh\tneeds the real shell\n"

// TestRepoGuardRunClean: the real wiring, over a fake git and a temp tree,
// exits 0 and asks git for exactly the tree, the base and the tracked files.
func TestRepoGuardRunClean(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, repoguardAllowlist), []byte(repoAllowlistBody), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	var out, warn bytes.Buffer
	err := repoGuardRun(&out, &warn, "origin/main", false, fakeGit(&calls, cleanGit(root)))
	if err != nil {
		t.Fatalf("repoGuardRun on a clean tree = %v, want nil", err)
	}
	if out.String() != "" || warn.String() != "" {
		t.Errorf("out = %q, warn = %q, want both empty", out.String(), warn.String())
	}
	if len(calls) != 5 {
		t.Errorf("git calls = %q, want the five a clean run makes", calls)
	}
}

// TestRepoGuardRunGitRefusals drives every git failure the wiring reports: a
// directory outside a repository, a base that does not resolve, a base the
// tree or the allowlist read fails on, a missing base allowlist, and a file
// list that cannot be read.
func TestRepoGuardRunGitRefusals(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, repoguardAllowlist), []byte(repoAllowlistBody), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("git refused")
	tests := []struct {
		name     string
		failCall string
		noBase   bool // the base revision holds no allowlist
		wantErr  string
	}{
		{name: "outside a repository", failCall: "rev-parse --show-toplevel", wantErr: "not inside a git repository: git refused: rev-parse --show-toplevel"},
		{name: "base does not resolve", failCall: "rev-parse --verify --quiet --end-of-options origin/main^{commit}", wantErr: "does not resolve: git refused: rev-parse --verify"},
		{name: "base tree unreadable", failCall: "ls-tree -z --name-only origin/main -- .repoguard-allowlist", wantErr: "git refused: ls-tree -z --name-only origin/main"},
		{name: "base allowlist unreadable", failCall: "show origin/main:.repoguard-allowlist", wantErr: "git refused: show origin/main:.repoguard-allowlist"},
		{name: "tracked files unreadable", failCall: "ls-files -z", wantErr: "git refused: ls-files -z"},
		{name: "base has no allowlist", noBase: true, wantErr: "pass --init"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			handle := guardGit(root, "origin/main", !tc.noBase)
			run := fakeGit(&calls, func(args []string) ([]byte, error) {
				call := strings.Join(args, " ")
				if call == tc.failCall {
					return nil, fmt.Errorf("%w: %s", refused, call)
				}
				return handle(args)
			})
			var out, warn bytes.Buffer
			err := repoGuardRun(&out, &warn, "origin/main", false, run)
			if err == nil {
				t.Fatal("repoGuardRun = nil, want the git refusal")
			}
			if !tc.noBase && !errors.Is(err, refused) {
				t.Errorf("error = %v, want it to carry the git failure", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestRepoGuardRunRefusesDashBase: a --base git would read as an option is
// refused before any command carrying it runs.
func TestRepoGuardRunRefusesDashBase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var calls []string
	run := fakeGit(&calls, func(args []string) ([]byte, error) {
		if strings.Join(args, " ") != "rev-parse --show-toplevel" {
			t.Errorf("git %q ran for a dash base", args)
		}
		return []byte(root + "\n"), nil
	})
	var out, warn bytes.Buffer
	err := repoGuardRun(&out, &warn, "-n", false, run)
	if err == nil || !strings.Contains(err.Error(), "starts with '-'") {
		t.Fatalf("repoGuardRun with --base -n = %v, want a refusal", err)
	}
	if len(calls) != 1 {
		t.Errorf("git calls = %q, want only the repository lookup", calls)
	}
}

// TestRepoGuardReadAllowlist: a missing allowlist is no entries, so the
// scripts it would hold fail as unlisted, while a path that cannot be read is
// an error rather than a clean tree.
func TestRepoGuardReadAllowlist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		entries, err := repoGuardReadAllowlist(filepath.Join(dir, "absent"))()
		if err != nil || entries != nil {
			t.Fatalf("entries = %v, err = %v; want no entries and no error", entries, err)
		}
	})
	t.Run("unreadable path", func(t *testing.T) {
		t.Parallel()
		if _, err := repoGuardReadAllowlist(filepath.Join(file, "under-a-file"))(); err == nil {
			t.Fatal("repoGuardReadAllowlist on a path under a file = nil, want an error")
		}
	})
}

// TestRepoGuardGitError pins what a failed git call reports: the command, and
// git's own words when it wrote any.
func TestRepoGuardGitError(t *testing.T) {
	t.Parallel()
	args := []string{"rev-parse", "--verify", "origin/main"}
	cause := errors.New("exit status 128")
	withStderr := repoGuardGitError(args, " fatal: not a git repository \n", cause)
	if !errors.Is(withStderr, cause) {
		t.Errorf("error = %v, want it to wrap git's failure", withStderr)
	}
	if want := "git rev-parse --verify origin/main: exit status 128: fatal: not a git repository"; withStderr.Error() != want {
		t.Errorf("error = %q, want %q", withStderr, want)
	}
	silent := repoGuardGitError(args, "", cause)
	if want := "git rev-parse --verify origin/main: exit status 128"; silent.Error() != want {
		t.Errorf("error = %q, want %q", silent, want)
	}
}

// TestRunRepoGuardReaderError: an unread allowlist is returned, never read as
// a clean tree.
func TestRunRepoGuardReaderError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("allowlist is unreadable")
	var out bytes.Buffer
	err := runRepoGuard(&out, io.Discard, repoGuardInputs{
		listFiles:     func() ([]string, error) { return []string{"scripts/a.sh"}, nil },
		readAllowlist: func() ([]repoguard.Entry, error) { return nil, wantErr },
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runRepoGuard = %v, want the reader's %v", err, wantErr)
	}
	if out.String() != "" {
		t.Errorf("out = %q, want nothing printed for an unread tree", out.String())
	}
}

// TestRunRepoGuardCountedFindings: more than one violation is counted in the
// error a gate reads.
func TestRunRepoGuardCountedFindings(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runRepoGuard(&out, io.Discard, repoGuardInputs{
		listFiles:     func() ([]string, error) { return []string{"scripts/a.sh", "scripts/b.sh"}, nil },
		readAllowlist: entriesReader(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "2 violations") {
		t.Fatalf("runRepoGuard = %v, want it to report 2 violations", err)
	}
	for _, want := range []string{"scripts/a.sh", "scripts/b.sh"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("out = %q, want %s named", out.String(), want)
		}
	}
}

// TestRepoGuardCommandFlagErrors: a command without the flags reports the
// lookup failure instead of running the guard.
func TestRepoGuardCommandFlagErrors(t *testing.T) {
	t.Parallel()
	bare := &cobra.Command{}
	if err := runRepoGuardCommand(bare, nil); err == nil {
		t.Fatal("runRepoGuardCommand on a command with no --base = nil, want an error")
	}
	onlyBase := &cobra.Command{}
	onlyBase.Flags().String("base", "origin/main", "")
	if err := runRepoGuardCommand(onlyBase, nil); err == nil {
		t.Fatal("runRepoGuardCommand on a command with no --init = nil, want an error")
	}
}

func entriesReader(entries []repoguard.Entry) repoguard.Reader {
	return func() ([]repoguard.Entry, error) { return entries, nil }
}
