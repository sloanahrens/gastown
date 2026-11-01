package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/metrics"
)

// shapeSHA is the commit id the fake archive log answers with.
const shapeSHA = "0123456789abcdef0123456789abcdef01234567"

// shapeTown writes the review log a run reads and returns the town.
func shapeTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"bead":"gt-a","attempt":1,"score":0.8,"verdict":"approve","at":"2026-09-10T12:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "reviews.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// shapeRow returns the fields of the table row for feature and bucket.
func shapeRow(t *testing.T, out, feature, bucket string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 2 && f[0] == feature && f[1] == bucket {
			return f
		}
	}
	t.Fatalf("no %s %s row in:\n%s", feature, bucket, out)
	return nil
}

// shapeGoodEvents date both ends of gt-a's life, so the bead has a time to
// close.
const shapeGoodEvents = `{"issue_id":"gt-a","event_type":"created","created_at":"2026-09-10 09:00:00"}` + "\n" +
	`{"issue_id":"gt-a","event_type":"closed","created_at":"2026-09-10 21:00:00"}` + "\n"

// shapeGoodEnv is a town whose archive and review log answer one landed bead,
// and the runner calls it made. It takes the run to the print stage, so a test
// can drive any later failure.
func shapeGoodEnv(t *testing.T) (shapeArchiveEnv, *[][]string) {
	t.Helper()
	return shapeEnvWithEvents(t, shapeGoodEvents)
}

// shapeEnvWithEvents is shapeGoodEnv with the archive's event table chosen by
// the test, so a test can drop a stamp and read how the receipt counts it.
func shapeEnvWithEvents(t *testing.T, events string) (shapeArchiveEnv, *[][]string) {
	t.Helper()
	town := shapeTown(t)
	issues := `{"id":"gt-a","description":"## Size\nOne worker, one landing.","acceptance_criteria":"- [ ] a","created_at":"2026-09-10T09:00:00Z"}` + "\n"
	calls := &[][]string{}
	env := shapeArchiveEnv{
		Town:     town,
		Rig:      "gt",
		ReadFile: os.ReadFile,
		Run: func(_ context.Context, argv []string) ([]byte, error) {
			*calls = append(*calls, argv)
			joined := strings.Join(argv, " ")
			switch {
			case strings.Contains(joined, " log "):
				return []byte(shapeSHA + " 2026-09-10T23:00:00Z\n"), nil
			case strings.Contains(joined, " ls-tree "):
				return []byte("gt/events.jsonl\ngt/issues.jsonl\n"), nil
			case strings.Contains(joined, "show HEAD:gt/events.jsonl"):
				return []byte(events), nil
			case strings.Contains(joined, "show ") && strings.Contains(joined, ":gt/issues.jsonl"):
				return []byte(issues), nil
			default:
				return nil, fmt.Errorf("unexpected argv %v", argv)
			}
		},
	}
	return env, calls
}

