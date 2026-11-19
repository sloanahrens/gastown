package townhealth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// TestLineNamesALandingPause: the line says why no landing starts while the
// overseer's landing hold stands, and the report carries that reason from its
// inputs (gt-ckc93z).
func TestLineNamesALandingPause(t *testing.T) {
	t.Parallel()
	r := Report{At: ago(40 * time.Second), Verdict: Green, Landed: intp(7), LandingPaused: "overseer: drain the queue"}
	if got, want := Line(r, now, DefaultStaleAfter), "GREEN tick 40s ago (landings paused: overseer: drain the queue), 7 landed/24h"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}

	in := inputs(healthy())
	in.LandingPaused = "overseer: drain the queue"
	if got := Compute(context.Background(), in).LandingPaused; got != "overseer: drain the queue" {
		t.Errorf("Compute dropped the landing pause: report carries %q", got)
	}
}

// TestLandingPassesRoundTrip: the report carries the in-flight count from its
// inputs, writes it as landing_passes (zero included), and reads back as known.
func TestLandingPassesRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	in := inputs(healthy())
	in.LandingPasses = 3
	r := Compute(context.Background(), in)
	if r.LandingPasses != 3 || !r.PassesKnown {
		t.Fatalf("Compute carried passes %d known %v, want 3 known", r.LandingPasses, r.PassesKnown)
	}
	if err := Write(root, r); err != nil {
		t.Fatal(err)
	}
	if got := onDiskPasses(t, root); got == nil || *got != 3 {
		t.Errorf("file landing_passes = %v, want 3", got)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.LandingPasses != 3 || !got.PassesKnown {
		t.Errorf("read back passes %d known %v, want 3 known", got.LandingPasses, got.PassesKnown)
	}

	in.LandingPasses = 0
	if err := Write(root, Compute(context.Background(), in)); err != nil {
		t.Fatal(err)
	}
	if got := onDiskPasses(t, root); got == nil || *got != 0 {
		t.Errorf("file landing_passes = %v when no pass is in flight, want 0", got)
	}
}

