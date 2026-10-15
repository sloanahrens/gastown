package overseer

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func TestOverseerJournalRefusesAnUnknownKindAndListsTheAllowedOnes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	err := AppendEntry(town, Entry{TS: t0, Kind: "tinker", Text: "did a thing"})
	if err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	for _, k := range Kinds() {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("the refusal %q does not list %q", err, k)
		}
	}
	if _, statErr := os.Stat(JournalPath(town)); statErr == nil {
		t.Error("a refused line created the journal")
	}
}

func TestOverseerJournalLineLandsAsOneJSONLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	if err := AppendEntry(town, Entry{TS: t0, Kind: KindFileBead, Text: "filed the\nfollow-up\t for   X", Bead: " gt-abc "}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(JournalPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\n") != 1 || !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("journal is not exactly one line: %q", raw)
	}
	got, err := ReadJournal(town)
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadJournal = %v, %v", got, err)
	}
	want := Entry{TS: t0, Kind: KindFileBead, Text: "filed the follow-up for X", Bead: "gt-abc"}
	if got[0] != want {
		t.Errorf("entry = %+v, want %+v", got[0], want)
	}
}

func TestOverseerJournalCutsLongText(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	long := strings.Repeat("é", MaxTextRunes+50)
	if err := AppendEntry(town, Entry{TS: t0, Kind: KindDecision, Text: long}); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadJournal(town)
	if n := len([]rune(got[0].Text)); n != MaxTextRunes {
		t.Errorf("text has %d runes, want %d", n, MaxTextRunes)
	}
}

func TestOverseerJournalRefusesEmptyText(t *testing.T) {
	t.Parallel()
	if err := AppendEntry(t.TempDir(), Entry{TS: t0, Kind: KindReview, Text: " \n "}); err == nil {
		t.Fatal("empty text was accepted")
	}
}

// Concurrent writers each land a whole line: the journal is appended from
// several gt processes at once.
func TestOverseerJournalConcurrentAppendsStayWholeLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := AppendEntry(town, Entry{TS: t0, Kind: KindAnswer, Text: strings.Repeat("x", 200)}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, err := ReadJournal(town)
	if err != nil || len(got) != n {
		t.Fatalf("read %d entries, err %v; want %d", len(got), err, n)
	}
}

