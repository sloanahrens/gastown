package cmd

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
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
	t.Parallel()
	j := &fakeTailJournal{config: "false", records: []beads.EventRecord{
		{Seq: 1, TS: "2026-09-30T12:00:00Z", Op: "create", IssueID: "gt-old", Actor: "a", Status: "open"},
		{Seq: 2, TS: "2026-09-30T13:50:00Z", Op: "update", IssueID: "gt-1", Actor: "gastown/polecats/opal", Status: "in_progress"},
		{Seq: 3, TS: "2026-09-30T13:51:00.5Z", Op: "close", IssueID: "gt-1", Actor: "gastown/polecats/opal", Status: "closed"},
		{Seq: 4, TS: "not-a-time", Op: "delete", IssueID: "gt-2"},
	}}
	s := &eventsSource{rig: "gastown", journal: j, cutoff: at("2026-09-30T13:45:00Z"), now: fixedNow, pageSize: 2}
	got := s.Poll()
	want := []string{
		"gastown events journal off in config (events-journal=false): nothing is journaled",
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
	t.Parallel()
	j := &fakeTailJournal{config: "true"}
	if got := (&eventsSource{rig: "hq", journal: j, cutoff: tailNow, now: fixedNow}).Poll(); len(got) != 0 {
		t.Fatalf("journal on, empty: %q", texts(got))
	}
}

func TestEventsSource_UnreadableConfigSaysSoOnce(t *testing.T) {
	t.Parallel()
	j := &fakeTailJournal{configErr: errors.New("boom")}
	s := &eventsSource{rig: "hq", journal: j, cutoff: tailNow, now: fixedNow}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"hq events cannot read events-journal config (boom): the journal may be off"}) {
		t.Fatalf("got %q", got)
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("second poll repeated: %q", texts(got))
	}
}

func TestEventsSource_PrunedJournalResumesWithOneLine(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	s := &eventsSource{rig: "mango", openErr: errors.New("no beads directory"), now: fixedNow}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"mango events cannot read the journal: no beads directory"}) {
		t.Fatalf("got %q", got)
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("repeated: %q", texts(got))
	}
}

func TestLandingsSource_BacklogFollowAndBadLines(t *testing.T) {
	t.Parallel()
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

// TestWatchSource_ReadsBothFilesColorsBySeverityAndNotesBadLines: the feed's
// two files are one source, each line draws by its severity rather than its
// words, the backlog is cut at --since, and a line that is not an alert is
// skipped with one note naming the file.
func TestWatchSource_ReadsBothFilesColorsBySeverityAndNotesBadLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	alerts := filepath.Join(town, ".runtime", "watch", "alerts.jsonl")
	if err := os.MkdirAll(filepath.Dir(alerts), 0o700); err != nil {
		t.Fatal(err)
	}
	unknown := `{"ts":"2026-09-30T13:52:00Z","class":"x","severity":"loud","text":"unknown severity"}`
	appendFile(t, alerts,
		`{"ts":"2026-09-30T12:00:00Z","class":"stall","severity":"high","text":"before the cutoff"}`+"\n"+
			`{"ts":"2026-09-30T13:50:00Z","class":"bd-slow","severity":"low","text":"bd took 4s"}`+"\n"+
			"garbage\n"+
			`{"ts":"2026-09-30T13:51:00Z","class":"direct-push","severity":"high","text":"a commit landed outside the queue"}`+"\n"+
			unknown+"\n")
	// The attention log is the same schema with two extra keys, which the
	// source ignores.
	events := attention.EventsPath(town)
	if err := os.MkdirAll(filepath.Dir(events), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, events,
		`{"ts":"2026-09-30T13:53:00Z","class":"polecat-stall","severity":"low","text":"gastown/opal silent 40m","key":"stall:gastown/opal","state":"new"}`+"\n")

	s := newTailWatchSource(town, at("2026-09-30T13:00:00Z"), fixedNow)
	got := s.Poll()
	// The backlog is the file's own order, each skipped-line note beside the
	// line it replaced.
	want := []string{
		"watch watch bd-slow: bd took 4s",
		"watch watch skipped unreadable watch line in alerts.jsonl (7 bytes)",
		"watch watch direct-push: a commit landed outside the queue",
		fmt.Sprintf("watch watch skipped unreadable watch line in alerts.jsonl (%d bytes)", len(unknown)),
		"watch watch polecat-stall: gastown/opal silent 40m",
	}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("backlog:\n%q\nwant\n%q", texts(got), want)
	}
	wantClasses := []tailClass{tailClassWarning, tailClassPlain, tailClassFailure, tailClassPlain, tailClassWarning}
	for i, c := range wantClasses {
		if got[i].Class != c {
			t.Errorf("line %d (%q) class = %v, want %v", i, got[i].Text, got[i].Class, c)
		}
	}
	if !got[0].At.Equal(at("2026-09-30T13:50:00Z")) || !got[4].At.Equal(at("2026-09-30T13:53:00Z")) {
		t.Fatalf("times: %v %v", got[0].At, got[4].At)
	}

	// Follow: the appended line prints, the notes do not repeat, and an idle
	// poll says nothing.
	appendFile(t, alerts, `{"ts":"2026-09-30T14:00:01Z","class":"red-main","severity":"high","text":"main is red"}`+"\n")
	got = s.Poll()
	if len(got) != 1 || got[0].Text != "red-main: main is red" || got[0].Class != tailClassFailure {
		t.Fatalf("follow = %+v", got)
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("idle poll = %q", texts(got))
	}
}

// TestWatchSource_MissingFilesAreSilent: a town whose writers have not run yet
// has no feed, and the stream says nothing about it.
func TestWatchSource_MissingFilesAreSilent(t *testing.T) {
	t.Parallel()
	s := newTailWatchSource(t.TempDir(), tailNow, fixedNow)
	for i := 0; i < 2; i++ {
		if got := s.Poll(); len(got) != 0 {
			t.Fatalf("poll %d printed %q", i, texts(got))
		}
	}
}

