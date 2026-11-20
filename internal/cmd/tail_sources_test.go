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
	"github.com/steveyegge/gastown/internal/landings"
)

func texts(lines []tailLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.Rig+" "+l.Kind+" "+l.Text)
	}
	return out
}

var tailNow = at("2026-09-30T14:00:00Z")

func fixedNow() time.Time { return tailNow }

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
// words, the backlog is cut at --since, and a line that is not an alert —
// appended after the backlog read, since the backlog cannot show it is inside
// the window — is skipped with one note naming the file.
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
	// The backlog is the file's own order, cut at the cutoff. Its unreadable
	// lines go with the rest of the history --since excludes: they carry no
	// timestamp to say they fall inside the window. Their count is reported
	// once, at the end of the backlog.
	want := []string{
		"watch watch bd-slow: bd took 4s",
		"watch watch direct-push: a commit landed outside the queue",
		"watch watch polecat-stall: gastown/opal silent 40m",
		"watch watch skipped 2 unreadable watch line(s) in backlog",
	}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("backlog:\n%q\nwant\n%q", texts(got), want)
	}
	wantClasses := []tailClass{tailClassWarning, tailClassFailure, tailClassWarning}
	for i, c := range wantClasses {
		if got[i].Class != c {
			t.Errorf("line %d (%q) class = %v, want %v", i, got[i].Text, got[i].Class, c)
		}
	}
	if !got[0].At.Equal(at("2026-09-30T13:50:00Z")) || !got[2].At.Equal(at("2026-09-30T13:53:00Z")) {
		t.Fatalf("times: %v %v", got[0].At, got[2].At)
	}

	// Follow: the appended line prints, an unreadable line appended after the
	// backlog read is noted once, and an idle poll says nothing.
	appendFile(t, alerts, "garbage\n"+`{"ts":"2026-09-30T14:00:01Z","class":"red-main","severity":"high","text":"main is red"}`+"\n")
	got = s.Poll()
	if len(got) != 2 || got[0].Text != "skipped unreadable watch line in alerts.jsonl (7 bytes)" ||
		got[1].Text != "red-main: main is red" || got[1].Class != tailClassFailure {
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
		{"medium", tailClassWarning},
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
			"2026/09/30 08:00:01 spec_dispatch: om: dispatched 2 spec(s)\n"+
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
		"om daemon spec_dispatch: om: dispatched 2 spec(s)",
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
			"2026/09/30 08:00:02 spec_dispatch: no specs to dispatch\n"+
			"2026/09/30 08:00:03 rig_worker: nothing ready to work\n")
	s := &daemonSource{dir: dir, cutoff: at("2026-09-30T12:00:00Z"), loc: tailTestLoc, now: fixedNow, rigs: loadTailRigs(townRoot)}
	want := []string{
		"town daemon jsonl_git_backup: not due - last run 5m0s ago",
		"town daemon patrol_scan: no rigs to scan",
		"town daemon spec_dispatch: no specs to dispatch",
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
	b := newTailBeads(store.show, tailTitleCacheSize)

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

// TestTailBeads_CachesSuccessAndRetriesFailure: one bd show per new id, a
// failed read cached as nothing so it is retried, and never an error. A
// failure cached forever would blank that bead's title for the life of the
// long-lived dashboard (gt-2czgm).
func TestTailBeads_CachesSuccessAndRetriesFailure(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{
		issues: map[string]*beads.Issue{
			"gt-1": {ID: "gt-1", Title: "show the bead a line is about"},
		},
		errs: map[string]error{"gt-lost": errors.New("dolt: connection refused")},
	}
	b := newTailBeads(store.show, tailTitleCacheSize)

	if got := b.title("gastown", "gt-1"); got != "show the bead a line is about" {
		t.Fatalf("title = %q", got)
	}
	if got := b.title("gastown", "gt-1"); got != "show the bead a line is about" {
		t.Fatalf("cached title = %q", got)
	}
	if n := store.readsFor("gt-1"); n != 1 {
		t.Fatalf("gt-1 was read %d times; a successful read is cached", n)
	}
	for i := 0; i < 3; i++ {
		if got := b.title("gastown", "gt-lost"); got != "" {
			t.Fatalf("a failed read produced %q", got)
		}
	}
	if n := store.readsFor("gt-lost"); n != 3 {
		t.Fatalf("a failed read was attempted %d times; a failure must not be cached", n)
	}
	// The store recovers: the title appears on the next ask.
	store.errs = map[string]error{}
	store.issues["gt-lost"] = &beads.Issue{ID: "gt-lost", Title: "recovered"}
	if got := b.title("gastown", "gt-lost"); got != "recovered" {
		t.Fatalf("after recovery the title = %q", got)
	}
	if got := b.title("gastown", "gt-absent"); got != "" {
		t.Fatalf("an unknown bead produced %q", got)
	}
}

// TestTailBeads_EvictsTheLeastRecentlyUsed: past the cap the cache evicts the
// least recently used id rather than refusing new ones, so a long-lived reader
// keeps learning titles instead of freezing on its first max beads (gt-2czgm).
func TestTailBeads_EvictsTheLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{issues: map[string]*beads.Issue{
		"gt-1": {ID: "gt-1", Title: "one"},
		"gt-2": {ID: "gt-2", Title: "two"},
		"gt-3": {ID: "gt-3", Title: "three"},
	}}
	b := newTailBeads(store.show, tailTitleCacheSize)
	b.max = 2

	b.title("r", "gt-1")
	b.title("r", "gt-2")
	b.title("r", "gt-2") // gt-2 is now the more recently used of the two

	if got := b.title("r", "gt-3"); got != "three" {
		t.Fatalf("a new id past the cap = %q; it must be read, not refused", got)
	}
	if n := store.readsFor("gt-3"); n != 1 {
		t.Fatalf("gt-3 was read %d times", n)
	}
	// gt-1 was the least recently used and was evicted, so it is read again.
	b.title("r", "gt-1")
	if n := store.readsFor("gt-1"); n != 2 {
		t.Fatalf("gt-1 was read %d times; the LRU entry should have been evicted", n)
	}
	if n := store.readsFor("gt-2"); n != 1 {
		t.Fatalf("gt-2 was read %d times; a recently used title stays cached", n)
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

// TestTailDeploysStartLineShipsAncestors: a pod image roll starts the daemon
// on a new build and writes "daemon-start: running <commit>" instead of an
// upgrade restart's line. A landing whose commit is an ancestor of that commit
// ships at the start line; a commit that is not, or one git cannot answer for,
// keeps the bead waiting.
func TestTailDeploysStartLineShipsAncestors(t *testing.T) {
	t.Parallel()
	unknown := func(string, string) (bool, bool) { return false, false }
	for _, tc := range []struct {
		name     string
		ancestry tailAncestry
		start    string
		want     []string
	}{
		{
			name:     "an ancestor of the start commit ships at the start line",
			ancestry: fakeTailAncestry("1111aaaa 2222bbbb"),
			start:    "2222bbbb",
			want:     []string{"town daemon gt-1 deployed in 3.0m (work 1.0m, land 1.0m, deploy 1.0m)"},
		},
		{
			name:     "a commit that is not an ancestor of the start commit waits",
			ancestry: fakeTailAncestry("1111aaaa 2222bbbb"),
			start:    "3333cccc",
		},
		{
			name:     "an ancestry question git cannot answer waits",
			ancestry: unknown,
			start:    "2222bbbb",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			track := newTailDeploys(fixedNow, tc.ancestry, at("2026-09-30T13:00:00Z"))
			got := track.observe([]tailLine{
				daemonAt("2026-09-30T13:01:00Z", "spec_dispatch: dispatched: gt-1: slung to gastown/opal on x (seat 1/3)"),
				daemonAt("2026-09-30T13:02:00Z", "landing_worker: [land] gt-1: merged 0f0f onto origin/main (aaaa) as 1111aaaa; gating the merged tree"),
				daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 9e9e)"),
				daemonAt("2026-09-30T13:04:00Z", "daemon-start: running "+tc.start),
			})
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("deployed = %q; want the bead waiting", texts(got))
				}
				if s := track.snapshot(); s.Waiting != 1 {
					t.Fatalf("snapshot = %+v; want the bead waiting", s)
				}
				return
			}
			if !reflect.DeepEqual(texts(got), tc.want) {
				t.Fatalf("deployed = %q\nwant %q", texts(got), tc.want)
			}
			if !got[0].At.Equal(at("2026-09-30T13:04:00Z")) {
				t.Fatalf("deployed line time = %v; want the start line's time", got[0].At)
			}
		})
	}
}

