package cmd

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Unit tier: the command over fakes; no real files, git or clock.

func questionsFakeEnv(files map[string]string) questionsEnv {
	town := "/town"
	return questionsEnv{
		Town: town,
		Now:  func() time.Time { return time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) },
		ReadFile: func(p string) ([]byte, error) {
			if s, ok := files[p]; ok {
				return []byte(s), nil
			}
			return nil, fs.ErrNotExist
		},
		Glob: func(pattern string) ([]string, error) {
			return []string{"/town/.runtime/metrics/2026-10-31", "/town/.runtime/metrics/2026-10-30", "/town/.runtime/metrics/bin"}, nil
		},
		GitShow: func(_ context.Context, repo, spec string) ([]byte, error) {
			if repo != "/town/.dolt-archive/git" {
				return nil, errors.New("unexpected repo " + repo)
			}
			if spec == "HEAD:gt/events.jsonl" {
				return []byte(`{"issue_id":"gt-a","event_type":"created","created_at":"2026-11-01 10:00:00"}` + "\n" +
					`{"issue_id":"gt-a","event_type":"label_added","comment":"Added label: gt:ready-to-land","created_at":"2026-11-01 12:00:00"}` + "\n" +
					`{"issue_id":"gt-a","event_type":"closed","created_at":"2026-11-01 12:30:00"}` + "\n"), nil
			}
			return nil, errors.New("no such shard")
		},
	}
}

func TestMetricsQuestionsPrintsAllTenWithTheLatestLedger(t *testing.T) {
	t.Parallel()
	rt := "/town/.runtime"
	env := questionsFakeEnv(map[string]string{
		filepath.Join(rt, "landings", "reviews.jsonl"): `{"bead":"gt-a","attempt":2,"at":"2026-11-01T01:00:00Z"}` + "\n",
		filepath.Join(rt, "metrics", "snapshots.jsonl"): `{"ts":"old","host":{"load1":1}}` + "\n" +
			`{"ts":"2026-11-01T16:13:49Z","host":{"load1":13.5},"queue":{"ready":215},"money":{"deepseek_spend_age_s":60,"deepseek_balance":74.55}}` + "\n",
		filepath.Join(rt, "metrics", "2026-10-31", "landings.csv"): "bead,rig,landed_at,route\ngt-a,gastown,2026-11-01T12:30:00Z,daemon\n",
	})
	var out bytes.Buffer
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		" 3. Review loop", "1 beads reviewed", // review log read
		"load1 13.5",         // the LAST snapshot line, not the first
		"balance $74.55",     // fresh balance printed
		"ready 215",          // backlog
		"11-01 2.0/0.50/2.5", // time to land from the fake archive events
		"Ledger 2026-10-31",  // the newest dated dir, not bin
		"10. Key and credential health",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestMetricsQuestionsNamesAnUnreadableSourceAndKeepsGoing(t *testing.T) {
	t.Parallel()
	env := questionsFakeEnv(map[string]string{}) // every file missing
	var out bytes.Buffer
	if err := runMetricsQuestions(context.Background(), &out, env, 7); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"review log unreadable", "snapshot log unreadable", "landing ledger unreadable", "note: review log:", "note: snapshot log:", " 7. Onboarding friction"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// questionsFailAfter is a writer that fails once n bytes have been written.
type questionsFailAfter struct{ left int }

func (w *questionsFailAfter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errors.New("write refused")
	}
	w.left -= len(p)
	return len(p), nil
}

func TestMetricsQuestionsReportsAWriteFailureOfTheTable(t *testing.T) {
	t.Parallel()
	err := runMetricsQuestions(context.Background(), &questionsFailAfter{left: 0}, questionsFakeEnv(map[string]string{}), 7)
	if err == nil || !strings.Contains(err.Error(), "write refused") {
		t.Errorf("a refused write of the table must be returned, got %v", err)
	}
}

func TestMetricsQuestionsReportsAWriteFailureOfTheNotes(t *testing.T) {
	t.Parallel()
	// Room for the whole table but not for the first note line: measure the table first.
	var table bytes.Buffer
	env := questionsFakeEnv(map[string]string{})
	if err := runMetricsQuestions(context.Background(), &table, env, 7); err != nil {
		t.Fatal(err)
	}
	idx := strings.Index(table.String(), "note: ")
	if idx < 0 {
		t.Fatalf("fixture must produce a note line:\n%s", table.String())
	}
	err := runMetricsQuestions(context.Background(), &questionsFailAfter{left: idx + 1}, env, 7)
	if err == nil {
		t.Error("a refused write of a note line must be returned")
	}
}

func TestMetricsQuestionsInReturnsTheTownLookupError(t *testing.T) {
	t.Parallel()
	want := errors.New("not in a town")
	err := runMetricsQuestionsIn(context.Background(), &bytes.Buffer{}, func() (string, error) { return "", want }, 7)
	if !errors.Is(err, want) {
		t.Errorf("got %v, want the lookup error", err)
	}
}
