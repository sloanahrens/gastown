package cmd

import (
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/landings"
)

// fakeTailJournal serves a journal the way bd events tail does: records with
// seq above since, at most limit, More when the limit cut the page.
type fakeTailJournal struct {
	records   []beads.EventRecord
	config    string
	configErr error
	errs      []error // returned, in order, by the next EventsTail calls
	trunc     *beads.EventsTruncatedError
	calls     []int64
}

func (f *fakeTailJournal) ConfigGet(key string) (string, error) {
	if key != "events-journal" {
		return "", errors.New("unexpected key " + key)
	}
	return f.config, f.configErr
}

func (f *fakeTailJournal) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	f.calls = append(f.calls, since)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	if f.trunc != nil && since < f.trunc.Floor-1 {
		t := *f.trunc
		t.Since = since
		f.trunc = nil
		return nil, &t
	}
	page := &beads.EventsPage{NextSince: since}
	for _, r := range f.records {
		if r.Seq <= since {
			continue
		}
		if limit > 0 && len(page.Records) == limit {
			page.More = true
			break
		}
		page.Records = append(page.Records, r)
		page.NextSince = r.Seq
	}
	return page, nil
}

func texts(lines []tailLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.Rig+" "+l.Kind+" "+l.Text)
	}
	return out
}

var tailNow = at("2026-09-30T14:00:00Z")

func fixedNow() time.Time { return tailNow }

func TestEventsSource_JournalOffBacklogPagingAndFollow(t *testing.T) {
	j := &fakeTailJournal{config: "false", records: []beads.EventRecord{
		{Seq: 1, TS: "2026-09-30T12:00:00Z", Op: "create", IssueID: "gt-old", Actor: "a", Status: "open"},
		{Seq: 2, TS: "2026-09-30T13:50:00Z", Op: "update", IssueID: "gt-1", Actor: "gastown/polecats/opal", Status: "in_progress"},
		{Seq: 3, TS: "2026-09-30T13:51:00.5Z", Op: "close", IssueID: "gt-1", Actor: "gastown/polecats/opal", Status: "closed"},
		{Seq: 4, TS: "not-a-time", Op: "delete", IssueID: "gt-2"},
	}}
	s := &eventsSource{rig: "gastown", journal: j, cutoff: at("2026-09-30T13:45:00Z"), now: fixedNow, pageSize: 2}
	got := s.Poll()
	want := []string{
		"gastown events journal off in config (events-journal=false): only mutations made through gt are journaled",
		"gastown events update gt-1 status=in_progress actor=gastown/polecats/opal seq=2",
		"gastown events close gt-1 status=closed actor=gastown/polecats/opal seq=3",
		"gastown events delete gt-2 seq=4 ts=not-a-time",
	}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("first poll:\n%q\nwant\n%q", texts(got), want)
	}
	if !got[2].At.Equal(at("2026-09-30T13:51:00.5Z")) || !got[3].At.Equal(tailNow) || !got[0].At.Equal(tailNow) {
		t.Fatalf("times: %v %v %v", got[0].At, got[2].At, got[3].At)
	}
	if !reflect.DeepEqual(j.calls, []int64{0, 2}) {
		t.Fatalf("paging cursors = %v", j.calls)
	}

	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("idle poll = %q", texts(got))
	}
	// Follow: a record after the cursor prints; the notice does not repeat.
	j.records = append(j.records, beads.EventRecord{Seq: 5, TS: "2026-09-30T14:00:01Z", Op: "comment", IssueID: "gt-3", Actor: "mayor"})
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"gastown events comment gt-3 actor=mayor seq=5"}) {
		t.Fatalf("follow poll = %q", got)
	}
}

func TestEventsSource_JournalOnSaysNothing(t *testing.T) {
	j := &fakeTailJournal{config: "true"}
	if got := (&eventsSource{rig: "hq", journal: j, cutoff: tailNow, now: fixedNow}).Poll(); len(got) != 0 {
		t.Fatalf("journal on, empty: %q", texts(got))
	}
}

func TestEventsSource_UnreadableConfigSaysSoOnce(t *testing.T) {
	j := &fakeTailJournal{configErr: errors.New("boom")}
	s := &eventsSource{rig: "hq", journal: j, cutoff: tailNow, now: fixedNow}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"hq events cannot read events-journal config (boom): the journal may hold only mutations made through gt"}) {
		t.Fatalf("got %q", got)
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("second poll repeated: %q", texts(got))
	}
}

