package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jobledger"
)

// cacheNeedTestDeps fakes the town, clock and ledger; a job in errs fails.
func cacheNeedTestDeps(runs map[string][]jobledger.Run, errs map[string]error, now time.Time) cacheNeedDeps {
	return cacheNeedDeps{
		findTown: func() (string, error) { return "/town", nil },
		now:      func() time.Time { return now },
		tail:     func(_, job string, _ int) ([]jobledger.Run, error) { return runs[job], errs[job] },
	}
}

func runCacheNeedCommand(t *testing.T, deps cacheNeedDeps, args ...string) (string, error) {
	t.Helper()
	c := newCacheCmd(deps)
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(io.Discard)
	c.SetArgs(append([]string{"need"}, args...))
	err := c.Execute()
	return out.String(), err
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

	text, err := runCacheNeedCommand(t, deps)
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
	raw, err := runCacheNeedCommand(t, deps, "--json")
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
		text, err := runCacheNeedCommand(t, deps, args...)
		if err == nil || !strings.Contains(err.Error(), "ci_cache_trim:ci-gomod") || !strings.Contains(err.Error(), boom.Error()) {
			t.Fatalf("%v: err = %v, want one naming the job and its cause", args, err)
		}
		if text != "" {
			t.Errorf("%v: wrote %q before failing", args, text)
		}
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
			if _, err := runCacheNeedCommand(t, deps, tc.arg); err == nil || !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("%s: err = %v, want one naming %q", tc.arg, err, tc.cause)
			}
		})
	}
}
