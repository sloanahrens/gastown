package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
)

// fakeHost is a hostProbes whose answers a test sets. The zero value is not
// healthy; healthyHost is.
type fakeHost struct {
	disk    hostDisk
	diskErr error
	// node is the control node's disk and nodeErr the read's failure. A zero
	// node with no error is a town with no control cluster: a reading always
	// names its node. nodeReads, when set, is the per-node reads in full, for a
	// test where one node fails and another reads.
	node                hostNodeDisk
	nodeErr             error
	nodeReads           []hostNodeRead
	nodeCalls           int
	memFree             float64
	memErr              error
	forgejoErr          error
	diskCalls, memCalls int
	forgejoCalls        int
}

// noForgejo is a town that runs no Forgejo: the production health probe built
// over it makes no request, so a test that drives the daemon's own tick
// reaches no network.
func noForgejo() (string, error) { return "", nil }

// noNodeDisk is a town with no control node read: the production probe built
// over it reports the node disk not configured.
func noNodeDisk(context.Context) ([]hostNodeRead, error) {
	return nil, errHostNodeNotConfigured
}

// healthyHost is a host no rule fires on.
func healthyHost() *fakeHost {
	return &fakeHost{
		disk:    hostDisk{UsedPct: 40, FreeGB: 300},
		node:    hostNodeDisk{Node: "colima-gt-control", Disk: hostDisk{UsedPct: 40, FreeGB: 300}},
		memFree: 60,
	}
}

func (f *fakeHost) Disk() (hostDisk, error) {
	f.diskCalls++
	return f.disk, f.diskErr
}

// NodeDisks answers with nodeReads when a test sets them, and otherwise with the
// one node in node: a zero node with no error is a town with no control cluster.
func (f *fakeHost) NodeDisks(context.Context) ([]hostNodeRead, error) {
	f.nodeCalls++
	if f.nodeReads != nil {
		return f.nodeReads, nil
	}
	if f.nodeErr != nil {
		return nil, f.nodeErr
	}
	if f.node.Node == "" {
		return nil, errHostNodeNotConfigured
	}
	return []hostNodeRead{{hostNodeDisk: f.node}}, nil
}

func (f *fakeHost) MemFreePct(context.Context) (float64, error) {
	f.memCalls++
	return f.memFree, f.memErr
}

func (f *fakeHost) ForgejoHealthy(context.Context) error {
	f.forgejoCalls++
	return f.forgejoErr
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
		{"forgejo", func(h *fakeHost) {
			h.forgejoErr = errors.New("forgejo health https://forgejo.example/api/healthz: no route to host")
		}, hostProbeForgejo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			// The two probes with a rule to fire each have it firing, so the
			// test can see they were not hidden by the failure. The Forgejo
			// probe has none of its own: a healthy instance raises nothing.
			h := &fakeHost{
				disk:    hostDisk{UsedPct: 95, FreeGB: 3},
				memFree: 5,
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
			rules := map[string]string{hostProbeDisk: hostRuleDiskCrit, hostProbeMem: hostRuleMemLow}
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
	h.memFree = 5
	f.src.host = h
	st := f.tick(t, now)
	first := itemByKey(t, st, hostRuleMemLow)

	h.memFree, h.memErr = 0, errors.New("exec: \"memory_pressure\": not found")
	st = f.tick(t, now.Add(time.Minute))
	kept := itemByKey(t, st, hostRuleMemLow)
	if !kept.FirstSeen.Equal(first.FirstSeen) {
		t.Errorf("FirstSeen = %v, want the original %v", kept.FirstSeen, first.FirstSeen)
	}
	itemByKey(t, st, hostProbeFailedPrefix+hostProbeMem)

	h.memFree, h.memErr = 60, nil
	st = f.tick(t, now.Add(2*time.Minute))
	if keys := hostKeys(st); len(keys) != 0 {
		t.Errorf("host items = %v, want both cleared once memory reads healthy", keys)
	}
}

// The collectors are registered: the daemon's own tick, with fake probes
// injected, writes the host items to state.json and events.jsonl.
func TestAttentionHostCollectorsAreRegistered(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()
	h := &fakeHost{
		disk:    hostDisk{UsedPct: 82, FreeGB: 80},
		memFree: 9,
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
	for _, key := range []string{hostRuleDiskWarn, hostRuleMemLow} {
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
	if len(classes) != 2 {
		t.Errorf("event classes = %v, want the two host rules raised", classes)
	}
	if h.diskCalls != 1 || h.memCalls != 1 || h.forgejoCalls != 1 {
		t.Errorf("probe calls = disk %d mem %d forgejo %d, want one each per tick",
			h.diskCalls, h.memCalls, h.forgejoCalls)
	}
}

// The host probe's memory reading goes through the shared parser.
func TestAttentionHostMemFreePctReadsSharedParser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     []byte
		runErr  error
		want    float64
		wantErr bool
	}{
		{"reading", []byte("System-wide memory free percentage: 9%"), nil, 9, false},
		{"failed run", nil, errors.New("exit status 1"), 0, true},
		{"no figure is an error, not zero", []byte("The system has 1 pages\n"), nil, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := memFreePctFrom(tt.out, tt.runErr)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("memFreePctFrom = %v, %v; want %v, err %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
