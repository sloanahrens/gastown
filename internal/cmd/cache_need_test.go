package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jobledger"
)

// cacheNeedTestDeps fakes the town, clock and ledger; a job in errs fails.
func cacheNeedTestDeps(runs map[string][]jobledger.Run, errs map[string]error, now time.Time) cacheNeedDeps {
	return cacheNeedTestDepsSkipped(runs, errs, nil, now)
}

// cacheNeedTestDepsSkipped also reports skipped lines per read, as the ledger does.
func cacheNeedTestDepsSkipped(runs map[string][]jobledger.Run, errs map[string]error, skipped map[string]int, now time.Time) cacheNeedDeps {
	return cacheNeedDeps{
		findTown: func() (string, error) { return "/town", nil },
		now:      func() time.Time { return now },
		tail: func(_, job string, _ int) ([]jobledger.Run, int, error) {
			return runs[job], skipped[job], errs[job]
		},
	}
}

func runCacheNeedCommand(t *testing.T, deps cacheNeedDeps, args ...string) (string, string, error) {
	t.Helper()
	c := newCacheCmd(deps)
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(append([]string{"need"}, args...))
	err := c.Execute()
	return out.String(), errOut.String(), err
}

func TestCacheNeedRows(t *testing.T) {
	t.Parallel()
	at := func(h int) time.Time {
		return time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour)
	}
	run := func(job string, h int, outcome string, stats map[string]int64) jobledger.Run {
		return jobledger.Run{Job: job, Outcome: outcome, EndedAt: at(h), Stats: stats}
	}
	runs := map[string][]jobledger.Run{
		"sweep_cache_trim": {
			run("sweep_cache_trim", 1, "ok", map[string]int64{"young_bytes": 1000, "claim_bytes_before": 4000, "cap_bytes": 3000}),
			run("sweep_cache_trim", 3, "ok", map[string]int64{"young_bytes": 2000, "claim_bytes_before": 5000, "cap_bytes": 3000}),
			run("sweep_cache_trim", 4, "failed", map[string]int64{"young_bytes": 99999}),
		},
		"ci_cache_trim:ci-gocache": {run("ci_cache_trim:ci-gocache", 2, "ok", map[string]int64{"young_bytes": 500})},
	}
	deps := cacheNeedTestDeps(runs, nil, at(0).Add(7*24*time.Hour))

	text, _, err := runCacheNeedCommand(t, deps)
	if err != nil {
		t.Fatalf("text run: %v", err)
	}
	want := strings.Join([]string{
		"sweep_cache_trim runs=2 window=2026-11-27T01:00:00Z..2026-11-27T03:00:00Z young_bytes=2000 size_bytes=5000 cap_bytes=3000 need_bytes=2500",
		"ci_cache_trim:ci-gocache runs=1 window=2026-11-27T02:00:00Z..2026-11-27T02:00:00Z young_bytes=500 size_bytes=none cap_bytes=none need_bytes=625",
		"ci_cache_trim:ci-gomod no data",
		"ci_cache_trim:ci-npm no data",
	}, "\n") + "\n"
	if text != want {
		t.Errorf("text =\n%s\nwant\n%s", text, want)
	}

	decode := func(s string) any {
		t.Helper()
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, s)
		}
		return v
	}
	raw, _, err := runCacheNeedCommand(t, deps, "--json")
	if err != nil {
		t.Fatalf("json run: %v", err)
	}
	wantJSON := `[{"job":"sweep_cache_trim","runs":2,"first_end":"2026-11-27T01:00:00Z","last_end":"2026-11-27T03:00:00Z","young_bytes":2000,"size_bytes":5000,"cap_bytes":3000,"need_bytes":2500},` +
		`{"job":"ci_cache_trim:ci-gocache","runs":1,"first_end":"2026-11-27T02:00:00Z","last_end":"2026-11-27T02:00:00Z","young_bytes":500,"need_bytes":625},` +
		`{"job":"ci_cache_trim:ci-gomod","no_data":true},{"job":"ci_cache_trim:ci-npm","no_data":true}]`
	if got, want := decode(raw), decode(wantJSON); !reflect.DeepEqual(got, want) {
		t.Errorf("json = %s\nwant %s", raw, wantJSON)
	}
}

func TestCacheNeedLedgerError(t *testing.T) {
	t.Parallel()
	boom := errors.New("ledger on a broken disk")
	deps := cacheNeedTestDeps(nil, map[string]error{"ci_cache_trim:ci-gomod": boom}, time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC))
	for _, args := range [][]string{nil, {"--json"}} {
		text, _, err := runCacheNeedCommand(t, deps, args...)
		if err == nil || !strings.Contains(err.Error(), "ci_cache_trim:ci-gomod") || !strings.Contains(err.Error(), boom.Error()) {
			t.Fatalf("%v: err = %v, want one naming the job and its cause", args, err)
		}
		if text != "" {
			t.Errorf("%v: wrote %q before failing", args, text)
		}
	}
}

