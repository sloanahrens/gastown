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
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadversions"
	"github.com/steveyegge/gastown/internal/metrics"
)

// importVersionsSHA is the sampled commit the fake archive answers for: 40
// lowercase hex digits, the form git prints.
const importVersionsSHA = "0123456789abcdef0123456789abcdef01234567"

// importVersionsIssues is one archive issues.jsonl with a bead written no more
// since creation than at creation, so its text is the text as filed.
const importVersionsIssues = `{"id":"gt-a","description":"## Goal\ntext","acceptance_criteria":"- [ ] a","created_at":"2026-09-09T09:00:00Z","updated_at":"2026-09-09T09:00:00Z"}` + "\n"

// importVersionsTwoIssues holds two as-filed beads, so a test that refuses one
// append has another row either side of it.
const importVersionsTwoIssues = importVersionsIssues +
	`{"id":"gt-b","description":"## Goal\nother","acceptance_criteria":"- [ ] b","created_at":"2026-09-09T10:00:00Z","updated_at":"2026-09-09T10:00:00Z"}` + "\n"

// importVersionsThreeIssues holds three as-filed beads, so a row after a
// refused one can still be recorded.
const importVersionsThreeIssues = importVersionsTwoIssues +
	`{"id":"gt-c","description":"## Goal\nthird","acceptance_criteria":"- [ ] c","created_at":"2026-09-09T11:00:00Z","updated_at":"2026-09-09T11:00:00Z"}` + "\n"

// importVersionsEditedIssues holds one as-filed bead and one bead filed on the
// sampled commit's own day and written again later that day, so its text is
// not the text as filed.
const importVersionsEditedIssues = importVersionsIssues +
	`{"id":"gt-b","description":"## Goal\nedited","acceptance_criteria":"- [ ] b","created_at":"2026-09-10T09:00:00Z","updated_at":"2026-09-10T12:00:00Z"}` + "\n"

// importVersionsLateFiledIssues holds a bead filed after its day's last export
// and so first carried by the next day's sample, in two forms: one written
// again before the sample and one untouched. The filed-day flag cannot tell
// them apart, because neither was filed on the sample's day; the row's own
// updated_at can.
const importVersionsLateFiledIssues = importVersionsIssues +
	`{"id":"gt-edited","description":"## Goal\nedited late","acceptance_criteria":"- [ ] e","created_at":"2026-09-09T23:50:00Z","updated_at":"2026-09-10T08:00:00Z"}` + "\n" +
	`{"id":"gt-clean","description":"## Goal\nuntouched late","acceptance_criteria":"- [ ] c","created_at":"2026-09-09T23:50:00Z","updated_at":"2026-09-09T23:50:00Z"}` + "\n"

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
			return []byte(importVersionsSHA + " 2026-09-10T23:00:00Z\n"), nil
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

// importVersionsBeads reads every version the store holds, keyed by bead, so a
// test asserts which beads were recorded rather than how.
func importVersionsBeads(t *testing.T, path string) map[string]beadversions.Version {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the store was not written: %v", err)
	}
	beads := make(map[string]beadversions.Version)
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var v beadversions.Version
		if err := json.Unmarshal(line, &v); err != nil {
			t.Fatalf("store line is not a version: %v", err)
		}
		beads[v.Bead] = v
	}
	return beads
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

