package townstatus

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/templates"
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

// respawnProbe is the live supervisor reading of a job launchd cannot start:
// it knows the job, has no process for it, and every spawn has died before the
// daemon wrote anything (gt-92qx3).
func respawnProbe(townRoot string) (templates.SupervisorState, bool) {
	return templates.SupervisorState{Kind: "launchd", Loaded: true, Runs: 49, LastExit: templates.LaunchdExitConfig}, true
}

// notRespawning is a supervisor that is running the daemon.
func notRespawning(townRoot string) (templates.SupervisorState, bool) {
	return templates.SupervisorState{Kind: "launchd", Loaded: true, PID: 64604, Runs: 53, LastExit: 0}, false
}

// The daemon that writes the health file is the one launchd cannot start, so
// the file is stale or absent in exactly this state: the live reading is the
// only evidence, and it has to reach the line as RED rather than UNKNOWN.
func TestHealthView_SupervisorRespawnLoop(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	t.Run("no health report", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		lines, v, rep := healthView(town, now, respawnProbe)

		if v != townhealth.Red || v.ExitCode() != 2 {
			t.Errorf("verdict %s exit %d, want red 2: %q", v, v.ExitCode(), lines)
		}
		if !strings.Contains(lines[0], "daemon=launchd_respawn_loop") {
			t.Errorf("line = %q, want the daemon field RED", lines[0])
		}
		if rep == nil || len(rep.Fields) != 1 || rep.Fields[0].Name != townhealth.FieldDaemon {
			t.Fatalf("report = %+v, want the one live daemon field", rep)
		}
		if d := rep.Fields[0].Detail; !strings.Contains(d, "bootout") || !strings.Contains(d, "bootstrap") {
			t.Errorf("detail = %q, want the repair named", d)
		}
	})

	t.Run("stale green report", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		landed := 3
		stale := townhealth.Report{At: now.Add(-time.Hour), Verdict: townhealth.Green, Landed: &landed}
		if err := townhealth.Write(town, stale); err != nil {
			t.Fatal(err)
		}
		lines, v, _ := healthView(town, now, respawnProbe)

		if v != townhealth.Red {
			t.Errorf("verdict %s, want red: %q", v, lines)
		}
		if strings.Contains(lines[0], "stale") {
			t.Errorf("line = %q, want the live reading rather than the stale report", lines[0])
		}
	})

	t.Run("fresh report keeps its fields", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		r := townhealth.Report{At: now.Add(-time.Minute), Verdict: townhealth.Green, Fields: []townhealth.Field{
			{Name: townhealth.FieldDolt, Tag: townhealth.Live, Verdict: townhealth.Green, Value: "p50 4ms"},
		}}
		if err := townhealth.Write(town, r); err != nil {
			t.Fatal(err)
		}
		lines, v, rep := healthView(town, now, respawnProbe)

		if v != townhealth.Red {
			t.Errorf("verdict %s, want red: %q", v, lines)
		}
		if rep == nil || len(rep.Fields) != 2 || rep.Fields[1].Name != townhealth.FieldDaemon {
			t.Errorf("report fields = %+v, want the dolt field kept and the daemon field replaced", rep)
		}
	})
}

// A supervisor that is not looping leaves the file-backed reading alone: the
// live probe is corroboration, never a second opinion that can mask the file.
func TestHealthView_SupervisorNotLooping(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	lines, v, rep := healthView(t.TempDir(), now, notRespawning)
	if v != townhealth.VerdictUnknown || rep != nil || !strings.HasPrefix(lines[0], "UNKNOWN no health report") {
		t.Fatalf("no report: %v %s %v, want UNKNOWN", lines, v, rep)
	}
}
