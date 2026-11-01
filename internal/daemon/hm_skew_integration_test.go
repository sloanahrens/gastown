//go:build integration

package daemon

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// The real-clone cases of the hm-skew check: git runs for real against a local
// origin, so the clone, the fetch, the commit probe and the diff are the ones a
// production reading makes. Only `hm version` is answered from the test, since
// the test has no installed hm to ask.

// hmSkewGitIn runs git in dir with a fixed identity and no user or system
// config, and fails the test on error.
func hmSkewGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=hm-skew", "GIT_AUTHOR_EMAIL=hm-skew@example.invalid",
		"GIT_COMMITTER_NAME=hm-skew", "GIT_COMMITTER_EMAIL=hm-skew@example.invalid",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return strings.TrimSpace(out.String())
}

// hmSkewCommit writes path with content in the rig, commits it and returns the
// new commit's full sha.
func hmSkewCommit(t *testing.T, rig, path, content string) string {
	t.Helper()
	full := filepath.Join(rig, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hmSkewGitIn(t, rig, "add", "--", path)
	hmSkewGitIn(t, rig, "commit", "-q", "-m", "change "+path)
	return hmSkewGitIn(t, rig, "rev-parse", "HEAD")
}

// hmSkewRealTown is a town whose hm rig is a real repository with a local
// origin: rig is the rig's worktree, origin the bare repository it pushes main
// to. The first commit (the installed hm's) is returned as base.
func hmSkewRealTown(t *testing.T) (town, rig, origin, base string) {
	t.Helper()
	town = t.TempDir()
	origin = filepath.Join(town, "origin.git")
	rig = filepath.Join(town, "hm", "mayor", "rig")
	mkdirs(t, rig)
	hmSkewGitIn(t, rig, "init", "-q", "-b", "main")
	base = hmSkewCommit(t, rig, "README.md", "one\n")
	hmSkewGitIn(t, town, "init", "-q", "--bare", origin)
	hmSkewGitIn(t, rig, "remote", "add", "origin", origin)
	hmSkewGitIn(t, rig, "push", "-q", "origin", "main")
	return town, rig, origin, base
}

// hmSkewRealDaemon is a daemon whose installed hm prints the sha *installed
// points at, and whose git calls are real. Its escalations and clears are
// recorded.
func hmSkewRealDaemon(t *testing.T, town string, installed *string) (*Daemon, *notifyfake.Recorder, *bytes.Buffer) {
	t.Helper()
	rec := notifyfake.New()
	var logs bytes.Buffer
	d := &Daemon{
		logger:   log.New(&logs, "", 0),
		config:   &Config{TownRoot: town},
		notifier: rec,
		clock:    newFixedClock(),
		ctx:      t.Context(),
		execCmd: func(cmd *exec.Cmd) ([]byte, []byte, error) {
			if cmd.Args[0] == hmSkewTestPath {
				_, err := io.WriteString(cmd.Stdout, "hm "+*installed+"\n")
				return nil, nil, err
			}
			return runCmdProcess(cmd)
		},
	}
	d.hmSkew.pathFn = func() string { return hmSkewTestPath }
	return d, rec, &logs
}

// TestHMSkewRealCloneEscalatesOnACodeChange: an installed hm at the first commit,
// with origin's main moved by a commit that changes a code path, escalates once
// under hm-skew, and the clone the monitor made is the job-owned one.
func TestHMSkewRealCloneEscalatesOnACodeChange(t *testing.T) {
	t.Parallel()
	town, rig, _, base := hmSkewRealTown(t)
	tip := hmSkewCommit(t, rig, "internal/cli/cmd_query.go", "package cli\n")
	hmSkewGitIn(t, rig, "push", "-q", "origin", "main")
	installed := base
	d, rec, logs := hmSkewRealDaemon(t, town, &installed)

	d.hmSkewOnce()

	if esc := rec.Escalations(); len(esc) != 1 {
		t.Fatalf("escalations = %+v, logs = %q, want one for a code change behind the installed hm", esc, logs.String())
	}
	if got := rec.Escalations()[0].Escalation.Fingerprint; got != hmSkewAlertKey {
		t.Errorf("fingerprint = %q, want %q", got, hmSkewAlertKey)
	}
	if !strings.Contains(rec.Escalations()[0].Escalation.Description+rec.Escalations()[0].Escalation.Reason, tip[:8]) {
		t.Errorf("alert does not name origin's main %s", tip[:8])
	}
	if _, err := os.Stat(filepath.Join(town, hmSkewCloneDir, ".git")); err != nil {
		t.Errorf("the job-owned clone was not made: %v", err)
	}
}

// TestHMSkewRealCloneIgnoresADocsOnlyChange: the same skew over a commit that
// changes only markdown and docs escalates nothing.
func TestHMSkewRealCloneIgnoresADocsOnlyChange(t *testing.T) {
	t.Parallel()
	town, rig, _, base := hmSkewRealTown(t)
	hmSkewCommit(t, rig, "docs/guide.md", "guide\n")
	hmSkewCommit(t, rig, "README.md", "two\n")
	hmSkewGitIn(t, rig, "push", "-q", "origin", "main")
	installed := base
	d, rec, logs := hmSkewRealDaemon(t, town, &installed)

	d.hmSkewOnce()

	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("escalations = %+v, logs = %q, want none for a docs-only change", esc, logs.String())
	}
}

// TestHMSkewRealCloneFollowsOriginMain: a clone made by one reading judges the
// tip a later reading names. The first reading alarms on a code change; once
// the installed hm is at the new tip and that tip only changes docs, the next
// reading clears the alert. A clone that never fetched the new tip could not
// make that clear.
func TestHMSkewRealCloneFollowsOriginMain(t *testing.T) {
	t.Parallel()
	town, rig, _, base := hmSkewRealTown(t)
	codeTip := hmSkewCommit(t, rig, "internal/cli/cmd_query.go", "package cli\n")
	hmSkewGitIn(t, rig, "push", "-q", "origin", "main")
	installed := base
	d, rec, logs := hmSkewRealDaemon(t, town, &installed)
	d.hmSkewOnce()

	docsTip := hmSkewCommit(t, rig, "docs/guide.md", "guide\n")
	hmSkewGitIn(t, rig, "push", "-q", "origin", "main")
	installed = codeTip
	d.hmSkewOnce()

	if n := len(rec.Escalations()); n != 1 {
		t.Errorf("escalations = %d, want the one alarm from the first reading", n)
	}
	clears := rec.Clears()
	if len(clears) != 1 || !strings.Contains(strings.Join(clears[0].Fingerprints, " "), hmSkewAlertKey) {
		t.Errorf("clears = %+v, logs = %q, want one clear under %s once origin main %s only changed docs",
			clears, logs.String(), hmSkewAlertKey, docsTip[:8])
	}
}
