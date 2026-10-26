package cmd

import (
	"bytes"
	"strings"
	"testing"

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
	err := runRepoGuard(&out, repoGuardInputs{
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
	err := runRepoGuard(&out, repoGuardInputs{
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

func entriesReader(entries []repoguard.Entry) repoguard.Reader {
	return func() ([]repoguard.Entry, error) { return entries, nil }
}