// TestTailDeploysOldRestartLineStillWorks: a log holding only the upgrade
// restart's line ships a landing exactly as it did before the start line.
func TestTailDeploysOldRestartLineStillWorks(t *testing.T) {
	t.Parallel()
	track := newTailDeploys(fixedNow, fakeTailAncestry("1111aaaa 2222bbbb"), at("2026-09-30T13:00:00Z"))
	got := track.observe([]tailLine{
		daemonAt("2026-09-30T13:01:00Z", "spec_dispatch: dispatched: gt-1: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:02:00Z", "landing_worker: [land] gt-1: merged 0f0f onto origin/main (aaaa) as 1111aaaa; gating the merged tree"),
		daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 9e9e)"),
		daemonAt("2026-09-30T13:04:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	})
	want := []string{"town daemon gt-1 deployed in 3.0m (work 1.0m, land 1.0m, deploy 1.0m)"}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("deployed = %q\nwant %q", texts(got), want)
	}
	if !got[0].At.Equal(at("2026-09-30T13:04:00Z")) {
		t.Fatalf("deployed line time = %v; want the restart line's time", got[0].At)
	}
	if s := track.snapshot(); s.Waiting != 0 {
		t.Fatalf("snapshot after deploy = %+v", s)
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

// TestWatchSource_AttentionEventsWithRuleFieldsMergeUnchanged is the golden for
// gt-0f3nd: the rule, handled and wake keys an attention event now carries are
// ignored by the watch source, so the same events print the same lines and
// classes as before they existed.
func TestWatchSource_AttentionEventsWithRuleFieldsMergeUnchanged(t *testing.T) {
	t.Parallel()
	legacy := `{"ts":"2026-09-30T13:53:00Z","class":"polecat-stall","severity":"low","text":"gastown/opal silent 40m","key":"stall:gastown/opal","state":"new"}` + "\n" +
		`{"ts":"2026-09-30T13:54:00Z","class":"red-main","severity":"high","text":"main is red","key":"red-main:gastown","state":"new"}` + "\n" +
		`{"ts":"2026-09-30T13:55:00Z","class":"red-main","severity":"high","text":"main is red","key":"red-main:gastown","state":"cleared"}` + "\n"
	current := `{"ts":"2026-09-30T13:53:00Z","class":"polecat-stall","severity":"low","text":"gastown/opal silent 40m","key":"stall:gastown/opal","state":"new","rule":"polecat.stall","handled":"operator_action","wake":true}` + "\n" +
		`{"ts":"2026-09-30T13:54:00Z","class":"red-main","severity":"high","text":"main is red","key":"red-main:gastown","state":"new","rule":"main.red","handled":"operator_action","wake":true}` + "\n" +
		`{"ts":"2026-09-30T13:55:00Z","class":"red-main","severity":"high","text":"main is red","key":"red-main:gastown","state":"cleared","rule":"main.red","handled":"info"}` + "\n"

	poll := func(content string) []tailLine {
		town := t.TempDir()
		path := attention.EventsPath(town)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		appendFile(t, path, content)
		return newTailWatchSource(town, at("2026-09-30T13:00:00Z"), fixedNow).Poll()
	}
	want := []string{
		"watch watch polecat-stall: gastown/opal silent 40m",
		"watch watch red-main: main is red",
		"watch watch red-main: main is red",
	}
	before, after := poll(legacy), poll(current)
	if !reflect.DeepEqual(texts(before), want) {
		t.Fatalf("legacy lines:\n%q\nwant\n%q", texts(before), want)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("lines with rule fields differ from the legacy ones:\n%+v\nwant\n%+v", after, before)
	}
}

// TestTailWatchSourceTwoFailingFilesNotDeduped: each watch file remembers its
// own last failure, so two files failing for different reasons each report
// once instead of overwriting each other and reprinting every poll.
func TestTailWatchSourceTwoFailingFilesNotDeduped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where each feed file belongs: opening it succeeds and the
	// read fails, which is a per-file failure the source names.
	for _, path := range []string{
		filepath.Join(town, ".runtime", "watch", "alerts.jsonl"),
		attention.EventsPath(town),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	s := newTailWatchSource(town, at("2026-09-30T13:00:00Z"), fixedNow)
	first := texts(s.Poll())
	if len(first) != 2 ||
		!strings.HasPrefix(first[0], "watch watch cannot read alerts.jsonl: ") ||
		!strings.HasPrefix(first[1], "watch watch cannot read events.jsonl: ") {
		t.Fatalf("first poll = %q; want one note per failing file", first)
	}
	if got := texts(s.Poll()); len(got) != 0 {
		t.Fatalf("second poll reprinted the failures: %q", got)
	}
}

// TestTailWatchSourceBacklogReportsOldMalformedLines: a line that is not an
// alert has no timestamp, so the backlog read --since starts the stream with
// cannot place it in the window and does not echo it. It still reports the
// count: a feed file the operator cannot fully read must not look clean for
// the rest of the run.
func TestTailWatchSourceBacklogReportsOldMalformedLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	alerts := filepath.Join(town, ".runtime", "watch", "alerts.jsonl")
	if err := os.MkdirAll(filepath.Dir(alerts), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, alerts, "garbage from days ago\n"+
		`{"ts":"2026-09-30T13:59:00Z","severity":"high","text":"in window"}`+"\n")

	s := newTailWatchSource(town, at("2026-09-30T13:00:00Z"), fixedNow)
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{
		"watch watch in window",
		"watch watch skipped 1 unreadable watch line(s) in backlog",
	}) {
		t.Fatalf("backlog = %q; want the count noted, not the line echoed", got)
	}
	// A malformed line appended after the backlog read is current, and noted.
	appendFile(t, alerts, "garbage\n")
	if got := texts(s.Poll()); !reflect.DeepEqual(got,
		[]string{"watch watch skipped unreadable watch line in alerts.jsonl (7 bytes)"}) {
		t.Fatalf("follow = %q", got)
	}
}

// TestTailWatchSourceBacklogCountSurvivesAFailedSibling: a sibling that cannot
// be read holds the backlog open, and the files that did read have already
// advanced past their bad lines. The count therefore has to live on the source
// until the backlog completes, or the damage vanishes the moment the retry
// succeeds — the same silent drop, one poll later.
func TestTailWatchSourceBacklogCountSurvivesAFailedSibling(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	alerts := filepath.Join(town, ".runtime", "watch", "alerts.jsonl")
	if err := os.MkdirAll(filepath.Dir(alerts), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, alerts, "garbage from days ago\n"+
		`{"ts":"2026-09-30T13:59:00Z","severity":"high","text":"in window"}`+"\n")
	// A directory where the events file belongs: opening it succeeds and the
	// read fails, so the backlog cannot complete.
	events := attention.EventsPath(town)
	if err := os.MkdirAll(events, 0o700); err != nil {
		t.Fatal(err)
	}

	s := newTailWatchSource(town, at("2026-09-30T13:00:00Z"), fixedNow)
	first := texts(s.Poll())
	if len(first) != 2 ||
		first[0] != "watch watch in window" ||
		!strings.HasPrefix(first[1], "watch watch cannot read events.jsonl: ") {
		t.Fatalf("first poll = %q; want the alert and the sibling's failure, no count yet", first)
	}

	if err := os.Remove(events); err != nil {
		t.Fatal(err)
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got,
		[]string{"watch watch skipped 1 unreadable watch line(s) in backlog"}) {
		t.Fatalf("second poll = %q; want the count carried from the failed poll", got)
	}
}

// TestLandingsSourceBacklogReportsOldMalformedLines: a non-record line has no
// landed_at to cut at, so the backlog read cannot place it in the window and
// does not echo it. It still reports the count: a file the operator cannot
// fully read must not look clean for the rest of the run.
func TestLandingsSourceBacklogReportsOldMalformedLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gastown.jsonl")
	appendFile(t, path, "garbage from days ago\n"+
		`{"bead":"gt-new","rig":"gastown","landed_at":"2026-09-30T13:55:00Z"}`+"\n")
	s := &landingsSource{rig: "gastown", reader: &landings.Reader{Path: path}, cutoff: at("2026-09-30T13:00:00Z"), now: fixedNow}
	got := texts(s.Poll())
	if len(got) != 2 ||
		got[0] != "gastown landings skipped 1 unreadable landings line(s) in backlog" ||
		!strings.HasPrefix(got[1], "gastown landings landed gt-new ") {
		t.Fatalf("backlog = %q; want the count noted, not the line echoed", got)
	}
	appendFile(t, path, "garbage\n")
	if got := texts(s.Poll()); !reflect.DeepEqual(got,
		[]string{"gastown landings unreadable landings line (7 bytes)"}) {
		t.Fatalf("follow = %q", got)
	}
}

