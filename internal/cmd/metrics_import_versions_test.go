package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadversions"
	"github.com/steveyegge/gastown/internal/metrics"
)

// importVersionsIssues is one archive issues.jsonl with a bead filed the day
// before the sampled commit, so its text is the text as filed.
const importVersionsIssues = `{"id":"gt-a","description":"## Goal\ntext","acceptance_criteria":"- [ ] a","created_at":"2026-09-09T09:00:00Z"}` + "\n"

// importVersionsTwoIssues holds two beads filed before the sampled commit, so
// a test that stops midway has a first row already recorded.
const importVersionsTwoIssues = importVersionsIssues +
	`{"id":"gt-b","description":"## Goal\nother","acceptance_criteria":"- [ ] b","created_at":"2026-09-09T10:00:00Z"}` + "\n"

// importVersionsSameDayIssues holds one bead filed before the sampled commit
// and one filed on the commit's own day.
const importVersionsSameDayIssues = importVersionsIssues +
	`{"id":"gt-b","description":"## Goal\nedited","acceptance_criteria":"- [ ] b","created_at":"2026-09-10T09:00:00Z"}` + "\n"

// importVersionsRunner is the archive's 15-minute export as the fake git
// answers it: one commit holding issues, and the commit's issues.jsonl.
func importVersionsRunner(issues string, failOn string) metrics.Runner {
	return func(_ context.Context, argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		if failOn != "" && strings.Contains(joined, failOn) {
			return nil, errors.New("archive refused")
		}
		switch {
		case strings.Contains(joined, " log "):
			return []byte(shapeSHA + " 2026-09-10T23:00:00Z\n"), nil
		case strings.Contains(joined, " show ") && strings.Contains(joined, ":gt/issues.jsonl"):
			return []byte(issues), nil
		default:
			return nil, fmt.Errorf("unexpected argv %v", argv)
		}
	}
}

// importVersionsStore reads the store's one line, so a test asserts what the
// command wrote rather than how.
func importVersionsStore(t *testing.T, path string) beadversions.Version {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the store was not written: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("store holds %d line(s), want 1:\n%s", len(lines), data)
	}
	var v beadversions.Version
	if err := json.Unmarshal(lines[0], &v); err != nil {
		t.Fatalf("store line is not a version: %v", err)
	}
	return v
}

// importVersionsFakeStore is a store the test drives. It answers versions from
// a real MemoryStore, while Load and Put can be made to fail, so a store
// failure is driven without the host's permissions.
type importVersionsFakeStore struct {
	beadversions.Store
	loadErr error
	// failPut is the 1-based Put call that returns an error; 0 fails none.
	failPut int
	puts    int
}

func (s *importVersionsFakeStore) Load() (int, error) { return 0, s.loadErr }

func (s *importVersionsFakeStore) Skipped() int { return 0 }

func (s *importVersionsFakeStore) Put(bead, text, actor string, at time.Time) (bool, error) {
	s.puts++
	if s.failPut != 0 && s.puts == s.failPut {
		return false, errors.New("store refused the append")
	}
	return s.Store.Put(bead, text, actor, at)
}

// importVersionsRefusingWriter refuses every write, so the receipt's failure
// path runs without a helper defined in another test file.
type importVersionsRefusingWriter struct{}

func (importVersionsRefusingWriter) Write([]byte) (int, error) {
	return 0, errors.New("output refused")
}

