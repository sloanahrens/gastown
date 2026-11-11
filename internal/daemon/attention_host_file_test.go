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
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

// writeFileProbes publishes p through hostprobe.Write at the path the file
// probes read, so a test that spells a value the wrong way there fails in both
// the writer and the reader.
func writeFileProbes(t *testing.T, p fileHostProbes, probes hostprobe.Probes) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := hostprobe.Write(p.path, probes); err != nil {
		t.Fatalf("writing the probe file: %v", err)
	}
}

// cgroupFiles writes the named cgroup memory files into a fresh temp dir and
// returns it: the cgroup a test points the memory read at, so the read never
// touches the machine's own cgroup. A name absent from files is an absent file.
func cgroupFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFileHostProbesServeAFreshFile(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	p := newFileHostProbes(t.TempDir(), nil, nil)
	writeFileProbes(t, p, hostprobe.Probes{
		At:         now,
		MemFreePct: hostprobe.Field[float64]{Value: 42.5},
		Disk:       hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 71.5, FreeGB: 22.25}},
		CPUIdlePct: hostprobe.Field[float64]{Value: 80},
		ExecTaxMS:  hostprobe.Field[float64]{Value: 5},
	})
	p.now = func() time.Time { return now.Add(time.Second) }
	p.cgroupDir = cgroupFiles(t, map[string]string{"memory.max": "1000", "memory.current": "250"})
	p.rootCgroupDir = t.TempDir()

	disk, err := p.Disk()
	if err != nil || disk != (hostDisk{UsedPct: 71.5, FreeGB: 22.25}) {
		t.Errorf("Disk = %+v, %v; want the written disk", disk, err)
	}
	// The file's memory field says 42.5, and the reading is the pod's cgroup,
	// which has 750 of 1000 bytes free: the file's memory field is not read.
	if free, err := p.MemFreePct(context.Background()); err != nil || free != 75 {
		t.Errorf("MemFreePct = %v, %v; want 75 from the cgroup", free, err)
	}
}

func TestFileHostProbesRefuseAMissingOrStaleFile(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		write bool
		at    time.Time
		serve bool
	}{
		{name: "missing file"},
		{name: "stale file", write: true, at: now.Add(-fileHostProbeMaxAge - time.Second)},
		// Just inside the window, so the stale row above is the age and not
		// the file write that made it refuse.
		{name: "file one second inside the window", write: true, at: now.Add(-fileHostProbeMaxAge + time.Second), serve: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newFileHostProbes(t.TempDir(), nil, nil)
			if tt.write {
				writeFileProbes(t, p, hostprobe.Probes{
					At:   tt.at,
					Disk: hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 30, FreeGB: 100}},
				})
			}
			p.now = func() time.Time { return now }

			_, err := p.Disk()
			switch {
			case tt.serve && err != nil:
				t.Errorf("Disk error = %v; want the file served inside the window", err)
			case !tt.serve && !errors.Is(err, errProbeFileRefused):
				t.Errorf("Disk error = %v; want a refused probe file", err)
			}
		})
	}
}

// A field a probe recorded as failed is that field's error only. The memory
// reading does not come from the file at all, so a failed memory field in the
// file leaves the cgroup reading served.
func TestFileHostProbesReturnOneFieldsErrorOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	const recorded = "the tool failed"
	p := newFileHostProbes(t.TempDir(), nil, nil)
	writeFileProbes(t, p, hostprobe.Probes{
		At:         now,
		MemFreePct: hostprobe.Field[float64]{Err: recorded},
		Disk:       hostprobe.Field[hostprobe.Disk]{Err: recorded},
	})
	p.now = func() time.Time { return now }
	p.cgroupDir = cgroupFiles(t, map[string]string{"memory.max": "1000", "memory.current": "250"})
	p.rootCgroupDir = t.TempDir()

	if _, err := p.Disk(); err == nil || !strings.Contains(err.Error(), recorded) {
		t.Errorf("Disk error = %v; want the recorded failure", err)
	}
	if free, err := p.MemFreePct(context.Background()); err != nil || free != 75 {
		t.Errorf("MemFreePct = %v, %v; want 75, since the file's failed memory field is not read", free, err)
	}
}