// TestWatchSource_FailedFirstReadIsRetriedAsABacklogRead: a file that cannot
// be read says so once, and the next poll reads it as a backlog cut at the
// cutoff rather than dumping every old alert.
func TestWatchSource_FailedFirstReadIsRetriedAsABacklogRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	alerts := filepath.Join(town, ".runtime", "watch", "alerts.jsonl")
	if err := os.MkdirAll(alerts, 0o700); err != nil { // unreadable as a file
		t.Fatal(err)
	}
	s := newTailWatchSource(town, at("2026-09-30T13:00:00Z"), fixedNow)
	got := s.Poll()
	if len(got) != 1 || !strings.HasPrefix(got[0].Text, "cannot read alerts.jsonl: ") {
		t.Fatalf("failed first poll = %q", texts(got))
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("the same failure printed again: %q", texts(got))
	}
	if err := os.Remove(alerts); err != nil {
		t.Fatal(err)
	}
	appendFile(t, alerts, `{"ts":"2026-09-30T12:00:00Z","severity":"low","text":"too old"}`+"\n"+
		`{"ts":"2026-09-30T13:59:00Z","severity":"high","text":"in window"}`+"\n")
	if got := s.Poll(); len(got) != 1 || got[0].Text != "in window" {
		t.Fatalf("retry ignored the cutoff or repeated the note: %q", texts(got))
	}
}

// TestWatchSource_LeavesAPartialLineThenReadsARotatedFileFromItsStart: a
// writer killed mid-line leaves no complete line to report, and a writer that
// rotates at 1 MB replaces the file the next read starts over.
func TestWatchSource_LeavesAPartialLineThenReadsARotatedFileFromItsStart(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	alerts := filepath.Join(town, ".runtime", "watch", "alerts.jsonl")
	if err := os.MkdirAll(filepath.Dir(alerts), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, alerts, `{"ts":"2026-09-30T13:50:00Z","severity":"low","text":"whole"}`+"\n"+
		`{"ts":"2026-09-30T13:51:00Z","severity":"low","text":"half`)
	s := newTailWatchSource(town, at("2026-09-30T13:00:00Z"), fixedNow)
	if got := s.Poll(); len(got) != 1 || got[0].Text != "whole" {
		t.Fatalf("a half-written line was reported: %q", texts(got))
	}
	appendFile(t, alerts, ` written"}`+"\n")
	if got := s.Poll(); len(got) != 1 || got[0].Text != "half written" {
		t.Fatalf("the completed line = %q", texts(got))
	}

	if err := os.Rename(alerts, alerts+".1"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, alerts, `{"ts":"2026-09-30T14:00:00Z","severity":"high","text":"after rotation"}`+"\n")
	if got := s.Poll(); len(got) != 1 || got[0].Text != "after rotation" {
		t.Fatalf("after rotation = %q", texts(got))
	}
}

// TestWatchLine_DrawsBySeverityAndAlwaysShows: the default view keeps a watch
// line whatever it says, and its severity — not a word in its text — decides
// the color.
func TestWatchLine_DrawsBySeverityAndAlwaysShows(t *testing.T) {
	t.Parallel()
	filter := &tailDefaultFilter{}
	for _, tc := range []struct {
		severity string
		class    tailClass
	}{
		{"low", tailClassWarning},
		{"high", tailClassFailure},
	} {
		class, ok := tailWatchSeverityClass(tc.severity)
		if !ok || class != tc.class {
			t.Fatalf("severity %q = %v, %v", tc.severity, class, ok)
		}
		line := tailLine{At: tailNow, Rig: tailKindWatch, Kind: tailKindWatch, Class: class, Text: "queue-stuck: nothing has landed"}
		shown, ok := filter.visible(line)
		if !ok {
			t.Fatalf("a %s watch line was hidden", tc.severity)
		}
		if got := tailLineClass(shown); got != class {
			t.Errorf("a %s watch line drew as %v", tc.severity, got)
		}
	}
	if _, ok := tailWatchSeverityClass("loud"); ok {
		t.Error("a severity the schema does not name was accepted")
	}
	// Without a class the text decides, which is what makes the Class field
	// worth carrying.
	if got := tailLineClass(tailLine{Rig: tailKindWatch, Kind: tailKindWatch, Text: "x: failed"}); got != tailClassFailure {
		t.Errorf("text-classified watch line drew as %v", got)
	}
}

