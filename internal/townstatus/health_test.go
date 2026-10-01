package townstatus

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townhealth"
)

func TestHealthView(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()

	lines, v, rep := HealthView(town, now)
	if v != townhealth.VerdictUnknown || rep != nil || !strings.HasPrefix(lines[0], "UNKNOWN no health report") {
		t.Fatalf("no report: %v %s %v, want UNKNOWN", lines, v, rep)
	}

	landed := 3
	r := townhealth.Report{At: now.Add(-time.Minute), Verdict: townhealth.Red, Landed: &landed, Fields: []townhealth.Field{
		{Name: townhealth.FieldDolt, Tag: townhealth.Live, Verdict: townhealth.Green, Value: "p50 4ms"},
		{Name: townhealth.FieldMain, Rig: "gastown", Tag: townhealth.Recorded, Verdict: townhealth.Red, Value: "red", Detail: "main red at bbbb"},
	}}
	if err := townhealth.Write(town, r); err != nil {
		t.Fatal(err)
	}
	lines, v, rep = HealthView(town, now)
	if v != townhealth.Red || v.ExitCode() != 2 || rep == nil {
		t.Errorf("verdict %s exit %d, want red 2", v, v.ExitCode())
	}
	if want := "RED tick 1m ago: main/gastown=red[R]"; lines[0] != want {
		t.Errorf("line = %q, want %q", lines[0], want)
	}
	if len(lines) != 3 || !strings.Contains(lines[2], "main red at bbbb") {
		t.Errorf("every field: %q", lines)
	}

	lines, v, _ = HealthView(town, now.Add(time.Hour))
	if v != townhealth.VerdictUnknown || !strings.Contains(lines[0], "stale; last red") {
		t.Errorf("an hour later: %q %s, want stale UNKNOWN", lines[0], v)
	}
}