// A probe built without its cgroup refuses the memory read. Without the guard,
// an empty dir would read memory.current from the working directory.
func TestFileHostProbesMemoryNeedsItsCgroup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pod     string
		root    string
		wantErr bool
	}{
		{"no cgroup dir", "", t.TempDir(), true},
		{"no root cgroup dir with a pod limit fallback", t.TempDir(), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := fileHostProbes{cgroupDir: tt.pod, rootCgroupDir: tt.root}
			if _, err := p.MemFreePct(context.Background()); err == nil || !strings.Contains(err.Error(), "no cgroup") {
				t.Errorf("MemFreePct error = %v, want the missing cgroup named", err)
			}
		})
	}
}

// The memory reading is the pod's cgroup: free share of memory.max less
// memory.current, with the root cgroup's limit when the pod has none. Anything
// that cannot be measured is an error, never a reading of 0 or 100.
func TestMemFreePctFromCgroup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pod     map[string]string
		root    map[string]string
		want    float64
		wantErr bool
	}{
		{"limit set", map[string]string{"memory.max": "1000", "memory.current": "250"}, nil, 75, false},
		{"limit set, trailing newline", map[string]string{"memory.max": "1000\n", "memory.current": "250\n"}, nil, 75, false},
		{"limit set, nothing free", map[string]string{"memory.max": "1000", "memory.current": "1000"}, nil, 0, false},
		{"limit set, over the limit is nothing free", map[string]string{"memory.max": "1000", "memory.current": "1200"}, nil, 0, false},
		{"limit max falls back to the root limit", map[string]string{"memory.max": "max", "memory.current": "250"}, map[string]string{"memory.max": "1000"}, 75, false},
		{"limit max and root limit max", map[string]string{"memory.max": "max", "memory.current": "250"}, map[string]string{"memory.max": "max"}, 0, true},
		{"limit max and no root limit file", map[string]string{"memory.max": "max", "memory.current": "250"}, nil, 0, true},
		{"limit file unreadable", map[string]string{"memory.current": "250"}, nil, 0, true},
		{"current file unreadable", map[string]string{"memory.max": "1000"}, nil, 0, true},
		{"limit neither a count nor max", map[string]string{"memory.max": "lots", "memory.current": "250"}, nil, 0, true},
		{"current neither a count nor max", map[string]string{"memory.max": "1000", "memory.current": "lots"}, nil, 0, true},
		{"current reads max", map[string]string{"memory.max": "1000", "memory.current": "max"}, nil, 0, true},
		{"zero limit is no measure", map[string]string{"memory.max": "0", "memory.current": "0"}, nil, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := memFreePctFromCgroup(cgroupFiles(t, tt.pod), cgroupFiles(t, tt.root))
			if (err != nil) != tt.wantErr {
				t.Fatalf("memFreePctFromCgroup error = %v, want error %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("memFreePctFromCgroup = %v; want %v", got, tt.want)
			}
		})
	}
}

// The disk stays on the probe file. A fresh file that sets only disk, with every
// other field marked not probed, yields the disk reading and no failed-probe
// item for memory or containers: memory is the pod's cgroup, and the container
// probe is gone.
func TestHostDiskStillReadsTheProbeFile(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	const notProbed = "not probed"
	// Container mode reads the control node through the cluster, so a tick
	// needs a node read that answers; a healthy one keeps it out of the way.
	nodes := func(context.Context) ([]hostNodeRead, error) {
		return []hostNodeRead{{hostNodeDisk: hostNodeDisk{Node: "colima-gt-control", Disk: hostDisk{UsedPct: 40, FreeGB: 300}}}}, nil
	}
	town := t.TempDir()
	p := newFileHostProbes(town, noForgejo, nodes)
	writeFileProbes(t, p, hostprobe.Probes{
		At:           now,
		MemFreePct:   hostprobe.Field[float64]{Err: notProbed},
		Disk:         hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 85, FreeGB: 60}},
		DockerDisk:   hostprobe.Field[hostprobe.Disk]{Err: notProbed},
		Containers:   hostprobe.Field[[]string]{Err: notProbed},
		CPUIdlePct:   hostprobe.Field[float64]{Err: notProbed},
		TopConsumers: hostprobe.Field[[]hostprobe.Consumer]{Err: notProbed},
		ExecTaxMS:    hostprobe.Field[float64]{Err: notProbed},
		Launchd:      &hostprobe.Field[hostprobe.Launchd]{Err: notProbed},
	})
	p.now = func() time.Time { return now.Add(time.Second) }
	p.cgroupDir = cgroupFiles(t, map[string]string{"memory.max": "1000", "memory.current": "250"})
	p.rootCgroupDir = t.TempDir()

	f := newAttentionFixture(t, now)
	f.src.host = p

	disk := f.collect(t, f.src.collectHostDisk)
	if len(disk) != 1 || disk[0].Key != hostRuleDiskWarn {
		t.Errorf("disk items = %+v, want the warning from the probe file's disk field", disk)
	}
	mem := f.collect(t, f.src.collectHostMem)
	if len(mem) != 0 {
		t.Errorf("memory items = %+v, want none: 75%% of the pod's memory is free", mem)
	}
	st := f.tick(t, now)
	for _, it := range hostItems(st) {
		if strings.HasPrefix(it.Key, hostProbeFailedPrefix) {
			t.Errorf("failed-probe item %q raised; want none for a fresh file with disk set", it.Key)
		}
	}
}

