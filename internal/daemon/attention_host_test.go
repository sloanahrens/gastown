package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
)

// fakeHost is a hostProbes whose answers a test sets. The zero value is not
// healthy; healthyHost is.
type fakeHost struct {
	disk                hostDisk
	diskErr             error
	memFree             float64
	memErr              error
	containers          []string
	containersErr       error
	diskCalls, memCalls int
	containerCalls      int
	// parked and parkedErr are the hm rig's park state; the zero value is a
	// live rig.
	parked    bool
	parkedErr error
}

// allStackContainers is every container a live hm rig requires.
func allStackContainers() []string {
	return append(append([]string(nil), hostStackContainers...), hostHMContainer)
}

func (f *fakeHost) RigParked(rig string) (bool, error) {
	if rig != hostHMRig {
		return false, errors.New("unexpected rig " + rig)
	}
	return f.parked, f.parkedErr
}

// healthyHost is a host no rule fires on.
func healthyHost() *fakeHost {
	return &fakeHost{
		disk:       hostDisk{UsedPct: 40, FreeGB: 300},
		memFree:    60,
		containers: allStackContainers(),
	}
}

func (f *fakeHost) Disk() (hostDisk, error) {
	f.diskCalls++
	return f.disk, f.diskErr
}

func (f *fakeHost) MemFreePct(context.Context) (float64, error) {
	f.memCalls++
	return f.memFree, f.memErr
}

func (f *fakeHost) Containers(context.Context) ([]string, error) {
	f.containerCalls++
	return f.containers, f.containersErr
}

// hostItems are the items of the state that name a host rule or probe.
func hostItems(st attention.State) []attention.Item {
	var out []attention.Item
	for _, it := range st.Items {
		if strings.HasPrefix(it.Key, "host.") {
			out = append(out, it)
		}
	}
	return out
}

func hostKeys(st attention.State) []string {
	var keys []string
	for _, it := range hostItems(st) {
		keys = append(keys, it.Key)
	}
	return keys
}

// Each disk rule fires at its edge on either condition and is silent just
// under it; the worse rule replaces the milder, never joins it.
func TestAttentionHostDisk(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		disk hostDisk
		want string // rule key, "" for silence
		sev  attention.Severity
	}{
		{"healthy", hostDisk{UsedPct: 79.9, FreeGB: 12}, "", ""},
		{"warn at 80% used", hostDisk{UsedPct: 80, FreeGB: 100}, hostRuleDiskWarn, attention.SeverityLow},
		{"warn under 12 GB free", hostDisk{UsedPct: 10, FreeGB: 11.9}, hostRuleDiskWarn, attention.SeverityLow},
		{"exactly 12 GB free is silent", hostDisk{UsedPct: 10, FreeGB: 12}, "", ""},
		{"crit at 90% used", hostDisk{UsedPct: 90, FreeGB: 100}, hostRuleDiskCrit, attention.SeverityHigh},
		{"just under 90% used is warn", hostDisk{UsedPct: 89.9, FreeGB: 100}, hostRuleDiskWarn, attention.SeverityLow},
		{"crit under 5 GB free", hostDisk{UsedPct: 10, FreeGB: 4.9}, hostRuleDiskCrit, attention.SeverityHigh},
		{"exactly 5 GB free is warn", hostDisk{UsedPct: 10, FreeGB: 5}, hostRuleDiskWarn, attention.SeverityLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			h := healthyHost()
			h.disk = tt.disk
			f.src.host = h
			items := f.collect(t, f.src.collectHostDisk)
			if tt.want == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one", items)
			}
			it := items[0]
			if it.Key != tt.want || it.Kind != attention.KindHostDisk || it.Severity != tt.sev {
				t.Errorf("item = %+v, want key %s severity %s", it, tt.want, tt.sev)
			}
		})
	}
}