func TestCacheNeedSkippedLinesWarn(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	runs := map[string][]jobledger.Run{
		"sweep_cache_trim": {{Job: "sweep_cache_trim", Outcome: "ok", EndedAt: now.Add(time.Hour), Stats: map[string]int64{"young_bytes": 1000}}},
	}
	for _, args := range [][]string{nil, {"--json"}} {
		want, _, err := runCacheNeedCommand(t, cacheNeedTestDeps(runs, nil, now.Add(2*time.Hour)), args...)
		if err != nil {
			t.Fatalf("%v: clean run: %v", args, err)
		}
		for _, tc := range []struct {
			name    string
			skipped map[string]int
			warn    string
		}{
			{"none", nil, ""},
			{"one", map[string]int{"sweep_cache_trim": 1}, "gt cache need: 1 ledger line(s) could not be decoded; the figures may be understated\n"},
			// Each read counts every undecodable line, so the largest read is the count, not their sum.
			{"largest read", map[string]int{"sweep_cache_trim": 1, "ci_cache_trim:ci-gocache": 3}, "gt cache need: 3 ledger line(s) could not be decoded; the figures may be understated\n"},
		} {
			t.Run(strings.Join(append([]string{tc.name}, args...), " "), func(t *testing.T) {
				deps := cacheNeedTestDepsSkipped(runs, nil, tc.skipped, now.Add(2*time.Hour))
				got, warn, err := runCacheNeedCommand(t, deps, args...)
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				if got != want {
					t.Errorf("rows changed by skipped lines:\n%s\nwant\n%s", got, want)
				}
				if warn != tc.warn {
					t.Errorf("stderr = %q, want %q", warn, tc.warn)
				}
			})
		}
	}
}

// cacheNeedFailWriter fails every Write, to drive the output error branches.
type cacheNeedFailWriter struct{}

func (cacheNeedFailWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestCacheNeedOutputError(t *testing.T) {
	t.Parallel()
	deps := cacheNeedTestDeps(nil, nil, time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC))
	for _, args := range [][]string{nil, {"--json"}} {
		c := newCacheCmd(deps)
		c.SetOut(cacheNeedFailWriter{})
		c.SetErr(io.Discard)
		c.SetArgs(append([]string{"need"}, args...))
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("%v: err = %v, want the write error", args, err)
		}
	}
}

func TestCacheNeedStderrError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	deps := cacheNeedTestDepsSkipped(nil, nil, map[string]int{"sweep_cache_trim": 1}, now)
	c := newCacheCmd(deps)
	c.SetOut(io.Discard)
	c.SetErr(cacheNeedFailWriter{})
	c.SetArgs([]string{"need"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v, want the stderr write error", err)
	}
}

func TestCacheNeedFindTownError(t *testing.T) {
	t.Parallel()
	deps := cacheNeedTestDeps(nil, nil, time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC))
	deps.findTown = func() (string, error) { return "", errors.New("no town") }
	if _, _, err := runCacheNeedCommand(t, deps); err == nil || !strings.Contains(err.Error(), "find the town root") {
		t.Errorf("err = %v, want one naming the town root", err)
	}
}

func TestCacheNeedRejectsBadFlags(t *testing.T) {
	t.Parallel()
	deps := cacheNeedTestDeps(nil, nil, time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC))
	for _, tc := range []struct{ arg, cause string }{
		{"--days=0", "at least 1"},
		{"--headroom-percent=201", "headroom"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			if _, _, err := runCacheNeedCommand(t, deps, tc.arg); err == nil || !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("%s: err = %v, want one naming %q", tc.arg, err, tc.cause)
			}
		})
	}
}

func TestCacheNeedRealDepsLedgerError(t *testing.T) {
	t.Parallel()
	if _, _, err := cacheNeedRealDeps().tail("", "sweep_cache_trim", 1); err == nil {
		t.Error("an empty town root read a ledger, want the NewLedger error")
	}
}

func TestCacheNeedRealLedgerRead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ledger, err := jobledger.NewLedger(townRoot)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	const job = "sweep_cache_trim"
	ended := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	for i := range 2 {
		if err := ledger.Append(jobledger.Run{Job: job, StartedAt: ended, EndedAt: ended.Add(time.Duration(i) * time.Hour), Outcome: "ok"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// One line the decoder rejects, as a damaged rotation or a bad write leaves behind.
	live, err := os.OpenFile(filepath.Join(townRoot, ".runtime", "jobs", "runs.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open live ledger: %v", err)
	}
	if _, err := live.WriteString("{not a run\n"); err != nil {
		t.Fatalf("write damaged line: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("close live ledger: %v", err)
	}
	runs, skipped, err := cacheNeedRealDeps().tail(townRoot, job, cacheNeedTailRuns)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(runs) != 2 || skipped != 1 {
		t.Errorf("tail = %d runs, %d skipped; want 2 runs, 1 skipped", len(runs), skipped)
	}
}
