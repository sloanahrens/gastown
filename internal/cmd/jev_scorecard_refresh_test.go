package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// jevRefreshScorer stands in for the battery adapters: it records the calls,
// answers with a report naming each battery, and fails the batteries in fail.
type jevRefreshScorer struct {
	calls []string
	from  []time.Time
	fail  map[string]error
}

func (s *jevRefreshScorer) score(_ context.Context, name, _ string, from time.Time) (scorecard.Report, error) {
	s.calls = append(s.calls, name)
	s.from = append(s.from, from)
	if err := s.fail[name]; err != nil {
		return scorecard.Report{}, err
	}
	return scorecard.Report{Battery: name, Items: 3, Labeled: 2}, nil
}

func (s *jevRefreshScorer) calledFor(name string) bool {
	for _, c := range s.calls {
		if c == name {
			return true
		}
	}
	return false
}

// readJevCache reads the town's cache file through the production reader.
func readJevCache(t *testing.T, town string) scorecard.CacheDoc {
	t.Helper()
	doc, err := (scorecard.Cache{}).Read(scorecard.CachePath(town))
	if err != nil {
		t.Fatalf("reading the cache: %v", err)
	}
	return doc
}

// blockFor finds a battery's block in doc.
func blockFor(t *testing.T, doc scorecard.CacheDoc, name string) scorecard.BatteryBlock {
	t.Helper()
	for _, b := range doc.Batteries {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("cache has no block for %q", name)
	return scorecard.BatteryBlock{}
}

// writeBatteryLog puts body at the log the battery registered under name
// reads for the town at town, and returns that path.
func writeBatteryLog(t *testing.T, town, name, body string) string {
	t.Helper()
	a, err := scorecard.Open(name, town)
	if err != nil {
		t.Fatalf("opening battery %q: %v", name, err)
	}
	path := a.LogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating the log directory for %q: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the log for %q: %v", name, err)
	}
	return path
}

// A battery whose log has not changed since its block was scored is not
// scored again, and the reused block keeps the scored_at the dashboard ages.
func TestJevScorecardRefreshReusesAnUnchangedLogsBlock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	writeBatteryLog(t, town, "finding", "one\ntwo\n")
	scored := now.Add(-24 * time.Hour)
	prev := scorecard.CacheDoc{
		ScoredAt: now.Add(-25 * time.Hour),
		Since:    "7d",
		Batteries: []scorecard.BatteryBlock{{
			Name: "finding", ScoredAt: scored, LogLines: 2, LogBytes: 8,
			Report: &scorecard.Report{Battery: "finding", Items: 2, Labeled: 1},
		}},
	}
	if err := (scorecard.Cache{}).Write(scorecard.CachePath(town), prev); err != nil {
		t.Fatal(err)
	}
	scorer := &jevRefreshScorer{}

	if err := jevScorecardRun(context.Background(), io.Discard, town,
		jevScorecardOptions{Refresh: true}, now, scorer.score, scorecard.Cache{}); err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if scorer.calledFor("finding") {
		t.Error("finding was scored, want the unchanged log's block reused")
	}
	b := blockFor(t, readJevCache(t, town), "finding")
	if !b.ScoredAt.Equal(scored) {
		t.Errorf("scored_at = %v, want the reused block's %v", b.ScoredAt, scored)
	}
	if b.Report == nil || b.Report.Items != 2 || b.Report.Labeled != 1 {
		t.Errorf("report = %+v, want the previous report", b.Report)
	}
	if b.LogLines != 2 || b.LogBytes != 8 {
		t.Errorf("fingerprint = %d lines %d bytes, want the log's 2 and 8", b.LogLines, b.LogBytes)
	}
}