// TestRenderLine_WatchTagAndSeverityIcon: a feed line prints under the watch
// tag, one column like the daemon's, and its severity's icon.
func TestRenderLine_WatchTagAndSeverityIcon(t *testing.T) {
	t.Parallel()
	line := tailLine{At: at("2026-09-30T14:05:06Z"), Rig: tailKindWatch, Kind: tailKindWatch, Class: tailClassWarning, Text: "bd-slow: bd took 4s"}
	short := tailView{Loc: tailTestLoc, Layout: tailClockLayout, Decor: newTailDecor()}
	got := short.renderLine(line)
	if !strings.HasPrefix(got, "09:05:06 watch ") {
		t.Errorf("the tag moved: %q", got)
	}
	if strings.Contains(got, tailKindWatch+" "+tailKindWatch) {
		t.Errorf("the short form printed the kind as well: %q", got)
	}
	if !strings.Contains(got, tailIconWarning) || strings.Contains(got, tailIconFailure) {
		t.Errorf("a low alert drew with the wrong icon: %q", got)
	}
	verbose := tailView{Loc: tailTestLoc, Layout: tailClockLayout, FullSource: true, Decor: newTailDecor()}
	if got := verbose.renderLine(line); !strings.Contains(got, "watch watch ") {
		t.Errorf("--verbose dropped a column: %q", got)
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
	t.Parallel()
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
	t.Parallel()
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

// writeTailRoutes writes the town's routes.jsonl: one route per rig, the way
// bd routes a bead, plus the town store's "." entry.
func writeTailRoutes(t *testing.T, townRoot string) {
	t.Helper()
	dir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"hq-","path":"."}
{"prefix":"gt-","path":"gastown/mayor/rig"}
{"prefix":"om-","path":"om/mayor/rig"}
{"prefix":"hm-","path":"hm/mayor/rig"}
{"prefix":"be-","path":"beads/mayor/rig"}
`
	if err := os.WriteFile(filepath.Join(dir, "routes.jsonl"), []byte(routes), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonSource_LabelsLinesWithTheRigTheyBelongTo: the daemon's per-rig
// lines print under the rig they name, a "[land]" line under the rig its bead
// routes to, and the daemon's own town-wide lines stay "town".
func TestDaemonSource_LabelsLinesWithTheRigTheyBelongTo(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTailRoutes(t, townRoot)
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 08:00:00 landing_worker: gastown: worker started\n"+
			"2026/09/30 08:00:01 landing_worker: gastown: pass: 1 landed, 0 repaired, 0 rejected, 0 skipped, 0 failed\n"+
			"2026/09/30 08:00:02 landing_worker: om: post-land: checking origin/main\n"+
			"2026/09/30 08:00:03 landing_worker: hm: red-main: origin/main is red\n"+
			"2026/09/30 08:00:04 landing_worker: [land] gt-abc: merged 0f0f onto origin/main (aaaa) as 1111aaaa\n"+
			"2026/09/30 08:00:05 landing_worker: [land] be-1: landed 1111aaaa on origin/main\n"+
			"2026/09/30 08:00:06 landing_worker: [land] zz-1: landed 1111aaaa on origin/main\n"+
			"2026/09/30 08:00:07 tier_sweep: beads: sweep started a9be03e4 (shell, integration, race)\n"+
			"2026/09/30 08:00:08 tier_sweep: no rig configured to sweep; nothing to do\n"+
			"2026/09/30 08:00:09 tier_sweep: WARNING: no repository at /x; nothing to sweep\n"+
			"2026/09/30 08:00:10 landing_worker: enabled (pass interval 1m0s, land timeout 30m0s)\n"+
			"2026/09/30 08:00:11 upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared\n")
	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T12:00:00Z"), loc: tailTestLoc, now: fixedNow, rigs: loadTailRigs(townRoot)}
	want := []string{
		"gastown daemon landing_worker: gastown: worker started",
		"gastown daemon landing_worker: gastown: pass: 1 landed, 0 repaired, 0 rejected, 0 skipped, 0 failed",
		"om daemon landing_worker: om: post-land: checking origin/main",
		"hm daemon landing_worker: hm: red-main: origin/main is red",
		"gastown daemon landing_worker: [land] gt-abc: merged 0f0f onto origin/main (aaaa) as 1111aaaa",
		"beads daemon landing_worker: [land] be-1: landed 1111aaaa on origin/main",
		"town daemon landing_worker: [land] zz-1: landed 1111aaaa on origin/main",
		"beads daemon tier_sweep: beads: sweep started a9be03e4 (shell, integration, race)",
		"town daemon tier_sweep: no rig configured to sweep; nothing to do",
		"town daemon tier_sweep: WARNING: no repository at /x; nothing to sweep",
		"town daemon landing_worker: enabled (pass interval 1m0s, land timeout 30m0s)",
		"town daemon upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("labeled = %q\nwant %q", got, want)
	}
}

// TestDaemonSource_LabelsEveryPerRigComponent: the daemon's other per-rig
// components name their rig the way landing_worker and tier_sweep do, and one
// resolver places them all — the component is not a list it carries.
func TestDaemonSource_LabelsEveryPerRigComponent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTailRoutes(t, townRoot)
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 08:00:00 patrol_scan: gastown: checked 8 polecat(s)\n"+
			"2026/09/30 08:00:01 steward: om: mixing the work blend\n"+
			"2026/09/30 08:00:02 rig_worker: hm: starting on hm/quartz\n"+
			"2026/09/30 08:00:03 compactor_dog: beads: compacted 3 convoy(s)\n"+
			"2026/09/30 08:00:04 git_hygiene: gastown: pruned 2 worktree(s)\n"+
			"2026/09/30 08:00:05 jsonl_git_backup: om: pushed refs/dolt/data\n"+
			"2026/09/30 08:00:06 scheduled_maintenance: hm: running gc\n"+
			"2026/09/30 08:00:07 scheduled_slings: beads: slung 1 bead\n"+
			"2026/09/30 08:00:08 wisp_reaper: gastown: reaped 4 wisp(s)\n")
	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T12:00:00Z"), loc: tailTestLoc, now: fixedNow, rigs: loadTailRigs(townRoot)}
	want := []string{
		"gastown daemon patrol_scan: gastown: checked 8 polecat(s)",
		"om daemon steward: om: mixing the work blend",
		"hm daemon rig_worker: hm: starting on hm/quartz",
		"beads daemon compactor_dog: beads: compacted 3 convoy(s)",
		"gastown daemon git_hygiene: gastown: pruned 2 worktree(s)",
		"om daemon jsonl_git_backup: om: pushed refs/dolt/data",
		"hm daemon scheduled_maintenance: hm: running gc",
		"beads daemon scheduled_slings: beads: slung 1 bead",
		"gastown daemon wisp_reaper: gastown: reaped 4 wisp(s)",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("labeled = %q\nwant %q", got, want)
	}
}

// TestDaemonSource_TownWideLineOfAPerRigComponentStaysTown: the same
// components also log town-wide lines, whose second token is not a rig; they
// stay "town" rather than taking the nearest rig name they mention.
func TestDaemonSource_TownWideLineOfAPerRigComponentStaysTown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTailRoutes(t, townRoot)
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 08:00:00 jsonl_git_backup: not due - last run 5m0s ago\n"+
			"2026/09/30 08:00:01 patrol_scan: no rigs to scan\n"+
			"2026/09/30 08:00:02 steward: work blend is off\n"+
			"2026/09/30 08:00:03 rig_worker: nothing ready to work\n")
	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T12:00:00Z"), loc: tailTestLoc, now: fixedNow, rigs: loadTailRigs(townRoot)}
	want := []string{
		"town daemon jsonl_git_backup: not due - last run 5m0s ago",
		"town daemon patrol_scan: no rigs to scan",
		"town daemon steward: work blend is off",
		"town daemon rig_worker: nothing ready to work",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("town-wide = %q\nwant %q", got, want)
	}
}

// TestDaemonSource_NoRoutesLeavesEveryLineTown: a town with no routes file (or
// an unreadable one) cannot place a line, so nothing is labelled on a guess.
func TestDaemonSource_NoRoutesLeavesEveryLineTown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 08:00:00 landing_worker: gastown: worker started\n"+
			"2026/09/30 08:00:01 landing_worker: [land] gt-abc: landed 1111aaaa on origin/main\n")
	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T12:00:00Z"), loc: tailTestLoc, now: fixedNow, rigs: loadTailRigs(townRoot)}
	want := []string{
		"town daemon landing_worker: gastown: worker started",
		"town daemon landing_worker: [land] gt-abc: landed 1111aaaa on origin/main",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("unrouted = %q\nwant %q", got, want)
	}
}

// TestDaemonSource_RigFilterKeepsALineThatBelongsToTheRig: --rig gastown keeps
// a "[land] gt-x:" line that names no rig, because the line's own rig is
// gastown; another rig's landing line is still dropped.
func TestDaemonSource_RigFilterKeepsALineThatBelongsToTheRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTailRoutes(t, townRoot)
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 08:00:00 landing_worker: [land] gt-abc: landed 1111aaaa on origin/main\n"+
			"2026/09/30 08:00:01 landing_worker: [land] om-1: landed 2222bbbb on origin/main\n"+
			"2026/09/30 08:00:02 landing_worker: gastown: pass: 1 landed\n"+
			"2026/09/30 08:00:03 upgrade-restart: running 3333cccc covers marker 3333cccc; cleared\n")
	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T12:00:00Z"), loc: tailTestLoc, now: fixedNow,
		rigFilter: tailRigFilter("gastown"), rigName: "gastown", rigs: loadTailRigs(townRoot)}
	want := []string{
		"gastown daemon landing_worker: [land] gt-abc: landed 1111aaaa on origin/main",
		"gastown daemon landing_worker: gastown: pass: 1 landed",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered = %q\nwant %q", got, want)
	}
}

func TestDaemonSource_MissingLogSaysSoOnce(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	for _, v := range []string{"true", "1", "TRUE"} {
		j := &fakeTailJournal{config: v}
		if got := (&eventsSource{rig: "hq", journal: j, cutoff: tailNow, now: fixedNow}).Poll(); len(got) != 0 {
			t.Errorf("events-journal=%s printed %q", v, texts(got))
		}
	}
}

func TestEventsSource_FailedFirstReadIsRetriedAsABacklogRead(t *testing.T) {
	t.Parallel()
	down := errors.New("bd events tail: exit status 25")
	j := &fakeTailJournal{config: "false", errs: []error{down}, records: []beads.EventRecord{
		{Seq: 1, TS: "2026-09-30T12:00:00Z", Op: "create", IssueID: "gt-old"},
		{Seq: 2, TS: "2026-09-30T13:59:00Z", Op: "create", IssueID: "gt-new"},
	}}
	s := &eventsSource{rig: "gastown", journal: j, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}
	want := []string{
		"gastown events journal off in config (events-journal=false): nothing is journaled",
		"gastown events read failed: bd events tail: exit status 25",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("failed first poll = %q", got)
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"gastown events create gt-new seq=2"}) {
		t.Fatalf("retry ignored the cutoff or repeated the notice: %q", got)
	}
}

func TestLandingsSource_FailedFirstReadIsRetriedAsABacklogRead(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gastown.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil { // unreadable as a file
		t.Fatal(err)
	}
	s := &landingsSource{rig: "gastown", reader: &landings.Reader{Path: path}, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}
	if got := texts(s.Poll()); len(got) != 1 || !strings.HasPrefix(got[0], "gastown landings read failed: ") {
		t.Fatalf("failed first poll = %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, `{"bead":"gt-old","landed_at":"2026-09-30T12:00:00Z"}`+"\n"+`{"bead":"gt-new","landed_at":"2026-09-30T13:59:00Z"}`+"\n")
	got := texts(s.Poll())
	if len(got) != 1 || !strings.HasPrefix(got[0], "gastown landings landed gt-new ") {
		t.Fatalf("retry ignored the cutoff: %q", got)
	}
}

func TestDaemonSource_FailedFirstReadIsRetriedAsABacklogRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeGz(t, filepath.Join(dir, "daemon-2026-09-30T13-30-00.000.log.gz"), "2026/09/30 08:10:00 in-backup\n")
	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T13:00:00Z"), loc: tailTestLoc, now: fixedNow}
	got := texts(s.Poll())
	if len(got) != 2 || got[0] != "town daemon in-backup" || !strings.HasPrefix(got[1], "town daemon cannot read daemon.log: ") {
		t.Fatalf("failed first poll = %q", got)
	}
	appendFile(t, filepath.Join(dir, "daemon.log"), "2026/09/30 07:00:00 too-old\n2026/09/30 08:40:00 in-window\n")
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"town daemon in-window"}) {
		t.Fatalf("retry ignored the cutoff or reread the backups: %q", got)
	}
}

// fakeTailStore is the fake store the title and verdict tests read through. It
// answers by id and counts every read, so a test can see how many bd show
// calls the stream made for which bead.
type fakeTailStore struct {
	issues map[string]*beads.Issue
	errs   map[string]error
	reads  []string
}

func (s *fakeTailStore) show(_ string, id string) (*beads.Issue, error) {
	s.reads = append(s.reads, id)
	if err := s.errs[id]; err != nil {
		return nil, err
	}
	return s.issues[id], nil
}

func (s *fakeTailStore) readsFor(id string) int {
	n := 0
	for _, r := range s.reads {
		if r == id {
			n++
		}
	}
	return n
}

// TestTailSpend_ReportsOnlyAFreshReading: the DeepSeek rate prints only while
// the watch keeps measuring it. A reading older than the freshness window, one
// with no timestamp, a malformed file and a missing file all report nothing —
// never a zero the operator would read as a real rate.
func TestTailSpend_ReportsOnlyAFreshReading(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "watch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "spend.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func(d time.Duration) string {
		return time.Now().Add(d).UTC().Format(time.RFC3339)
	}

	write(`{"ts":"` + stamp(0) + `","per_hour":1.25,"balance":100}`)
	perHour, at, err := tailSpend(town)
	if err != nil || perHour != 1.25 || at.IsZero() {
		t.Fatalf("fresh spend = %v, %v, %v", perHour, at, err)
	}

	write(`{"ts":"` + stamp(-tailSummarySpendFresh-time.Minute) + `","per_hour":1.25}`)
	if _, _, err := tailSpend(town); err == nil {
		t.Error("a stale reading must report nothing")
	}
	write(`{"per_hour":1.25}`)
	if _, _, err := tailSpend(town); err == nil {
		t.Error("a reading with no timestamp must report nothing")
	}
	write("not json")
	if _, _, err := tailSpend(town); err == nil {
		t.Error("a malformed reading must report nothing")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tailSpend(town); err == nil {
		t.Error("a missing reading must report nothing")
	}
}

// TestTailSeatPicture_ARigThatCannotBeReadIsAnError: a store that is down must
// not read as an idle town, so a rig whose agent beads cannot be listed fails
// the whole seat read instead of silently counting that rig as zero.
func TestTailSeatPicture_ARigThatCannotBeReadIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o700); err != nil {
		t.Fatal(err)
	}
	rigs := `{"version":1,"rigs":{"gastown":{"git_url":"x","added_at":"2026-01-01T00:00:00Z","beads":{"repo":"","prefix":"gt"}}}}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(rigs), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", "opal"), 0o700); err != nil {
		t.Fatal(err)
	}

	down := func(string) (map[string]*beads.Issue, error) {
		return nil, errors.New("dolt: connection refused")
	}
	_, _, _, err := tailSeatPicture(town, down)
	if err == nil || !strings.Contains(err.Error(), "agent beads of gastown") {
		t.Fatalf("tailSeatPicture = %v; want the rig's read failure", err)
	}
}

