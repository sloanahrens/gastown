package townhealth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func intp(n int) *int { return &n }

func TestWriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	r := Compute(context.Background(), inputs(healthy()))
	if err := Write(root, r); err != nil {
		t.Fatal(err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if !got.At.Equal(r.At) || got.Verdict != r.Verdict || len(got.Fields) != len(r.Fields) || *got.Landed != *r.Landed {
		t.Errorf("read back %+v, want %+v", got, r)
	}
	if _, err := Read(t.TempDir()); err == nil {
		t.Error("Read of a town with no file succeeded")
	}
}

func TestLineGreen(t *testing.T) {
	t.Parallel()
	r := Report{At: ago(40 * time.Second), Verdict: Green, Landed: intp(7)}
	if got, want := Line(r, now, DefaultStaleAfter), "GREEN tick 40s ago, 7 landed/24h"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
	r.Landed = nil
	if got := Line(r, now, DefaultStaleAfter); !strings.HasSuffix(got, "? landed/24h") {
		t.Errorf("Line = %q, want an uncounted day shown as ?", got)
	}
}

func TestLineStaleIsUnknown(t *testing.T) {
	t.Parallel()
	r := Report{At: ago(15 * time.Minute), Verdict: Green, Landed: intp(1)}
	if got, want := Line(r, now, DefaultStaleAfter), "UNKNOWN tick 15m ago (stale; last green)"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
	if v := Effective(r, now, DefaultStaleAfter); v.ExitCode() != 3 {
		t.Errorf("stale verdict %s exits %d, want 3", v, v.ExitCode())
	}
	if v := Effective(r, now, time.Hour); v != Green {
		t.Errorf("fresh verdict = %s, want green", v)
	}
}

func TestLineNonGreenFieldsWorstFirstWithTags(t *testing.T) {
	t.Parallel()
	r := Report{At: ago(time.Minute), Verdict: VerdictUnknown, Fields: []Field{
		{Name: FieldDolt, Tag: Live, Verdict: Green, Value: "p50 9ms"},
		{Name: FieldTick, Subject: "wisp_reaper", Tag: Recorded, Verdict: Degraded, Value: "70m/30m"},
		{Name: FieldEscalation, Tag: Unknown, Verdict: VerdictUnknown, Value: "?"},
		{Name: FieldMain, Rig: "gastown", Tag: Recorded, Verdict: Red, Value: "red"},
		{Name: FieldDolt, Tag: Live, Verdict: Red, Value: "unreachable"},
	}}
	want := "UNKNOWN tick 1m ago: escalation[?] main/gastown=red[R] dolt=unreachable tick:wisp_reaper=70m/30m[R]"
	if got := Line(r, now, DefaultStaleAfter); got != want {
		t.Errorf("Line =\n %q\nwant\n %q", got, want)
	}
}

func TestLineFitsAndCountsTheRest(t *testing.T) {
	t.Parallel()
	r := Report{At: ago(time.Minute), Verdict: Degraded}
	for i := 0; i < 20; i++ {
		r.Fields = append(r.Fields, Field{Name: FieldSeat, Rig: "gastown", Subject: "polecat/p" + strings.Repeat("x", i%3), Tag: Recorded, Verdict: Degraded, Value: "stalled 45m"})
	}
	got := Line(r, now, DefaultStaleAfter)
	if len(got) > LineMax {
		t.Errorf("Line is %d chars, want <= %d: %q", len(got), LineMax, got)
	}
	if !strings.Contains(got, " +") || !strings.Contains(got, "seat/gastown:polecat/p=stalled_45m[R]") {
		t.Errorf("Line = %q, want the first fields and a +N remainder", got)
	}
}

func TestLinesPrintsEveryField(t *testing.T) {
	t.Parallel()
	r := Compute(context.Background(), inputs(healthy()))
	ls := Lines(r, now, DefaultStaleAfter)
	if len(ls) != len(r.Fields)+1 {
		t.Fatalf("got %d lines for %d fields", len(ls), len(r.Fields))
	}
	if !strings.Contains(ls[1], "dolt") || !strings.Contains(ls[1], "LIVE") {
		t.Errorf("first field line = %q", ls[1])
	}
}

func TestSettingsResolve(t *testing.T) {
	t.Parallel()
	th, stale, err := (*Settings)(nil).Resolve()
	if err != nil || stale != DefaultStaleAfter || th != DefaultThresholds() {
		t.Fatalf("nil settings = %+v %v %v, want defaults", th, stale, err)
	}
	th, stale, err = (&Settings{StaleAfter: "20m", BackupRed: "96h", DoltSamples: 5, TickRedFactor: 6}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if stale != 20*time.Minute || th.Backup.Red != 96*time.Hour || th.Backup.Degraded != 36*time.Hour || th.DoltSamples != 5 || th.TickRedFactor != 6 {
		t.Errorf("resolved %+v stale %v", th, stale)
	}
	for _, bad := range []Settings{{BackupRed: "soon"}, {SeatEvidence: "-1m"}, {StaleAfter: "0s"}, {DoltSamples: -1}} {
		if _, _, err := bad.Resolve(); err == nil {
			t.Errorf("Resolve(%+v) succeeded, want an error", bad)
		}
	}
}