// A battery whose log grew, whose block is four days old, whose block was
// scored over another window, or whose log cannot be measured is scored, not
// reused.
func TestJevScorecardRefreshScoresWhenTheBlockIsNotReusable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	body := "one\ntwo\n"
	cases := []struct {
		name      string
		log       string
		lines     int
		bytes     int64
		age       time.Duration
		since     string
		wantLines int
		wantBytes int64
	}{
		{"log grew", body, 1, 4, 24 * time.Hour, "7d", 2, 8},
		{"block is four days old", body, 2, 8, 4 * 24 * time.Hour, "7d", 2, 8},
		{"log cannot be stat-ed", "", 0, 0, 24 * time.Hour, "7d", 0, 0},
		{"block was scored over another window", body, 2, 8, 24 * time.Hour, "14d", 2, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.log != "" {
				writeBatteryLog(t, town, "finding", tc.log)
			}
			prev := scorecard.CacheDoc{
				ScoredAt: now.Add(-25 * time.Hour),
				Since:    tc.since,
				Batteries: []scorecard.BatteryBlock{{
					Name: "finding", ScoredAt: now.Add(-tc.age), LogLines: tc.lines, LogBytes: tc.bytes,
					Report: &scorecard.Report{Battery: "finding", Items: 9},
				}},
			}
			if err := (scorecard.Cache{}).Write(scorecard.CachePath(town), prev); err != nil {
				t.Fatal(err)
			}
			scorer := &jevRefreshScorer{}

			if err := jevScorecardRun(context.Background(), io.Discard, town,
				jevScorecardOptions{Refresh: true}, now, scorer.score, scorecard.Cache{}); err != nil {
				t.Fatalf("refreshing: %v", err)
			}
			if !scorer.calledFor("finding") {
				t.Error("finding was not scored, want the block re-scored")
			}
			b := blockFor(t, readJevCache(t, town), "finding")
			if !b.ScoredAt.Equal(now) {
				t.Errorf("scored_at = %v, want this refresh's %v", b.ScoredAt, now)
			}
			if b.LogLines != tc.wantLines || b.LogBytes != tc.wantBytes {
				t.Errorf("fingerprint = %d lines %d bytes, want the log's %d and %d", b.LogLines, b.LogBytes, tc.wantLines, tc.wantBytes)
			}
		})
	}
}

func TestJevScorecardRefreshWritesEveryRegisteredBattery(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	scorer := &jevRefreshScorer{}

	var out bytes.Buffer
	err := jevScorecardRun(context.Background(), &out, town,
		jevScorecardOptions{Refresh: true, Since: "7d"}, now, scorer.score, scorecard.Cache{})
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	doc := readJevCache(t, town)
	if !doc.ScoredAt.Equal(now) || doc.Since != "7d" {
		t.Errorf("cache scored_at %v since %q, want %v and 7d", doc.ScoredAt, doc.Since, now)
	}
	want := scorecard.Registered()
	if len(doc.Batteries) != len(want) {
		t.Fatalf("cache holds %d batteries, want %d", len(doc.Batteries), len(want))
	}
	for i, name := range want {
		b := doc.Batteries[i]
		if b.Name != name || b.Report == nil || b.Failed || b.Kept {
			t.Errorf("block %d = %+v, want a scored report for %q", i, b, name)
		}
		if !scorer.calledFor(name) {
			t.Errorf("battery %q was not scored", name)
		}
	}
	for _, from := range scorer.from {
		if !from.Equal(now.Add(-7 * 24 * time.Hour)) {
			t.Errorf("battery scored from %v, want %v", from, now.Add(-7*24*time.Hour))
		}
	}
	if !strings.Contains(out.String(), scorecard.CachePath(town)) {
		t.Errorf("output %q does not name the cache file", out.String())
	}
}

// A refresh without --since scores the last seven days, not all time.
func TestJevScorecardRefreshDefaultsToSevenDays(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	scorer := &jevRefreshScorer{}

	if err := jevScorecardRun(context.Background(), io.Discard, town,
		jevScorecardOptions{Refresh: true}, now, scorer.score, scorecard.Cache{}); err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if len(scorer.from) == 0 {
		t.Fatal("no battery was scored")
	}
	if want := now.Add(-7 * 24 * time.Hour); !scorer.from[0].Equal(want) {
		t.Errorf("window starts at %v, want %v", scorer.from[0], want)
	}
	if doc := readJevCache(t, town); doc.Since != "7d" {
		t.Errorf("cache since %q, want 7d", doc.Since)
	}
}

