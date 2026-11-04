package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"encoding/json"

	"github.com/steveyegge/gastown/internal/hostprobe"
	"github.com/steveyegge/gastown/internal/templates"
)

// writeHostProbe publishes a probe file under townRoot the way the host agent
// does, stamped at, with the given launchd reading.
func writeHostProbe(t *testing.T, townRoot string, at time.Time, launchd hostprobe.Field[hostprobe.Launchd]) {
	t.Helper()
	path := hostprobe.Path(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(hostprobe.Probes{V: hostprobe.SchemaVersion, At: at, Launchd: launchd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHostLaunchdFallback pins when the guard may learn the launchd job's state
// from the host agent's probe file: only when the job cannot be read here and
// the file holds a fresh reading that itself succeeded. Everything else stays
// unknown, which refuses a takeover.
func TestHostLaunchdFallback(t *testing.T) {
	t.Parallel()
	unreadable := templates.SupervisorState{Kind: "launchd", LastExit: -1, Err: errors.New("launchctl: executable file not found")}
	cases := []struct {
		name       string
		inner      templates.SupervisorState
		kind       string
		probe      *hostprobe.Field[hostprobe.Launchd] // nil: no probe file
		probeAge   time.Duration
		wantUnread bool // the answer still carries the inner read's error
		wantLoaded bool
		wantPID    int
	}{
		{name: "a readable job is returned as read and the file is not consulted",
			inner: templates.SupervisorState{Kind: "launchd", Loaded: true, PID: 77, LastExit: -1}, kind: "launchd",
			wantLoaded: true, wantPID: 77},
		{name: "unreadable job and a fresh reading of a job that is not loaded",
			inner: unreadable, kind: "launchd",
			probe: &hostprobe.Field[hostprobe.Launchd]{Value: hostprobe.Launchd{Loaded: false}}, probeAge: 30 * time.Second},
		{name: "unreadable job and a fresh reading of a running job",
			inner: unreadable, kind: "launchd",
			probe: &hostprobe.Field[hostprobe.Launchd]{Value: hostprobe.Launchd{Loaded: true, PID: 223}}, probeAge: 30 * time.Second,
			wantLoaded: true, wantPID: 223},
		{name: "unreadable job and a reading the agent could not take stays unknown",
			inner: unreadable, kind: "launchd",
			probe: &hostprobe.Field[hostprobe.Launchd]{Err: "launchd job read: boom"}, probeAge: 30 * time.Second, wantUnread: true},
		{name: "unreadable job and a reading never filled stays unknown",
			inner: unreadable, kind: "launchd",
			probe: &hostprobe.Field[hostprobe.Launchd]{Err: hostprobe.LaunchdNotCollected}, probeAge: 30 * time.Second, wantUnread: true},
		{name: "unreadable job and a reading older than the allowed age stays unknown",
			inner: unreadable, kind: "launchd",
			probe: &hostprobe.Field[hostprobe.Launchd]{Value: hostprobe.Launchd{Loaded: false}}, probeAge: hostLaunchdMaxAge + time.Second, wantUnread: true},
		{name: "unreadable job and no probe file stays unknown",
			inner: unreadable, kind: "launchd", wantUnread: true},
		{name: "another supervisor kind is never answered from the file",
			inner: templates.SupervisorState{Kind: "systemd", LastExit: -1, Err: errors.New("no systemctl")}, kind: "systemd",
			probe: &hostprobe.Field[hostprobe.Launchd]{Value: hostprobe.Launchd{Loaded: false}}, probeAge: 10 * time.Second, wantUnread: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
			if tc.probe != nil {
				writeHostProbe(t, town, now.Add(-tc.probeAge), *tc.probe)
			}
			read := hostLaunchdFallback(town, func() time.Time { return now }, jobAnswers(tc.inner))
			got := read(tc.kind)
			if tc.wantUnread {
				if got.Err == nil {
					t.Fatalf("got a known state %+v, want the inner read's error kept so the guard refuses", got)
				}
				return
			}
			if got.Err != nil || got.Loaded != tc.wantLoaded || got.PID != tc.wantPID {
				t.Errorf("got %+v, want Loaded=%v PID=%d and no error", got, tc.wantLoaded, tc.wantPID)
			}
		})
	}
}

// TestStartupOwnerProbeLaunchdFromHostFile pins the guard's end of the fix: a
// launchd owner that cannot be read here is judged stopped from a fresh agent
// reading of a job that is not loaded, and unknown without one.
func TestStartupOwnerProbeLaunchdFromHostFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	unreadable := jobAnswers(templates.SupervisorState{Kind: "launchd", LastExit: -1, Err: errors.New("no launchctl")})
	probe := startupOwnerProbe(nil, fixtureHostname("fixture-host"), hostLaunchdFallback(town, func() time.Time { return now }, unreadable))

	if running, known := probe(guardLaunchd); known {
		t.Fatalf("with no probe file got running=%v known=%v, want unknown", running, known)
	}
	writeHostProbe(t, town, now.Add(-20*time.Second), hostprobe.Field[hostprobe.Launchd]{Value: hostprobe.Launchd{Loaded: false}})
	if running, known := probe(guardLaunchd); running || !known {
		t.Errorf("with a fresh not-loaded reading got running=%v known=%v, want stopped and known", running, known)
	}
}
