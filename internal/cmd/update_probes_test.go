package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// updateProbeRun records each command the probes run and answers with one fixed
// result, so a test can see the exact argv and prove no fetch was issued.
type updateProbeRun struct {
	calls  []string
	output string
	exit   int
	err    error
}

func (f *updateProbeRun) run(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return f.output, f.exit, f.err
}

// TestUpdateProbeInstalledAndMainSHA: the installed probe answers with the
// build commit and refuses a blank one; the main probe reads the first field
// of the ls-remote line and never runs a fetch.
func TestUpdateProbeInstalledAndMainSHA(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("installed returns the build commit", func(t *testing.T) {
		t.Parallel()
		probes := newUpdateProbes(updateProbeEnv{Commit: func() string { return "abc123def456" }})
		sha, err := probes.InstalledSHA(ctx)
		if err != nil {
			t.Fatalf("installed sha: %v", err)
		}
		if sha != "abc123def456" {
			t.Errorf("installed sha = %q, want the injected commit", sha)
		}
	})

	t.Run("installed refuses an empty commit", func(t *testing.T) {
		t.Parallel()
		probes := newUpdateProbes(updateProbeEnv{Commit: func() string { return "" }})
		if _, err := probes.InstalledSHA(ctx); err == nil {
			t.Error("an empty installed commit reported no error")
		}
	})

	t.Run("main parses the first field of the first line", func(t *testing.T) {
		t.Parallel()
		run := &updateProbeRun{output: "deadbeefdeadbeef\trefs/heads/main\n"}
		probes := newUpdateProbes(updateProbeEnv{RepoRoot: "/town/repo", Run: run.run})

		sha, err := probes.MainSHA(ctx)
		if err != nil {
			t.Fatalf("main sha: %v", err)
		}
		if sha != "deadbeefdeadbeef" {
			t.Errorf("main sha = %q, want the first field", sha)
		}
		want := "git -C /town/repo ls-remote origin refs/heads/main"
		if len(run.calls) != 1 || run.calls[0] != want {
			t.Errorf("ran %v, want [%q]", run.calls, want)
		}
		for _, call := range run.calls {
			if strings.Contains(call, "fetch") {
				t.Errorf("the main probe ran a fetch: %q", call)
			}
		}
	})

	t.Run("main refuses a failed ls-remote", func(t *testing.T) {
		t.Parallel()
		run := &updateProbeRun{exit: 2}
		probes := newUpdateProbes(updateProbeEnv{RepoRoot: "/town/repo", Run: run.run})
		if _, err := probes.MainSHA(ctx); err == nil {
			t.Error("a nonzero ls-remote exit reported no error")
		}
	})

	t.Run("main refuses empty output", func(t *testing.T) {
		t.Parallel()
		run := &updateProbeRun{output: "\n"}
		probes := newUpdateProbes(updateProbeEnv{RepoRoot: "/town/repo", Run: run.run})
		if _, err := probes.MainSHA(ctx); err == nil {
			t.Error("empty ls-remote output reported no error")
		}
	})
}