// blockingTailStore holds its read open until the test releases it, so a test
// can observe what a second ask does while the first is in flight.
type blockingTailStore struct {
	entered chan string
	release chan struct{}
	mu      sync.Mutex
	reads   []string
}

func (s *blockingTailStore) show(_ string, id string) (*beads.Issue, error) {
	s.mu.Lock()
	s.reads = append(s.reads, id)
	s.mu.Unlock()
	s.entered <- id
	<-s.release
	return &beads.Issue{ID: id, Title: "the title"}, nil
}

func (s *blockingTailStore) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reads {
		if r == id {
			n++
		}
	}
	return n
}

// TestTailBeads_ConcurrentAskSharesTheInFlightRead: the sources poll at once
// and share the title cache. A second ask while the first read is still in
// flight answers "" rather than starting a second bd show for the same bead.
func TestTailBeads_ConcurrentAskSharesTheInFlightRead(t *testing.T) {
	t.Parallel()
	store := &blockingTailStore{entered: make(chan string, 1), release: make(chan struct{})}
	b := newTailBeads(store.show)

	done := make(chan string, 1)
	go func() { done <- b.title("gastown", "gt-1") }()
	<-store.entered // the first read is in flight
	if got := b.title("gastown", "gt-1"); got != "" {
		t.Errorf("a second ask during the read = %q; it must not start a second read", got)
	}
	close(store.release)
	if got := <-done; got != "the title" {
		t.Errorf("the first read = %q", got)
	}
	if n := store.count("gt-1"); n != 1 {
		t.Errorf("gt-1 was read %d times; want 1", n)
	}
	if got := b.title("gastown", "gt-1"); got != "the title" {
		t.Errorf("the cached ask = %q", got)
	}
	if n := store.count("gt-1"); n != 1 {
		t.Errorf("the cached ask read %d times", n)
	}
}