// TestMetricsImportVersionsUsesTheInjectedRunner proves the archive is read
// only through the injected runner: the runner's issues are what the store
// holds, and the town it names has no archive of its own.
func TestMetricsImportVersionsUsesTheInjectedRunner(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	calls := 0
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		calls++
		return importVersionsRunner(importVersionsIssues, "")(ctx, argv)
	}
	env := importVersionsEnv{Town: town, Rig: "gt", Run: run}

	var out bytes.Buffer
	if err := runMetricsImportVersions(context.Background(), &out, env); err != nil {
		t.Fatalf("runMetricsImportVersions: %v", err)
	}
	if calls == 0 {
		t.Fatal("the injected runner never ran, so the archive was not read through it")
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

// TestMetricsImportVersionsLeavesOutABeadEditedAfterCreation drives the
// import's proof: a bead written again after its created_at may not hold its
// filed text, so it is not recorded and the receipt counts it.
func TestMetricsImportVersionsLeavesOutABeadEditedAfterCreation(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := importVersionsEnv{Town: town, Rig: "gt", Run: importVersionsRunner(importVersionsEditedIssues, "")}

	var out bytes.Buffer
	if err := runMetricsImportVersions(context.Background(), &out, env); err != nil {
		t.Fatalf("runMetricsImportVersions: %v", err)
	}

	path := filepath.Join(town, ".runtime", "metrics", "bead-versions.jsonl")
	if v := importVersionsStore(t, path); v.Bead != "gt-a" {
		t.Errorf("stored bead = %q, want gt-a: gt-b was written after creation", v.Bead)
	}
	if got := out.String(); !strings.Contains(got, "1 edited after creation (not imported)") || !strings.Contains(got, "put 1, skipped 0") {
		t.Errorf("receipt = %q, want the edited bead counted and only gt-a put", got)
	}
}

// TestMetricsImportVersionsLeavesOutALateFiledBeadThatWasEdited is the om
// finding's regression: a bead filed after its day's last export is first
// carried by the next day's sample, so the filed-day flag does not mark it; its
// own updated_at does, and the untouched bead beside it is still imported.
func TestMetricsImportVersionsLeavesOutALateFiledBeadThatWasEdited(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := importVersionsEnv{Town: town, Rig: "gt", Run: importVersionsRunner(importVersionsLateFiledIssues, "")}

	var out bytes.Buffer
	if err := runMetricsImportVersions(context.Background(), &out, env); err != nil {
		t.Fatalf("runMetricsImportVersions: %v", err)
	}

	path := filepath.Join(town, ".runtime", "metrics", "bead-versions.jsonl")
	beads := importVersionsBeads(t, path)
	if _, ok := beads["gt-edited"]; ok {
		t.Error("gt-edited was recorded: it was written after creation, so its text is not the text as filed")
	}
	for _, id := range []string{"gt-a", "gt-clean"} {
		if _, ok := beads[id]; !ok {
			t.Errorf("%s was not recorded: nothing wrote it after creation", id)
		}
	}
	if got := out.String(); !strings.Contains(got, "1 edited after creation (not imported)") || !strings.Contains(got, "put 2, skipped 0") {
		t.Errorf("receipt = %q, want the edited late-filed bead counted and the other two put", got)
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
				return importVersionsEnv{Town: t.TempDir(), Rig: "gt", Run: importVersionsRunner(importVersionsIssues, "show "+importVersionsSHA)}
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

// TestMetricsImportVersionsKeepsGoingWhenTheStoreRefusesARow drives a refused
// second Put: the third row is still recorded, the run fails naming the
// refused bead, and the receipt reports every row it recorded.
func TestMetricsImportVersionsKeepsGoingWhenTheStoreRefusesARow(t *testing.T) {
	t.Parallel()
	env := importVersionsEnv{
		Town: t.TempDir(), Rig: "gt",
		Run: importVersionsRunner(importVersionsThreeIssues, ""),
		NewStore: func(string) versionStore {
			return &importVersionsFakeStore{Store: beadversions.NewMemoryStore(), failPut: 2}
		},
	}

	var out bytes.Buffer
	err := runMetricsImportVersions(context.Background(), &out, env)
	if err == nil || !strings.Contains(err.Error(), "importing bead gt-b") {
		t.Fatalf("error = %v, want it to name the refused bead gt-b", err)
	}
	if got := out.String(); !strings.Contains(got, "put 2, skipped 0") || !strings.Contains(got, "failed:") {
		t.Errorf("receipt = %q, want the two puts and the failure", got)
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