// TestUpdateProbeBackupToday: a dated directory holding one subdirectory per
// database is today's backup; a missing directory or database is false, not an
// error, while a failed stat is an error.
func TestUpdateProbeBackupToday(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

	probesFor := func(root string, databases []string) *updateProbes {
		return &updateProbes{env: updateProbeEnv{
			Now:        func() time.Time { return now },
			Databases:  func(context.Context) ([]string, error) { return databases, nil },
			BackupRoot: func() (string, error) { return root, nil },
			Stat:       os.Stat,
			ReadFile:   os.ReadFile,
		}}
	}
	dated := func(root string) string { return filepath.Join(root, "2026-11-03") }

	t.Run("a complete dated directory", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		for _, db := range []string{"gt", "hq"} {
			if err := os.MkdirAll(filepath.Join(dated(root), db), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		present, err := probesFor(root, []string{"gt", "hq"}).BackupToday(ctx)
		if err != nil {
			t.Fatalf("backup today: %v", err)
		}
		if !present {
			t.Error("a complete backup directory reported missing")
		}
	})

	t.Run("a missing dated directory is false", func(t *testing.T) {
		t.Parallel()
		present, err := probesFor(t.TempDir(), []string{"gt"}).BackupToday(ctx)
		if err != nil {
			t.Fatalf("backup today: %v", err)
		}
		if present {
			t.Error("a missing backup directory reported present")
		}
	})

	t.Run("a directory missing one database is false", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dated(root), "gt"), 0o755); err != nil {
			t.Fatal(err)
		}

		present, err := probesFor(root, []string{"gt", "hq"}).BackupToday(ctx)
		if err != nil {
			t.Fatalf("backup today: %v", err)
		}
		if present {
			t.Error("a backup missing hq reported present")
		}
	})

	t.Run("a database entry that is not a directory is false", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.MkdirAll(dated(root), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dated(root), "gt"), []byte("not a backup"), 0o644); err != nil {
			t.Fatal(err)
		}

		present, err := probesFor(root, []string{"gt"}).BackupToday(ctx)
		if err != nil {
			t.Fatalf("backup today: %v", err)
		}
		if present {
			t.Error("a plain file where a backup belongs reported present")
		}
	})

	t.Run("a stat failure is an error", func(t *testing.T) {
		t.Parallel()
		probes := probesFor("/unused", []string{"gt"})
		probes.env.Stat = func(string) (os.FileInfo, error) {
			return nil, errors.New("permission denied")
		}
		if _, err := probes.BackupToday(ctx); err == nil {
			t.Error("an unreadable backup directory reported missing with no error")
		}
	})

	t.Run("an unreadable database list is an error", func(t *testing.T) {
		t.Parallel()
		probes := probesFor(t.TempDir(), nil)
		probes.env.Databases = func(context.Context) ([]string, error) {
			return nil, errors.New("dolt is down")
		}
		if _, err := probes.BackupToday(ctx); err == nil {
			t.Error("an unreadable database list reported a backup with no error")
		}
	})
}

// writeUpdateGCState writes daemon/maintenance_state.json with body.
func writeUpdateGCState(t *testing.T, townRoot, body string) {
	t.Helper()
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "maintenance_state.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateProbeGCDue: a database whose last gc is more than seven days old is
// due and named with its age; one with no entry is due; all-fresh is not due;
// a missing or unreadable state file is an error.
func TestUpdateProbeGCDue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	probesFor := func(townRoot string, databases []string) *updateProbes {
		return &updateProbes{env: updateProbeEnv{
			TownRoot:  townRoot,
			Now:       func() time.Time { return now },
			Databases: func(context.Context) ([]string, error) { return databases, nil },
			ReadFile:  os.ReadFile,
		}}
	}

	t.Run("an eight-day-old gc is due and named with its age", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeUpdateGCState(t, townRoot, fmt.Sprintf(`{"last_gc":{"gt":%q,"hq":%q}}`, ago(8*24*time.Hour), ago(6*24*time.Hour)))

		due, reason, err := probesFor(townRoot, []string{"gt", "hq"}).GCDue(ctx)
		if err != nil {
			t.Fatalf("gc due: %v", err)
		}
		if !due {
			t.Error("an eight-day-old gc reported not due")
		}
		if !strings.Contains(reason, "gt") || !strings.Contains(reason, "192h") {
			t.Errorf("reason = %q, want the database and its age", reason)
		}
	})

	t.Run("all gcs inside seven days are not due", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeUpdateGCState(t, townRoot, fmt.Sprintf(`{"last_gc":{"gt":%q,"hq":%q}}`, ago(6*24*time.Hour), ago(time.Hour)))

		due, reason, err := probesFor(townRoot, []string{"gt", "hq"}).GCDue(ctx)
		if err != nil {
			t.Fatalf("gc due: %v", err)
		}
		if due {
			t.Errorf("fresh gcs reported due: %q", reason)
		}
	})

	t.Run("a database with no entry is due", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeUpdateGCState(t, townRoot, fmt.Sprintf(`{"last_gc":{"gt":%q}}`, ago(time.Hour)))

		due, reason, err := probesFor(townRoot, []string{"gt", "hq"}).GCDue(ctx)
		if err != nil {
			t.Fatalf("gc due: %v", err)
		}
		if !due {
			t.Error("a database with no gc entry reported not due")
		}
		if !strings.Contains(reason, "hq") || !strings.Contains(reason, "no recorded gc") {
			t.Errorf("reason = %q, want hq named as never gc'd", reason)
		}
	})

	t.Run("a corrupt state file is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeUpdateGCState(t, townRoot, "{not json")

		if _, _, err := probesFor(townRoot, []string{"gt"}).GCDue(ctx); err == nil {
			t.Error("a corrupt state file reported not due")
		}
	})

	t.Run("a missing state file is an error", func(t *testing.T) {
		t.Parallel()
		if _, _, err := probesFor(t.TempDir(), []string{"gt"}).GCDue(ctx); err == nil {
			t.Error("a missing state file reported not due")
		}
	})

	t.Run("an unreadable database list is an error", func(t *testing.T) {
		t.Parallel()
		probes := probesFor(t.TempDir(), nil)
		probes.env.Databases = func(context.Context) ([]string, error) {
			return nil, errors.New("dolt is down")
		}
		if _, _, err := probes.GCDue(ctx); err == nil {
			t.Error("an unreadable database list reported not due")
		}
	})
}