// TestTailBeads_TitleIsReadOncePerBeadAndFailureIsSilence: one bd show per new
// id, a failed read remembered as "no title", and never an error.
func TestTailBeads_TitleIsReadOncePerBeadAndFailureIsSilence(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{
		issues: map[string]*beads.Issue{
			"gt-1": {ID: "gt-1", Title: "show the bead a line is about"},
		},
		errs: map[string]error{"gt-lost": errors.New("dolt: connection refused")},
	}
	b := newTailBeads(store.show)

	if got := b.title("gastown", "gt-1"); got != "show the bead a line is about" {
		t.Fatalf("title = %q", got)
	}
	if got := b.title("gastown", "gt-1"); got != "show the bead a line is about" {
		t.Fatalf("cached title = %q", got)
	}
	if n := store.readsFor("gt-1"); n != 1 {
		t.Fatalf("gt-1 was read %d times; the run reads each bead once", n)
	}
	for i := 0; i < 3; i++ {
		if got := b.title("gastown", "gt-lost"); got != "" {
			t.Fatalf("a failed read produced %q", got)
		}
	}
	if n := store.readsFor("gt-lost"); n != 1 {
		t.Fatalf("a failed read was retried %d times; failure is remembered", n)
	}
	if got := b.title("gastown", "gt-absent"); got != "" {
		t.Fatalf("an unknown bead produced %q", got)
	}
}

// TestTailBeads_TitleIsBounded: past the cap the run stops taking titles
// rather than growing without bound, and a bounded-out bead costs no read.
func TestTailBeads_TitleIsBounded(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{issues: map[string]*beads.Issue{}}
	b := newTailBeads(store.show)
	b.max = 2
	if got := b.title("r", "gt-1"); got != "" {
		t.Fatalf("title = %q", got)
	}
	b.title("r", "gt-2")
	if got := b.title("r", "gt-3"); got != "" {
		t.Fatalf("a bounded-out bead produced %q", got)
	}
	if len(store.reads) != 2 {
		t.Fatalf("reads = %v; the cap must stop the reads too", store.reads)
	}
}

// TestEventsSource_TitlesTheBeadAndReadsAVerdict: the source stamps the bead
// the stream should name and the review loop's own words, with one bd show per
// bead per poll, and never repeats a verdict already shown.
func TestTailEventsSource_TitlesTheBeadAndReadsAVerdict(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{issues: map[string]*beads.Issue{
		"gt-1": {
			ID: "gt-1", Title: "one line per thing that happened",
			Notes:    "MERGE REJECTION (attempt 1): gate-failed - make gate is red\n  findings:\n",
			Comments: []beads.Comment{{Text: "pushed: crew/x at 9f4171a4"}},
		},
		"gt-2": {
			ID: "gt-2", Title: "a bead with nothing to say",
			Comments: []beads.Comment{{Text: "OVERSEER REVIEW aaaa PASS: reads well"}},
		},
	}}
	j := &fakeTailJournal{config: "true", records: []beads.EventRecord{
		{Seq: 1, TS: "2026-09-30T13:50:00Z", Op: "create", IssueID: "gt-1", Actor: "sloan", Status: "open"},
		{Seq: 2, TS: "2026-09-30T13:50:00.5Z", Op: "update", IssueID: "gt-1", Actor: "daemon", Status: "open"},
		{Seq: 3, TS: "2026-09-30T13:51:00Z", Op: "comment", IssueID: "gt-1", Actor: "sloan"},
		{Seq: 4, TS: "2026-09-30T13:52:00Z", Op: "comment", IssueID: "gt-2", Actor: "steward"},
		{Seq: 5, TS: "2026-09-30T13:53:00Z", Op: "update", IssueID: "gt-gastown-polecat-opal", Actor: "daemon"},
	}}
	s := &eventsSource{rig: "gastown", journal: j, cutoff: at("2026-09-30T13:45:00Z"), now: fixedNow, beads: newTailBeads(store.show)}
	got := s.Poll()
	if len(got) != 5 {
		t.Fatalf("poll = %q", texts(got))
	}
	// The text is the journal's; the verdict is the only thing added.
	if got[0].Text != "create gt-1 status=open actor=sloan seq=1" {
		t.Fatalf("create line = %q", got[0].Text)
	}
	wantVerdicts := []string{
		"",
		"MERGE REJECTION (attempt 1): gate-failed - make gate is red",
		"",
		"OVERSEER REVIEW aaaa PASS: reads well",
		"",
	}
	for i, want := range wantVerdicts {
		if got[i].Verdict != want {
			t.Errorf("line %d (%q) verdict = %q, want %q", i, got[i].Text, got[i].Verdict, want)
		}
	}
	// One bd show per bead per poll, and none for a line the default view
	// hides: a worker's own agent bead is not read.
	if n := store.readsFor("gt-1"); n != 1 {
		t.Errorf("gt-1 was read %d times in one poll", n)
	}
	if n := store.readsFor("gt-gastown-polecat-opal"); n != 0 {
		t.Errorf("a polecat agent bead was read %d times", n)
	}

	// The next poll adds a comment the bead already answered with: it is not
	// the review loop's, and a verdict already shown is not shown again.
	j.records = append(j.records,
		beads.EventRecord{Seq: 6, TS: "2026-09-30T13:54:00Z", Op: "comment", IssueID: "gt-1", Actor: "sloan"},
		beads.EventRecord{Seq: 7, TS: "2026-09-30T13:55:00Z", Op: "update", IssueID: "gt-1", Actor: "daemon"},
	)
	got = s.Poll()
	if len(got) != 2 || got[0].Verdict != "" || got[1].Verdict != "" {
		t.Fatalf("a verdict was repeated: %+v", got)
	}
}

