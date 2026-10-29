package power

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// assertOffNotRecorded fails when Off left any power state behind.
func assertOffNotRecorded(t *testing.T, f *fake) {
	t.Helper()
	if data, err := os.ReadFile(StatePath(f.town)); !os.IsNotExist(err) {
		t.Errorf("a power state was recorded (read err %v): %q", err, data)
	}
}

func TestOffWhenComposeProbeFails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.failComposePs = true
	f.jobhost = true
	o, _, _ := f.optsWithOut()
	o.Yes = true
	o.WithDolt = true
	err := Off(context.Background(), o)
	if err == nil {
		t.Fatal("Off succeeded although the compose probe failed")
	}
	if code := codeOf(t, err); code != ExitNotHealthy {
		t.Errorf("exit code = %d, want %d", code, ExitNotHealthy)
	}
	if !f.has("docker compose stop") {
		t.Error("off skipped the compose stop after a failed probe")
	}
	if f.composeN != 0 {
		t.Errorf("compose stop did not take effect: compose=%d", f.composeN)
	}
	if !f.has("gt dolt stop") || f.dolt {
		t.Errorf("off returned before stopping Dolt after the blind stop: dolt=%v", f.dolt)
	}
	if !f.jobhostRan {
		t.Error("off returned before the jobhost hook after the blind stop")
	}
	assertOffNotRecorded(t, f)
}

func TestOffWhenComposeProbeAndStopFail(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.failComposePs = true
	f.failComposeSt = true
	o, _, _ := f.optsWithOut()
	o.Yes = true
	err := Off(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "docker compose stop") {
		t.Fatalf("error = %v, want the docker compose stop failure", err)
	}
	assertOffNotRecorded(t, f)
}

// trackerTown names 127.0.0.1:trackerPort, the port the fake answers the
// tracker's health probe on, as the town's tracker.
func (f *fake) trackerTown(t *testing.T) {
	t.Helper()
	f.writeTownJSON(t, fmt.Sprintf(`{"type":"town","version":1,"name":"test","tracker":{"host":"127.0.0.1","port":%d}}`, f.trackerPort))
}

// writeTownJSON writes mayor/town.json, the file ResolveTrackerEndpoint reads.
func (f *fake) writeTownJSON(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(f.town, "mayor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "town.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRoutes writes the town's route table.
func (f *fake) writeRoutes(t *testing.T, lines ...string) {
	t.Helper()
	dir := filepath.Join(f.town, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, beads.RoutesFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// statusLine is how a report line reads: the label in its column, then the
// value.
func statusLine(label, value string) string {
	return fmt.Sprintf("%-28s %s", label, value)
}

// statusOutput runs o's status and returns what it printed.
func (f *fake) statusOutput(t *testing.T, o Options) string {
	t.Helper()
	out := o.Out.(*bytes.Buffer)
	if err := Status(context.Background(), o); err != nil {
		t.Fatalf("Status: %v", err)
	}
	return out.String()
}

func TestStatusPrintsTheTrackerWhenARigIsOnIt(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		up   bool
		want string
	}{
		{"tracker up", true, "up"},
		{"tracker down", false, "DOWN"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.setHealthy()
			f.trackerPort, f.trackerUp = 7444, tt.up
			f.trackerTown(t)
			f.writeRoutes(t, `{"prefix":"gastown-","path":"gastown","backend":"tracker"}`)
			o, _, _ := f.optsWithOut()
			got := f.statusOutput(t, o)
			if want := statusLine("tracker :7444", tt.want); !strings.Contains(got, want) {
				t.Errorf("status output missing %q:\n%s", want, got)
			}
			// The probe went to the town's endpoint, and to /healthz.
			if want := []string{"http://127.0.0.1:7444/healthz"}; !slices.Equal(f.trackerProbes, want) {
				t.Errorf("tracker probes = %v, want %v", f.trackerProbes, want)
			}
		})
	}
}

func TestStatusOmitsTheTrackerForATownWithNoTrackerRig(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	// The endpoint is configured and up; only the route table decides what
	// prints, and no rig sends it work.
	f.trackerPort, f.trackerUp = 7444, true
	f.trackerTown(t)
	f.writeRoutes(t,
		`{"prefix":"gastown-","path":"gastown"}`,
		`{"prefix":"beads-","path":"beads","backend":"dolt"}`,
	)
	o, _, _ := f.optsWithOut()
	got := f.statusOutput(t, o)
	if want := statusLine("dolt :3307", "up"); !strings.Contains(got, want) {
		t.Errorf("status output lost the Dolt line %q:\n%s", want, got)
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "tracker") {
			t.Errorf("a town with no rig on the tracker printed %q:\n%s", l, got)
		}
	}
	if len(f.trackerProbes) != 0 {
		t.Errorf("a town with no rig on the tracker probed %v", f.trackerProbes)
	}
}

func TestStatusReportsAnUnconfiguredTracker(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	// A port outside the TCP range: the endpoint does not resolve, and the
	// report must not print a guessed one.
	f.trackerPort = 7444
	f.writeTownJSON(t, `{"type":"town","version":1,"name":"test","tracker":{"port":70000}}`)
	f.writeRoutes(t, `{"prefix":"gastown-","path":"gastown","backend":"tracker"}`)
	o, _, _ := f.optsWithOut()
	got := f.statusOutput(t, o)
	if want := statusLine("tracker", "unconfigured"); !strings.Contains(got, want) {
		t.Errorf("status output missing %q:\n%s", want, got)
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "tracker :") {
			t.Errorf("an endpoint that does not resolve printed a port: %q", l)
		}
	}
	if len(f.trackerProbes) != 0 {
		t.Errorf("an endpoint that does not resolve was probed: %v", f.trackerProbes)
	}
}

func TestStatusReportsAnUnreadableRouteTable(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	// A directory where the route table belongs: the path exists but cannot
	// be read, so "no rig on the tracker" is unknown rather than false.
	if err := os.MkdirAll(filepath.Join(f.town, ".beads", beads.RoutesFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	o, _, _ := f.optsWithOut()
	got := f.statusOutput(t, o)
	if !strings.Contains(got, "cannot read the route table") {
		t.Errorf("status output read an unreadable route table as no tracker rig:\n%s", got)
	}
}