// TestUpdateProbeImagePublished: docker exit 0 is published, exit 1 is not, and
// any other exit or run error is an error; the image tag carries the first nine
// characters of the commit.
func TestUpdateProbeImagePublished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const sha = "0123456789abcdef0123456789abcdef01234567"

	t.Run("exit 0 is published", func(t *testing.T) {
		t.Parallel()
		run := &updateProbeRun{exit: 0}
		probes := newUpdateProbes(updateProbeEnv{Run: run.run})

		published, err := probes.ImagePublished(ctx, sha)
		if err != nil {
			t.Fatalf("image published: %v", err)
		}
		if !published {
			t.Error("docker exit 0 reported not published")
		}
		want := "docker image inspect gastown-polecat:" + sha[:9]
		if len(run.calls) != 1 || run.calls[0] != want {
			t.Errorf("ran %v, want [%q]", run.calls, want)
		}
	})

	t.Run("exit 1 is not published", func(t *testing.T) {
		t.Parallel()
		probes := newUpdateProbes(updateProbeEnv{Run: (&updateProbeRun{exit: 1}).run})
		published, err := probes.ImagePublished(ctx, sha)
		if err != nil {
			t.Fatalf("image published: %v", err)
		}
		if published {
			t.Error("docker exit 1 reported published")
		}
	})

	t.Run("any other exit is an error", func(t *testing.T) {
		t.Parallel()
		probes := newUpdateProbes(updateProbeEnv{Run: (&updateProbeRun{exit: 2}).run})
		if _, err := probes.ImagePublished(ctx, sha); err == nil {
			t.Error("docker exit 2 reported a verdict with no error")
		}
	})

	t.Run("a run error is an error", func(t *testing.T) {
		t.Parallel()
		probes := newUpdateProbes(updateProbeEnv{Run: (&updateProbeRun{err: errors.New("docker is not installed")}).run})
		if _, err := probes.ImagePublished(ctx, sha); err == nil {
			t.Error("a docker run error reported a verdict with no error")
		}
	})

	t.Run("a short sha is used whole", func(t *testing.T) {
		t.Parallel()
		run := &updateProbeRun{exit: 0}
		probes := newUpdateProbes(updateProbeEnv{Run: run.run})

		if _, err := probes.ImagePublished(ctx, "abc"); err != nil {
			t.Fatalf("image published: %v", err)
		}
		want := "docker image inspect gastown-polecat:abc"
		if len(run.calls) != 1 || run.calls[0] != want {
			t.Errorf("ran %v, want [%q]", run.calls, want)
		}
	})
}

// TestUpdateProbeArchiveBehind: the archive probe is the supplied function's
// counts; an unwired or failing archive is an error, never zero.
func TestUpdateProbeArchiveBehind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("returns the supplied counts", func(t *testing.T) {
		t.Parallel()
		probes := newUpdateProbes(updateProbeEnv{
			Archive: func(context.Context) (int64, int64, error) { return 3, 4, nil },
		})
		commits, events, err := probes.ArchiveBehind(ctx)
		if err != nil {
			t.Fatalf("archive behind: %v", err)
		}
		if commits != 3 || events != 4 {
			t.Errorf("counts = (%d, %d), want (3, 4)", commits, events)
		}
	})

	t.Run("a failing archive is an error with no counts", func(t *testing.T) {
		t.Parallel()
		probes := newUpdateProbes(updateProbeEnv{
			Archive: func(context.Context) (int64, int64, error) {
				return 7, 8, errors.New("watermark unreadable")
			},
		})
		commits, events, err := probes.ArchiveBehind(ctx)
		if err == nil {
			t.Fatal("a failing archive reported caught up")
		}
		if commits != 0 || events != 0 {
			t.Errorf("counts = (%d, %d), want zeros", commits, events)
		}
	})

	t.Run("an unwired archive is an error", func(t *testing.T) {
		t.Parallel()
		if _, _, err := newUpdateProbes(updateProbeEnv{}).ArchiveBehind(ctx); err == nil {
			t.Error("an unwired archive reported caught up")
		}
	})
}