func TestMetricsImportVersionsUsesTheInjectedRunner(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	calls := &[][]string{}
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		*calls = append(*calls, argv)
		return importVersionsRunner(importVersionsIssues, "")(ctx, argv)
	}
	env := importVersionsEnv{Town: town, Rig: "gt", Run: run}

	var out bytes.Buffer
	if err := runMetricsImportVersions(context.Background(), &out, env); err != nil {
		t.Fatalf("runMetricsImportVersions: %v", err)
	}

	repo := filepath.Join(town, ".dolt-archive", "git")
	wantLog := []string{"git", "-C", repo, "log", "--first-parent", "--format=%H %cI", "--", "gt/issues.jsonl"}
	if len(*calls) == 0 || !slices.Equal((*calls)[0], wantLog) {
		t.Fatalf("first runner call = %v, want the archive log %v", *calls, wantLog)
	}
	if want := []string{"git", "-C", repo, "show", shapeSHA + ":gt/issues.jsonl"}; !slices.ContainsFunc(*calls, func(argv []string) bool { return slices.Equal(argv, want) }) {
		t.Errorf("no call read the sampled commit's issues.jsonl (%v): %v", want, *calls)
	}

	// The default path is <town>/.runtime/metrics/bead-versions.jsonl, and the
	// version is the row as filed, at its created_at, under the archive actor.
	path := filepath.Join(town, ".runtime", "metrics", "bead-versions.jsonl")
	v := importVersionsStore(t, path)
	if v.Bead != "gt-a" || v.Text != "## Goal\ntext\n\n- [ ] a" || v.Actor != "archive" {
		t.Errorf("stored version = %+v, want gt-a's filed text under archive", v)
	}
	if want := time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC); !v.At.Equal(want) {
		t.Errorf("stored at = %s, want the row's created_at %s", v.At, want)
	}
	if got := out.String(); !strings.Contains(got, "put 1, skipped 0") {
		t.Errorf("receipt = %q, want it to count put 1, skipped 0", got)
	}

	// A second run records nothing.
	out.Reset()
	if err := runMetricsImportVersions(context.Background(), &out, env); err != nil {
		t.Fatalf("second runMetricsImportVersions: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "put 0, skipped 1") {
		t.Errorf("second receipt = %q, want it to count put 0, skipped 1", got)
	}
	if v2 := importVersionsStore(t, path); v2.Text != v.Text || v2.Actor != v.Actor || !v2.At.Equal(v.At) {
		t.Errorf("second run changed the store: %+v, want %+v", v2, v)
	}
}

func TestMetricsImportVersionsWritesTheGivenPath(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.jsonl")
	env := importVersionsEnv{Town: t.TempDir(), Rig: "gt", Path: path, Run: importVersionsRunner(importVersionsIssues, "")}

	var out bytes.Buffer
	if err := runMetricsImportVersions(context.Background(), &out, env); err != nil {
		t.Fatalf("runMetricsImportVersions: %v", err)
	}
	if v := importVersionsStore(t, path); v.Bead != "gt-a" {
		t.Errorf("stored bead = %q, want gt-a at the given path", v.Bead)
	}
}

// TestMetricsImportVersionsLeavesOutABeadFromItsFiledDay drives the import's
// same-day limit: a bead first carried by a commit on its own filed day may
// already hold an edit, so it is not recorded and the receipt counts it.
func TestMetricsImportVersionsLeavesOutABeadFromItsFiledDay(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := importVersionsEnv{Town: town, Rig: "gt", Run: importVersionsRunner(importVersionsSameDayIssues, "")}

	var out bytes.Buffer
	if err := runMetricsImportVersions(context.Background(), &out, env); err != nil {
		t.Fatalf("runMetricsImportVersions: %v", err)
	}

	path := filepath.Join(town, ".runtime", "metrics", "bead-versions.jsonl")
	if v := importVersionsStore(t, path); v.Bead != "gt-a" {
		t.Errorf("stored bead = %q, want gt-a: gt-b was filed on the sampled day", v.Bead)
	}
	if got := out.String(); !strings.Contains(got, "1 edited_same_day (not imported)") || !strings.Contains(got, "put 1, skipped 0") {
		t.Errorf("receipt = %q, want the same-day bead counted and only gt-a put", got)
	}
}