// The journal rotates like the events log: lines past the age bound go from the
// front when a later line is appended.
func TestOverseerJournalPrunesOldLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	old := t0.Add(-JournalMaxAge - time.Hour)
	if err := AppendEntry(town, Entry{TS: old, Kind: KindHold, Text: "ancient"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendEntry(town, Entry{TS: t0, Kind: KindHold, Text: "fresh"}); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadJournal(town)
	if len(got) != 1 || got[0].Text != "fresh" {
		t.Errorf("journal after prune = %+v, want only the fresh line", got)
	}
}

func TestOverseerReadJournalSkipsGarbageAndAMissingFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	if got, err := ReadJournal(town); err != nil || got != nil {
		t.Fatalf("missing journal = %v, %v", got, err)
	}
	if err := os.MkdirAll(Dir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "not json\n" +
		`{"ts":"2026-10-15T12:00:00Z","kind":"review","text":"ok"}` + "\n" +
		`{"ts":"2026-10-15T12:00:00Z","text":"no kind"}` + "\n" +
		`{"ts":"2026-10-15T12:00:01Z","kind":"hold","te`
	if err := os.WriteFile(JournalPath(town), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadJournal(town)
	if err != nil || len(got) != 1 || got[0].Text != "ok" {
		t.Errorf("ReadJournal = %+v, %v; want the one good line", got, err)
	}
}

// Interventions by kind over seven days, from a journal with old and new lines.
func TestOverseerInterventionsCountOnlyTheWindow(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	entries := []Entry{
		{TS: t0.Add(-8 * day), Kind: KindHold, Text: "too old"},
		{TS: t0.Add(-7 * day), Kind: KindHold, Text: "on the edge: not counted"},
		{TS: t0.Add(-6 * day), Kind: KindHold, Text: "in"},
		{TS: t0.Add(-3 * day), Kind: KindAnswer, Text: "in"},
		{TS: t0.Add(-2 * day), Kind: KindAnswer, Text: "in"},
		{TS: t0.Add(-1 * day), Kind: KindAnswer, Text: "in"},
		{TS: t0.Add(-time.Hour), Kind: KindFixByHand, Text: "in"},
		{TS: t0.Add(-time.Minute), Kind: KindHold, Text: "in"},
	}
	got := CountByKind(entries, t0, 7*day)
	want := []KindCount{{KindAnswer, 3}, {KindHold, 2}, {KindFixByHand, 1}}
	if len(got) != len(want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("counts[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestOverseerRecentIsNewestFirstAndCapped(t *testing.T) {
	t.Parallel()
	var entries []Entry
	for i := range 12 {
		entries = append(entries, Entry{TS: t0.Add(time.Duration(i) * time.Minute), Kind: KindReview, Text: string(rune('a' + i))})
	}
	got := Recent(entries, 8)
	if len(got) != 8 || got[0].Text != "l" || got[7].Text != "e" {
		t.Errorf("Recent = %+v", got)
	}
	if len(Recent(entries[:3], 8)) != 3 {
		t.Error("Recent padded a short journal")
	}
}

func TestOverseerHeartbeatRoundTripAndStates(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	if _, have, err := ReadHeartbeat(town); have || err != nil {
		t.Fatalf("no heartbeat = have %v, err %v", have, err)
	}
	if err := WriteHeartbeat(town, Heartbeat{TS: t0, State: "napping"}); err == nil {
		t.Error("an unknown state was written")
	}
	want := Heartbeat{TS: t0, State: StateWorking, Session: "overseer-1"}
	if err := WriteHeartbeat(town, want); err != nil {
		t.Fatal(err)
	}
	got, have, err := ReadHeartbeat(town)
	if err != nil || !have || got != want {
		t.Errorf("ReadHeartbeat = %+v, %v, %v; want %+v", got, have, err, want)
	}
}

// A corrupt heartbeat is an error, never a silent overseer.
func TestOverseerCorruptHeartbeatIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(Dir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HeartbeatPath(town), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadHeartbeat(town); err == nil {
		t.Fatal("a corrupt heartbeat read as fine")
	}
}

func TestOverseerSilent(t *testing.T) {
	t.Parallel()
	after := 90 * time.Minute
	for _, tc := range []struct {
		name string
		hb   Heartbeat
		have bool
		want bool
	}{
		{"fresh", Heartbeat{TS: t0.Add(-time.Minute)}, true, false},
		{"just inside", Heartbeat{TS: t0.Add(-after)}, true, false},
		{"just past", Heartbeat{TS: t0.Add(-after - time.Second)}, true, true},
		{"never beat", Heartbeat{}, false, true},
		{"clock ahead", Heartbeat{TS: t0.Add(time.Hour)}, true, false},
	} {
		if got := Silent(tc.hb, tc.have, t0, after); got != tc.want {
			t.Errorf("%s: Silent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOverseerSilentPushMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	if _, have, err := ReadSilentPush(town); have || err != nil {
		t.Fatalf("no marker = have %v, err %v", have, err)
	}
	want := SilentPush{Heartbeat: t0.Add(-2 * time.Hour), PushedAt: t0}
	if err := WriteSilentPush(town, want); err != nil {
		t.Fatal(err)
	}
	got, have, err := ReadSilentPush(town)
	if err != nil || !have || !got.Heartbeat.Equal(want.Heartbeat) || !got.PushedAt.Equal(want.PushedAt) {
		t.Errorf("ReadSilentPush = %+v, %v, %v", got, have, err)
	}
}