// TestTailDeploysObserveReleasesLockDuringGit: the ancestry check shells out
// to git with its own timeout, so it runs outside the tracker's lock — a stuck
// mount must not stop the summary and the Landings table reading the tracker.
func TestTailDeploysObserveReleasesLockDuringGit(t *testing.T) {
	t.Parallel()
	var track *tailDeploys
	// freeDuringGit reports whether the tracker's lock was available when git
	// was asked. TryLock is false rather than blocking when observe is still
	// holding it, which is what a test without a wall-clock wait needs.
	freeDuringGit := make(chan bool, 1)
	ancestry := func(string, string) (bool, bool) {
		free := track.mu.TryLock()
		if free {
			track.mu.Unlock()
		}
		freeDuringGit <- free
		return true, true
	}
	track = newTailDeploys(fixedNow, ancestry, at("2026-09-30T12:00:00Z"))

	track.observe([]tailLine{
		daemonAt("2026-09-30T12:01:00Z", "spec_dispatch: dispatched: gt-1: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T12:03:00Z", "landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 9e9e)"),
		daemonAt("2026-09-30T12:04:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	})
	if free := <-freeDuringGit; !free {
		t.Fatal("the tracker lock was held while the ancestry check ran")
	}
	if snap := track.snapshot(); snap.Waiting != 0 || snap.Landed != 1 {
		t.Fatalf("snapshot = %+v; want the bead deployed", snap)
	}
}

// TestTailBeads_NotFoundIsRemembered: a bead bd reported as not found is not
// asked about again by issue or title, while a timeout stays retryable.
func TestTailBeads_NotFoundIsRemembered(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{errs: map[string]error{
		"gt-gone":  fmt.Errorf("show gt-gone: %w", beads.ErrNotFound),
		"gt-slow":  errors.New("bd show: timed out"),
		"gt-gone2": beads.ErrNotFound,
	}}
	b := newTailBeads(store.show, tailTitleCacheSize)

	for i := 0; i < 3; i++ {
		if got := b.issue("gastown", "gt-gone"); got != nil {
			t.Fatalf("issue = %v, want nil", got)
		}
		if got := b.title("gastown", "gt-gone"); got != "" {
			t.Fatalf("title = %q, want empty", got)
		}
	}
	if n := store.readsFor("gt-gone"); n != 1 {
		t.Fatalf("gt-gone read %d times, want 1", n)
	}

	for i := 0; i < 3; i++ {
		b.issue("gastown", "gt-slow")
		b.title("gastown", "gt-slow")
	}
	if n := store.readsFor("gt-slow"); n != 6 {
		t.Fatalf("gt-slow read %d times, want 6 (a timeout is retried)", n)
	}

	// title first, then issue: the miss title found is shared with pollIssue.
	b.title("gastown", "gt-gone2")
	b.issue("gastown", "gt-gone2")
	if n := store.readsFor("gt-gone2"); n != 1 {
		t.Fatalf("gt-gone2 read %d times, want 1", n)
	}
}

// TestTailBeads_CachedTitleNeverReads: the cache-only answer starts no bd read.
// A bead never read, or whose read failed, is unsettled; a title that was read
// and a not-found answer are settled, and asking for them again reads nothing.
func TestTailBeads_CachedTitleNeverReads(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{
		issues: map[string]*beads.Issue{"gt-ok": {ID: "gt-ok", Title: "the title"}},
		errs: map[string]error{
			"gt-gone": beads.ErrNotFound,
			"gt-slow": errors.New("bd show: timed out"),
		},
	}
	b := newTailBeads(store.show, tailTitleCacheSize)

	if title, settled := b.cachedTitle("gt-ok"); title != "" || settled {
		t.Fatalf("unread bead: cachedTitle = (%q, %v), want (\"\", false)", title, settled)
	}
	if n := len(store.reads); n != 0 {
		t.Fatalf("cachedTitle on an unread bead made %d reads, want 0", n)
	}

	b.title("gastown", "gt-ok")
	b.title("gastown", "gt-gone")
	b.title("gastown", "gt-slow")
	reads := len(store.reads)

	if title, settled := b.cachedTitle("gt-ok"); title != "the title" || !settled {
		t.Fatalf("read bead: cachedTitle = (%q, %v), want (\"the title\", true)", title, settled)
	}
	if title, settled := b.cachedTitle("gt-gone"); title != "" || !settled {
		t.Fatalf("not-found bead: cachedTitle = (%q, %v), want (\"\", true)", title, settled)
	}
	if title, settled := b.cachedTitle("gt-slow"); title != "" || settled {
		t.Fatalf("failed read: cachedTitle = (%q, %v), want (\"\", false): a failure is not a final answer", title, settled)
	}
	if n := len(store.reads); n != reads {
		t.Fatalf("cachedTitle made %d reads, want none", n-reads)
	}

	var nilReader *tailBeads
	if title, settled := nilReader.cachedTitle("gt-ok"); title != "" || settled {
		t.Fatalf("nil reader: cachedTitle = (%q, %v), want (\"\", false)", title, settled)
	}
}

// TestTailBeads_NotFoundCacheIsBounded: the set drops its oldest id when full,
// so a run never grows past tailMissingCacheSize.
func TestTailBeads_NotFoundCacheIsBounded(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{errs: map[string]error{}}
	for i := 0; i <= tailMissingCacheSize; i++ {
		store.errs[fmt.Sprintf("gt-%d", i)] = beads.ErrNotFound
	}
	b := newTailBeads(store.show, tailTitleCacheSize)
	for i := 0; i <= tailMissingCacheSize; i++ {
		b.issue("gastown", fmt.Sprintf("gt-%d", i))
	}
	if len(b.missing) != tailMissingCacheSize {
		t.Fatalf("remembered %d ids, want %d", len(b.missing), tailMissingCacheSize)
	}
	b.issue("gastown", "gt-0") // the oldest was dropped, so it is asked again
	if n := store.readsFor("gt-0"); n != 2 {
		t.Fatalf("gt-0 read %d times, want 2", n)
	}
	b.issue("gastown", fmt.Sprintf("gt-%d", tailMissingCacheSize)) // the newest is still remembered
	if n := store.readsFor(fmt.Sprintf("gt-%d", tailMissingCacheSize)); n != 1 {
		t.Fatalf("newest id read %d times, want 1", n)
	}
}

// TestTailBeads_NotFoundIsAskedAgainAfterTheWindow: a not-found answer is
// trusted for tailMissingTTL, then the bead is read again, so a bead bd could
// not read yet gets its title once it can be read.
func TestTailBeads_NotFoundIsAskedAgainAfterTheWindow(t *testing.T) {
	t.Parallel()
	store := &fakeTailStore{errs: map[string]error{"gt-late": beads.ErrNotFound}}
	b := newTailBeads(store.show, tailTitleCacheSize)
	clock := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }

	if got := b.title("gastown", "gt-late"); got != "" {
		t.Fatalf("title = %q, want empty", got)
	}
	clock = clock.Add(tailMissingTTL - time.Second)
	b.title("gastown", "gt-late")
	if n := store.readsFor("gt-late"); n != 1 {
		t.Fatalf("gt-late read %d times inside the window, want 1", n)
	}

	clock = clock.Add(2 * time.Second)
	delete(store.errs, "gt-late")
	store.issues = map[string]*beads.Issue{"gt-late": {ID: "gt-late", Title: "now readable"}}
	if got := b.title("gastown", "gt-late"); got != "now readable" {
		t.Fatalf("title = %q, want %q once the window has passed", got, "now readable")
	}
	if n := store.readsFor("gt-late"); n != 2 {
		t.Fatalf("gt-late read %d times, want 2", n)
	}
}

// TestTailBeadsSubmittedBy: the actor of a bead's newest "Submitted for
// landing" comment is its submitter, and a comment written after it (om's
// verdict, a follow-up) does not hide it. A successful read that names no
// submission, and a reader with no comment source, name no one, so the page
// reads the landing as the operator's; a read that fails names the page's
// unreadable-source mark, never the operator (gt-6ua0sn, gt-f1fc2f).
func TestTailBeadsSubmittedBy(t *testing.T) {
	t.Parallel()
	comments := []beads.Comment{
		{Author: "gastown/polecats/keeper", Text: "Submitted for landing: polecat/keeper/gt-1+old @ 1111 onto main"},
		{Author: "town/crew/overseer", Text: "Submitted for landing: crew/ops/gt-1 @ 2222 onto main"},
		{Author: "daemon", Text: "om approve 0.88"},
	}
	for name, tc := range map[string]struct {
		read func(rig, id string) ([]beads.Comment, error)
		want string
	}{
		"newest submission wins": {func(string, string) ([]beads.Comment, error) { return comments, nil }, "town/crew/overseer"},
		"no submission comment": {func(string, string) ([]beads.Comment, error) {
			return []beads.Comment{{Author: "daemon", Text: "om approve 0.88"}}, nil
		}, ""},
		"unreadable comments": {func(string, string) ([]beads.Comment, error) { return nil, fmt.Errorf("bd failed") }, submitterUnknown},
		"no comments source":  {nil, ""},
	} {
		b := newTailBeads(nil, tailTitleCacheSize)
		b.comments = tc.read
		if got := b.submittedBy("gastown", "gt-1"); got != tc.want {
			t.Errorf("%s: submittedBy = %q, want %q", name, got, tc.want)
		}
	}
	var nilReader *tailBeads
	if got := nilReader.submittedBy("gastown", "gt-1"); got != "" {
		t.Errorf("nil reader submittedBy = %q, want none", got)
	}

	read := false
	b := newTailBeads(nil, tailTitleCacheSize)
	b.comments = func(string, string) ([]beads.Comment, error) { read = true; return comments, nil }
	if got := b.submittedBy("gastown", "-danger"); got != "" {
		t.Errorf("flag-shaped id submittedBy = %q, want none", got)
	}
	if read {
		t.Error("a flag-shaped bead id reached the comments read")
	}
}

// TestTailBeadsSubmittedByIsCached: a successful answer is reused inside
// tailSubmitterTTL and read again once it passes. The dashboard asks each ops
// row on every poll, and each ask was a bd comments subprocess (gt-f1fc2f).
func TestTailBeadsSubmittedByIsCached(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	reads := 0
	b := newTailBeads(nil, tailTitleCacheSize)
	b.now = func() time.Time { return clock }
	b.comments = func(string, string) ([]beads.Comment, error) {
		reads++
		return []beads.Comment{{Author: "town/crew/overseer", Text: "Submitted for landing: crew/ops/gt-1 @ 2222 onto main"}}, nil
	}
	for i := 0; i < 2; i++ {
		if got := b.submittedBy("gastown", "gt-1"); got != "town/crew/overseer" {
			t.Fatalf("ask %d: submittedBy = %q, want the submitter", i, got)
		}
	}
	if reads != 1 {
		t.Fatalf("read %d times inside the window, want once", reads)
	}
	clock = clock.Add(tailSubmitterTTL)
	if got := b.submittedBy("gastown", "gt-1"); got != "town/crew/overseer" {
		t.Fatalf("submittedBy after the window = %q, want the submitter", got)
	}
	if reads != 2 {
		t.Fatalf("read %d times after the window, want a second read", reads)
	}
}

// TestTailBeadsSubmittedByFailureIsNotCached: a read that failed answers the
// unreadable mark and is retried on the next ask, so a transient failure does
// not hold the page's "?" for the whole window (gt-f1fc2f).
func TestTailBeadsSubmittedByFailureIsNotCached(t *testing.T) {
	t.Parallel()
	failing := true
	reads := 0
	b := newTailBeads(nil, tailTitleCacheSize)
	b.comments = func(string, string) ([]beads.Comment, error) {
		reads++
		if failing {
			return nil, fmt.Errorf("bd failed")
		}
		return []beads.Comment{{Author: "town/crew/overseer", Text: "Submitted for landing: crew/ops/gt-1 @ 2222 onto main"}}, nil
	}
	if got := b.submittedBy("gastown", "gt-1"); got != submitterUnknown {
		t.Fatalf("submittedBy on a failed read = %q, want %q", got, submitterUnknown)
	}
	failing = false
	if got := b.submittedBy("gastown", "gt-1"); got != "town/crew/overseer" {
		t.Fatalf("submittedBy after the read recovers = %q, want the submitter", got)
	}
	if reads != 2 {
		t.Fatalf("read %d times, want the failed read retried", reads)
	}
}

// fakeCommentStore is a rig's bead store for the production comments path: it
// records the reads the real store would send to bd.
type fakeCommentStore struct {
	comments []beads.Comment
	err      error
	shows    int
	comment  int

	sampleRows []beads.Sample
	sampleErr  error
	kind       string
	limit      int
}

func (s *fakeCommentStore) Show(id string) (*beads.Issue, error) {
	s.shows++
	return &beads.Issue{ID: id, Title: "title of " + id}, nil
}

func (s *fakeCommentStore) Comments(string) ([]beads.Comment, error) {
	s.comment++
	return s.comments, s.err
}

func (s *fakeCommentStore) ReadCostSamples(_ context.Context, kind string, _ time.Time, limit int) ([]beads.Sample, error) {
	s.kind, s.limit = kind, limit
	return s.sampleRows, s.sampleErr
}

// TestTailBeadsFromStoreReadsComments: the production reader's comments
// closure asks the same store the titles come from and returns its read, so a
// landing's submitter is read through the wiring the dashboard uses
// (gt-f1fc2f). A store that cannot be opened answers the same way a failed
// comments read does.
func TestTailBeadsFromStoreReadsComments(t *testing.T) {
	t.Parallel()
	store := &fakeCommentStore{comments: []beads.Comment{
		{Author: "town/crew/overseer", Text: "Submitted for landing: crew/ops/gt-1 @ 2222 onto main"},
	}}
	var opened []string
	b := newTailBeadsFromStore(func(rig string) (tailStore, error) {
		opened = append(opened, rig)
		return store, nil
	}, tailTitleCacheSize)
	if got := b.title("gastown", "gt-1"); got != "title of gt-1" {
		t.Fatalf("title = %q, want the store's title", got)
	}
	if got := b.submittedBy("gastown", "gt-1"); got != "town/crew/overseer" {
		t.Fatalf("submittedBy = %q, want the store's submitter", got)
	}
	if store.shows != 1 || store.comment != 1 {
		t.Fatalf("store reads = %d shows, %d comments, want one of each", store.shows, store.comment)
	}
	if len(opened) != 2 || opened[0] != "gastown" || opened[1] != "gastown" {
		t.Fatalf("opened %v, want the same rig's store for both reads", opened)
	}

	b = newTailBeadsFromStore(func(string) (tailStore, error) { return nil, fmt.Errorf("no store") }, tailTitleCacheSize)
	if got := b.submittedBy("gastown", "gt-1"); got != submitterUnknown {
		t.Fatalf("submittedBy with no store = %q, want %q", got, submitterUnknown)
	}
}

// TestTailBeadsReadsCostSamplesThroughTheTownStore: the plan meter's rows come
// through the store the hub's reader already opens, over the town's own
// database, and a reader with no store reports that rather than answering a
// town with no rows (gt-hutsr6).
func TestTailBeadsReadsCostSamplesThroughTheTownStore(t *testing.T) {
	t.Parallel()

	store := &fakeCommentStore{sampleRows: []beads.Sample{{Kind: beads.SampleKindClaudePlan}}}
	var opened []string
	b := newTailBeadsFromStore(func(rig string) (tailStore, error) {
		opened = append(opened, rig)
		return store, nil
	}, tailTitleCacheSize)

	rows, err := b.costSamples(beads.SampleKindClaudePlan, time.Time{}, 7)
	if err != nil {
		t.Fatalf("costSamples: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("costSamples = %v, want the store's one row", rows)
	}
	if len(opened) != 1 || opened[0] != tailTownRig {
		t.Fatalf("opened %v, want the town's store", opened)
	}
	if store.kind != beads.SampleKindClaudePlan || store.limit != 7 {
		t.Errorf("the store was asked for kind %q with limit %d, want the caller's read", store.kind, store.limit)
	}

	var noStore *tailBeads
	if _, err := noStore.costSamples(beads.SampleKindClaudePlan, time.Time{}, 0); err == nil {
		t.Error("a reader with no store answered no error, want the missing store named")
	}
	unopenable := newTailBeadsFromStore(func(string) (tailStore, error) { return nil, fmt.Errorf("no store") }, tailTitleCacheSize)
	if _, err := unopenable.costSamples(beads.SampleKindClaudePlan, time.Time{}, 0); err == nil {
		t.Error("a store that cannot be opened answered no error, want the failed open named")
	}
}

// TestOpenTailBeadsWithoutATown: a reader with no town to resolve a rig's beads
// directory from names no title rather than reaching bd, and its comments read
// reports the missing store instead of an empty answer. A submitter read then
// answers the unreadable mark, never the operator (gt-f1fc2f).
func TestOpenTailBeadsWithoutATown(t *testing.T) {
	t.Parallel()
	b := openTailBeads("", tailTitleCacheSize)
	if got := b.title("gastown", "gt-1"); got != "" {
		t.Errorf("title = %q with no beads directory, want none", got)
	}
	if got := b.submittedBy("gastown", "gt-1"); got != submitterUnknown {
		t.Errorf("submittedBy = %q with no beads directory, want %q", got, submitterUnknown)
	}
	if _, err := b.comments("gastown", "gt-1"); err == nil {
		t.Error("comments read with no beads directory answered no error")
	}
}
