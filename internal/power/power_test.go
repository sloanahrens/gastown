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

	releasebot    bool
	jobhostRan    bool
	failJobhost   bool
	unexpectedRun error
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
		HTTPStatus: func(string) (int, error) {
			if f.forgejo {
				return 200, nil
			}
			return 0, errors.New("connection refused")
		},
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
	return o
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
		f.dolt = true
		return "", nil
	case name == "gt" && len(args) >= 2 && args[0] == "dolt" && args[1] == "stop":
		f.dolt = false
		return "", nil
	case name == "gt" && len(args) >= 1 && args[0] == "estop":
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
		return strings.Repeat("container-id\n", f.composeN), nil
	case name == "docker" && len(args) >= 2 && args[0] == "compose" && args[1] == "up":
		f.composeN, f.forgejo = 5, true
		return "", nil
	case name == "docker" && len(args) >= 2 && args[0] == "compose" && args[1] == "stop":
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
}

func TestOffRefusesReleaseBotWithoutForceCloud(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.releasebot = true
	o, _, _ := f.optsWithOut()
	o.Yes = true
	err := Off(context.Background(), o)
	if code := codeOf(t, err); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "--force-cloud") {
		t.Errorf("error %q does not name --force-cloud", err)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(statErr) {
		t.Errorf("refused off changed the hold: %v", statErr)
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
	for _, want := range []string{"[dry-run] touch ", "[dry-run] gt estop", "[dry-run] launchctl bootout", "[dry-run] docker compose stop"} {
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
