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
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
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

func TestStatusNamesTheCauseOfAMalformedTrackerConfig(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		town  string
		cause string
	}{
		{
			name:  "a port outside the TCP range",
			town:  `{"type":"town","version":1,"name":"test","tracker":{"port":70000}}`,
			cause: "tracker.port 70000 is not a TCP port",
		},
		{
			name:  "a port that is not a number",
			town:  `{"type":"town","version":1,"name":"test","tracker":{"port":"seven"}}`,
			cause: "town.json",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.setHealthy()
			// The endpoint does not resolve, so the report must not print a
			// guessed port, and it must name what is wrong with the config.
			f.trackerPort = 7444
			f.writeTownJSON(t, tt.town)
			f.writeRoutes(t, `{"prefix":"gastown-","path":"gastown","backend":"tracker"}`)
			o, _, _ := f.optsWithOut()
			got := f.statusOutput(t, o)
			var trackerLines []string
			for _, l := range strings.Split(got, "\n") {
				if strings.HasPrefix(l, "tracker") {
					trackerLines = append(trackerLines, l)
				}
			}
			if len(trackerLines) != 1 {
				t.Fatalf("a malformed tracker config printed %d tracker lines, want 1:\n%s", len(trackerLines), got)
			}
			if !strings.Contains(trackerLines[0], tt.cause) {
				t.Errorf("tracker line %q does not name the cause %q:\n%s", trackerLines[0], tt.cause, got)
			}
			if !strings.HasPrefix(trackerLines[0], statusLine("tracker", "? ")) {
				t.Errorf("an endpoint that does not resolve is not reported as unknown: %q", trackerLines[0])
			}
			if len(f.trackerProbes) != 0 {
				t.Errorf("an endpoint that does not resolve was probed: %v", f.trackerProbes)
			}
		})
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

// testRingKey signs the Ring records the status tests write; the fake hands
// it out as the town's ring key.
var testRingKey = []byte("ring-status-test-key")

// ringCase is one Ring state written into a fake town, and the ring line the
// status report must print for it.
type ringCase struct {
	name  string
	setup func(t *testing.T, f *fake)
	line  string
}

var ringCases = []ringCase{
	{"no ring section", func(t *testing.T, f *fake) {
		f.writeTownJSON(t, `{"type":"town","version":1,"name":"test"}`)
	}, "ring: absent (" + ring.ReasonNoSection + ")"},
	{"valid", func(t *testing.T, f *fake) {
		// Forged 5h before now with a 36h lease: 31h of it is left.
		ringtest.Valid(t, f.town, testRingKey, f.now.Add(-5*time.Hour))
	}, "ring: valid, wanes in 31h"},
	{"waned", func(t *testing.T, f *fake) {
		// The lease ended 2h before now: the record expires at now-2h.
		ringtest.Waned(t, f.town, testRingKey, f.now.Add(-2*time.Hour+time.Minute))
	}, "ring: waned 2h ago"},
	{"tampered", func(t *testing.T, f *fake) {
		ringtest.Tampered(t, f.town, testRingKey, f.now)
	}, "ring: absent (" + ring.ReasonBadSignature + ")"},
	{"key unavailable", func(t *testing.T, f *fake) {
		ringtest.Valid(t, f.town, testRingKey, f.now)
		f.ringKey = nil
	}, "ring: key unavailable"},
}

// statusWithRing runs the status report on a healthy fake whose part is set up
// by part and whose Ring by ring, and returns what it printed.
func statusWithRing(t *testing.T, part func(t *testing.T, f *fake), rc ringCase) string {
	t.Helper()
	f := newFake(t)
	part(t, f)
	f.ringKey = testRingKey
	rc.setup(t, f)
	o, _, _ := f.optsWithOut()
	return f.statusOutput(t, o)
}

func TestStatusPrintsTheRingLine(t *testing.T) {
	t.Parallel()
	healthy := func(t *testing.T, f *fake) { f.setHealthy() }
	for _, tt := range ringCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := statusWithRing(t, healthy, tt)
			if !slices.Contains(strings.Split(got, "\n"), tt.line) {
				t.Errorf("status output has no line %q:\n%s", tt.line, got)
			}
		})
	}
}

func TestStatusReadsAnUnreadableRingAsUnreadable(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.ringKey = testRingKey
	f.writeTownJSON(t, `{"type":"town"`)
	o, _, _ := f.optsWithOut()
	got := f.statusOutput(t, o)
	if !slices.ContainsFunc(strings.Split(got, "\n"), func(l string) bool {
		return strings.HasPrefix(l, "ring: unreadable (")
	}) {
		t.Errorf("a town config that cannot be parsed did not read as an unreadable ring:\n%s", got)
	}
}

// stateLine is the report's derived state line.
func stateLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "state ") {
			return l
		}
	}
	return ""
}

// touchSentinel creates a file a brake reads by its presence: the ESTOP or the
// hold.
func touchSentinel(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRingDoesNotChangeTheDerivedState(t *testing.T) {
	t.Parallel()
	for _, part := range []struct {
		name  string
		setup func(t *testing.T, f *fake)
		want  string
	}{
		{"full", func(t *testing.T, f *fake) { f.setHealthy() }, "full"},
		{"standby", func(t *testing.T, f *fake) {
			f.setHealthy()
			touchSentinel(t, estop.FilePath(f.town))
			touchSentinel(t, dispatch.HoldFilePath(f.town))
		}, "standby"},
		{"off", func(t *testing.T, f *fake) {
			f.setHealthy()
			f.forgejo = false
			f.loaded = map[string]bool{}
		}, "off"},
	} {
		t.Run(part.name, func(t *testing.T) {
			t.Parallel()
			base := stateLine(statusWithRing(t, part.setup, ringCases[0]))
			if want := statusLine("state", part.want); base != want {
				t.Fatalf("with no ring section, state line = %q, want %q", base, want)
			}
			for _, r := range ringCases[1:] {
				got := stateLine(statusWithRing(t, part.setup, r))
				if got != base {
					t.Errorf("the %s ring changed the state line to %q, want %q as with no ring", r.name, got, base)
				}
			}
		})
	}
}