func TestEventsSource_PrunedJournalResumesWithOneLine(t *testing.T) {
	j := &fakeTailJournal{config: "true",
		trunc: &beads.EventsTruncatedError{Floor: 7, Head: 8},
		records: []beads.EventRecord{
			{Seq: 7, TS: "2026-09-30T13:59:00Z", Op: "create", IssueID: "gt-7"},
			{Seq: 8, TS: "2026-09-30T13:59:30Z", Op: "create", IssueID: "gt-8"},
		}}
	s := &eventsSource{rig: "gastown", journal: j, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}
	want := []string{
		"gastown events journal pruned past seq 0 (oldest retained 7, head 8): records before 7 are gone",
		"gastown events create gt-7 seq=7",
		"gastown events create gt-8 seq=8",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestEventsSource_ReadErrorsPrintOncePerDistinctFailure(t *testing.T) {
	down := errors.New("bd events tail: exit status 25")
	j := &fakeTailJournal{config: "true", errs: []error{down, down}}
	s := &eventsSource{rig: "gastown", journal: j, cutoff: tailNow, now: fixedNow}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"gastown events read failed: bd events tail: exit status 25"}) {
		t.Fatalf("first failure: %q", got)
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("repeated failure printed again: %q", texts(got))
	}
	s.Poll() // recovers
	j.errs = []error{down}
	if got := s.Poll(); len(got) != 1 {
		t.Fatalf("failure after recovery not printed: %q", texts(got))
	}
}

func TestEventsSource_OpenErrorIsOneLine(t *testing.T) {
	s := &eventsSource{rig: "mango", openErr: errors.New("no beads directory"), now: fixedNow}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"mango events cannot read the journal: no beads directory"}) {
		t.Fatalf("got %q", got)
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("repeated: %q", texts(got))
	}
}

func TestLandingsSource_BacklogFollowAndBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gastown.jsonl")
	appendFile(t, path,
		`{"bead":"gt-old","rig":"gastown","landed_at":"2026-09-30T12:00:00Z"}`+"\n"+
			`{"bead":"gt-abc","rig":"gastown","branch":"polecat/opal/gt-abc","target":"main","landed_commit":"3333333333333333333333333333333333333333","patch_id":"4444444444444444444444444444444444444444","gate_result":"pass","om_verdict":"approve","om_score":0.92,"route":"worker","landed_at":"2026-09-30T13:55:00Z"}`+"\n")
	s := &landingsSource{rig: "gastown", reader: &landings.Reader{Path: path}, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}
	got := s.Poll()
	want := []string{"gastown landings landed gt-abc polecat/opal/gt-abc -> main commit=33333333 patch=44444444 gate=pass om=approve/0.92 route=worker"}
	if !reflect.DeepEqual(texts(got), want) || !got[0].At.Equal(at("2026-09-30T13:55:00Z")) {
		t.Fatalf("backlog = %q", texts(got))
	}
	appendFile(t, path, "garbage\n"+`{"bead":"gt-new","rig":"gastown","landed_at":"2026-09-30T14:00:05Z"}`+"\n")
	want = []string{
		"gastown landings unreadable landings line (7 bytes)",
		"gastown landings landed gt-new - -> - commit=- patch=- gate=- om=-/0.00 route=-",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("follow = %q\nwant %q", got, want)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeGz(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonSource_BackupsContinuationFollowAndRotation(t *testing.T) {
	dir := t.TempDir()
	// Rotated before the cutoff: none of its lines can qualify, never read.
	if err := os.WriteFile(filepath.Join(dir, "daemon-2026-09-29T00-00-00.000.log"), []byte("2026/09/30 07:59:59 must-not-appear\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Rotated at 13:00 UTC (08:00 CDT): holds lines on both sides of the cutoff.
	writeGz(t, filepath.Join(dir, "daemon-2026-09-30T13-00-00.000.log.gz"),
		"2026/09/30 07:30:00 too-old\n2026/09/30 07:59:00 in-backup\n")
	logPath := filepath.Join(dir, "daemon.log")
	appendFile(t, logPath, "2026/09/30 08:01:00 Convoy: close detected: gt-1 (from gastown)\n  continuation\n2026/09/30 08:02:00 hm witness started\n")

	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T12:45:00Z"), loc: tailTestLoc, now: fixedNow}
	got := s.Poll()
	want := []string{
		"town daemon in-backup",
		"town daemon Convoy: close detected: gt-1 (from gastown)",
		"town daemon   continuation",
		"town daemon hm witness started",
	}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("backlog:\n%q\nwant\n%q", texts(got), want)
	}
	if !got[2].At.Equal(at("2026-09-30T13:01:00Z")) {
		t.Fatalf("continuation line time = %v", got[2].At)
	}

	appendFile(t, logPath, "2026/09/30 09:00:00 partial")
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("partial line read early: %q", texts(got))
	}
	appendFile(t, logPath, " now whole\n")
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"town daemon partial now whole"}) {
		t.Fatalf("follow = %q", got)
	}

	// lumberjack renames daemon.log away and starts a new one.
	if err := os.Rename(logPath, filepath.Join(dir, "daemon-2026-09-30T14-00-00.000.log")); err != nil {
		t.Fatal(err)
	}
	appendFile(t, logPath, "2026/09/30 09:01:00 fresh\n")
	want = []string{
		"town daemon daemon.log was rotated; reading the new file from its start",
		"town daemon fresh",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("after rotation = %q\nwant %q", got, want)
	}
}