// A battery whose scoring fails keeps the report from its last good score.
func TestJevScorecardRefreshKeepsTheLastGoodBlock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	scored := now.Add(-2 * time.Hour)
	good := scorecard.CacheDoc{
		ScoredAt: now.Add(-4 * time.Hour),
		Since:    "7d",
		Batteries: []scorecard.BatteryBlock{{
			Name:     "finding",
			ScoredAt: scored,
			Report:   &scorecard.Report{Battery: "finding", Items: 11, Labeled: 8},
		}},
	}
	if err := (scorecard.Cache{}).Write(scorecard.CachePath(town), good); err != nil {
		t.Fatal(err)
	}
	scorer := &jevRefreshScorer{fail: map[string]error{"finding": errors.New("reading the finding log")}}

	err := jevScorecardRun(context.Background(), io.Discard, town,
		jevScorecardOptions{Refresh: true}, now, scorer.score, scorecard.Cache{})
	if err == nil || !strings.Contains(err.Error(), "finding") {
		t.Fatalf("refresh error = %v, want one naming finding", err)
	}

	b := blockFor(t, readJevCache(t, town), "finding")
	if !b.Failed || !b.Kept {
		t.Errorf("block failed %v kept %v, want both set", b.Failed, b.Kept)
	}
	if b.KeptSince == nil || !b.KeptSince.Equal(scored) {
		t.Errorf("kept_since = %v, want %v", b.KeptSince, scored)
	}
	if !b.ScoredAt.Equal(scored) {
		t.Errorf("scored_at = %v, want the last good score at %v", b.ScoredAt, scored)
	}
	if b.Report == nil || b.Report.Items != 11 || b.Report.Labeled != 8 {
		t.Errorf("kept report = %+v, want the previous report", b.Report)
	}
	if !strings.Contains(b.Error, "reading the finding log") {
		t.Errorf("block error %q does not name this refresh's failure", b.Error)
	}
}

// A battery that never produced a good report has nothing to keep: its block
// carries the failure alone.
func TestJevScorecardRefreshNeverScoredBatteryStaysFailed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	scorer := &jevRefreshScorer{fail: map[string]error{"stale": errors.New("no labels")}}

	err := jevScorecardRun(context.Background(), io.Discard, town,
		jevScorecardOptions{Refresh: true}, now, scorer.score, scorecard.Cache{})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("refresh error = %v, want one naming stale", err)
	}

	b := blockFor(t, readJevCache(t, town), "stale")
	if !b.Failed {
		t.Errorf("block failed false, want a failed block")
	}
	if b.Kept || b.KeptSince != nil || b.Report != nil {
		t.Errorf("block = %+v, want no kept data for a battery that never scored", b)
	}
	if !b.ScoredAt.Equal(now) {
		t.Errorf("scored_at = %v, want the failed refresh at %v", b.ScoredAt, now)
	}
}

// --refresh refuses the flags that describe a single battery's report, before
// it scores or writes anything.
func TestJevScorecardRefreshRefusesTheSingleBatteryFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		o    jevScorecardOptions
	}{
		{"battery", jevScorecardOptions{Refresh: true, Battery: "finding"}},
		{"json", jevScorecardOptions{Refresh: true, JSON: true}},
		{"lines", jevScorecardOptions{Refresh: true, Lines: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			scorer := &jevRefreshScorer{}
			err := jevScorecardRun(context.Background(), io.Discard, town, tc.o,
				time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), scorer.score, scorecard.Cache{})
			if err == nil {
				t.Fatalf("--refresh with --%s was accepted", tc.name)
			}
			if len(scorer.calls) != 0 {
				t.Errorf("refused refresh still scored %v", scorer.calls)
			}
			if _, err := (scorecard.Cache{}).Read(scorecard.CachePath(town)); !errors.Is(err, scorecard.ErrNoScorecard) {
				t.Errorf("refused refresh wrote a cache file: %v", err)
			}
		})
	}
}

func TestJevScorecardRefreshRefusesABadSince(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	scorer := &jevRefreshScorer{}
	err := jevScorecardRun(context.Background(), io.Discard, town,
		jevScorecardOptions{Refresh: true, Since: "week"},
		time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), scorer.score, scorecard.Cache{})
	if err == nil {
		t.Fatal("--since week was accepted")
	}
	if len(scorer.calls) != 0 {
		t.Errorf("refused refresh still scored %v", scorer.calls)
	}
}