// The event text names the numbers and points at the runbook, and no
// directory sizes are computed.
func TestAttentionHostDiskSummaryNamesTheNumbers(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	h := healthyHost()
	h.disk = hostDisk{UsedPct: 91.4, FreeGB: 18.26}
	f.src.host = h
	items := f.collect(t, f.src.collectHostDisk)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	if got, want := items[0].Summary, "disk 91% used, 18.3 GB free; see docker-watch runbook"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestAttentionHostMem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		free float64
		want bool
	}{
		{"plenty", 60, false},
		{"exactly 15% is silent", 15, false},
		{"just under 15%", 14.9, true},
		{"nearly none", 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			h := healthyHost()
			h.memFree = tt.free
			f.src.host = h
			items := f.collect(t, f.src.collectHostMem)
			if !tt.want {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != hostRuleMemLow || items[0].Kind != attention.KindHostMem {
				t.Fatalf("items = %+v, want %s", items, hostRuleMemLow)
			}
			if !strings.Contains(items[0].Summary, "free") {
				t.Errorf("summary = %q, want the free percentage named", items[0].Summary)
			}
		})
	}
}

// The stack is down when any of the five containers is not running, and the
// event names which.
func TestAttentionHostDocker(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	without := func(drop ...string) []string {
		var out []string
		for _, n := range allStackContainers() {
			keep := true
			for _, d := range drop {
				if n == d {
					keep = false
				}
			}
			if keep {
				out = append(out, n)
			}
		}
		return out
	}
	tests := []struct {
		name    string
		running []string
		missing string // substring of the summary, "" for silence
	}{
		{"all running", without(), ""},
		{"extra containers are fine", append(without(), "unrelated-1"), ""},
		{"forgejo down", without("forgejo-forgejo-1"), "forgejo-forgejo-1"},
		{"db down", without("forgejo-db-1"), "forgejo-db-1"},
		{"runner down", without("forgejo-runner-daemon-1"), "forgejo-runner-daemon-1"},
		{"dind down", without("forgejo-docker-in-docker-1"), "forgejo-docker-in-docker-1"},
		{"hm-postgres down", without("hm-postgres"), "hm-postgres"},
		{"two down", without("forgejo-db-1", "hm-postgres"), "2 of 5"},
		{"nothing running", nil, "5 of 5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			h := healthyHost()
			h.containers = tt.running
			f.src.host = h
			items := f.collect(t, f.src.collectHostDocker)
			if tt.missing == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one", items)
			}
			it := items[0]
			if it.Key != hostRuleDockerDown || it.Kind != attention.KindHostDocker || it.Severity != attention.SeverityHigh {
				t.Errorf("item = %+v", it)
			}
			if !strings.Contains(it.Summary, tt.missing) {
				t.Errorf("summary = %q, want it to contain %q", it.Summary, tt.missing)
			}
		})
	}
}

// hm-postgres is required only while the hm rig is live. A parked rig, or a
// park state that cannot be read, drops it from the required set; the Forgejo
// four stay required in every case.
func TestAttentionHostDockerHMPostgresFollowsParkState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	forgejoOnly := func(drop ...string) []string {
		var out []string
		for _, n := range hostStackContainers {
			dropped := false
			for _, d := range drop {
				dropped = dropped || n == d
			}
			if !dropped {
				out = append(out, n)
			}
		}
		return out
	}
	unreadable := errors.New("rig hm is not registered")
	tests := []struct {
		name      string
		parked    bool
		parkedErr error
		running   []string
		summary   string // exact summary, "" for silence
	}{
		{"parked, hm-postgres off, forgejo up", true, nil, forgejoOnly(), ""},
		{"parked, hm-postgres on, forgejo up", true, nil, allStackContainers(), ""},
		{"parked, forgejo-db-1 missing", true, nil, forgejoOnly("forgejo-db-1"),
			"1 of 4 stack containers not running: forgejo-db-1"},
		{"parked, two forgejo missing, hm-postgres off", true, nil, forgejoOnly("forgejo-db-1", "forgejo-forgejo-1"),
			"2 of 4 stack containers not running: forgejo-forgejo-1, forgejo-db-1"},
		{"live, hm-postgres missing", false, nil, forgejoOnly(),
			"1 of 5 stack containers not running: hm-postgres"},
		{"live, all up", false, nil, allStackContainers(), ""},
		{"live, forgejo-db-1 and hm-postgres missing", false, nil, forgejoOnly("forgejo-db-1"),
			"2 of 5 stack containers not running: forgejo-db-1, hm-postgres"},
		{"unreadable park state treats hm-postgres as not required", false, unreadable, forgejoOnly(), ""},
		{"unreadable park state still requires forgejo", false, unreadable, forgejoOnly("forgejo-runner-daemon-1"),
			"1 of 4 stack containers not running: forgejo-runner-daemon-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			h := healthyHost()
			h.parked, h.parkedErr, h.containers = tt.parked, tt.parkedErr, tt.running
			f.src.host = h
			items := f.collect(t, f.src.collectHostDocker)
			if tt.summary == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one", items)
			}
			it := items[0]
			if it.Key != hostRuleDockerDown || it.Kind != attention.KindHostDocker || it.Severity != attention.SeverityHigh {
				t.Errorf("item = %+v", it)
			}
			if it.Summary != tt.summary {
				t.Errorf("summary = %q, want %q", it.Summary, tt.summary)
			}
		})
	}
}

