package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/metrics"
)

// analyseNow is the clock both analyse tests run at: a day after the stamp one
// of them writes.
var analyseNow = time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)

// The command is a thin receipt: what the analysis published, or that the last
// one is still current. The analysis itself is internal/metrics', with its own
// tests; this one pins the two lines an operator reads in the plugin's output.
func TestMetricsAnalysePrintsTheReceiptOfWhatItDid(t *testing.T) {
	t.Parallel()
	t.Run("not due", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		latest := filepath.Join(town, ".runtime", "metrics", "latest")
		if err := os.MkdirAll(latest, 0o755); err != nil {
			t.Fatal(err)
		}
		stamp := analyseNow.Add(-time.Hour).Format(time.RFC3339)
		if err := os.WriteFile(filepath.Join(latest, "stamp"), []byte(stamp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		env := metrics.Env{Town: town, Now: func() time.Time { return analyseNow }, ReadFile: os.ReadFile}
		var out bytes.Buffer
		if err := runMetricsAnalyse(context.Background(), &out, env, analysisPaths(town)); err != nil {
			t.Fatalf("runMetricsAnalyse: %v", err)
		}
		if got := out.String(); !strings.HasPrefix(got, "analyse: not due") || strings.Count(got, "\n") != 1 {
			t.Fatalf("receipt = %q, want one not-due line", got)
		}
	})
	t.Run("ran", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		day := filepath.Join(town, ".runtime", "metrics", "2026-10-01")
		if err := os.MkdirAll(day, 0o755); err != nil {
			t.Fatal(err)
		}
		// A header and no rows: a day with no landings, so the mango stage has
		// no target to run and the panel still publishes.
		if err := os.WriteFile(filepath.Join(day, "landings.csv"), []byte("bead,rig,landed_at\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		env := metrics.Env{
			Town:     town,
			Now:      func() time.Time { return analyseNow },
			ReadFile: os.ReadFile,
			Run:      func(context.Context, []string) ([]byte, error) { return nil, errors.New("mango must not run") },
			RunEnv:   analyseRunEnv(t),
		}
		var out bytes.Buffer
		if err := runMetricsAnalyse(context.Background(), &out, env, analysisPaths(town)); err != nil {
			t.Fatalf("runMetricsAnalyse: %v", err)
		}
		want := "analyse " + analyseNow.Format(time.RFC3339) + ": 0 landings, 2 targets -> " +
			filepath.Join(town, ".runtime", "metrics", "latest", "panel.json") + "\n"
		if got := out.String(); got != want || strings.Count(got, "\n") != 1 {
			t.Fatalf("receipt = %q, want %q", got, want)
		}
	})
	t.Run("a snapshot with an unparseable ts is named", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		day := filepath.Join(town, ".runtime", "metrics", "2026-10-01")
		if err := os.MkdirAll(day, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(day, "landings.csv"), []byte("bead,rig,landed_at\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(metrics.SnapshotPath(town), []byte(`{"ts":"not a time"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		env := metrics.Env{
			Town:     town,
			Now:      func() time.Time { return analyseNow },
			ReadFile: os.ReadFile,
			Run:      func(context.Context, []string) ([]byte, error) { return nil, errors.New("mango must not run") },
			RunEnv:   analyseRunEnv(t),
		}
		var out bytes.Buffer
		if err := runMetricsAnalyse(context.Background(), &out, env, analysisPaths(town)); err != nil {
			t.Fatalf("runMetricsAnalyse: %v", err)
		}
		const skipped = "analyse: 1 snapshot(s) skipped, their ts does not parse\n"
		if got := out.String(); !strings.HasSuffix(got, skipped) {
			t.Fatalf("receipt = %q, want it to end with %q", got, skipped)
		}
	})
	t.Run("a legacy peak gz that is not gzip is named", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		day := filepath.Join(town, ".runtime", "metrics", "2026-10-01")
		if err := os.MkdirAll(day, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(day, "landings.csv"), []byte("bead,rig,landed_at\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		resources := filepath.Join(town, ".runtime", "metrics", "resources")
		if err := os.MkdirAll(resources, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(resources, "bead-peaks.jsonl.gz"), []byte("not gzip, just text"), 0o644); err != nil {
			t.Fatal(err)
		}
		env := metrics.Env{
			Town:     town,
			Now:      func() time.Time { return analyseNow },
			ReadFile: os.ReadFile,
			Run:      func(context.Context, []string) ([]byte, error) { return nil, errors.New("mango must not run") },
			RunEnv:   analyseRunEnv(t),
		}
		var out bytes.Buffer
		if err := runMetricsAnalyse(context.Background(), &out, env, analysisPaths(town)); err != nil {
			t.Fatalf("runMetricsAnalyse: %v", err)
		}
		const skipped = "analyse: 1 peak file(s) skipped, their rows are not joined: bead-peaks.jsonl.gz\n"
		if got := out.String(); !strings.HasSuffix(got, skipped) {
			t.Fatalf("receipt = %q, want it to end with %q", got, skipped)
		}
	})
	t.Run("failed", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		env := metrics.Env{
			Town:     town,
			Now:      func() time.Time { return analyseNow },
			ReadFile: os.ReadFile,
			Run:      func(context.Context, []string) ([]byte, error) { return nil, errors.New("unused") },
			RunEnv: func(context.Context, []string, []string) ([]byte, error) {
				return nil, errors.New("git: no repository")
			},
		}
		var out bytes.Buffer
		if err := runMetricsAnalyse(context.Background(), &out, env, analysisPaths(town)); err == nil {
			t.Fatal("runMetricsAnalyse returned no error for a failed analysis")
		}
		if out.Len() != 0 {
			t.Fatalf("a failed run printed %q, want no receipt", out.String())
		}
	})
}

// A town with no gt source checkout still analyses. The extract runs in
// process (gt-fsoh3), so the command needs no gastown/cmd/gt/main.go. Both
// cases drive runMetricsAnalyse, where the lookup that failed such a town used
// to be (gt-w4brv): a reintroduced one leaves neither the not-due receipt nor
// the published panel.
func TestRunMetricsAnalyseNeedsNoSourceCheckout(t *testing.T) {
	t.Parallel()
	t.Run("not due", func(t *testing.T) {
		t.Parallel()
		town := analyseTown(t)
		writeAnalyseStamp(t, town, analyseNow.Add(-time.Hour))
		var out bytes.Buffer
		if err := runMetricsAnalyse(context.Background(), &out, fakeAnalyseEnv(t, town), analysisPaths(town)); err != nil {
			t.Fatalf("runMetricsAnalyse on a town with no gt source checkout: %v", err)
		}
		want := "analyse: not due, the last analysis is under " + metrics.DailyEvery.String() + " old\n"
		if got := out.String(); got != want {
			t.Fatalf("receipt = %q, want %q", got, want)
		}
	})
	t.Run("due", func(t *testing.T) {
		t.Parallel()
		town := analyseTown(t)
		writeAnalyseStamp(t, town, analyseNow.Add(-25*time.Hour))
		var out bytes.Buffer
		if err := runMetricsAnalyse(context.Background(), &out, fakeAnalyseEnv(t, town), analysisPaths(town)); err != nil {
			t.Fatalf("runMetricsAnalyse on a town with no gt source checkout: %v", err)
		}
		want := "analyse " + analyseNow.Format(time.RFC3339) + ": 0 landings, 2 targets -> " +
			filepath.Join(town, ".runtime", "metrics", "latest", "panel.json") + "\n"
		if got := out.String(); got != want {
			t.Fatalf("receipt = %q, want %q", got, want)
		}
		if _, err := os.Stat(filepath.Join(town, ".runtime", "metrics", "latest", "panel.json")); err != nil {
			t.Fatalf("the analysis was not reached: no panel published: %v", err)
		}
	})
}

// analyseTown is a town with a metrics day and no gt source checkout: the day
// the fake extract answers with, and no gastown/cmd/gt/main.go.
func analyseTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	day := filepath.Join(town, ".runtime", "metrics", "2026-10-01")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	// A header and no rows: no landing is old enough to train on, so the mango
	// stage never reaches its binary.
	if err := os.WriteFile(filepath.Join(day, "landings.csv"), []byte("bead,rig,landed_at\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// writeAnalyseStamp writes the last-run stamp a run compares its clock to.
func writeAnalyseStamp(t *testing.T, town string, at time.Time) {
	t.Helper()
	latest := filepath.Join(town, ".runtime", "metrics", "latest")
	if err := os.MkdirAll(latest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(latest, "stamp"), []byte(at.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeAnalyseEnv answers a run's reads of the town: the clock is analyseNow,
// the files are the test's, and the commands are stubs. The mango head and the
// build run through RunEnv; the stage would run the binary, and no landing on
// this day is old enough to train it, so Run fails the run if it is asked.
func fakeAnalyseEnv(t *testing.T, town string) metrics.Env {
	t.Helper()
	return metrics.Env{
		Town:     town,
		Now:      func() time.Time { return analyseNow },
		ReadFile: os.ReadFile,
		Run:      func(context.Context, []string) ([]byte, error) { return nil, errors.New("mango must not run") },
		RunEnv:   analyseRunEnv(t),
	}
}

// analysisPaths is the analysis's places for town. The extract is answered by
// the day directory the fixtures write, so no run reads the host.
func analysisPaths(town string) metrics.AnalysisOptions {
	day := filepath.Join(town, ".runtime", "metrics", "2026-10-01")
	return metrics.AnalysisOptions{
		MangoGitDir: filepath.Join(town, "mango", ".repo.git"),
		MangoBin:    filepath.Join(town, ".runtime", "metrics", "bin", "mango"),
		Extract:     func(context.Context, string, string) (string, error) { return day, nil },
	}
}

// analyseRunEnv answers the commands the analysis runs before it reads the
// extract's landings: the mango head, its archive and the build.
func analyseRunEnv(t *testing.T) metrics.RunnerEnv {
	t.Helper()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	body := "module mango\n"
	if err := tw.WriteHeader(&tar.Header{Name: "go.mod", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, argv, _ []string) ([]byte, error) {
		switch argv[0] {
		case "git":
			if strings.Contains(strings.Join(argv, " "), "archive") {
				return archive.Bytes(), nil
			}
			return []byte("0123456789abcdef0123456789abcdef01234567\n"), nil
		case "go":
			return nil, nil
		}
		return nil, errors.New("unexpected command: " + strings.Join(argv, " "))
	}
}
