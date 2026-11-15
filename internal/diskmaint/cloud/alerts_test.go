package cloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
)

// usageKey is the alert key the cloud VM's disk raises.
const usageKey = "diskmaint:cloud-vm"

// unreadableKey is the alert key a disk the daemon cannot show raises.
const unreadableKey = usageKey + ":unreadable"

// containerID is a container id docker ps prints, as a test names one.
func containerID(c byte) string { return strings.Repeat(string(c), 64) }

// dfAnswer is what df -P prints for a disk at used percent.
func dfAnswer(used float64) string {
	return fmt.Sprintf("Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 100 1 1 %s%% /\n",
		strconv.FormatFloat(used, 'f', -1, 64))
}

// diskDocker answers the two calls the disk read makes and records every one,
// so a test sees exactly what the target asked the daemon for.
type diskDocker struct {
	calls [][]string
	envs  [][]string
	// ps is the container list's answer; psErr is the error of a list that fails.
	ps    string
	psErr error
	// df is each container's df answer; dfErr is the error of a read that fails.
	df    map[string]string
	dfErr map[string]error
}

func (f *diskDocker) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(args))
	f.envs = append(f.envs, slices.Clone(env))
	if name != "docker" {
		return nil, errors.New("unexpected command")
	}
	if slices.Equal(args, []string{"ps", "-q"}) {
		return []byte(f.ps), f.psErr
	}
	if len(args) == len(dfArgs)+2 && args[0] == "exec" && slices.Equal(args[2:], dfArgs) {
		if err := f.dfErr[args[1]]; err != nil {
			return nil, err
		}
		out, ok := f.df[args[1]]
		if !ok {
			return nil, errors.New("unexpected exec")
		}
		return []byte(out), nil
	}
	return nil, errors.New("unexpected command")
}

// diskTarget builds the cloud target over a fake daemon.
func diskTarget(t *testing.T, run diskmaint.Runner) *Target {
	t.Helper()
	target, err := NewTarget(Config{DockerHost: dockerHost, Run: run, Ledger: &diskmaint.Ledger{}})
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	return target
}

// onlyAlert asserts the pass returned exactly one alert.
func onlyAlert(t *testing.T, alerts []disks.Alert) disks.Alert {
	t.Helper()
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want exactly one", alerts)
	}
	return alerts[0]
}

// TestDiskAlertsThresholds drives the 80/70 scale through the target: a disk at
// or over 80 raises the usage key, a disk under 70 clears it, and a disk between
// the two changes nothing.
func TestDiskAlertsThresholds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		used      float64
		active    map[string]bool
		wantKey   string
		wantRaise bool
		wantClear bool
	}{
		{name: "at 80 raises", used: 80, wantKey: usageKey, wantRaise: true},
		{name: "below 80 is quiet", used: 79.9},
		{name: "at 70 stays open", used: 70, active: map[string]bool{usageKey: true}},
		{name: "below 70 clears", used: 69.9, active: map[string]bool{usageKey: true}, wantKey: usageKey, wantClear: true},
		{name: "an open alert does not raise again", used: 90, active: map[string]bool{usageKey: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &diskDocker{ps: containerID('a'), df: map[string]string{containerID('a'): dfAnswer(tc.used)}}
			alerts := diskTarget(t, f.run).DiskAlerts(context.Background(), tc.active)
			if tc.wantKey == "" {
				if len(alerts) != 0 {
					t.Fatalf("alerts = %+v, want none at %v%%", alerts, tc.used)
				}
				return
			}
			got := onlyAlert(t, alerts)
			if got.Key != tc.wantKey || got.Raise != tc.wantRaise || got.Clear != tc.wantClear {
				t.Fatalf("alert = %+v, want key %q raise=%v clear=%v", got, tc.wantKey, tc.wantRaise, tc.wantClear)
			}
			if got.Name != readingName {
				t.Fatalf("alert names the disk %q, want %q", got.Name, readingName)
			}
		})
	}
}