func TestMetricsImportVersionsCountsADamagedStoreLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.jsonl")
	if err := os.WriteFile(path, []byte("not a version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := importVersionsEnv{Town: t.TempDir(), Rig: "gt", Path: path, Run: importVersionsRunner(importVersionsIssues, "")}

	var out bytes.Buffer
	if err := runMetricsImportVersions(context.Background(), &out, env); err != nil {
		t.Fatalf("runMetricsImportVersions: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "1 damaged store line(s)") || !strings.Contains(got, "put 1") {
		t.Errorf("receipt = %q, want the damaged line counted and the bead recorded", got)
	}
}

func TestMetricsImportVersionsRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  func(t *testing.T) importVersionsEnv
		want string
	}{
		{
			name: "no runner",
			env: func(t *testing.T) importVersionsEnv {
				return importVersionsEnv{Town: t.TempDir(), Rig: "gt"}
			},
			want: "no runner",
		},
		{
			name: "the rig is not a database name",
			env: func(t *testing.T) importVersionsEnv {
				return importVersionsEnv{Town: t.TempDir(), Rig: "../gt", Run: func(context.Context, []string) ([]byte, error) {
					t.Error("the runner ran for a refused rig")
					return nil, nil
				}}
			},
			want: "not a database name",
		},
		{
			name: "the archive log names no commit",
			env: func(t *testing.T) importVersionsEnv {
				return importVersionsEnv{Town: t.TempDir(), Rig: "gt", Run: func(_ context.Context, argv []string) ([]byte, error) {
					if !strings.Contains(strings.Join(argv, " "), " log ") {
						t.Fatalf("unexpected argv %v", argv)
					}
					return nil, nil
				}}
			},
			want: "holds no commit",
		},
		{
			name: "the log cannot be read",
			env: func(t *testing.T) importVersionsEnv {
				return importVersionsEnv{Town: t.TempDir(), Rig: "gt", Run: importVersionsRunner(importVersionsIssues, " log ")}
			},
			want: "import-versions: reading",
		},
		{
			name: "a sampled commit cannot be read",
			env: func(t *testing.T) importVersionsEnv {
				return importVersionsEnv{Town: t.TempDir(), Rig: "gt", Run: importVersionsRunner(importVersionsIssues, "show "+shapeSHA)}
			},
			want: "issues.jsonl at",
		},
		{
			name: "the store cannot be read",
			env: func(t *testing.T) importVersionsEnv {
				return importVersionsEnv{
					Town: t.TempDir(), Rig: "gt",
					Run: importVersionsRunner(importVersionsIssues, ""),
					NewStore: func(string) versionStore {
						return &importVersionsFakeStore{Store: beadversions.NewMemoryStore(), loadErr: errors.New("store refused to load")}
					},
				}
			},
			want: "reading the version store",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := runMetricsImportVersions(context.Background(), io.Discard, c.env(t))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to name %q", err, c.want)
			}
		})
	}
}

// TestMetricsImportVersionsStopsWhenTheStoreCannotBeAppended drives a refused
// second Put: the run fails, and the receipt still reports the row it recorded
// before the refusal.
func TestMetricsImportVersionsStopsWhenTheStoreCannotBeAppended(t *testing.T) {
	t.Parallel()
	env := importVersionsEnv{
		Town: t.TempDir(), Rig: "gt",
		Run: importVersionsRunner(importVersionsTwoIssues, ""),
		NewStore: func(string) versionStore {
			return &importVersionsFakeStore{Store: beadversions.NewMemoryStore(), failPut: 2}
		},
	}

	var out bytes.Buffer
	err := runMetricsImportVersions(context.Background(), &out, env)
	if err == nil || !strings.Contains(err.Error(), "import-versions: importing bead") {
		t.Fatalf("error = %v, want the refused append named", err)
	}
	if got := out.String(); !strings.Contains(got, "put 1") || !strings.Contains(got, "stopped:") {
		t.Errorf("receipt = %q, want it to report the one put and the stop", got)
	}
}

func TestMetricsImportVersionsStopsWhenTheOutputFails(t *testing.T) {
	t.Parallel()
	env := importVersionsEnv{Town: t.TempDir(), Rig: "gt", Run: importVersionsRunner(importVersionsIssues, "")}
	if err := runMetricsImportVersions(context.Background(), importVersionsRefusingWriter{}, env); err == nil {
		t.Fatal("a failed receipt write did not stop the run")
	}
}

func TestMetricsImportVersionsRefusesAnUnknownTown(t *testing.T) {
	t.Parallel()
	err := runMetricsImportVersionsIn(context.Background(), io.Discard, func() (string, error) {
		return "", errors.New("no town here")
	}, "gt", "")
	if err == nil || !strings.Contains(err.Error(), "no town here") {
		t.Fatalf("error = %v, want the town lookup's failure", err)
	}
}
