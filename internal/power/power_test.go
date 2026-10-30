package power

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// call is one command the fake host was asked to run.
type call struct {
	dir  string
	name string
	args []string
}

func (c call) line() string {
	return strings.Join(append([]string{c.name}, c.args...), " ")
}

// polecatRow is one `gt polecat list --all --json` row.
type polecatRow struct {
	Rig   string `json:"rig"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// fake is a host: a town directory with the state files, a set of parts that
// the probes and commands move, and the commands it ran.
type fake struct {
	t    *testing.T
	town string

	now        time.Time
	cmds       []call
	docker     bool
	dolt       bool
	forgejo    bool
	composeN   int
	loaded     map[string]bool
	polecats   []polecatRow
	jobhost    bool // the binary is on PATH
	polecatErr error

	// ringKey is what RingKey hands out; nil leaves the Ring keyless.
	ringKey []byte

	releasebot    bool
	jobhostRan    bool
	failJobhost   bool
	unexpectedRun error

	// The town's tracker: trackerPort is the port its endpoint names, and
	// trackerUp answers the health probe there. trackerProbes records every
	// tracker health probe, so a test can show which URL was asked.
	trackerPort   int
	trackerUp     bool
	trackerProbes []string

	// broken parts: commands that fail or leave a part down, for the alarm
	// and refusal branches.
	doltStartNoop bool
	failEstop     bool
	failBootout   bool
	failComposeSt bool
	failComposePs bool
}

func newFake(t *testing.T) *fake {
	t.Helper()
	return &fake{
		t:        t,
		town:     t.TempDir(),
		now:      time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
		docker:   true,
		dolt:     true,
		forgejo:  true,
		composeN: 5,
		loaded:   map[string]bool{},
	}
}

// opts is a healthy town's Options, wired to the fake.
func (f *fake) opts() Options {
	var out, errOut bytes.Buffer
	o := Options{
		TownRoot:         f.town,
		HomeDir:          filepath.Join(f.town, "home"),
		ForgejoDir:       filepath.Join(f.town, "forgejo"),
		ForgejoURL:       "http://forgejo.test:3000",
		LaunchDaemonsDir: filepath.Join(f.town, "LaunchDaemons"),
		GTCommand:        "gt",
		UID:              501,
		Timeout:          time.Minute,
		Run:              f.run,
		LookPath: func(name string) (string, error) {
			if name == "jobhost" && f.jobhost {
				return "/usr/local/bin/jobhost", nil
			}
			return "", exec.ErrNotFound
		},
		HTTPStatus: f.httpStatus,
		Glob: func(pattern string) ([]string, error) {
			if f.releasebot {
				return []string{filepath.Join(f.town, "LaunchDaemons", "local.gt-cloud.releasebot.plist")}, nil
			}
			return nil, nil
		},
		Now:    func() time.Time { return f.now },
		Sleep:  func(_ context.Context, d time.Duration) error { f.now = f.now.Add(d); return nil },
		Out:    &out,
		ErrOut: &errOut,
	}
	o.RingKey = func(string) ([]byte, error) {
		if f.ringKey == nil {
			return nil, errors.New("no ring key in the fake")
		}
		return f.ringKey, nil
	}
	return o
}

// httpStatus answers the health probes: Forgejo's, and the tracker's /healthz
// on the port the test's town names. Any other URL is refused, so a probe of a
// port the town does not name reads DOWN.
func (f *fake) httpStatus(url string) (int, error) {
	if strings.HasSuffix(url, "/api/healthz") {
		if f.forgejo {
			return 200, nil
		}
		return 0, errors.New("connection refused")
	}
	f.trackerProbes = append(f.trackerProbes, url)
	if f.trackerPort == 0 || url != fmt.Sprintf("http://127.0.0.1:%d/healthz", f.trackerPort) {
		return 0, errors.New("connection refused")
	}
	if f.trackerUp {
		return 200, nil
	}
	return 503, nil
}

// run implements the exec seam: probes answer from the fake's state and
// state-changing commands move it, the way the real commands would.
func (f *fake) run(_ context.Context, dir, name string, args ...string) (string, error) {
	f.cmds = append(f.cmds, call{dir: dir, name: name, args: args})
	switch {
	case name == "gt" && len(args) >= 2 && args[0] == "dolt" && args[1] == "status":
		if f.dolt {
			return "● Dolt server is running (PID 1)\n", nil
		}
		return "○ Dolt server is not running\n", errors.New("exit status 1")
	case name == "gt" && len(args) >= 2 && args[0] == "dolt" && args[1] == "start":
		if f.doltStartNoop {
			return "", nil
		}
		f.dolt = true
		return "", nil
	case name == "gt" && len(args) >= 2 && args[0] == "dolt" && args[1] == "stop":
		f.dolt = false
		return "", nil
	case name == "gt" && len(args) >= 1 && args[0] == "estop":
		if f.failEstop {
			return "estop refused", errors.New("exit status 1")
		}
		if err := os.WriteFile(estop.FilePath(f.town), []byte("manual\t2026-10-19T12:00:00Z\ttest\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
		return "", nil
	case name == "gt" && len(args) >= 1 && args[0] == "thaw":
		_ = os.Remove(estop.FilePath(f.town))
		return "", nil
	case name == "gt" && len(args) >= 1 && args[0] == "polecat":
		if f.polecatErr != nil {
			return "", f.polecatErr
		}
		b, err := json.Marshal(f.polecats)
		if err != nil {
			f.t.Fatal(err)
		}
		return string(b), nil
	case name == "gt" && len(args) >= 1 && args[0] == "status":
		return "Health: GREEN tick 1m ago, 3 landed/24h\n", nil
	case name == "launchctl" && args[0] == "print":
		label := filepath.Base(args[1])
		if f.loaded[label] {
			return "pid = 1\n", nil
		}
		return "", errors.New("exit status 113")
	case name == "launchctl" && args[0] == "bootstrap":
		label := strings.TrimSuffix(filepath.Base(args[2]), ".plist")
		f.loaded[label] = true
		return "", nil
	case name == "launchctl" && args[0] == "bootout":
		if f.failBootout {
			return "bootout refused", errors.New("exit status 5")
		}
		delete(f.loaded, filepath.Base(args[1]))
		return "", nil
	case name == "docker" && args[0] == "info":
		if f.docker {
			return "Server Version: 24\n", nil
		}
		return "", errors.New("Cannot connect to the Docker daemon")
	case name == "open" && len(args) >= 1 && args[0] == "-a":
		f.docker = true
		return "", nil
	case name == "docker" && len(args) >= 2 && args[0] == "compose" && args[1] == "ps":
		if f.failComposePs {
			return "", errors.New("exit status 1")
		}
		return strings.Repeat("container-id\n", f.composeN), nil
	case name == "docker" && len(args) >= 2 && args[0] == "compose" && args[1] == "up":
		f.composeN, f.forgejo = 5, true
		return "", nil
	case name == "docker" && len(args) >= 2 && args[0] == "compose" && args[1] == "stop":
		if f.failComposeSt {
			return "stop failed", errors.New("exit status 1")
		}
		f.composeN, f.forgejo = 0, false
		return "", nil
	case name == "jobhost":
		f.jobhostRan = true
		if f.failJobhost {
			return "boom", errors.New("exit status 1")
		}
		return "", nil
	}
	f.unexpectedRun = fmt.Errorf("unexpected command %s", strings.Join(append([]string{name}, args...), " "))
	return "", f.unexpectedRun
}

// setPending writes a health report recording n pending landings.
func (f *fake) setPending(n int) {
	f.t.Helper()
	err := townhealth.Write(f.town, townhealth.Report{
		At:      f.now,
		Verdict: townhealth.Green,
		Fields: []townhealth.Field{{
			Name:    townhealth.FieldLanding,
			Rig:     "gastown",
			Tag:     townhealth.Recorded,
			Verdict: townhealth.Green,
			Value:   fmt.Sprintf("%d pending", n),
		}},
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fake) setGCDone(active bool) {
	f.t.Helper()
	if !active {
		return
	}
	if err := doltpause.Write(f.town, doltpause.Marker{
		Actor:  "daemon/scheduled_maintenance",
		Reason: "weekly gc",
		Until:  f.now.Add(time.Hour),
		Since:  f.now,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// has reports whether any command line starts with prefix.
func (f *fake) has(prefix string) bool {
	for _, c := range f.cmds {
		if strings.HasPrefix(c.line(), prefix) {
			return true
		}
	}
	return false
}

func (f *fake) optsWithOut() (Options, *bytes.Buffer, *bytes.Buffer) {
	o := f.opts()
	out := o.Out.(*bytes.Buffer)
	errOut := o.ErrOut.(*bytes.Buffer)
	return o, out, errOut
}

func (f *fake) setHealthy() {
	f.docker, f.dolt, f.forgejo, f.composeN = true, true, true, 5
	f.loaded[DaemonLabel] = true
	for _, label := range WatcherLabels {
		f.loaded[label] = true
	}
}

func codeOf(t *testing.T, err error) int {
	t.Helper()
	var pe *ExitError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want *ExitError", err)
	}
	return pe.Code
}

func TestDerive(t *testing.T) {
	t.Parallel()
	ok := readings{dolt: true, forgejo: true, daemon: true}
	cases := []struct {
		name string
		r    readings
		want string
	}{
		{"full", ok, "full"},
		{"estop only", func() readings { r := ok; r.estop = true; return r }(), "standby (partial: ESTOP set, hold missing)"},
		{"hold only", func() readings { r := ok; r.hold = true; return r }(), "standby (partial: hold set, no ESTOP; drained or draining)"},
		{"draining", func() readings {
			r := ok
			r.estop, r.hold, r.working = true, true, []string{"gastown/dementus"}
			return r
		}(), "standby (draining: 1 polecat(s) working)"},
		{"standby", func() readings { r := ok; r.estop, r.hold = true, true; return r }(), "standby"},
		{"off", readings{dolt: true}, "off"},
		{"off dolt down", readings{}, "off (dolt down)"},
		{"mixed", readings{dolt: true, daemon: true}, "mixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := derive(tc.r); got != tc.want {
				t.Errorf("derive = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatusPrintsEveryPart(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(1)
	o, out, _ := f.optsWithOut()
	if err := Status(context.Background(), o); err != nil {
		t.Fatalf("Status: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"state                        full",
		"dolt :3307                   up",
		"docker engine                up",
		"forgejo healthz              ok (compose services running: 5)",
		"daemon (com.gastown.daemon)  loaded",
		"ESTOP file                   absent",
		"hold file                    absent",
		"landings pending             1",
		"polecats working             0",
		"dolt gc running              0",
		"last requested               unknown",
		"cloud-deploy daemons         0 plist(s)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
	for _, label := range WatcherLabels {
		line := ""
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(l, label) {
				line = l
				break
			}
		}
		if !strings.HasSuffix(line, "loaded") {
			t.Errorf("status output has no loaded watcher %q (line %q):\n%s", label, line, got)
		}
	}
	if len(f.cmds) != 0 {
		for _, c := range f.cmds {
			if isMutation(c) {
				t.Errorf("status ran a state-changing command: %s", c.line())
			}
		}
	}
}

func isMutation(c call) bool {
	line := c.line()
	for _, writable := range []string{"launchctl bootstrap", "launchctl bootout", "docker compose up", "docker compose stop", "gt dolt start", "gt dolt stop", "gt estop", "gt thaw", "open -a", "jobhost"} {
		if strings.HasPrefix(line, writable) {
			return true
		}
	}
	return false
}

func TestFullDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.docker, f.dolt, f.forgejo, f.composeN = false, false, false, 0
	f.loaded = map[string]bool{}
	if err := os.WriteFile(estop.FilePath(f.town), []byte("manual\tx\tt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, out, _ := f.optsWithOut()
	o.DryRun = true
	if err := Full(context.Background(), o); err != nil {
		t.Fatalf("Full dry-run: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"[dry-run] open -a Docker",
		"[dry-run] docker compose up -d",
		"[dry-run] gt dolt start",
		"[dry-run] launchctl bootstrap gui/501",
		"[dry-run] gt thaw",
		"[dry-run] rm -f " + dispatch.HoldFilePath(f.town),
		"would then check: gt status Health",
		"dry-run complete: nothing was changed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(err) {
		t.Errorf("dry-run created the hold: %v", err)
	}
	if _, err := os.Stat(StatePath(f.town)); !os.IsNotExist(err) {
		t.Errorf("dry-run recorded a state: %v", err)
	}
	if !estop.IsActive(f.town) {
		t.Error("dry-run cleared the ESTOP")
	}
}

func TestFullBringsEverythingUp(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.docker, f.dolt, f.forgejo, f.composeN = false, false, false, 0
	f.loaded = map[string]bool{}
	if err := os.WriteFile(dispatch.HoldFilePath(f.town), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(estop.FilePath(f.town), []byte("manual\tx\tt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, out, _ := f.optsWithOut()
	if err := Full(context.Background(), o); err != nil {
		t.Fatalf("Full: %v (output:\n%s)", err, out)
	}
	if !f.docker || !f.dolt || !f.forgejo || f.composeN < 4 {
		t.Errorf("parts after full: docker=%v dolt=%v forgejo=%v compose=%d", f.docker, f.dolt, f.forgejo, f.composeN)
	}
	for _, label := range append([]string{DaemonLabel}, WatcherLabels...) {
		if !f.loaded[label] {
			t.Errorf("%s not loaded after full", label)
		}
	}
	if _, err := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(err) {
		t.Errorf("hold still present after full: %v", err)
	}
	if estop.IsActive(f.town) {
		t.Error("ESTOP still active after full")
	}
	data, err := os.ReadFile(StatePath(f.town))
	if err != nil || !strings.HasPrefix(string(data), "full ") {
		t.Errorf("state file = %q, %v; want a full record", data, err)
	}
	for _, want := range []string{"open -a Docker", "docker compose up -d", "gt dolt start", "launchctl bootstrap", "gt thaw"} {
		if !f.has(want) && !strings.Contains(out.String(), want) {
			t.Errorf("full did not run %q:\n%s", want, out)
		}
	}
}

func TestStandbySetsHoldBeforeEstop(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	o, out, _ := f.optsWithOut()
	if err := Standby(context.Background(), o); err != nil {
		t.Fatalf("Standby: %v (output:\n%s)", err, out)
	}
	got := out.String()
	holdAt := strings.Index(got, "touch "+dispatch.HoldFilePath(f.town))
	estopAt := strings.Index(got, "gt estop -r gt-power: standby")
	if holdAt < 0 || estopAt < 0 || holdAt > estopAt {
		t.Fatalf("hold must be set before the ESTOP (hold at %d, estop at %d):\n%s", holdAt, estopAt, got)
	}
	if !estop.IsActive(f.town) {
		t.Error("standby did not set the ESTOP")
	}
	if _, err := os.Stat(dispatch.HoldFilePath(f.town)); err != nil {
		t.Errorf("standby did not set the hold: %v", err)
	}
	data, err := os.ReadFile(StatePath(f.town))
	if err != nil || !strings.HasPrefix(string(data), "standby ") {
		t.Errorf("state file = %q, %v; want a standby record", data, err)
	}
}

func TestStandbyDrainTimeoutExits3(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(2)
	f.polecats = []polecatRow{{Rig: "gastown", Name: "dementus", State: "working"}}
	o, _, errOut := f.optsWithOut()
	o.Timeout = time.Minute
	err := Standby(context.Background(), o)
	if code := codeOf(t, err); code != ExitDrainTimeout {
		t.Fatalf("exit code = %d, want %d", code, ExitDrainTimeout)
	}
	for _, want := range []string{"landings pending=2", "polecats working=1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error %q missing %q", err, want)
		}
	}
	if !strings.Contains(errOut.String(), "gastown/dementus working") {
		t.Errorf("timeout did not name the working polecat:\n%s", errOut)
	}
	if estop.IsActive(f.town) {
		t.Error("a timed-out drain must not set the ESTOP")
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); statErr != nil {
		t.Errorf("the hold must stay set on a timeout: %v", statErr)
	}
	if _, statErr := os.Stat(StatePath(f.town)); !os.IsNotExist(statErr) {
		t.Errorf("a timed-out standby recorded a state: %v", statErr)
	}
}

// A polecat inside its spawn grace reads "spawning", not "working", but its
// session is still coming up: the town is not drained while it boots. Once the
// grace expires the inventory reads the same dead session as "stalled", which
// is not work in flight: the drain proceeds and standby sets the ESTOP. Both
// sides of that boundary are pinned here, so widening the spawn check to every
// non-working state cannot pass and freeze standby on a town of stalled seats.
func TestStandbyTreatsASpawningPolecatAsWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state string
		holds bool
	}{
		{name: "inside the grace holds the drain", state: "spawning", holds: true},
		{name: "past the grace drains", state: "stalled", holds: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.setHealthy()
			f.setPending(0)
			f.polecats = []polecatRow{{Rig: "gastown", Name: "furiosa", State: tt.state}}
			o, _, errOut := f.optsWithOut()
			o.Timeout = time.Minute
			err := Standby(context.Background(), o)
			if !tt.holds {
				if err != nil {
					t.Fatalf("Standby: %v (a %s polecat is not work in flight)", err, tt.state)
				}
				if !estop.IsActive(f.town) {
					t.Error("standby did not set the ESTOP on a drained town")
				}
				return
			}
			if code := codeOf(t, err); code != ExitDrainTimeout {
				t.Fatalf("exit code = %d, want %d (a %s polecat must hold the drain)", code, ExitDrainTimeout, tt.state)
			}
			if !strings.Contains(errOut.String(), "gastown/furiosa working") {
				t.Errorf("drain did not name the %s polecat:\n%s", tt.state, errOut)
			}
			if estop.IsActive(f.town) {
				t.Errorf("a drain held by a %s polecat must not set the ESTOP", tt.state)
			}
		})
	}
}

func TestStandbyTreatsUnknownLandingQueueAsNotDrained(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy() // no health report: the landing queue cannot be read
	o, _, _ := f.optsWithOut()
	o.Timeout = time.Minute
	err := Standby(context.Background(), o)
	if code := codeOf(t, err); code != ExitDrainTimeout {
		t.Fatalf("exit code = %d, want %d", code, ExitDrainTimeout)
	}
}

// TestStandbyTreatsUnparseableLandingFieldAsNotDrained pins the mixed report:
// one rig's landing queue read as "0 pending" must not hide another rig whose
// read failed (Value "?"). The queue is then not known, so standby times out
// before it sets the ESTOP.
func TestStandbyTreatsUnparseableLandingFieldAsNotDrained(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	err := townhealth.Write(f.town, townhealth.Report{
		At:      f.now,
		Verdict: townhealth.Green,
		Fields: []townhealth.Field{
			{Name: townhealth.FieldLanding, Rig: "gastown", Tag: townhealth.Recorded, Verdict: townhealth.Green, Value: "0 pending"},
			{Name: townhealth.FieldLanding, Rig: "beads", Tag: townhealth.Unknown, Verdict: townhealth.VerdictUnknown, Value: "?"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _, _ := f.optsWithOut()
	o.Timeout = time.Minute
	err = Standby(context.Background(), o)
	if code := codeOf(t, err); code != ExitDrainTimeout {
		t.Fatalf("exit code = %d, want %d", code, ExitDrainTimeout)
	}
	if _, statErr := os.Stat(estop.FilePath(f.town)); !os.IsNotExist(statErr) {
		t.Errorf("ESTOP was set although a landing queue was unread (stat err = %v)", statErr)
	}
}

// TestPendingLandingsStaleZeroNotDrained pins the stale report: once the
// daemon stops writing the health file, its last "0 pending" is a count from
// before it stopped, not a drained queue. The queue is then unread, so
// standby times out before it sets the ESTOP.
func TestPendingLandingsStaleZeroNotDrained(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	err := townhealth.Write(f.town, townhealth.Report{
		At:      f.now.Add(-townhealth.DefaultStaleAfter - time.Minute),
		Verdict: townhealth.Green,
		Fields: []townhealth.Field{
			{Name: townhealth.FieldLanding, Rig: "gastown", Tag: townhealth.Recorded, Verdict: townhealth.Green, Value: "0 pending"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _, _ := f.optsWithOut()
	e := &engine{o: o}
	if n, ok := e.pendingLandings(); ok {
		t.Errorf("pendingLandings = (%d, true) for a stale report, want ok=false", n)
	}
	o.Timeout = time.Minute
	err = Standby(context.Background(), o)
	if code := codeOf(t, err); code != ExitDrainTimeout {
		t.Fatalf("exit code = %d, want %d", code, ExitDrainTimeout)
	}
	if _, statErr := os.Stat(estop.FilePath(f.town)); !os.IsNotExist(statErr) {
		t.Errorf("ESTOP was set from a stale landing count (stat err = %v)", statErr)
	}
}

// writeHealthConfig writes the town's settings/config.json with health set
// to the given JSON object.
func writeHealthConfig(t *testing.T, town, health string) {
	t.Helper()
	dir := filepath.Join(town, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"town-settings","version":1,"operational":{"health":` + health + `}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPendingLandingsStaleUnknownVerdictNotDrained pins a stale report whose
// recorded verdict was already unknown: Effective() then returns unknown too,
// so comparing the two cannot see the staleness and the last landing count
// would read as drained.
func TestPendingLandingsStaleUnknownVerdictNotDrained(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	err := townhealth.Write(f.town, townhealth.Report{
		At:      f.now.Add(-townhealth.DefaultStaleAfter - time.Minute),
		Verdict: townhealth.VerdictUnknown,
		Fields: []townhealth.Field{
			{Name: townhealth.FieldDolt, Tag: townhealth.Unknown, Verdict: townhealth.VerdictUnknown, Value: "?"},
			{Name: townhealth.FieldLanding, Rig: "gastown", Tag: townhealth.Recorded, Verdict: townhealth.Green, Value: "0 pending"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _, _ := f.optsWithOut()
	e := &engine{o: o}
	if n, ok := e.pendingLandings(); ok {
		t.Errorf("pendingLandings = (%d, true) for a stale report recorded unknown, want ok=false", n)
	}
}

// TestPendingLandingsUsesConfiguredStaleAge pins the operator's stale_after:
// a report the compiled default would call stale is read inside the
// configured window, and one past the window is not.
func TestPendingLandingsUsesConfiguredStaleAge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		age    time.Duration
		wantOK bool
	}{
		{"inside the configured window", 15 * time.Minute, true},
		{"past the configured window", 45 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.setHealthy()
			writeHealthConfig(t, f.town, `{"stale_after":"30m"}`)
			err := townhealth.Write(f.town, townhealth.Report{
				At:      f.now.Add(-tc.age),
				Verdict: townhealth.Green,
				Fields: []townhealth.Field{
					{Name: townhealth.FieldLanding, Rig: "gastown", Tag: townhealth.Recorded, Verdict: townhealth.Green, Value: "0 pending"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			o, _, _ := f.optsWithOut()
			e := &engine{o: o}
			if _, ok := e.pendingLandings(); ok != tc.wantOK {
				t.Errorf("pendingLandings ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// TestPendingLandingsBrokenStaleAgeFallsBackToDefault pins the fallback: an
// unparseable stale_after must apply the compiled default, not a zero window
// that would call every report stale.
func TestPendingLandingsBrokenStaleAgeFallsBackToDefault(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	writeHealthConfig(t, f.town, `{"stale_after":"soon"}`)
	err := townhealth.Write(f.town, townhealth.Report{
		At:      f.now.Add(-5 * time.Minute),
		Verdict: townhealth.Green,
		Fields: []townhealth.Field{
			{Name: townhealth.FieldLanding, Rig: "gastown", Tag: townhealth.Recorded, Verdict: townhealth.Green, Value: "0 pending"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _, _ := f.optsWithOut()
	e := &engine{o: o}
	n, ok := e.pendingLandings()
	if !ok || n != 0 {
		t.Errorf("pendingLandings = (%d, %v), want (0, true): the default stale age must apply", n, ok)
	}
}

func TestStandbyWaitsForDoltGC(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.setGCDone(true)
	o, _, _ := f.optsWithOut()
	o.Timeout = time.Minute
	err := Standby(context.Background(), o)
	if code := codeOf(t, err); code != ExitDrainTimeout {
		t.Fatalf("exit code = %d, want %d", code, ExitDrainTimeout)
	}
	if !strings.Contains(err.Error(), "drain timed out") {
		t.Errorf("error = %v", err)
	}
}

// TestDrainFailsWhenDoltIsUnreachable is the fail-open regression: a drain
// that cannot read the landing queues must fail, never skip the wait.
func TestDrainFailsWhenDoltIsUnreachable(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.dolt = false
	o, _, _ := f.optsWithOut()
	err := newEngine(o).drain(context.Background())
	if code := codeOf(t, err); code != ExitDrainTimeout {
		t.Fatalf("exit code = %d, want %d", code, ExitDrainTimeout)
	}
	if !strings.Contains(err.Error(), "Dolt") {
		t.Errorf("error %q does not name Dolt", err)
	}
	if estop.IsActive(f.town) {
		t.Error("a failed drain set the ESTOP")
	}
}

func TestStandbyTreatsUnreadablePolecatListAsNotDrained(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.polecatErr = errors.New("gt polecat list failed")
	o, _, _ := f.optsWithOut()
	o.Timeout = time.Minute
	err := Standby(context.Background(), o)
	if code := codeOf(t, err); code != ExitDrainTimeout {
		t.Fatalf("exit code = %d, want %d", code, ExitDrainTimeout)
	}
	if !strings.Contains(err.Error(), "polecats working=?") {
		t.Errorf("timeout error %q does not report the unreadable polecat list", err)
	}
}

func TestStandbyReportsEstopFailure(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.failEstop = true
	o, _, _ := f.optsWithOut()
	err := Standby(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "gt estop") {
		t.Fatalf("error = %v, want a gt estop failure", err)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); statErr != nil {
		t.Errorf("the hold must stay set when the ESTOP fails: %v", statErr)
	}
	if _, statErr := os.Stat(StatePath(f.town)); !os.IsNotExist(statErr) {
		t.Errorf("a failed standby recorded a state: %v", statErr)
	}
}

func TestFullFailsWhenDoltNeverComesUp(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.dolt = false
	f.doltStartNoop = true // gt dolt start succeeds but Dolt never answers
	o, _, _ := f.optsWithOut()
	err := Full(context.Background(), o)
	if code := codeOf(t, err); code != ExitNotHealthy {
		t.Fatalf("exit code = %d, want %d", code, ExitNotHealthy)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); statErr != nil {
		t.Errorf("full must leave the hold in place when Dolt does not come up: %v", statErr)
	}
}

func TestHoldOnTreatsAStatErrorAsNoHold(t *testing.T) {
	t.Parallel()
	// A regular file where the town root should be makes stat fail with
	// ENOTDIR, which is a stat error but not "does not exist".
	town := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(town, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if newEngine(Options{TownRoot: town}).holdOn() {
		t.Error("holdOn reported a hold for a path it could not stat")
	}
}

// TestNestedGTCallsRunFromTheTownRoot pins the script's cd "$GT_ROOT": the
// caller's cwd must not decide which town the nested gt calls act on.
func TestNestedGTCallsRunFromTheTownRoot(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	o, _, _ := f.optsWithOut()
	if err := Standby(context.Background(), o); err != nil {
		t.Fatalf("Standby: %v", err)
	}
	sawGT := false
	for _, c := range f.cmds {
		if c.name == "gt" {
			sawGT = true
			if c.dir != f.town {
				t.Errorf("nested %q ran in %q, want the town root %q", c.line(), c.dir, f.town)
			}
		}
	}
	if !sawGT {
		t.Fatal("standby ran no nested gt command")
	}
}

func TestOffRequiresYes(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	o, _, _ := f.optsWithOut()
	if code := codeOf(t, Off(context.Background(), o)); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if _, err := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(err) {
		t.Errorf("off without --yes changed the hold: %v", err)
	}
	if f.jobhostRan || f.composeN != 5 {
		t.Error("off without --yes changed a part")
	}
	if len(f.cmds) != 0 {
		t.Errorf("off without --yes ran %d command(s) before refusing: %v", len(f.cmds), f.cmds)
	}
}

func TestOffRefusesReleaseBotWithoutForceCloud(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.releasebot = true
	o, out, _ := f.optsWithOut()
	o.Yes = true
	err := Off(context.Background(), o)
	if code := codeOf(t, err); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "--force-cloud") {
		t.Errorf("error %q does not name --force-cloud", err)
	}
	// The alarm comes before the refusal, and the refusal comes before any
	// change: the hold is not set and no command ran.
	if !strings.Contains(out.String(), "cloud release bot") {
		t.Errorf("refused off did not warn about the release bot:\n%s", out)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(statErr) {
		t.Errorf("refused off changed the hold: %v", statErr)
	}
	if len(f.cmds) != 0 {
		t.Errorf("refused off ran %d command(s): %v", len(f.cmds), f.cmds)
	}
}

func TestOffDryRunNeedsNoYesAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.releasebot = true
	o, out, _ := f.optsWithOut()
	o.DryRun = true
	if err := Off(context.Background(), o); err != nil {
		t.Fatalf("Off dry-run: %v", err)
	}
	got := out.String()
	for _, want := range []string{"[dry-run] touch ", "[dry-run] gt estop", "[dry-run] launchctl bootout", "[dry-run] docker compose stop", "cloud release bot"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, got)
		}
	}
	if !f.dolt || !f.docker || f.composeN != 5 {
		t.Errorf("dry-run changed parts: dolt=%v docker=%v compose=%d", f.dolt, f.docker, f.composeN)
	}
	if _, err := os.Stat(StatePath(f.town)); !os.IsNotExist(err) {
		t.Errorf("dry-run recorded a state: %v", err)
	}
}

func TestOffStopsTheTownButKeepsDolt(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	o, out, _ := f.optsWithOut()
	o.Yes = true
	if err := Off(context.Background(), o); err != nil {
		t.Fatalf("Off: %v (output:\n%s)", err, out)
	}
	if !f.dolt {
		t.Error("off without --dolt stopped Dolt")
	}
	if f.composeN != 0 || f.forgejo {
		t.Errorf("Forgejo stack still running: compose=%d healthz=%v", f.composeN, f.forgejo)
	}
	if f.loaded[DaemonLabel] {
		t.Error("daemon still loaded after off")
	}
	for _, label := range WatcherLabels {
		if f.loaded[label] {
			t.Errorf("%s still loaded after off", label)
		}
	}
	if !estop.IsActive(f.town) {
		t.Error("off did not set the ESTOP")
	}
	data, err := os.ReadFile(StatePath(f.town))
	if err != nil || !strings.HasPrefix(string(data), "off ") {
		t.Errorf("state file = %q, %v; want an off record", data, err)
	}
	assertNeverDestructive(t, f)
}

func TestOffRefusesWhenDoltIsDown(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.dolt = false
	o, _, _ := f.optsWithOut()
	o.Yes = true
	err := Off(context.Background(), o)
	if code := codeOf(t, err); code != ExitNotHealthy {
		t.Fatalf("exit code = %d, want %d", code, ExitNotHealthy)
	}
	if !strings.Contains(err.Error(), "Dolt is down") {
		t.Errorf("error %q does not name Dolt", err)
	}
	// The script raises the hold before it checks Dolt, so a refused off
	// leaves the town held rather than half-set.
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); statErr != nil {
		t.Errorf("refused off did not set the hold: %v", statErr)
	}
	if !f.loaded[DaemonLabel] || f.composeN != 5 || estop.IsActive(f.town) {
		t.Errorf("refused off stopped a part: daemon=%v compose=%d estop=%v",
			f.loaded[DaemonLabel], f.composeN, estop.IsActive(f.town))
	}
}

func TestOffDryRunRefusesWhenDoltIsDown(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.dolt = false
	o, out, _ := f.optsWithOut()
	o.DryRun = true
	err := Off(context.Background(), o)
	if code := codeOf(t, err); code != ExitNotHealthy {
		t.Fatalf("exit code = %d, want %d (dry-run must refuse what a real off refuses)", code, ExitNotHealthy)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(statErr) {
		t.Errorf("dry-run created the hold: %v", statErr)
	}
	if _, statErr := os.Stat(StatePath(f.town)); !os.IsNotExist(statErr) {
		t.Errorf("dry-run recorded a state: %v", statErr)
	}
	if strings.Contains(out.String(), "[dry-run] gt estop") {
		t.Errorf("dry-run printed a stop past the Dolt refusal:\n%s", out)
	}
}

func TestOffReportsBootoutFailure(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.failBootout = true
	o, _, _ := f.optsWithOut()
	o.Yes = true
	err := Off(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "launchctl bootout") {
		t.Fatalf("error = %v, want a launchctl bootout failure", err)
	}
	if !estop.IsActive(f.town) {
		t.Error("off failed before setting the ESTOP")
	}
	if f.composeN != 5 {
		t.Errorf("off continued past the failed bootout: compose=%d", f.composeN)
	}
}

func TestOffReportsComposeStopFailure(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.failComposeSt = true
	o, _, _ := f.optsWithOut()
	o.Yes = true
	err := Off(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "docker compose stop") {
		t.Fatalf("error = %v, want a docker compose stop failure", err)
	}
	if f.loaded[DaemonLabel] {
		t.Error("off did not stop the daemon before the compose failure")
	}
}

func TestOffWithDoltStopsDolt(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	o, _, _ := f.optsWithOut()
	o.Yes = true
	o.WithDolt = true
	if err := Off(context.Background(), o); err != nil {
		t.Fatalf("Off: %v", err)
	}
	if f.dolt {
		t.Error("off --dolt left Dolt running")
	}
	if !f.has("gt dolt stop") {
		t.Error("off --dolt did not run gt dolt stop")
	}
}

// assertNeverDestructive is the grep test: off never reaches for the commands
// that delete worktrees.
func assertNeverDestructive(t *testing.T, f *fake) {
	t.Helper()
	for _, c := range f.cmds {
		if c.name != "gt" {
			continue
		}
		line := strings.Join(c.args, " ")
		if strings.HasPrefix(line, "down") || strings.HasPrefix(line, "shutdown") || strings.Contains(line, "--nuke") {
			t.Errorf("destructive command run: %s", c.line())
		}
	}
}

func TestJobhostHook(t *testing.T) {
	t.Parallel()
	t.Run("runs when on PATH", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.setHealthy()
		f.setPending(0)
		f.jobhost = true
		o, _, _ := f.optsWithOut()
		if err := Standby(context.Background(), o); err != nil {
			t.Fatalf("Standby: %v", err)
		}
		if !f.jobhostRan {
			t.Error("jobhost down did not run while jobhost is on PATH")
		}
	})
	t.Run("skipped when absent", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.setHealthy()
		f.setPending(0)
		o, _, _ := f.optsWithOut()
		if err := Standby(context.Background(), o); err != nil {
			t.Fatalf("Standby: %v", err)
		}
		if f.jobhostRan {
			t.Error("jobhost down ran while jobhost is missing")
		}
	})
	t.Run("a failing hook does not undo the state", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.setHealthy()
		f.setPending(0)
		f.jobhost = true
		f.failJobhost = true
		o, _, errOut := f.optsWithOut()
		if err := Standby(context.Background(), o); err != nil {
			t.Fatalf("Standby: %v", err)
		}
		if !strings.Contains(errOut.String(), "jobhost down is a hook") {
			t.Errorf("a failing hook was not reported:\n%s", errOut)
		}
	})
}

func TestBootHold(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	o, _, _ := f.optsWithOut()
	if err := BootHold(context.Background(), o); err != nil {
		t.Fatalf("BootHold: %v", err)
	}
	if _, err := os.Stat(dispatch.HoldFilePath(f.town)); err != nil {
		t.Errorf("boot-hold did not write the hold: %v", err)
	}
	data, err := os.ReadFile(StatePath(f.town))
	if err != nil || !strings.HasPrefix(string(data), "standby (boot) ") {
		t.Errorf("state file = %q, %v; want a boot standby record", data, err)
	}
}

// TestWaitUntilBoundsHungProbe: a probe that blocks until its context ends
// must not hold the wait past its deadline.
func TestWaitUntilBoundsHungProbe(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	e := newEngine(f.opts())

	guard, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- e.waitUntil(context.Background(), "hung probe", 200*time.Millisecond, func(ctx context.Context) bool {
			<-ctx.Done()
			return false
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out waiting for hung probe") {
			t.Fatalf("error = %v, want a timed-out error", err)
		}
	case <-guard.Done():
		t.Fatal("waitUntil still blocked on a hung probe 5s after its 200ms deadline")
	}
}

// TestWaitUntilStopsWhenContextEnds: a context that ends during a wait stops
// the wait with the context's error, and the probe is not run again.
func TestWaitUntilStopsWhenContextEnds(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	e := newEngine(f.opts())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probes := 0
	err := e.waitUntil(ctx, "cancelled wait", time.Minute, func(context.Context) bool {
		probes++
		cancel()
		return false
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want 1", probes)
	}
}
