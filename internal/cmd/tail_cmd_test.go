package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// scriptedTailSource returns one scripted batch per poll, then nothing.
type scriptedTailSource struct {
	batches [][]tailLine
	polls   int
}

func (s *scriptedTailSource) Poll() []tailLine {
	s.polls++
	if len(s.batches) == 0 {
		return nil
	}
	b := s.batches[0]
	s.batches = s.batches[1:]
	return b
}

func TestRunTailStream_GoldenMergedStream(t *testing.T) {
	ev := func(ts, rig, text string) tailLine {
		return tailLine{At: at(ts), Rig: rig, Kind: tailKindEvents, Text: text}
	}
	gastownEvents := &scriptedTailSource{batches: [][]tailLine{{
		ev("2026-09-30T14:00:00Z", "gastown", "journal off in config (events-journal=false): only mutations made through gt are journaled"),
		ev("2026-09-30T13:50:00Z", "gastown", "update gt-1 status=in_progress actor=gastown/polecats/opal seq=2"),
		ev("2026-09-30T13:52:00Z", "gastown", "close gt-1 status=closed actor=gastown/polecats/opal seq=3"),
	}}}
	gastownLandings := &scriptedTailSource{batches: [][]tailLine{{
		{At: at("2026-09-30T13:55:00Z"), Rig: "gastown", Kind: tailKindLandings, Text: "landed gt-1 polecat/opal/gt-1 -> main commit=33333333 patch=44444444 gate=pass om=approve/0.92 route=worker"},
	}}}
	hqEvents := &scriptedTailSource{batches: [][]tailLine{{ev("2026-09-30T13:58:00Z", "hq", "create hq-9 status=open actor=mayor seq=41")}}}
	hmEvents := &scriptedTailSource{batches: [][]tailLine{{ev("2026-09-30T14:00:00Z", "hm", "read failed: bd events tail: exit status 25")}}}
	daemon := &scriptedTailSource{batches: [][]tailLine{{
		{At: at("2026-09-30T13:51:00Z"), Rig: "town", Kind: tailKindDaemon, Text: "Convoy: close detected: gt-1 (from gastown)"},
		{At: at("2026-09-30T13:51:00Z"), Rig: "town", Kind: tailKindDaemon, Text: "  continuation of the convoy line"},
	}}}

	var buf bytes.Buffer
	err := runTailStream(context.Background(), &buf, []tailSource{gastownEvents, gastownLandings, hmEvents, hqEvents, daemon}, nil, false, nil, tailTestLoc)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "tail_golden.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(golden) {
		t.Fatalf("stream differs from testdata/tail_golden.txt:\n%s", buf.String())
	}
	if gastownEvents.polls != 1 || daemon.polls != 1 {
		t.Fatalf("without --follow each source polls once; got %d, %d", gastownEvents.polls, daemon.polls)
	}
}