func TestDaemonSource_RigFilterKeepsLinesNamingTheRig(t *testing.T) {
	dir := t.TempDir()
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 08:01:00 Convoy: close detected: gt-1 (from gastown)\n"+
			"2026/09/30 08:01:01 Skipping crash detection for gastown/jade\n"+
			"2026/09/30 08:01:02 hm: gastownish is not gastown-x\n"+
			"2026/09/30 08:01:03 mango only\n")
	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T12:00:00Z"), loc: tailTestLoc, now: fixedNow, rigFilter: tailRigFilter("gastown")}
	want := []string{
		"town daemon Convoy: close detected: gt-1 (from gastown)",
		"town daemon Skipping crash detection for gastown/jade",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered = %q", got)
	}
}

func TestDaemonSource_MissingLogSaysSoOnce(t *testing.T) {
	s := &daemonSource{dir: t.TempDir(), cutoff: tailNow, loc: tailTestLoc, now: fixedNow}
	got := texts(s.Poll())
	if len(got) != 1 || !regexp.MustCompile(`^town daemon cannot read daemon.log: `).MatchString(got[0]) {
		t.Fatalf("missing log = %q", got)
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("repeated: %q", texts(got))
	}
}

func TestEventsSource_PruneThatCannotAdvanceIsAReadFailure(t *testing.T) {
	// bd reports a floor and head at or below the cursor: resuming would not
	// move forward, so the read fails once instead of looping.
	j := &fakeTailJournal{config: "true", errs: []error{&beads.EventsTruncatedError{Since: 0, Floor: 0, Head: 0}}}
	s := &eventsSource{rig: "gastown", journal: j, cutoff: tailNow, now: fixedNow}
	got := texts(s.Poll())
	want := []string{"gastown events read failed: events journal pruned past --since 0 (oldest retained 0, head 0)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if len(j.calls) != 1 {
		t.Fatalf("calls = %v", j.calls)
	}
}

func TestDaemonSource_CorruptBackupIsOneLineAndTheLogStillReads(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "daemon-2026-09-30T13-30-00.000.log.gz"), []byte("this is plainly not a gzip stream"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(dir, "daemon.log"), "2026/09/30 08:01:00 alive\n")
	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T12:45:00Z"), loc: tailTestLoc, now: fixedNow}
	want := []string{
		"town daemon cannot read daemon-2026-09-30T13-30-00.000.log.gz: gzip: invalid header",
		"town daemon alive",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestTailBDJournal_ConfigGetReadsTheStoreConfigNotGtsOverride(t *testing.T) {
	var gotEnv, gotArgs []string
	j := &tailBDJournal{dir: "/town/gastown/mayor/rig/.beads", run: func(_ context.Context, env []string, args ...string) ([]byte, []byte, error) {
		gotEnv, gotArgs = env, args
		return []byte(`{"key":"events-journal","location":"config.yaml","value":"false"}`), nil, nil
	}}
	v, err := j.ConfigGet("events-journal")
	if err != nil || v != "false" {
		t.Fatalf("ConfigGet = %q, %v", v, err)
	}
	if want := []string{"config", "get", "events-journal", "--json"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("argv = %v", gotArgs)
	}
	if want := []string{"BEADS_DIR=/town/gastown/mayor/rig/.beads", "BD_EVENTS_JOURNAL=", "BD_MACHINE=1"}; !reflect.DeepEqual(gotEnv, want) {
		t.Fatalf("env = %v", gotEnv)
	}

	j.run = func(context.Context, []string, ...string) ([]byte, []byte, error) {
		return nil, []byte("database not found\n"), errors.New("exit status 1")
	}
	if _, err := j.ConfigGet("events-journal"); err == nil || !strings.Contains(err.Error(), "database not found") {
		t.Fatalf("failure = %v", err)
	}
}

func TestEventsSource_TruthyConfigIsOn(t *testing.T) {
	for _, v := range []string{"true", "1", "TRUE"} {
		j := &fakeTailJournal{config: v}
		if got := (&eventsSource{rig: "hq", journal: j, cutoff: tailNow, now: fixedNow}).Poll(); len(got) != 0 {
			t.Errorf("events-journal=%s printed %q", v, texts(got))
		}
	}
}
