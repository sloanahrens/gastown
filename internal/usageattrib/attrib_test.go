package usageattrib

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/events"
)

var (
	t0  = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
)

func at(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

func ev(ts, typ, actor string, payload map[string]interface{}) events.Event {
	return events.Event{Timestamp: ts, Type: typ, Actor: actor, Payload: payload}
}

func sling(ts time.Duration, bead, target string) events.Event {
	return ev(at(ts), events.TypeSling, "mayor/", map[string]interface{}{"bead": bead, "target": target})
}

func done(ts time.Duration, bead, actor string) events.Event {
	return ev(at(ts), events.TypeDone, actor, map[string]interface{}{"bead": bead})
}

// runKey is a run as one line, for comparing whole lists of runs in a table.
func runKey(r Run) string {
	f := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Sub(t0).String()
	}
	return fmt.Sprintf("%s %s/%s %s %s ended=%t", r.Bead, r.Rig, r.Seat, f(r.Start), f(r.End), r.Ended)
}

func TestUsageAttribRunsFromEventsAndLandings(t *testing.T) {
	t.Parallel()
	const nux = "gastown/polecats/nux"
	tests := []struct {
		name     string
		evs      []events.Event
		landings []Landing
		want     []string
	}{
		{
			name: "sling then done takes the seat of the done actor",
			evs:  []events.Event{sling(0, "b-1", "gastown/polecats/nux"), done(time.Hour, "b-1", "gastown/polecats/furiosa")},
			want: []string{"b-1 gastown/furiosa 0s 1h0m0s ended=true"},
		},
		{
			name: "a re-dispatch ends the run it replaces without an end event",
			evs: []events.Event{
				sling(0, "b-1", nux),
				sling(time.Hour, "b-1", nux),
				done(2*time.Hour, "b-1", nux),
			},
			want: []string{
				"b-1 gastown/nux 0s 1h0m0s ended=false",
				"b-1 gastown/nux 1h0m0s 2h0m0s ended=true",
			},
		},
		{
			name: "a done with no open run names no run",
			evs:  []events.Event{done(0, "b-2", nux)},
		},
		{
			name:     "a landing closes an open run when no done came",
			evs:      []events.Event{sling(0, "b-1", nux)},
			landings: []Landing{{Bead: "b-1", Rig: "gastown", LandedAt: t0.Add(time.Hour)}},
			want:     []string{"b-1 gastown/nux 0s 1h0m0s ended=true"},
		},
		{
			name:     "a landing after the done changes nothing",
			evs:      []events.Event{sling(0, "b-1", nux), done(time.Hour, "b-1", nux)},
			landings: []Landing{{Bead: "b-1", Rig: "gastown", LandedAt: t0.Add(2 * time.Hour)}},
			want:     []string{"b-1 gastown/nux 0s 1h0m0s ended=true"},
		},
		{
			name: "a target that names no polecat leaves the seat empty",
			evs:  []events.Event{sling(0, "b-1", "mayor"), done(time.Hour, "b-1", "mayor")},
			want: []string{"b-1 / 0s 1h0m0s ended=true"},
		},
		{
			name: "a bead id that is not a plain id is skipped",
			evs:  []events.Event{sling(0, "../b", nux), done(time.Hour, "../b", nux)},
		},
		{
			name: "events out of file order are read in time order",
			evs:  []events.Event{done(time.Hour, "b-1", nux), sling(0, "b-1", nux)},
			want: []string{"b-1 gastown/nux 0s 1h0m0s ended=true"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs, err := Runs(tt.evs, tt.landings)
			if err != nil {
				t.Fatalf("Runs: %v", err)
			}
			var got []string
			for _, r := range runs {
				got = append(got, runKey(r))
			}
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("runs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

func TestUsageAttribRunsRefuseUnreadableTimestamp(t *testing.T) {
	t.Parallel()
	_, err := Runs([]events.Event{ev("yesterday", events.TypeSling, "", map[string]interface{}{"bead": "b-1", "target": "gastown/polecats/nux"})}, nil)
	if err == nil {
		t.Fatal("Runs accepted an event timestamp that does not parse")
	}
}

func TestUsageAttribMatchRun(t *testing.T) {
	t.Parallel()
	runs := []Run{
		{Bead: "b-1", Rig: "gastown", Seat: "nux", Start: t0, End: t0.Add(time.Hour), Ended: true},
		{Bead: "b-2", Rig: "gastown", Seat: "nux", Start: t0.Add(2 * time.Hour)},
		{Bead: "b-3", Rig: "gastown", Seat: "nux", Start: t0.Add(30 * time.Minute), End: t0.Add(3 * time.Hour), Ended: true},
		{Bead: "b-4", Rig: "gastown", Seat: "furiosa", Start: t0},
	}
	tests := []struct {
		name  string
		seat  string
		mtime time.Duration
		want  int
		ok    bool
	}{
		{"before the first start", "nux", -time.Minute, 0, false},
		{"inside a closed window", "nux", 20 * time.Minute, 0, true},
		{"after a closed window, inside a longer run that still holds it", "nux", 90 * time.Minute, 2, true},
		{"after the next start, while it is open", "nux", 5 * time.Hour, 1, true},
		{"overlapping windows take the later start", "nux", 40 * time.Minute, 2, true},
		{"a later start wins over an overlap", "nux", 2*time.Hour + 30*time.Minute, 1, true},
		{"another seat's open run holds its own mtime", "furiosa", 4 * time.Hour, 3, true},
		{"a seat with runs but none in window", "rictus", time.Hour, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchRun(runs, "gastown", tt.seat, t0.Add(tt.mtime))
			if ok != tt.ok || (ok && got != tt.want) {
				t.Errorf("matchRun = (%d, %t), want (%d, %t)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestUsageAttribIsFinal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		allEnded, anyEnded bool
		latestEnd, newest  time.Duration
		want               bool
	}{
		{"ended more than ten minutes ago", true, true, -11 * time.Minute, 0, true},
		{"ended under ten minutes ago", true, true, -9 * time.Minute, 0, false},
		{"no end event and the newest transcript over a day old", false, false, 0, -25 * time.Hour, true},
		{"no end event and the newest transcript under a day old", false, false, 0, -23 * time.Hour, false},
		{"no end event and no transcript", false, false, 0, 0, false},
		{"one run open and one ended, however old", false, true, -48 * time.Hour, -48 * time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var end, newest time.Time
			if tt.latestEnd != 0 {
				end = now.Add(tt.latestEnd)
			}
			if tt.newest != 0 {
				newest = now.Add(tt.newest)
			}
			if got := isFinal(tt.allEnded, tt.anyEnded, end, newest, now); got != tt.want {
				t.Errorf("isFinal = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestUsageAttribDedupeAcrossSessions(t *testing.T) {
	t.Parallel()
	id := func(s string) *string { return &s }
	a := SessionUsage{ID: "a", Main: []Message{
		{ID: id("m1"), Input: 10, Output: 5},
		{ID: id("m2"), Output: 7},
	}}
	b := SessionUsage{ID: "b",
		Main:     []Message{{ID: id("m1"), Input: 4, Output: 9}, {Output: 3}},
		Subagent: []Message{{ID: id("m2"), Output: 12}},
	}
	sessions := []SessionUsage{a, b}
	dedupeAcross(sessions)
	if len(sessions[0].Main) != 2 || sessions[0].Main[0].Input != 10 || sessions[0].Main[0].Output != 9 {
		t.Errorf("first session's m1 = %+v, want the maximum of both sightings", sessions[0].Main)
	}
	if len(sessions[0].Main) == 2 && sessions[0].Main[1].Output != 12 {
		t.Errorf("first session's m2 output = %d, want 12 from the subagent sighting", sessions[0].Main[1].Output)
	}
	if len(sessions[1].Main) != 1 || sessions[1].Main[0].ID != nil {
		t.Errorf("second session's main = %+v, want only the id-less message", sessions[1].Main)
	}
	if len(sessions[1].Subagent) != 0 {
		t.Errorf("second session's subagent = %+v, want the repeated message merged away", sessions[1].Subagent)
	}
}

func TestUsageAttribReadLandingsRefusesMalformedLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gastown.jsonl"), []byte(`{"bead":"b-1","rig":"gastown","landed_at":"2026-11-13T11:00:00Z"}`+"\n"+`{"bead":`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLandings(town); err == nil {
		t.Fatal("ReadLandings accepted a line that does not parse")
	}
}

func TestUsageAttribReadLandingsMissingDirectoryHoldsNone(t *testing.T) {
	t.Parallel()
	got, err := ReadLandings(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("ReadLandings = (%v, %v), want no landings and no error", got, err)
	}
}

// fixture is a town's events and seat copies for a scan, with each transcript's read.
type fixture struct {
	evs      []events.Event
	landings []Landing
	seats    map[string][]string
	reads    map[string]fakeRead
	listErr  error
	evErr    error
	landErr  error
	// clock is the scan's time; zero is now.
	clock   time.Time
	replace func(usageDir string, rec dashboard.UsageRecord, sessionIDs []string, reason string) error
}

type fakeRead struct {
	s     SessionUsage
	mtime time.Time
	err   error
}

func (f fixture) deps() ScanDeps {
	clock := f.clock
	if clock.IsZero() {
		clock = now
	}
	return ScanDeps{
		Now: func() time.Time { return clock },
		Events: func(string) ([]events.Event, error) {
			return f.evs, f.evErr
		},
		Landings: func(string) ([]Landing, error) {
			return f.landings, f.landErr
		},
		ListSeat: func(_, rig, seat string) ([]string, int, error) {
			return f.seats[rig+"/"+seat], 0, f.listErr
		},
		Read: func(path string) (SessionUsage, time.Time, error) {
			r := f.reads[path]
			return r.s, r.mtime, r.err
		},
		Replace: f.replace,
	}
}

// session is a transcript path's read: one message with the given tokens, last written at mtime.
func session(id string, input, output int64, mtime time.Duration) fakeRead {
	m := id + "-msg"
	return fakeRead{
		s:     SessionUsage{ID: id, Main: []Message{{ID: &m, Model: "claude-sonnet-5-5", TS: at(mtime), Input: input, Output: output}}, Complete: true},
		mtime: t0.Add(mtime),
	}
}

// twoBeads is a town where b-1 ran on nux and b-2 on furiosa, each with one session.
func twoBeads() fixture {
	return fixture{
		evs: []events.Event{
			sling(0, "b-1", "gastown/polecats/nux"),
			done(time.Hour, "b-1", "gastown/polecats/nux"),
			sling(2*time.Hour, "b-2", "gastown/polecats/furiosa"),
			done(3*time.Hour, "b-2", "gastown/polecats/furiosa"),
		},
		seats: map[string][]string{
			"gastown/nux":     {"/copy/nux/s1.jsonl"},
			"gastown/furiosa": {"/copy/furiosa/s2.jsonl"},
		},
		reads: map[string]fakeRead{
			"/copy/nux/s1.jsonl":     session("s1", 10, 20, 30*time.Minute),
			"/copy/furiosa/s2.jsonl": session("s2", 3, 4, 150*time.Minute),
		},
	}
}

func readRecord(t *testing.T, town, bead string) Stored {
	t.Helper()
	st, err := ReadWork(filepath.Join(town, ".runtime", "usage"), bead)
	if err != nil {
		t.Fatalf("ReadWork(%s): %v", bead, err)
	}
	return st
}

func TestUsageAttribScanWritesOneRecordPerBead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	res, err := Scan(ScanOptions{Town: town}, twoBeads().deps())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if strings.Join(res.Written, ",") != "b-1,b-2" || len(res.Kept) != 0 || res.Unmatched != 0 {
		t.Fatalf("result = %+v, want b-1 and b-2 written, none kept or unmatched", res)
	}
	st := readRecord(t, town, "b-1")
	if st.Record.Totals.Input != 10 || st.Record.Totals.Output != 20 || !st.Record.Complete {
		t.Errorf("b-1 totals = %+v complete=%t, want input 10 output 20 complete", st.Record.Totals, st.Record.Complete)
	}
	if strings.Join(st.Sessions, ",") != "s1" || st.Record.Workspace != "customer-zero" {
		t.Errorf("b-1 sessions=%v workspace=%q, want [s1] customer-zero", st.Sessions, st.Record.Workspace)
	}
	if st.Record.Seat == nil || *st.Record.Seat != "nux" {
		t.Errorf("b-1 seat = %v, want nux", st.Record.Seat)
	}
	if _, err := os.Stat(filepath.Join(town, ".runtime", "usage", StateFile)); err != nil {
		t.Errorf("state file not written: %v", err)
	}
}

func TestUsageAttribScanMissingTranscriptIsIncompleteNotZero(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f := fixture{
		evs: []events.Event{
			sling(0, "b-3", "gastown/polecats/ghost"),
			done(time.Hour, "b-3", "gastown/polecats/ghost"),
		},
	}
	res, err := Scan(ScanOptions{Town: town}, f.deps())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if strings.Join(res.Incomplete, ",") != "b-3" {
		t.Fatalf("incomplete = %v, want b-3", res.Incomplete)
	}
	st := readRecord(t, town, "b-3")
	if st.Record.Complete || st.Record.Totals.Calls != 0 {
		t.Errorf("b-3 complete=%t calls=%d, want incomplete with no calls", st.Record.Complete, st.Record.Totals.Calls)
	}
	if !strings.Contains(st.Error, "no transcript") {
		t.Errorf("b-3 error = %q, want it to name the missing transcript", st.Error)
	}
}

func TestUsageAttribScanIncompleteSessionMarksRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f := twoBeads()
	r := f.reads["/copy/nux/s1.jsonl"]
	r.s.Complete = false
	r.err = errors.New("s1.jsonl has 2 lines that do not parse")
	f.reads["/copy/nux/s1.jsonl"] = r

	if _, err := Scan(ScanOptions{Town: town}, f.deps()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	st := readRecord(t, town, "b-1")
	if st.Record.Complete || !strings.Contains(st.Error, "do not parse") {
		t.Errorf("b-1 complete=%t error=%q, want incomplete naming the unreadable transcript", st.Record.Complete, st.Error)
	}
}

// writeFinal writes a bead's record as a scan left it: session s1 counted, with the record's run ended at end.
func writeFinal(t *testing.T, town, bead string, sessions []string) {
	t.Helper()
	seat := "nux"
	rec := dashboard.UsageRecord{
		Bead: bead, Kind: dashboard.UsageKindWork, Class: dashboard.UsageClassPolecat,
		Rig: "gastown", Workspace: "customer-zero", Seat: &seat, Complete: true,
		Start: t0, End: t0.Add(time.Hour), Models: map[string]dashboard.UsageModel{},
	}
	if err := Replace(filepath.Join(town, ".runtime", "usage"), rec, sessions, ""); err != nil {
		t.Fatalf("Replace: %v", err)
	}
}

func recordBytes(t *testing.T, town, bead string) []byte {
	t.Helper()
	b, err := os.ReadFile(WorkRecordPath(filepath.Join(town, ".runtime", "usage"), bead))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUsageAttribScanFinalRecordLeftAloneUnlessSessionsChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		stored   []string
		rebuild  bool
		wantKept bool
	}{
		{"final and same sessions is left alone", []string{"s1"}, false, true},
		{"final and a session changed is rewritten", []string{"s0"}, false, false},
		{"rebuild rewrites a final record with the same sessions", []string{"s1"}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			writeFinal(t, town, "b-1", tt.stored)
			before := recordBytes(t, town, "b-1")

			res, err := Scan(ScanOptions{Town: town, Rebuild: tt.rebuild}, twoBeads().deps())
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			kept := strings.Join(res.Kept, ",") == "b-1"
			if kept != tt.wantKept {
				t.Fatalf("kept = %t (result %+v), want %t", kept, res, tt.wantKept)
			}
			after := recordBytes(t, town, "b-1")
			if tt.wantKept && string(before) != string(after) {
				t.Error("a final record that was kept changed on disk")
			}
			if !tt.wantKept && string(before) == string(after) {
				t.Error("the record was not rewritten")
			}
		})
	}
}

func TestUsageAttribScanRewritesRecordNotYetFinal(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFinal(t, town, "b-1", []string{"s1"})
	f := twoBeads()
	f.clock = t0.Add(time.Hour + 5*time.Minute)

	res, err := Scan(ScanOptions{Town: town}, f.deps())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !strings.Contains(strings.Join(res.Written, ","), "b-1") {
		t.Errorf("result = %+v, want b-1 rewritten: its done is under ten minutes old", res)
	}
}

func TestUsageAttribScanRefusesWithoutWriting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(f *fixture, town string)
	}{
		{"a malformed workspaces file", func(_ *fixture, town string) {
			dir := filepath.Join(town, "settings")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "workspaces.json"), []byte(`{"default":`), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"an events read that fails", func(f *fixture, _ string) { f.evErr = errors.New("events unreadable") }},
		{"a landings read that fails", func(f *fixture, _ string) { f.landErr = errors.New("landings unreadable") }},
		{"a seat copy that cannot be listed", func(f *fixture, _ string) { f.listErr = errors.New("copy unreadable") }},
		{"a transcript whose last write cannot be read", func(f *fixture, _ string) {
			f.reads["/copy/nux/s1.jsonl"] = fakeRead{err: errors.New("stat failed")}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			f := twoBeads()
			tt.setup(&f, town)
			if _, err := Scan(ScanOptions{Town: town}, f.deps()); err == nil {
				t.Fatal("Scan succeeded, want a hard error")
			}
			if _, err := os.Stat(filepath.Join(town, ".runtime", "usage")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the usage directory was written: stat err = %v", err)
			}
		})
	}
}

func TestUsageAttribScanUnmatchedSessionIsListed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f := twoBeads()
	f.seats["gastown/nux"] = append(f.seats["gastown/nux"], "/copy/nux/s3.jsonl")
	f.reads["/copy/nux/s3.jsonl"] = session("s3", 1, 1, 5*time.Hour)

	res, err := Scan(ScanOptions{Town: town}, f.deps())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Unmatched != 1 {
		t.Fatalf("unmatched = %d, want 1", res.Unmatched)
	}
	b, err := os.ReadFile(filepath.Join(town, ".runtime", "usage", UnmatchedFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"session":"s3"`) {
		t.Errorf("unmatched.jsonl = %s, want the s3 line", b)
	}
}

func TestUsageAttribScanSinceDropsEarlierRuns(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	res, err := Scan(ScanOptions{Town: town, Since: t0.Add(2 * time.Hour)}, twoBeads().deps())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if strings.Join(res.Written, ",") != "b-2" {
		t.Errorf("written = %v, want only b-2: b-1 ended before since", res.Written)
	}
}