// A healthy host raises nothing and a tick on it writes no host events.
func TestAttentionHostHealthyProducesNoEvents(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.host = healthyHost()
	st := f.tick(t, now)
	if keys := hostKeys(st); len(keys) != 0 {
		t.Errorf("host items = %v, want none", keys)
	}
	if got := f.events(t); len(got) != 0 {
		t.Errorf("events = %+v, want none", got)
	}
}

// A failing probe raises one info item naming it, does not panic, and leaves
// the other probes' rules in force.
func TestAttentionHostFailingProbeIsNamedAndIsolated(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		break_ func(h *fakeHost)
		probe  string
	}{
		{"disk", func(h *fakeHost) { h.diskErr = errors.New("statfs /town: no such file") }, hostProbeDisk},
		{"memory", func(h *fakeHost) { h.memErr = errors.New("exec: \"memory_pressure\": not found") }, hostProbeMem},
		{"docker", func(h *fakeHost) { h.containersErr = errors.New("Cannot connect to the Docker daemon\nIs it running?") }, hostProbeDocker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			// The other two probes each have a rule firing, so the test can
			// see they were not hidden by the failure.
			h := &fakeHost{
				disk:       hostDisk{UsedPct: 95, FreeGB: 3},
				memFree:    5,
				containers: nil,
			}
			tt.break_(h)
			f.src.host = h
			st := f.tick(t, now)

			probe := itemByKey(t, st, hostProbeFailedPrefix+tt.probe)
			if probe.Severity != attention.SeverityLow || probe.Kind != attention.KindHostProbe {
				t.Errorf("probe item = %+v, want a low host-probe item", probe)
			}
			if want := "host probe " + tt.probe + " failed: "; !strings.HasPrefix(probe.Summary, want) {
				t.Errorf("summary = %q, want prefix %q", probe.Summary, want)
			}
			if strings.Contains(probe.Summary, "\n") {
				t.Errorf("summary = %q, want one line", probe.Summary)
			}
			failed := 0
			for _, it := range hostItems(st) {
				if strings.HasPrefix(it.Key, hostProbeFailedPrefix) {
					failed++
				}
			}
			if failed != 1 {
				t.Errorf("host items = %v, want exactly one failed-probe item", hostKeys(st))
			}
			rules := map[string]string{hostProbeDisk: hostRuleDiskCrit, hostProbeMem: hostRuleMemLow, hostProbeDocker: hostRuleDockerDown}
			for probe, rule := range rules {
				_, ok := attention.Find(st, rule)
				if probe == tt.probe && ok {
					t.Errorf("rule %s raised on a failed probe", rule)
				}
				if probe != tt.probe && !ok {
					t.Errorf("rule %s hidden by the %s probe failing; host items = %v", rule, tt.probe, hostKeys(st))
				}
			}
		})
	}
}