// onDiskPasses is the landing_passes key of the health file, nil when the key
// is absent.
func onDiskPasses(t *testing.T, root string) *int {
	t.Helper()
	raw, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		LandingPasses *int `json:"landing_passes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.LandingPasses
}

// TestReadWithoutLandingPassesIsUnknown: a report an older daemon wrote has no
// landing_passes key, so it reads as passes unknown, not as zero passes.
func TestReadWithoutLandingPassesIsUnknown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeRaw := func(body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(Path(root)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(Path(root), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeRaw(`{"at":"2026-11-19T11:00:00Z","verdict":"green","fields":[]}`)
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.PassesKnown || got.LandingPasses != 0 {
		t.Errorf("older report read passes %d known %v, want unknown", got.LandingPasses, got.PassesKnown)
	}

	writeRaw(`{"at":"2026-11-19T11:00:00Z","verdict":"green","fields":[],"landing_passes":0}`)
	got, err = Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PassesKnown || got.LandingPasses != 0 {
		t.Errorf("report with landing_passes 0 read passes %d known %v, want known 0", got.LandingPasses, got.PassesKnown)
	}

	writeRaw(`{"at":"2026-11-19T11:00:00Z","verdict":"green","fields":[],"landing_passes":"two"}`)
	if _, err := Read(root); err == nil {
		t.Error("a landing_passes that is not a number read without error")
	}
}

// TestLandingPassesLeaveVerdictAndLineUnchanged: the count is shown nowhere on
// the line or in the verdict, so the same inputs judge and render the same
// whether one pass or none is in flight.
func TestLandingPassesLeaveVerdictAndLineUnchanged(t *testing.T) {
	t.Parallel()
	in := inputs(healthy())
	none := Compute(context.Background(), in)
	in.LandingPasses = 3
	some := Compute(context.Background(), in)

	if none.Verdict != some.Verdict {
		t.Errorf("verdict %s with passes, %s without", some.Verdict, none.Verdict)
	}
	if a, b := Line(none, now, DefaultStaleAfter), Line(some, now, DefaultStaleAfter); a != b {
		t.Errorf("line %q with passes, %q without", b, a)
	}
	if a, b := Lines(none, now, DefaultStaleAfter), Lines(some, now, DefaultStaleAfter); !slices.Equal(a, b) {
		t.Errorf("status lines differ with passes:\n%q\n%q", b, a)
	}

	red := Report{At: ago(40 * time.Second), Verdict: Red, Landed: intp(7), Fields: []Field{
		{Name: FieldMain, Rig: "gastown", Tag: Recorded, Verdict: Red, Value: "red"},
	}}
	before := Line(red, now, DefaultStaleAfter)
	red.LandingPasses = 3
	if got := Line(red, now, DefaultStaleAfter); got != before {
		t.Errorf("red line %q with passes, want %q", got, before)
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

// A report the daemon wrote stale-after ago or more is UNKNOWN whatever it
// recorded, green included, so a green main carried in an old report cannot keep
// the town green (gt-f701fm).
func TestReportAtOrPastStaleAfterIsUnknown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		age  time.Duration
		want Verdict
	}{
		{"just-fresh", DefaultStaleAfter - time.Second, Green},
		{"at-stale-after", DefaultStaleAfter, VerdictUnknown},
		{"well-past", DefaultStaleAfter + time.Hour, VerdictUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := Report{At: now.Add(-tt.age), Verdict: Green}
			if got := Effective(r, now, DefaultStaleAfter); got != tt.want {
				t.Errorf("Effective at age %s = %s, want %s", tt.age, got, tt.want)
			}
		})
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

// gt status renders a degraded promotion beside the rig's main line, keyed
// promote/<rig>, with the lag as its value and the wait in the detail line
// (gt-fn9e6.39).
func TestLinesRenderThePromotionField(t *testing.T) {
	t.Parallel()
	r := Report{At: ago(time.Minute), Verdict: Degraded, Fields: []Field{
		{Name: FieldPromote, Rig: "gastown", Tag: Recorded, Verdict: Degraded, Value: "3 behind", Detail: "green bbbb2222 unpromoted for 7h"},
	}}
	if got := Line(r, now, DefaultStaleAfter); got != "DEGRADED tick 1m ago: promote/gastown=3_behind[R]" {
		t.Errorf("Line = %q", got)
	}
	ls := Lines(r, now, DefaultStaleAfter)
	if len(ls) != 2 || !strings.Contains(ls[1], "promote/gastown") || !strings.Contains(ls[1], "green bbbb2222 unpromoted for 7h") {
		t.Errorf("Lines = %q, want the field and its detail", ls)
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
	th, stale, err = (&Settings{StaleAfter: "20m", BackupRed: "96h", DoltSamples: 5, TickRedFactor: 6, ExecTaxRed: "120ms", ExecTaxDegraded: "60ms"}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if stale != 20*time.Minute || th.Backup.Red != 96*time.Hour || th.Backup.Degraded != 36*time.Hour || th.DoltSamples != 5 || th.TickRedFactor != 6 {
		t.Errorf("resolved %+v stale %v", th, stale)
	}
	if th.ExecTax.Red != 120*time.Millisecond || th.ExecTax.Degraded != 60*time.Millisecond {
		t.Errorf("ExecTax = %+v, want the configured 60ms/120ms (default %+v)", th.ExecTax, DefaultThresholds().ExecTax)
	}
	for _, bad := range []Settings{{BackupRed: "soon"}, {SeatEvidence: "-1m"}, {StaleAfter: "0s"}, {DoltSamples: -1}, {ExecTaxRed: "50"}} {
		if _, _, err := bad.Resolve(); err == nil {
			t.Errorf("Resolve(%+v) succeeded, want an error", bad)
		}
	}
}

// DefaultSettings is the canonical serialization of the compiled defaults:
// resolving it returns exactly DefaultThresholds and DefaultStaleAfter, so
// migrate writes a self-consistent block, not a second copy of the defaults.
func TestDefaultSettingsResolveToDefaults(t *testing.T) {
	t.Parallel()
	s := DefaultSettings()
	th, stale, err := s.Resolve()
	if err != nil {
		t.Fatalf("Resolve(DefaultSettings()) = %v", err)
	}
	if stale != DefaultStaleAfter || th != DefaultThresholds() {
		t.Errorf("Resolve(DefaultSettings()) = %+v %v, want %+v %v", th, stale, DefaultThresholds(), DefaultStaleAfter)
	}
	if s.NotifyCommand != "" {
		t.Errorf("NotifyCommand = %q, want empty (no default pager)", s.NotifyCommand)
	}
}