func TestMetricsShapeOutcomeUsesTheInjectedRunner(t *testing.T) {
	t.Parallel()
	env, callsPtr := shapeGoodEnv(t)
	town := env.Town
	var out bytes.Buffer
	if err := runMetricsShapeOutcome(context.Background(), &out, env); err != nil {
		t.Fatalf("runMetricsShapeOutcome: %v", err)
	}
	calls := *callsPtr
	if len(calls) < 4 {
		t.Fatalf("runner calls = %d, want the log, the tree listing and both shows", len(calls))
	}
	for _, argv := range calls {
		if argv[0] != "git" {
			t.Errorf("runner call %v, want git", argv)
		}
	}
	repo := filepath.Join(town, ".dolt-archive", "git")
	if want := []string{"git", "-C", repo, "log", "--first-parent", "--format=%H %cI", "--", "gt/issues.jsonl"}; !slices.Equal(calls[0], want) {
		t.Errorf("first call = %v, want the archive log %v", calls[0], want)
	}
	if want := []string{"git", "-C", repo, "show", shapeSHA + ":gt/issues.jsonl"}; !slices.ContainsFunc(calls, func(argv []string) bool { return slices.Equal(argv, want) }) {
		t.Errorf("no call read the sampled commit's issues.jsonl (%v): %v", want, calls)
	}
	got := out.String()
	wantReceipt := "shape-outcome gt: 1 sampled day(s) 2026-09-10..2026-09-10, 1 first-filed bead(s); 1 joined to a landing, " +
		"0 with no review, 0 reviewed but not landed, 0 landed without a first-filed row; " +
		"1 read from their filed day's last export (edited_same_day); 0 landed with no time to close; 0 row(s) dropped\n"
	if !strings.HasPrefix(got, wantReceipt) {
		t.Fatalf("receipt = %q, want it to start %q", got, wantReceipt)
	}
	want := []string{"acceptance_boxes", "1", "1", "1.00", "100%", "0.80", "12.0", "1", "thin"}
	gotRow := shapeRow(t, got, "acceptance_boxes", "1")
	if strings.Join(gotRow, " ") != strings.Join(want, " ") {
		t.Fatalf("acceptance_boxes 1 row = %v, want %v", gotRow, want)
	}
}

// TestMetricsShapeOutcomeCountsLandedBeadsWithoutATimeToClose drives the
// receipt's hours accounting: a landed bead whose closed stamp is missing from
// the archive is counted in the receipt rather than left as a silent gap in the
// hours column.
func TestMetricsShapeOutcomeCountsLandedBeadsWithoutATimeToClose(t *testing.T) {
	t.Parallel()
	events := `{"issue_id":"gt-a","event_type":"created","created_at":"2026-09-10 09:00:00"}` + "\n"
	env, _ := shapeEnvWithEvents(t, events)
	var out bytes.Buffer
	if err := runMetricsShapeOutcome(context.Background(), &out, env); err != nil {
		t.Fatalf("runMetricsShapeOutcome: %v", err)
	}
	if !strings.Contains(out.String(), "1 landed with no time to close") {
		t.Fatalf("receipt does not count the landed bead with no time to close:\n%s", out.String())
	}
}

// TestMetricsShapeOutcomeStopsWhenTheOutputFails drives each write the run
// makes: the receipt, the table header and a table row.
func TestMetricsShapeOutcomeStopsWhenTheOutputFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// accept is how many writes reach the output before it fails.
		accept int
	}{
		{"the receipt", 0},
		{"the table header", 1},
		{"a table row", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env, _ := shapeGoodEnv(t)
			if err := runMetricsShapeOutcome(context.Background(), &acceptWriter{accept: c.accept}, env); err == nil {
				t.Fatalf("a failed write at %s did not stop the run", c.name)
			}
		})
	}
}

func TestMetricsShapeOutcomeRefusesAnUnknownTown(t *testing.T) {
	t.Parallel()
	err := runMetricsShapeOutcomeIn(context.Background(), io.Discard, func() (string, error) {
		return "", errors.New("no town here")
	}, "gt")
	if err == nil || !strings.Contains(err.Error(), "no town here") {
		t.Fatalf("error = %v, want the town lookup's failure", err)
	}
}

func TestMetricsShapeOutcomeRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	okIssues := []byte(`{"id":"gt-a","description":"x","acceptance_criteria":"- [ ] a","created_at":"2026-09-10T09:00:00Z"}` + "\n")
	okEvents := []byte(`{"issue_id":"gt-a","event_type":"created","created_at":"2026-09-10 09:00:00"}` + "\n")
	okLog := []byte(shapeSHA + " 2026-09-10T23:00:00Z\n")
	// archive answers every command the run makes, or fails the one failOn
	// names so the run's handling of that read is the only thing under test.
	archive := func(failOn string, lsTree []byte) metrics.Runner {
		return func(_ context.Context, argv []string) ([]byte, error) {
			joined := strings.Join(argv, " ")
			if failOn != "" && strings.Contains(joined, failOn) {
				return nil, errors.New("archive refused")
			}
			switch {
			case strings.Contains(joined, " log "):
				return okLog, nil
			case strings.Contains(joined, " ls-tree "):
				return lsTree, nil
			case strings.Contains(joined, "show HEAD:gt/events.jsonl"):
				return okEvents, nil
			default:
				return okIssues, nil
			}
		}
	}
	cases := []struct {
		name string
		env  func(t *testing.T) shapeArchiveEnv
		want string
	}{
		{
			name: "no runner",
			env: func(t *testing.T) shapeArchiveEnv {
				return shapeArchiveEnv{Town: shapeTown(t), Rig: "gt", ReadFile: os.ReadFile}
			},
			want: "no runner",
		},
		{
			name: "no file reader",
			env: func(t *testing.T) shapeArchiveEnv {
				return shapeArchiveEnv{Town: shapeTown(t), Rig: "gt", Run: archive("", nil)}
			},
			want: "no file reader",
		},
		{
			name: "the archive log names no commit",
			env: func(t *testing.T) shapeArchiveEnv {
				return shapeArchiveEnv{Town: shapeTown(t), Rig: "gt", ReadFile: os.ReadFile,
					Run: func(_ context.Context, argv []string) ([]byte, error) {
						if strings.Contains(strings.Join(argv, " "), " log ") {
							return nil, nil
						}
						t.Fatalf("unexpected argv %v", argv)
						return nil, nil
					}}
			},
			want: "holds no commit",
		},
		{
			name: "the rig is not a database name",
			env: func(t *testing.T) shapeArchiveEnv {
				return shapeArchiveEnv{
					Town: shapeTown(t), Rig: "../gt", ReadFile: os.ReadFile,
					Run: func(context.Context, []string) ([]byte, error) {
						t.Error("the runner ran for a refused rig")
						return nil, nil
					},
				}
			},
			want: "not a database name",
		},
		{
			name: "the log cannot be read",
			env: func(t *testing.T) shapeArchiveEnv {
				return shapeArchiveEnv{Town: shapeTown(t), Rig: "gt", ReadFile: os.ReadFile,
					Run: archive(" log ", nil)}
			},
			want: "shape-outcome: reading",
		},
		{
			name: "a sampled commit cannot be read",
			env: func(t *testing.T) shapeArchiveEnv {
				return shapeArchiveEnv{Town: shapeTown(t), Rig: "gt", ReadFile: os.ReadFile,
					Run: archive("show "+shapeSHA, []byte("gt/events.jsonl\n"))}
			},
			want: "issues.jsonl at",
		},
		{
			name: "the tree cannot be listed",
			env: func(t *testing.T) shapeArchiveEnv {
				return shapeArchiveEnv{Town: shapeTown(t), Rig: "gt", ReadFile: os.ReadFile,
					Run: archive(" ls-tree ", nil)}
			},
			want: "listing",
		},
		{
			name: "an event shard cannot be read",
			env: func(t *testing.T) shapeArchiveEnv {
				return shapeArchiveEnv{Town: shapeTown(t), Rig: "gt", ReadFile: os.ReadFile,
					Run: archive("show HEAD:gt/events.jsonl", []byte("gt/events.jsonl\n"))}
			},
			want: "at HEAD",
		},
		{
			name: "the archive holds no events",
			env: func(t *testing.T) shapeArchiveEnv {
				return shapeArchiveEnv{Town: shapeTown(t), Rig: "gt", ReadFile: os.ReadFile,
					Run: archive("", []byte("gt/issues.jsonl\n"))}
			},
			want: "events*.jsonl",
		},
		{
			name: "the review log is missing",
			env: func(t *testing.T) shapeArchiveEnv {
				town := t.TempDir()
				return shapeArchiveEnv{Town: town, Rig: "gt", ReadFile: os.ReadFile,
					Run: archive("", []byte("gt/events.jsonl\n"))}
			},
			want: "review log",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runMetricsShapeOutcome(context.Background(), &out, c.env(t))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to name %q", err, c.want)
			}
		})
	}
}