// A corrupt cache is not a cache with no blocks: the refresh stops rather than
// overwrite the file the operator will want to read.
func TestJevScorecardRefreshRefusesACorruptCache(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := scorecard.CachePath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a scorecard"), 0o644); err != nil {
		t.Fatal(err)
	}
	scorer := &jevRefreshScorer{}

	err := jevScorecardRun(context.Background(), io.Discard, town,
		jevScorecardOptions{Refresh: true}, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), scorer.score, scorecard.Cache{})
	if !errors.Is(err, scorecard.ErrCorruptScorecard) {
		t.Fatalf("refresh error = %v, want ErrCorruptScorecard", err)
	}
	if len(scorer.calls) != 0 {
		t.Errorf("refused refresh still scored %v", scorer.calls)
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "not a scorecard" {
		t.Errorf("refused refresh rewrote the cache file: %q", body)
	}
}

// A declared battery the binary does not register stops the refresh at the
// block's own load, naming it, before anything is scored.
func TestJevScorecardRefreshRefusesARetiredBattery(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_scorecard":{` +
		`"enabled":true,"batteries":["retired_battery"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	scorer := &jevRefreshScorer{}

	err := jevScorecardRun(context.Background(), io.Discard, town,
		jevScorecardOptions{Refresh: true}, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), scorer.score, scorecard.Cache{})
	if err == nil || !strings.Contains(err.Error(), `"retired_battery"`) {
		t.Fatalf("refresh error = %v, want one naming retired_battery", err)
	}
	if len(scorer.calls) != 0 {
		t.Errorf("refused refresh still scored %v", scorer.calls)
	}
}

// A patrol config that does not load stops the refresh rather than reading as
// no block: what the town declares cannot be checked otherwise.
func TestJevScorecardRefreshRefusesAnUnreadablePatrolConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"daemon-patrol-config","patrols":`), 0o644); err != nil {
		t.Fatal(err)
	}
	scorer := &jevRefreshScorer{}

	err := jevScorecardRun(context.Background(), io.Discard, town,
		jevScorecardOptions{Refresh: true}, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), scorer.score, scorecard.Cache{})
	if err == nil || !strings.Contains(err.Error(), "daemon.json") {
		t.Fatalf("refresh error = %v, want one naming daemon.json", err)
	}
	if len(scorer.calls) != 0 {
		t.Errorf("refused refresh still scored %v", scorer.calls)
	}
}

// A cache file that cannot be written fails the refresh after it has scored;
// the error names the write, not a battery.
func TestJevScorecardRefreshReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	scorer := &jevRefreshScorer{}
	writeErr := errors.New("disk full")
	cache := scorecard.Cache{WriteFile: func(string, []byte, os.FileMode) error { return writeErr }}

	err := jevScorecardRun(context.Background(), io.Discard, town,
		jevScorecardOptions{Refresh: true}, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), scorer.score, cache)
	if !errors.Is(err, writeErr) {
		t.Fatalf("refresh error = %v, want %v", err, writeErr)
	}
	if len(scorer.calls) != len(scorecard.Registered()) {
		t.Errorf("scored %d batteries before the failed write, want %d", len(scorer.calls), len(scorecard.Registered()))
	}
}

// The fingerprint reader reports a battery name it cannot open.
func TestJevBatteryLogFingerprintRefusesAnUnknownName(t *testing.T) {
	t.Parallel()
	_, err := jevBatteryLogFingerprint("no-such-battery", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no-such-battery") {
		t.Fatalf("error = %v, want one naming no-such-battery", err)
	}
}

// The production scorer reports a battery name it cannot open.
func TestScoreJevBatteryRefusesAnUnknownName(t *testing.T) {
	t.Parallel()
	_, err := scoreJevBattery(context.Background(), "no-such-battery", t.TempDir(), time.Time{})
	if err == nil || !strings.Contains(err.Error(), "no-such-battery") {
		t.Fatalf("error = %v, want one naming no-such-battery", err)
	}
}

// Without --refresh the single-battery path still requires --battery.
func TestJevScorecardRefreshLeavesTheSingleBatteryPathAlone(t *testing.T) {
	t.Parallel()
	scorer := &jevRefreshScorer{}
	err := jevScorecardRun(context.Background(), io.Discard, t.TempDir(),
		jevScorecardOptions{}, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), scorer.score, scorecard.Cache{})
	if err == nil || !strings.Contains(err.Error(), "--battery") {
		t.Fatalf("error = %v, want one asking for --battery", err)
	}
}