// A probe that fails after its rule fired does not clear the rule: the
// condition is unknown, not gone. It clears only on a good probe that says so.
func TestAttentionHostFailedProbeKeepsTheRuleItHeld(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	h.containers = nil
	f.src.host = h
	st := f.tick(t, now)
	first := itemByKey(t, st, hostRuleDockerDown)

	h.containers, h.containersErr = nil, errors.New("docker.sock: connection refused")
	st = f.tick(t, now.Add(time.Minute))
	kept := itemByKey(t, st, hostRuleDockerDown)
	if !kept.FirstSeen.Equal(first.FirstSeen) {
		t.Errorf("FirstSeen = %v, want the original %v", kept.FirstSeen, first.FirstSeen)
	}
	itemByKey(t, st, hostProbeFailedPrefix+hostProbeDocker)

	h.containers, h.containersErr = allStackContainers(), nil
	st = f.tick(t, now.Add(2*time.Minute))
	if keys := hostKeys(st); len(keys) != 0 {
		t.Errorf("host items = %v, want both cleared once docker answers healthy", keys)
	}
}

// The collectors are registered: the daemon's own tick, with fake probes
// injected, writes the host items to state.json and events.jsonl.
func TestAttentionHostCollectorsAreRegistered(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()
	h := &fakeHost{
		disk:       hostDisk{UsedPct: 82, FreeGB: 80},
		memFree:    9,
		containers: []string{"forgejo-forgejo-1"},
	}
	logs := &bytes.Buffer{}
	d := &Daemon{
		config:     &Config{TownRoot: town},
		logger:     log.New(logs, "", 0),
		clock:      clockwork.NewFakeClockAt(now),
		hostProbes: h,
		// The bead reads the other collectors make are answered in-process.
		openWorkBeads: (&labelBeads{}).open,
	}

	d.writeAttention()

	st, err := attention.ReadState(town)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{hostRuleDiskWarn, hostRuleMemLow, hostRuleDockerDown} {
		itemByKey(t, st, key)
	}
	events, err := attention.ReadEvents(town)
	if err != nil {
		t.Fatal(err)
	}
	var classes []attention.Kind
	for _, e := range events {
		if strings.HasPrefix(e.Key, "host.") {
			classes = append(classes, e.Class)
		}
	}
	if len(classes) != 3 {
		t.Errorf("event classes = %v, want the three host rules raised", classes)
	}
	if h.diskCalls != 1 || h.memCalls != 1 || h.containerCalls != 1 {
		t.Errorf("probe calls = disk %d mem %d docker %d, want one each per tick", h.diskCalls, h.memCalls, h.containerCalls)
	}
}

// The memory-pressure parser lives in internal/hostprobe, which the host agent
// reads it from too (gt-qpeei.6). A copy left in this package would drift from
// the shared one, so the host probe reads the shared function and no second
// parser remains here.
func TestAttentionHostReadsSharedMemoryParser(t *testing.T) {
	t.Parallel()

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	callsShared := false
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "parseMemoryPressure(") {
			t.Errorf("%s defines or calls parseMemoryPressure; the host probe reads hostprobe.ParseMemoryPressure", src)
		}
		if src == "attention_host.go" && strings.Contains(string(body), "hostprobe.ParseMemoryPressure(") {
			callsShared = true
		}
	}
	if !callsShared {
		t.Error("attention_host.go does not call hostprobe.ParseMemoryPressure")
	}
}

func TestAttentionHostParseContainerNames(t *testing.T) {
	t.Parallel()
	got := parseContainerNames("forgejo-db-1\n  hm-postgres \n\nforgejo-forgejo-1\n")
	want := []string{"forgejo-db-1", "hm-postgres", "forgejo-forgejo-1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", got, want)
	}
	if got := parseContainerNames(""); len(got) != 0 {
		t.Errorf("names = %v, want none", got)
	}
}