// A refused probe file reaches the operator: a container-mode tick with no
// readable file raises one probe-failed item for the disk, the one reading
// still taken from the file, through the production tick and the production
// selection. The memory reading is the cgroup, so it raises no probe item here.
//
// It raises no host rule. A refused file is an unknown reading, so it must not
// read as healthy (which is why the refusal is an error) and must not read as
// critical (which would page for a stopped agent).
func TestAttentionHostRefusedProbeFileRaisesProbeAlarms(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()
	d := &Daemon{
		config:        &Config{TownRoot: town},
		logger:        log.New(&bytes.Buffer{}, "", 0),
		clock:         clockwork.NewFakeClockAt(now),
		openWorkBeads: (&labelBeads{}).open,
	}
	// The mode is in the daemon's environment in production; the injected
	// getenv selects the same branch without a process-wide setenv, which a
	// parallel test in this package would see.
	d.hostProbes = attentionHostProbes(town, func(string) string { return config.SupervisorContainer }, noForgejo, noNodeDisk)
	fp, ok := d.hostProbes.(fileHostProbes)
	if !ok {
		t.Fatalf("container mode selected %T, want fileHostProbes", d.hostProbes)
	}
	// Memory is read from the cgroup, which this test points at a healthy one
	// so the memory reading is served and cannot stand in for the refusal.
	fp.cgroupDir = cgroupFiles(t, map[string]string{"memory.max": "1000", "memory.current": "250"})
	fp.rootCgroupDir = t.TempDir()
	d.hostProbes = fp

	d.writeAttention()

	st, err := attention.ReadState(town)
	if err != nil {
		t.Fatal(err)
	}
	events, err := attention.ReadEvents(town)
	if err != nil {
		t.Fatal(err)
	}
	eventKeys := make(map[string]bool, len(events))
	for _, e := range events {
		eventKeys[e.Key] = true
	}
	it := itemByKey(t, st, hostProbeFailedPrefix+hostProbeDisk)
	if it.Kind != attention.KindHostProbe || it.Severity != attention.SeverityLow {
		t.Errorf("%s item = %+v, want a low host-probe item", hostProbeDisk, it)
	}
	if !strings.Contains(it.Summary, errProbeFileRefused.Error()) {
		t.Errorf("%s summary = %q, want the refusal named", hostProbeDisk, it.Summary)
	}
	if !eventKeys[it.Key] {
		t.Errorf("no event for %q; events = %+v", it.Key, events)
	}
	if _, ok := attention.Find(st, hostProbeFailedPrefix+hostProbeMem); ok {
		t.Errorf("memory probe item raised; the cgroup reading is served and should not fail")
	}
	for _, rule := range []string{hostRuleDiskWarn, hostRuleDiskCrit, hostRuleMemLow} {
		if it, ok := attention.Find(st, rule); ok {
			t.Errorf("rule %s raised on a refused probe file: %+v", rule, it)
		}
	}
}

func TestAttentionHostProbesSelectByMode(t *testing.T) {
	t.Parallel()
	const townRoot = "/town"
	getenv := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if _, ok := attentionHostProbes(townRoot, getenv(map[string]string{
		config.ContainerModeEnv: config.SupervisorContainer,
	}), noForgejo, noNodeDisk).(fileHostProbes); !ok {
		t.Errorf("container mode did not select fileHostProbes")
	}
	for _, mode := range []string{"", config.SupervisorLaunchd, config.SupervisorSystemd} {
		if _, ok := attentionHostProbes(townRoot, getenv(map[string]string{
			config.ContainerModeEnv: mode,
		}), noForgejo, noNodeDisk).(systemHostProbes); !ok {
			t.Errorf("mode %q did not select systemHostProbes", mode)
		}
	}
}
