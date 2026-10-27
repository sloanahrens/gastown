package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

// TestRepoGuardRegisteredCommandPath drives the registered node itself: flags
// parsed through it reach the body, and the body's error is what the run
// returns, so deleting the RunE wiring turns this red.
func TestRepoGuardRegisteredCommandPath(t *testing.T) {
	c := lookupCommand(rootCmd, []string{"repoguard"})
	if c == nil || c.RunE == nil {
		t.Fatal("gt repoguard is not a registered runnable command")
	}
	prevRun := repoGuardCommandRun
	prevBase, prevInit := repoguardBase, repoguardInit
	t.Cleanup(func() {
		repoGuardCommandRun = prevRun
		repoguardBase, repoguardInit = prevBase, prevInit
		c.SetOut(nil)
		c.SetErr(nil)
	})

	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	wantErr := errors.New("repoguard: 1 violation, listed above")
	repoGuardCommandRun = func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			t.Errorf("body got args %q, want none", args)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "scripts/new.sh: not in .repoguard-allowlist")
		return wantErr
	}
	if err := c.ParseFlags([]string{"--base", "upstream/main", "--init"}); err != nil {
		t.Fatalf("parsing --base and --init through the registered node = %v", err)
	}
	if repoguardBase != "upstream/main" || !repoguardInit {
		t.Fatalf("flags parsed to base=%q init=%v, want the values passed", repoguardBase, repoguardInit)
	}
	if err := c.RunE(c, nil); !errors.Is(err, wantErr) {
		t.Fatalf("RunE = %v, want the body's %v", err, wantErr)
	}
	if !strings.Contains(out.String(), "scripts/new.sh") {
		t.Errorf("output = %q, want the body's finding", out.String())
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

func entriesReader(entries []repoguard.Entry) repoguard.Reader {
	return func() ([]repoguard.Entry, error) { return entries, nil }
}