// TestEventsSource_UnreadableBeadIsNotAnErrorLine: a store that cannot answer
// costs the title and the verdict, and says nothing about it.
func TestTailEventsSource_UnreadableBeadIsNotAnErrorLine(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{errs: map[string]error{"gt-1": errors.New("dolt: connection refused")}}
	j := &fakeTailJournal{config: "true", records: []beads.EventRecord{
		{Seq: 1, TS: "2026-09-30T13:50:00Z", Op: "comment", IssueID: "gt-1", Actor: "sloan"},
	}}
	s := &eventsSource{rig: "gastown", journal: j, cutoff: at("2026-09-30T13:45:00Z"), now: fixedNow, beads: newTailBeads(store.show)}
	got := s.Poll()
	if len(got) != 1 || got[0].Text != "comment gt-1 actor=sloan seq=1" || got[0].Verdict != "" {
		t.Fatalf("poll = %+v", got)
	}
}

// --- dispatched to deployed ---

// fakeTailAncestry answers from a set of "<commit> <of>" ancestor pairs;
// anything else is "not an ancestor". It never answers unknown, so a test
// that wants the git-error path builds its own seam.
func fakeTailAncestry(pairs ...string) tailAncestry {
	set := map[string]bool{}
	for _, p := range pairs {
		set[p] = true
	}
	return func(commit, of string) (bool, bool) { return set[commit+" "+of], true }
}

func daemonAt(ts, text string) tailLine {
	return tailLine{At: at(ts), Rig: "town", Kind: tailKindDaemon, Text: text}
}

