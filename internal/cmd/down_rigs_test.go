package cmd

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// downTestTown writes the town the gt down tests tear down: rigs.json holding
// rigsJSON (empty for no file at all), a .dolt-data directory so the Dolt
// phase is reached, and a .beads directory in each named rig so a town whose
// registry will not load still has rig names to report. The town has no Dolt
// endpoint, so every port-scoped sweep in gt down resolves to port 0 and
// reaches no server outside the town; the services that signal a process come
// from the caller.
func downTestTown(t *testing.T, rigsJSON string, rigNames ...string) string {
	t.Helper()
	town := t.TempDir()
	if rigsJSON != "" {
		rigsPath := filepath.Join(town, "mayor", "rigs.json")
		if err := os.MkdirAll(filepath.Dir(rigsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rigsPath, []byte(rigsJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dirs := []string{".dolt-data"}
	for _, rigName := range rigNames {
		dirs = append(dirs, filepath.Join(rigName, ".beads"))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(town, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

// downServicesRecorder is downServices for tests: it records the stops gt down
// makes and never signals a process or reads the process table. The daemon and
// Dolt report themselves running, so a shutdown that reaches its town-level
// teardown stops both.
type downServicesRecorder struct {
	stopped []string
}

func (r *downServicesRecorder) services() downServices {
	return downServices{
		daemonRunning:    func(string) (bool, int, error) { return true, 4242, nil },
		stopDaemon:       func(string) error { r.stopped = append(r.stopped, "daemon"); return nil },
		doltRunning:      func(string) (bool, int, error) { return true, 4243, nil },
		stopDolt:         func(string) error { r.stopped = append(r.stopped, "dolt"); return nil },
		killImposters:    func(string) error { return nil },
		findIdleMonitors: func(string) []int { return nil },
		findOrphans:      func(string) []int { return nil },
	}
}

// stops is what gt down stopped, sorted so the comparison does not depend on
// the order the phases run in.
func (r *downServicesRecorder) stops() []string {
	out := append([]string(nil), r.stopped...)
	sort.Strings(out)
	return out
}

// TestDownStopsTownServicesWithUnreadableRigsJSON: a rigs.json gt down cannot
// read no longer leaves the daemon and Dolt running. The registry is only what
// names a rig's sessions, so the shutdown reports the rigs whose agents it did
// not stop and the registry error, and still exits non-zero (gt-sjtps; the
// fail-closed-before-teardown behavior was gt-52mgl).
//
// The town-level services are the part of a shutdown the unit tier can drive:
// the session phases need tmux, which the unit tier starts none of
// (docs/testing.md), and they are the phases a corrupted registry already
// skips.
func TestDownStopsTownServicesWithUnreadableRigsJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		rigsJSON string
		rigs     []string
		wantRigs string
	}{
		{name: "truncated", rigsJSON: `{"version":1,"rigs":{`, rigs: []string{"gastown"}, wantRigs: "per-rig agents of gastown"},
		{name: "unknown key", rigsJSON: `{"version":1,"rigs":{},"rigz":{}}`, rigs: []string{"beads", "gastown"}, wantRigs: "per-rig agents of beads, gastown"},
		{name: "no rig directory", rigsJSON: `{"version":1,"rigs":{`, wantRigs: "the per-rig agents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := downTestTown(t, tc.rigsJSON, tc.rigs...)
			rec := &downServicesRecorder{}

			err := runDownWith(town, rec.services())

			if err == nil {
				t.Fatal("runDownIn with an unreadable rigs.json = nil, want a non-zero exit")
			}
			if got, want := strings.Join(rec.stops(), ","), "daemon,dolt"; got != want {
				t.Errorf("stopped %q, want %q: these services do not read the rig registry", got, want)
			}
			if !strings.Contains(err.Error(), "rigs config") {
				t.Errorf("error = %v, want it to name the rigs config", err)
			}
			if !strings.Contains(err.Error(), tc.wantRigs) {
				t.Errorf("error = %v, want it to name the agents left running (%q)", err, tc.wantRigs)
			}
		})
	}
}

// TestDownStopsTownServicesWithReadableRigsJSON: a registry that loads reaches
// the same town-level teardown — the daemon and Dolt stop either way, which is
// what lets the phases above be skipped for a town whose registry will not
// load (gt-sjtps).
func TestDownStopsTownServicesWithReadableRigsJSON(t *testing.T) {
	t.Parallel()
	town := downTestTown(t, `{"version":1,"rigs":{"gastown":{"git_url":"file:///tmp/gastown","added_at":"2026-01-01T00:00:00Z"}}}`, "gastown")
	rec := &downServicesRecorder{}

	if !stopTownServices(town, rec.services(), false) {
		t.Error("stopTownServices with a readable registry reported a failure")
	}
	if got, want := strings.Join(rec.stops(), ","), "daemon,dolt"; got != want {
		t.Errorf("stopped %q, want %q", got, want)
	}
}