// A docker ps that errors on one tick is an info line; one that errors on three
// ticks in a row is the stack down, and wakes once. A good probe in between
// starts the count again.
func TestHostDockerProbeFailurePersistsWakes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	f.src.host = h
	wakes := func() []attention.Event {
		var out []attention.Event
		for _, e := range f.events(t) {
			if e.Wake {
				out = append(out, e)
			}
		}
		return out
	}
	beat := 0
	tick := func() attention.State {
		beat++
		return f.tick(t, now.Add(time.Duration(beat)*time.Minute))
	}

	h.containersErr = errors.New("Cannot connect to the Docker daemon")
	tick()
	if got := wakes(); len(got) != 0 {
		t.Fatalf("one failed tick wake events = %+v, want none", got)
	}

	// The probe recovers, so the next failures count from one again.
	h.containersErr = nil
	tick()
	h.containersErr = errors.New("Cannot connect to the Docker daemon")
	tick()
	st := tick()
	if got := wakes(); len(got) != 0 {
		t.Fatalf("two failed ticks after a good one wake events = %+v, want none", got)
	}
	if _, ok := attention.Find(st, hostRuleDockerDown); ok {
		t.Fatalf("host items = %v, want no stack-down item before the third failure", hostKeys(st))
	}

	st = tick()
	got := wakes()
	if len(got) != 1 {
		t.Fatalf("third consecutive failed tick wake events = %+v, want one", got)
	}
	if got[0].Key != hostRuleDockerDown || got[0].Handled != attention.HandledOperatorAction || got[0].State != attention.EventNew {
		t.Errorf("wake event = %+v, want a raised host.docker_stack_down", got[0])
	}
	down := itemByKey(t, st, hostRuleDockerDown)
	if !strings.Contains(down.Summary, "Cannot connect to the Docker daemon") || strings.Contains(down.Summary, "\n") {
		t.Errorf("summary = %q, want the one-line probe error", down.Summary)
	}
	itemByKey(t, st, hostProbeFailedPrefix+hostProbeDocker)

	tick()
	tick()
	if n := len(wakes()); n != 1 {
		t.Errorf("wake events after five failed ticks = %d, want the one", n)
	}

	h.containersErr = nil
	st = tick()
	if keys := hostKeys(st); len(keys) != 0 {
		t.Errorf("host items = %v, want both cleared once docker answers healthy", keys)
	}
}

// A stack-down item the probe kept from a real missing-container tick is
// replaced by the unreachable one when the failure persists, never doubled.
func TestHostDockerProbeFailureReplacesKeptStackDown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	h.containers = nil
	f.src.host = h
	f.tick(t, now)

	h.containersErr = errors.New("docker.sock: connection refused")
	var st attention.State
	for beat := 1; beat <= attentionDockerProbeDownBeats; beat++ {
		st = f.tick(t, now.Add(time.Duration(beat)*time.Minute))
	}
	n := 0
	for _, it := range st.Items {
		if it.Key == hostRuleDockerDown {
			n++
			if !strings.Contains(it.Summary, "connection refused") {
				t.Errorf("summary = %q, want the unreachable wording", it.Summary)
			}
		}
	}
	if n != 1 {
		t.Errorf("stack-down items = %d, want 1", n)
	}
}

// A docker ps that fails because the tick's own context is spent says nothing
// about docker, so three such ticks are not three beats toward the stack being
// down.
func TestHostDockerProbeExpiredContextNotCounted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	h.containersErr = context.DeadlineExceeded
	f.src.host = h

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for beat := 0; beat < attentionDockerProbeDownBeats+1; beat++ {
		items, err := f.src.collectHostDocker(expired)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Key == hostRuleDockerDown {
				t.Fatalf("beat %d raised %s on a context that was already expired", beat+1, hostRuleDockerDown)
			}
		}
	}
	if f.src.dockerProbeFails != 0 {
		t.Errorf("dockerProbeFails = %d, want the expired ticks uncounted", f.src.dockerProbeFails)
	}

	// A docker ps that fails on a live context still counts.
	h.containersErr = errors.New("Cannot connect to the Docker daemon")
	var items []attention.Item
	for beat := 0; beat < attentionDockerProbeDownBeats; beat++ {
		items, _ = f.src.collectHostDocker(context.Background())
	}
	found := false
	for _, it := range items {
		found = found || it.Key == hostRuleDockerDown
	}
	if !found {
		t.Errorf("items = %+v, want %s after %d real failures", items, hostRuleDockerDown, attentionDockerProbeDownBeats)
	}
}
