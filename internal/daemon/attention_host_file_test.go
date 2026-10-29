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
	"github.com/steveyegge/gastown/internal/townconfig"
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

func TestFileHostProbesServeAFreshFile(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	townRoot := t.TempDir()
	registerTestRigs(t, townRoot, map[string]string{"gastown": "gt", hostHMRig: "hm"})
	if _, err := townconfig.Park(townRoot, hostHMRig, config.RigParked{Since: now}); err != nil {
		t.Fatalf("parking %s: %v", hostHMRig, err)
	}

	p := newFileHostProbes(townRoot)
	writeFileProbes(t, p, hostprobe.Probes{
		At:         now,
		MemFreePct: hostprobe.Field[float64]{Value: 42.5},
		Disk:       hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 71.5, FreeGB: 22.25}},
		Containers: hostprobe.Field[[]string]{Value: []string{"forgejo-forgejo-1", "forgejo-db-1"}},
		CPUIdlePct: hostprobe.Field[float64]{Value: 80},
		ExecTaxMS:  hostprobe.Field[float64]{Value: 5},
	})
	p.now = func() time.Time { return now.Add(time.Second) }

	disk, err := p.Disk()
	if err != nil || disk != (hostDisk{UsedPct: 71.5, FreeGB: 22.25}) {
		t.Errorf("Disk = %+v, %v; want the written disk", disk, err)
	}
	if free, err := p.MemFreePct(context.Background()); err != nil || free != 42.5 {
		t.Errorf("MemFreePct = %v, %v; want 42.5", free, err)
	}
	containers, err := p.Containers(context.Background())
	if err != nil || strings.Join(containers, ",") != "forgejo-forgejo-1,forgejo-db-1" {
		t.Errorf("Containers = %v, %v; want the written names", containers, err)
	}
	// RigParked is the town config's answer, not the file's.
	if parked, err := p.RigParked(hostHMRig); err != nil || !parked {
		t.Errorf("RigParked(%s) = %v, %v; want parked", hostHMRig, parked, err)
	}
	if parked, err := p.RigParked("gastown"); err != nil || parked {
		t.Errorf("RigParked(gastown) = %v, %v; want live", parked, err)
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
			p := newFileHostProbes(t.TempDir())
			if tt.write {
				writeFileProbes(t, p, hostprobe.Probes{
					At:         tt.at,
					MemFreePct: hostprobe.Field[float64]{Value: 42},
					Disk:       hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 30, FreeGB: 100}},
					Containers: hostprobe.Field[[]string]{Value: []string{"forgejo-db-1"}},
				})
			}
			p.now = func() time.Time { return now }
			// The three probes that read the file; RigParked reads the town
			// config, so a stopped agent costs it nothing.
			reads := []struct {
				probe string
				call  func() error
			}{
				{hostProbeDisk, func() error { _, err := p.Disk(); return err }},
				{hostProbeMem, func() error { _, err := p.MemFreePct(context.Background()); return err }},
				{hostProbeDocker, func() error { _, err := p.Containers(context.Background()); return err }},
			}
			for _, r := range reads {
				err := r.call()
				switch {
				case tt.serve && err != nil:
					t.Errorf("%s error = %v; want the file served inside the window", r.probe, err)
				case !tt.serve && !errors.Is(err, errProbeFileRefused):
					t.Errorf("%s error = %v; want a refused probe file", r.probe, err)
				}
			}
		})
	}
}

func TestFileHostProbesReturnOneFieldsErrorOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	const recorded = "the tool failed"
	tests := []struct {
		name    string
		setErr  func(*hostprobe.Probes)
		failing string
	}{
		{"disk", func(p *hostprobe.Probes) { p.Disk.Err = recorded }, hostProbeDisk},
		{"memory", func(p *hostprobe.Probes) { p.MemFreePct.Err = recorded }, hostProbeMem},
		{"containers", func(p *hostprobe.Probes) { p.Containers.Err = recorded }, hostProbeDocker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newFileHostProbes(t.TempDir())
			probes := hostprobe.Probes{
				At:         now,
				MemFreePct: hostprobe.Field[float64]{Value: 42},
				Disk:       hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 30, FreeGB: 100}},
				Containers: hostprobe.Field[[]string]{Value: []string{"forgejo-db-1"}},
			}
			tt.setErr(&probes)
			writeFileProbes(t, p, probes)
			p.now = func() time.Time { return now }

			disk, err := p.Disk()
			switch {
			case tt.failing == hostProbeDisk && (err == nil || !strings.Contains(err.Error(), recorded)):
				t.Errorf("Disk error = %v; want the recorded failure", err)
			case tt.failing != hostProbeDisk && (err != nil || disk.UsedPct != 30):
				t.Errorf("Disk = %+v, %v; want the written disk, since another field failed", disk, err)
			}
			mem, err := p.MemFreePct(context.Background())
			switch {
			case tt.failing == hostProbeMem && (err == nil || !strings.Contains(err.Error(), recorded)):
				t.Errorf("MemFreePct error = %v; want the recorded failure", err)
			case tt.failing != hostProbeMem && (err != nil || mem != 42):
				t.Errorf("MemFreePct = %v, %v; want the written value, since another field failed", mem, err)
			}
			containers, err := p.Containers(context.Background())
			switch {
			case tt.failing == hostProbeDocker && (err == nil || !strings.Contains(err.Error(), recorded)):
				t.Errorf("Containers error = %v; want the recorded failure", err)
			case tt.failing != hostProbeDocker && (err != nil || len(containers) != 1):
				t.Errorf("Containers = %v, %v; want the written names, since another field failed", containers, err)
			}
		})
	}
}

// A refused probe file reaches the operator: a container-mode tick with no
// readable file raises one probe-failed item per file probe in state.json and
// events.jsonl, through the production tick and the production selection.
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
	d.hostProbes = attentionHostProbes(town, func(string) string { return config.SupervisorContainer })
	if _, ok := d.hostProbes.(fileHostProbes); !ok {
		t.Fatalf("container mode selected %T, want fileHostProbes", d.hostProbes)
	}

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
	for _, probe := range []string{hostProbeDisk, hostProbeMem, hostProbeDocker} {
		it := itemByKey(t, st, hostProbeFailedPrefix+probe)
		if it.Kind != attention.KindHostProbe || it.Severity != attention.SeverityLow {
			t.Errorf("%s item = %+v, want a low host-probe item", probe, it)
		}
		if !strings.Contains(it.Summary, errProbeFileRefused.Error()) {
			t.Errorf("%s summary = %q, want the refusal named", probe, it.Summary)
		}
		if !eventKeys[it.Key] {
			t.Errorf("no event for %q; events = %+v", it.Key, events)
		}
	}
	for _, rule := range []string{hostRuleDiskWarn, hostRuleDiskCrit, hostRuleMemLow, hostRuleDockerDown} {
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
	})).(fileHostProbes); !ok {
		t.Errorf("container mode did not select fileHostProbes")
	}
	for _, mode := range []string{"", config.SupervisorLaunchd, config.SupervisorSystemd} {
		if _, ok := attentionHostProbes(townRoot, getenv(map[string]string{
			config.ContainerModeEnv: mode,
		})).(systemHostProbes); !ok {
			t.Errorf("mode %q did not select systemHostProbes", mode)
		}
	}
}