// TestDiskAlertsUnreadableRaisesItsOwnKey drives each way the daemon's view can
// fail: every one raises the unreadable key and no usage alert.
func TestDiskAlertsUnreadableRaisesItsOwnKey(t *testing.T) {
	t.Parallel()
	running := containerID('a')
	// notAnID is daemon text that would be read happily if it reached the
	// command line: the row answers for it, so only the validation makes it
	// refuse.
	const notAnID = "../../etc/passwd"
	cases := []struct {
		name string
		f    *diskDocker
	}{
		{name: "the container list fails", f: &diskDocker{psErr: errors.New("daemon refused")}},
		{name: "no container runs", f: &diskDocker{}},
		{name: "an exec fails", f: &diskDocker{ps: running, dfErr: map[string]error{running: errors.New("no df")}}},
		{name: "df does not parse", f: &diskDocker{ps: running, df: map[string]string{running: "not a reading\n"}}},
		{name: "an id that is not an id", f: &diskDocker{ps: notAnID, df: map[string]string{notAnID: dfAnswer(90)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			alerts := diskTarget(t, tc.f.run).DiskAlerts(context.Background(), nil)
			got := onlyAlert(t, alerts)
			if got.Key != unreadableKey || !got.Raise || !got.Unreadable {
				t.Fatalf("alert = %+v, want a raise of %q", got, unreadableKey)
			}
			if got.Name != readingName {
				t.Fatalf("alert names the disk %q, want %q", got.Name, readingName)
			}
			for _, args := range tc.f.calls {
				if slices.Contains(args, notAnID) {
					t.Fatalf("call %v carries the unvalidated id", args)
				}
			}
		})
	}
}

// TestDiskAlertsUnreadableStaysOpenWhenAlreadyActive: an unreadable alert that is
// already open is reported as still unreadable rather than raised again.
func TestDiskAlertsUnreadableStaysOpenWhenAlreadyActive(t *testing.T) {
	t.Parallel()
	f := &diskDocker{psErr: errors.New("daemon refused")}
	alerts := diskTarget(t, f.run).DiskAlerts(context.Background(), map[string]bool{unreadableKey: true})
	got := onlyAlert(t, alerts)
	if got.Key != unreadableKey || got.Raise || !got.Still || !got.Unreadable {
		t.Fatalf("alert = %+v, want %q still open", got, unreadableKey)
	}
}

// TestReadingAsksTheDaemonsContainersAndTakesTheWorst: the share is the worst a
// container reports, every call names the cloud socket, and no call reads the
// Mac volume.
func TestReadingAsksTheDaemonsContainersAndTakesTheWorst(t *testing.T) {
	t.Parallel()
	roomy, tight := containerID('a'), containerID('b')
	f := &diskDocker{
		ps: tight + "\n" + roomy + "\n",
		df: map[string]string{
			roomy: dfAnswer(40),
			tight: dfAnswer(85),
		},
	}
	reading := diskTarget(t, f.run).Reading(context.Background())
	if reading.Err != nil {
		t.Fatalf("Reading = %v, want a share", reading.Err)
	}
	if reading.Name != readingName || reading.UsedPct != 85 || !reading.AlertOnly {
		t.Fatalf("Reading = %+v, want the worst share of %q, alert-only", reading, readingName)
	}
	want := [][]string{
		{"ps", "-q"},
		{"exec", tight, "df", "-P", "/"},
		{"exec", roomy, "df", "-P", "/"},
	}
	if !slices.EqualFunc(f.calls, want, slices.Equal[[]string]) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	for i, env := range f.envs {
		if !slices.Equal(env, []string{"DOCKER_HOST=" + dockerHost}) {
			t.Fatalf("call %v env = %v, want the cloud socket alone", f.calls[i], env)
		}
	}
	// The Mac volume holds the VM's disk image; df runs inside the VM, so no
	// argument names a host path.
	for _, args := range f.calls {
		for _, arg := range args {
			if strings.Contains(arg, ".colima") || strings.Contains(arg, "docker.img") {
				t.Fatalf("call %v names the Mac volume: %q", args, arg)
			}
		}
	}
}

// TestReadingRefusesAWholeContainerItCannotRead: a disk the daemon shows only
// partly may be the full one, so one failed container refuses the reading.
func TestReadingRefusesAWholeContainerItCannotRead(t *testing.T) {
	t.Parallel()
	roomy, broken := containerID('a'), containerID('b')
	f := &diskDocker{
		ps:    roomy + "\n" + broken + "\n",
		df:    map[string]string{roomy: dfAnswer(40)},
		dfErr: map[string]error{broken: errors.New("no df")},
	}
	if reading := diskTarget(t, f.run).Reading(context.Background()); reading.Err == nil {
		t.Fatalf("Reading = %+v, want an error for the unreadable container", reading)
	}
}