func TestRunTailStream_FollowPrintsEachBatchUntilCancelled(t *testing.T) {
	src := &scriptedTailSource{batches: [][]tailLine{
		{{At: at("2026-09-30T14:00:00Z"), Rig: "gastown", Kind: tailKindEvents, Text: "first"}},
		{{At: at("2026-09-30T14:00:05Z"), Rig: "gastown", Kind: tailKindEvents, Text: "second"}},
	}}
	preface := []tailLine{{At: at("2026-09-30T13:00:00Z"), Rig: "town", Kind: "tail", Text: "preface"}}
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- runTailStream(ctx, &buf, []tailSource{src}, preface, true, tick, tailTestLoc) }()
	tick <- time.Time{}
	tick <- time.Time{} // an empty poll prints nothing
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("follow returned %v", err)
	}
	want := "2026-09-30T08:00:00-05:00 town tail preface\n" +
		"2026-09-30T09:00:00-05:00 gastown events first\n" +
		"2026-09-30T09:00:05-05:00 gastown events second\n"
	if buf.String() != want {
		t.Fatalf("follow output:\n%s", buf.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestRunTailStream_WriteFailureStops(t *testing.T) {
	src := &scriptedTailSource{batches: [][]tailLine{{{At: tailNow, Rig: "x", Kind: "events", Text: "y"}}}}
	if err := runTailStream(context.Background(), failingWriter{}, []tailSource{src}, nil, true, make(chan time.Time), tailTestLoc); err == nil {
		t.Fatal("a failed write did not stop the stream")
	}
}

// fakeTailTown lays out a town with landings files and a daemon log, and
// swaps the rig registry and the journal opener for fakes.
func fakeTailTown(t *testing.T, rigs []string, regErr error) (string, map[string]*fakeTailJournal) {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, ".runtime", "landings"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, rig := range rigs {
		appendFile(t, filepath.Join(town, ".runtime", "landings", rig+".jsonl"),
			`{"bead":"`+rig+`-1","rig":"`+rig+`","landed_at":"2026-09-30T13:59:00Z"}`+"\n")
	}
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(town, "daemon", "daemon.log"),
		"2026/09/30 08:59:10 Convoy: close detected: gt-1 (from gastown)\n2026/09/30 08:59:20 hm witness restarted\n")

	journals := map[string]*fakeTailJournal{}
	origRigs, origJournal := tailRigNames, tailJournalFor
	t.Cleanup(func() { tailRigNames, tailJournalFor = origRigs, origJournal })
	tailRigNames = func(string) ([]string, error) { return rigs, regErr }
	tailJournalFor = func(_ string, rig string) (tailJournal, error) {
		j := &fakeTailJournal{config: "true", records: []beads.EventRecord{{Seq: 1, TS: "2026-09-30T13:58:00Z", Op: "create", IssueID: rig + "-e", Actor: "a", Status: "open"}}}
		journals[rig] = j
		return j, nil
	}
	return town, journals
}

func TestBuildTailSources_AllRigsAllKinds(t *testing.T) {
	town, journals := fakeTailTown(t, []string{"gastown", "hm"}, nil)
	sources, preface, err := buildTailSources(tailOptions{townRoot: town, kinds: allTailKinds(), cutoff: at("2026-09-30T13:00:00Z"), loc: tailTestLoc, now: fixedNow})
	if err != nil || len(preface) != 0 {
		t.Fatalf("build: %v %v", err, preface)
	}
	var buf bytes.Buffer
	if err := runTailStream(context.Background(), &buf, sources, preface, false, nil, tailTestLoc); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2026-09-30T08:58:00-05:00 hq events create hq-e status=open actor=a seq=1",
		"2026-09-30T08:58:00-05:00 gastown events create gastown-e status=open actor=a seq=1",
		"2026-09-30T08:58:00-05:00 hm events create hm-e status=open actor=a seq=1",
		"2026-09-30T08:59:00-05:00 gastown landings landed gastown-1 - -> - commit=- patch=- gate=- om=-/0.00 route=-",
		"2026-09-30T08:59:00-05:00 hm landings landed hm-1 - -> - commit=- patch=- gate=- om=-/0.00 route=-",
		"2026-09-30T08:59:10-05:00 town daemon Convoy: close detected: gt-1 (from gastown)",
		"2026-09-30T08:59:20-05:00 town daemon hm witness restarted",
	}
	if got := strings.Split(strings.TrimSpace(buf.String()), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("stream:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var opened []string
	for rig := range journals {
		opened = append(opened, rig)
	}
	sort.Strings(opened)
	if !reflect.DeepEqual(opened, []string{"gastown", "hm", "hq"}) {
		t.Fatalf("journals opened for %v", opened)
	}
}

func TestBuildTailSources_RigAndKindFilters(t *testing.T) {
	town, journals := fakeTailTown(t, []string{"gastown", "hm"}, nil)
	sources, _, err := buildTailSources(tailOptions{townRoot: town, rig: "hm", kinds: map[string]bool{tailKindLandings: true, tailKindDaemon: true}, cutoff: at("2026-09-30T13:00:00Z"), loc: tailTestLoc, now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := runTailStream(context.Background(), &buf, sources, nil, false, nil, tailTestLoc); err != nil {
		t.Fatal(err)
	}
	want := "2026-09-30T08:59:00-05:00 hm landings landed hm-1 - -> - commit=- patch=- gate=- om=-/0.00 route=-\n" +
		"2026-09-30T08:59:20-05:00 town daemon hm witness restarted\n"
	if buf.String() != want {
		t.Fatalf("filtered stream:\n%s", buf.String())
	}
	if len(journals) != 0 {
		t.Fatalf("--kind without events still opened journals: %v", journals)
	}
}

func TestBuildTailSources_UnknownRigIsRefused(t *testing.T) {
	town, _ := fakeTailTown(t, []string{"gastown"}, nil)
	if _, _, err := buildTailSources(tailOptions{townRoot: town, rig: "nope", kinds: allTailKinds(), loc: tailTestLoc, now: fixedNow}); err == nil {
		t.Fatal("--rig nope accepted")
	}
}

func TestBuildTailSources_UnreadableRegistryKeepsHQAndDaemon(t *testing.T) {
	town, journals := fakeTailTown(t, nil, errors.New("rigs.json: no such file"))
	sources, preface, err := buildTailSources(tailOptions{townRoot: town, kinds: allTailKinds(), cutoff: at("2026-09-30T13:00:00Z"), loc: tailTestLoc, now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(preface) != 1 || preface[0].Text != "cannot read the rig registry (rigs.json: no such file): showing hq and the daemon only" || preface[0].Rig != "town" {
		t.Fatalf("preface = %+v", preface)
	}
	if len(sources) != 2 || len(journals) != 1 || journals["hq"] == nil {
		t.Fatalf("sources %d journals %v", len(sources), journals)
	}
}