// TestTailDeploys_OnlyALaterRestartDeploys: a restart that predates the
// landing cannot have installed its commit, so the bead waits for a restart
// whose installed commit contains it.
func TestTailDeploys_OnlyALaterRestartDeploys(t *testing.T) {
	t.Parallel()
	track := newTailDeploys(fixedNow, fakeTailAncestry("1111aaaa 2222bbbb"), at("2026-09-30T13:00:00Z"))
	got := track.observe([]tailLine{
		daemonAt("2026-09-30T13:01:00Z", "spec_dispatch: dispatched: gt-1: slung to gastown/opal on deepseek-flash (seat deepseek-flash 1/3)"),
		daemonAt("2026-09-30T13:02:00Z", "landing_worker: [land] gt-1: merged 0f0f onto origin/main (aaaa) as 1111aaaa; gating the merged tree, then om review"),
		// A restart before the landing cannot have installed its commit.
		daemonAt("2026-09-30T13:02:30Z", "upgrade-restart: running 3333cccc covers marker 3333cccc; cleared"),
		daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 9e9e)"),
		// This restart predates the landing too, so it still does not count.
		daemonAt("2026-09-30T13:03:30Z", "upgrade-restart: running 4444dddd covers marker 4444dddd; cleared"),
		// The installed commit contains the landing: deployed here.
		daemonAt("2026-09-30T13:04:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	})
	want := []string{"town daemon gt-1 deployed in 3.0m (work 1.0m, land 1.0m, deploy 1.0m)"}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("deployed = %q\nwant %q", texts(got), want)
	}
	if !got[0].At.Equal(at("2026-09-30T13:04:00Z")) {
		t.Fatalf("deployed line time = %v; want the covering restart's time", got[0].At)
	}
	if s := track.snapshot(); s.Waiting != 0 || !s.HasMedian {
		t.Fatalf("snapshot after deploy = %+v", s)
	}
}

// TestTailDeploys_FirstDispatchWinsOverAReworkRedispatch: a second dispatch
// after the first merge is the rework loop, not the bead's start.
func TestTailDeploys_FirstDispatchWinsOverAReworkRedispatch(t *testing.T) {
	t.Parallel()
	track := newTailDeploys(fixedNow, fakeTailAncestry("1111aaaa 2222bbbb"), at("2026-09-30T12:00:00Z"))
	track.observe([]tailLine{
		daemonAt("2026-09-30T12:00:00Z", "spec_dispatch: dispatched: gt-1: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T12:02:00Z", "landing_worker: [land] gt-1: merged 0f0f onto origin/main (aaaa) as 1111aaaa; gating the merged tree"),
		daemonAt("2026-09-30T12:03:00Z", "spec_dispatch: dispatched: gt-1: slung to gastown/jade on x (seat 1/3)"),
		daemonAt("2026-09-30T12:04:00Z", "landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 9e9e)"),
	})
	got := texts(track.observe([]tailLine{
		daemonAt("2026-09-30T12:05:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	}))
	want := "town daemon gt-1 deployed in 5.0m (work 2.0m, land 2.0m, deploy 1.0m)"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("deployed = %q\nwant %q", got, want)
	}
}

// TestTailDeploys_HandSlungPrintsTheDeployPartAlone: a bead with no dispatch
// line has nothing to measure the work or the land from, so it prints the
// deploy stage only and is left out of the median.
func TestTailDeploys_HandSlungPrintsTheDeployPartAlone(t *testing.T) {
	t.Parallel()
	track := newTailDeploys(fixedNow, fakeTailAncestry("1111aaaa 2222bbbb"), at("2026-09-30T12:00:00Z"))
	got := track.observe([]tailLine{
		daemonAt("2026-09-30T12:02:00Z", "landing_worker: [land] gt-2: merged 0f0f onto origin/main (aaaa) as 1111aaaa; gating the merged tree"),
		daemonAt("2026-09-30T12:03:00Z", "landing_worker: [land] gt-2: landed 1111aaaa on origin/main (patch-id 9e9e)"),
		daemonAt("2026-09-30T12:04:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	})
	want := []string{"town daemon gt-2 deployed in 1.0m (deploy 1.0m)"}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("deployed = %q\nwant %q", texts(got), want)
	}
	if s := track.snapshot(); s.HasMedian {
		t.Fatalf("a hand-slung bead entered the median: %+v", s)
	}
}

// TestTailDeploys_RepairedLandingCounts: the landing worker records a bead
// already on main as "already landed as <sha>", the same landing in another
// wording.
func TestTailDeploys_RepairedLandingCounts(t *testing.T) {
	t.Parallel()
	track := newTailDeploys(fixedNow, fakeTailAncestry("a9f727de 2222bbbb"), at("2026-09-30T13:00:00Z"))
	got := track.observe([]tailLine{
		daemonAt("2026-09-30T13:20:00Z", "landing_worker: [land] gt-4k3fj.6: already landed as a9f727de; finishing its record"),
		daemonAt("2026-09-30T13:30:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	})
	want := []string{"town daemon gt-4k3fj.6 deployed in 10.0m (deploy 10.0m)"}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("deployed = %q\nwant %q", texts(got), want)
	}
}

// TestTailDeploys_UnansweredAncestryLeavesTheBeadWaiting: a git failure is not
// a deployment.
func TestTailDeploys_UnansweredAncestryLeavesTheBeadWaiting(t *testing.T) {
	t.Parallel()
	unknown := func(string, string) (bool, bool) { return false, false }
	track := newTailDeploys(fixedNow, unknown, at("2026-09-30T12:00:00Z"))
	got := track.observe([]tailLine{
		daemonAt("2026-09-30T12:01:00Z", "spec_dispatch: dispatched: gt-1: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T12:03:00Z", "landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 9e9e)"),
		daemonAt("2026-09-30T12:04:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	})
	if len(got) != 0 {
		t.Fatalf("an unanswered ancestry deployed the bead: %q", texts(got))
	}
	if s := track.snapshot(); s.Waiting != 1 || s.HasOldest != true {
		t.Fatalf("snapshot = %+v; want the bead waiting", s)
	}
}

// TestTailDeploys_SnapshotMedianAndWaiting: the median covers the beads that
// landed in the last hour and had a dispatch; every waiting landing is
// counted, with the oldest one's age.
func TestTailDeploys_SnapshotMedianAndWaiting(t *testing.T) {
	t.Parallel()
	// now = 14:00Z, so the median window is 13:00Z..14:00Z.
	track := newTailDeploys(fixedNow,
		fakeTailAncestry("aaaaaaaa x1", "bbbbbbbb x2", "dddddddd x3"), at("2026-09-30T11:00:00Z"))
	track.observe([]tailLine{
		daemonAt("2026-09-30T11:00:00Z", "spec_dispatch: dispatched: gt-d: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T12:00:00Z", "landing_worker: [land] gt-d: landed dddddddd on origin/main (patch-id 1)"),
		daemonAt("2026-09-30T12:30:00Z", "upgrade-restart: running x3 covers marker x3; cleared"),
		daemonAt("2026-09-30T13:00:00Z", "spec_dispatch: dispatched: gt-a: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:30:00Z", "landing_worker: [land] gt-a: landed aaaaaaaa on origin/main (patch-id 2)"),
		daemonAt("2026-09-30T13:20:00Z", "spec_dispatch: dispatched: gt-b: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:40:00Z", "landing_worker: [land] gt-b: landed bbbbbbbb on origin/main (patch-id 3)"),
		daemonAt("2026-09-30T13:45:00Z", "upgrade-restart: running x2 covers marker x2; cleared"),
		daemonAt("2026-09-30T13:45:00Z", "landing_worker: [land] gt-c: landed cccccccc on origin/main (patch-id 4)"),
		daemonAt("2026-09-30T13:50:00Z", "upgrade-restart: running x1 covers marker x1; cleared"),
	})
	s := track.snapshot()
	if s.Landed != 4 || s.Waiting != 1 || !s.HasOldest || s.OldestWaiting != 15*time.Minute {
		t.Fatalf("snapshot = %+v; want 4 landed, 1 waiting for 15m", s)
	}
	// gt-a: 13:00 -> 13:50 is 50m; gt-b: 13:20 -> 13:45 is 25m; gt-d landed
	// before the window so it is out; gt-c never deployed.
	if !s.HasMedian || s.MedianBeads != 2 || s.MedianMin != 37.5 {
		t.Fatalf("median = %+v; want 37.5 over 2 beads", s)
	}
}

// TestTailDeploySource_WindowBoundsTheStreamNotTheTracker: a covering restart
// older than the run's window still deploys the bead for the summary, but
// neither its raw line nor its deploy line prints.
func TestTailDeploySource_WindowBoundsTheStreamNotTheTracker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 08:00:00 spec_dispatch: dispatched: gt-1: slung to gastown/opal on x (seat 1/3)\n"+
			"2026/09/30 08:05:00 landing_worker: [land] gt-1: merged aa onto origin/main (bb) as cc; gating the merged tree\n"+
			"2026/09/30 08:10:00 landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 9e)\n"+
			"2026/09/30 08:20:00 upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared\n"+
			"2026/09/30 08:50:00 hm witness started\n")

	track := newTailDeploys(fixedNow, fakeTailAncestry("1111aaaa 2222bbbb"), at("2026-09-30T13:30:00Z"))
	s := &tailDeploySource{
		inner: &daemonSource{dir: dir, cutoff: track.logCutoff(at("2026-09-30T13:30:00Z")), loc: tailTestLoc, now: fixedNow},
		track: track, from: at("2026-09-30T13:30:00Z"),
	}
	got := texts(s.Poll())
	if !reflect.DeepEqual(got, []string{"town daemon hm witness started"}) {
		t.Fatalf("stream = %q; want only the line inside the window", got)
	}
	snap := track.snapshot()
	if snap.Landed != 1 || snap.Waiting != 0 || !snap.HasMedian || snap.MedianMin != 20 {
		t.Fatalf("snapshot = %+v; want the pre-window restart to have deployed gt-1", snap)
	}
}

// TestTailDeploySource_PrintsTheDeployedLineInItsWindow: a restart inside the
// run's window prints both its own line and the bead's deploy line, and the
// rig filter still narrows the raw lines.
func TestTailDeploySource_PrintsTheDeployedLineInItsWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 08:00:00 spec_dispatch: dispatched: gt-1: slung to gastown/opal on x (seat 1/3)\n"+
			"2026/09/30 08:05:00 landing_worker: [land] gt-1: merged aa onto origin/main (bb) as cc; gating the merged tree\n"+
			"2026/09/30 08:10:00 landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 9e)\n"+
			"2026/09/30 08:20:00 upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared\n"+
			"2026/09/30 08:45:00 Skipping crash detection for gastown/jade\n"+
			"2026/09/30 08:50:00 hm witness started\n")

	// --rig gastown: the restart line names no rig, so it does not print, but
	// the deployed bead's line does, and the untracked hm line stays out.
	from := at("2026-09-30T13:15:00Z")
	track := newTailDeploys(fixedNow, fakeTailAncestry("1111aaaa 2222bbbb"), from)
	s := &tailDeploySource{
		inner: &daemonSource{dir: dir, cutoff: track.logCutoff(from), loc: tailTestLoc, now: fixedNow},
		track: track, from: from, rigFilter: tailRigFilter("gastown"),
	}
	want := []string{
		"town daemon Skipping crash detection for gastown/jade",
		"town daemon gt-1 deployed in 20.0m (work 5.0m, land 5.0m, deploy 10.0m)",
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, want) {
		t.Fatalf("stream = %q\nwant %q", got, want)
	}
}

// TestEventsSource_CommentTextAndSubmittedLine: a plain comment carries its
// first 60 characters, a work bead's READY TO LAND block carries a submitted
// line, a comment with a review marker keeps the verdict instead, and neither
// annotation repeats (gt-vxr95).
func TestTailEventsSource_CommentTextAndSubmittedLine(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 70)
	store := &fakeTailStore{issues: map[string]*beads.Issue{
		"gt-1": {ID: "gt-1", Title: "t", Comments: []beads.Comment{{Text: "pushed: crew/x at 9f4171a4"}}},
		"gt-2": {ID: "gt-2", Title: "t", Labels: []string{land.LabelReadyToLand}, Notes: "READY TO LAND\nBranch: polecat/opal/gt-2\nHead: abcdef1234567890\nTarget: main\nWorker: gastown/polecats/opal\n"},
		"gt-3": {ID: "gt-3", Title: "t", Comments: []beads.Comment{{Text: long}}},
		"gt-4": {ID: "gt-4", Title: "t", Comments: []beads.Comment{{Text: "OVERSEER REVIEW aaaa PASS: reads well"}}},
	}}
	j := &fakeTailJournal{config: "true", records: []beads.EventRecord{
		{Seq: 1, TS: "2026-09-30T13:50:00Z", Op: "comment", IssueID: "gt-1", Actor: "sloan"},
		{Seq: 2, TS: "2026-09-30T13:51:00Z", Op: "update", IssueID: "gt-2", Actor: "opal", Status: "open"},
		{Seq: 3, TS: "2026-09-30T13:52:00Z", Op: "comment", IssueID: "gt-3", Actor: "sloan"},
		{Seq: 4, TS: "2026-09-30T13:53:00Z", Op: "comment", IssueID: "gt-4", Actor: "steward"},
	}}
	s := &eventsSource{rig: "gastown", journal: j, cutoff: at("2026-09-30T13:45:00Z"), now: fixedNow, beads: newTailBeads(store.show)}
	got := s.Poll()
	if len(got) != 4 {
		t.Fatalf("poll = %q", texts(got))
	}
	if got[0].Comment != "pushed: crew/x at 9f4171a4" || got[0].Verdict != "" {
		t.Errorf("plain comment = comment %q verdict %q", got[0].Comment, got[0].Verdict)
	}
	if got[1].Submit != "submitted gt-2 @ abcdef1 for landing" || got[1].Verdict != "" {
		t.Errorf("submission = submit %q verdict %q", got[1].Submit, got[1].Verdict)
	}
	if want := strings.Repeat("x", 60) + "…"; got[2].Comment != want {
		t.Errorf("a long comment = %q, want %q", got[2].Comment, want)
	}
	if got[3].Comment != "" || got[3].Verdict != "OVERSEER REVIEW aaaa PASS: reads well" {
		t.Errorf("a verdict comment = comment %q verdict %q", got[3].Comment, got[3].Verdict)
	}
	// One bd show per bead per poll answers every annotation read.
	if n := store.readsFor("gt-1"); n != 1 {
		t.Errorf("gt-1 was read %d times in one poll", n)
	}
	// The same text is not shown twice, however often the bead is rewritten.
	j.records = append(j.records,
		beads.EventRecord{Seq: 5, TS: "2026-09-30T13:54:00Z", Op: "comment", IssueID: "gt-1", Actor: "sloan"},
		beads.EventRecord{Seq: 6, TS: "2026-09-30T13:55:00Z", Op: "update", IssueID: "gt-2", Actor: "opal", Status: "open"},
	)
	got = s.Poll()
	if len(got) != 2 || got[0].Comment != "" || got[1].Submit != "" {
		t.Fatalf("an annotation repeated: %+v", got)
	}
	// A landing takes the ready-to-land label, so an update after it does not
	// reprint a submission that is over.
	store.issues["gt-2"].Labels = nil
	j.records = append(j.records, beads.EventRecord{Seq: 7, TS: "2026-09-30T13:56:00Z", Op: "update", IssueID: "gt-2", Actor: "opal", Status: "closed"})
	got = s.Poll()
	if len(got) != 1 || got[0].Submit != "" {
		t.Fatalf("a landed bead reprinted its submission: %+v", got)
	}
}
