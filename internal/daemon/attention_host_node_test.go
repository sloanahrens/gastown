package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// nodeSummary is a kubelet stats summary that names node and reports its image
// filesystem with the given byte counts. The imageFs usedBytes it carries
// disagrees with those counts, so a reading that used it would come out wrong.
func nodeSummary(node string, capacity, available int64) string {
	return fmt.Sprintf(`{"node":{"nodeName":%q,"fs":{"capacityBytes":1,"usedBytes":1},`+
		`"runtime":{"imageFs":{"capacityBytes":%d,"availableBytes":%d,"usedBytes":1}}}}`,
		node, capacity, available)
}

// nodeDiskPatrol is a town whose tier sweep runner is the kubernetes backend on
// kubeContext: the control cluster the node read goes through.
func nodeDiskPatrol(kubeContext string) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &PatrolsConfig{
			TierSweep: &agentconfig.TierSweepConfig{
				Enabled: true,
				Runner: &agentconfig.TierSweepRunnerConfig{
					Backend: agentconfig.TierSweepRunnerKubernetes,
					Context: kubeContext,
				},
			},
		},
	}
}

// nodeDiskDaemon is a daemon whose kubectl calls run through cli and whose
// environment is fixed, so the test reads no host state.
func nodeDiskDaemon(t *testing.T, cli *fakeCLI, patrols *DaemonPatrolConfig) *Daemon {
	t.Helper()
	d := &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(io.Discard, "", 0),
		patrolConfig: patrols,
	}
	d.execCmd = cli.run
	d.diskMaint.getenv = func(string) string { return "" }
	d.diskMaint.environ = func() []string { return []string{"PATH=/usr/bin", "HOME=/home/polecat"} }
	return d
}

// The node's disk is critical at 90% used or under 5 GB free, and one critical
// item raises whichever condition holds, never both as two items.
func TestCollectNodeDiskCritical(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		disk hostDisk
	}{
		{"crit at 90% used", hostDisk{UsedPct: 90, FreeGB: 100}},
		{"crit under 5 GB free", hostDisk{UsedPct: 10, FreeGB: 4.9}},
		{"crit when both hold", hostDisk{UsedPct: 95, FreeGB: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			h := healthyHost()
			h.node = hostNodeDisk{Node: "colima-gt-control", Disk: tt.disk}
			f.src.host = h
			items := f.collect(t, f.src.collectHostNodeDisk)
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one", items)
			}
			it := items[0]
			if it.Key != hostRuleNodeDiskCrit || it.Kind != attention.KindHostNodeDisk || it.Severity != attention.SeverityHigh {
				t.Errorf("item = %+v, want a high %s host-node-disk item", it, hostRuleNodeDiskCrit)
			}
			if !strings.Contains(it.Summary, "colima-gt-control") {
				t.Errorf("summary = %q, want the node named", it.Summary)
			}
		})
	}
}

// The warning sits at 80% used or under 12 GB free, and is silent just short of
// both edges.
func TestCollectNodeDiskWarning(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		disk hostDisk
		want string // the rule key, "" for silence
	}{
		{"healthy", hostDisk{UsedPct: 79.9, FreeGB: 12}, ""},
		{"exactly 12 GB free is silent", hostDisk{UsedPct: 10, FreeGB: 12}, ""},
		{"warn at 80% used", hostDisk{UsedPct: 80, FreeGB: 100}, hostRuleNodeDiskWarn},
		{"warn under 12 GB free", hostDisk{UsedPct: 10, FreeGB: 11.9}, hostRuleNodeDiskWarn},
		{"just under 90% used is warn", hostDisk{UsedPct: 89.9, FreeGB: 100}, hostRuleNodeDiskWarn},
		{"exactly 5 GB free is warn", hostDisk{UsedPct: 10, FreeGB: 5}, hostRuleNodeDiskWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			h := healthyHost()
			h.node = hostNodeDisk{Node: "colima-gt-control", Disk: tt.disk}
			f.src.host = h
			items := f.collect(t, f.src.collectHostNodeDisk)
			if tt.want == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one", items)
			}
			if it := items[0]; it.Key != tt.want || it.Severity != attention.SeverityLow {
				t.Errorf("item = %+v, want a low %s item", it, tt.want)
			}
		})
	}
}

// A node read that fails is the info probe line, not an alarm, and it keeps the
// node rule the previous tick held, as the other host probes do. A town with no
// control cluster raises nothing at all.
func TestCollectNodeDiskProbeFailureIsAnInfoItem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	h.nodeErr = errors.New("kubectl get --raw: the server could not find the requested resource")
	f.src.host = h
	held := attention.Item{Key: hostRuleNodeDiskCrit, Kind: attention.KindHostNodeDisk, Severity: attention.SeverityHigh, Summary: "held"}
	f.src.prev = attention.State{Items: []attention.Item{held}}

	items := f.collect(t, f.src.collectHostNodeDisk)
	probe, ok := findItem(items, hostProbeFailedPrefix+hostProbeNodeDisk)
	if !ok {
		t.Fatalf("items = %+v, want the failed node disk probe", items)
	}
	if probe.Kind != attention.KindHostProbe || probe.Severity != attention.SeverityLow {
		t.Errorf("probe item = %+v, want a low host-probe item", probe)
	}
	if want := "host probe " + hostProbeNodeDisk + " failed: "; !strings.HasPrefix(probe.Summary, want) {
		t.Errorf("summary = %q, want prefix %q", probe.Summary, want)
	}
	if _, ok := findItem(items, hostRuleNodeDiskCrit); !ok {
		t.Errorf("items = %+v, want the held %s kept while the probe fails", items, hostRuleNodeDiskCrit)
	}
}

func TestCollectNodeDiskNotConfiguredRaisesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	h.nodeErr = errHostNodeNotConfigured
	f.src.host = h
	if items := f.collect(t, f.src.collectHostNodeDisk); len(items) != 0 {
		t.Errorf("items = %+v, want none for a town with no control cluster", items)
	}
}

// The kubelet's imageFs is the reading: the used share is the capacity less the
// bytes available, and the free space is read in GiB.
func TestParseNodeStatsImageFs(t *testing.T) {
	t.Parallel()
	const capacity, available = int64(105356935168), int64(41629769728)
	tests := []struct {
		name    string
		summary string
		used    float64 // the expected used share, when the summary is valid
		free    float64 // the expected free GiB
		wantErr bool
	}{
		// A control node's figures, the capacity and available bytes of its image filesystem.
		{name: "control node", summary: nodeSummary("colima-gt-control", capacity, available), used: 60.4869203327, free: 38.7707443237},
		{name: "full available is 0% used", summary: nodeSummary("n", 100, 100), used: 0, free: 100.0 / (1 << 30)},
		{name: "no bytes available is 100% used", summary: nodeSummary("n", 1<<30, 0), used: 100, free: 0},
		{name: "no imageFs", summary: `{"node":{"nodeName":"n","runtime":{}}}`, wantErr: true},
		{name: "no runtime", summary: `{"node":{"nodeName":"n","fs":{"capacityBytes":10,"usedBytes":1}}}`, wantErr: true},
		{name: "no node name", summary: `{"node":{"runtime":{"imageFs":{"capacityBytes":10,"availableBytes":5}}}}`, wantErr: true},
		{name: "no availableBytes", summary: `{"node":{"nodeName":"n","runtime":{"imageFs":{"capacityBytes":10}}}}`, wantErr: true},
		{name: "no capacityBytes", summary: `{"node":{"nodeName":"n","runtime":{"imageFs":{"availableBytes":5}}}}`, wantErr: true},
		{name: "zero capacity", summary: nodeSummary("n", 0, 0), wantErr: true},
		{name: "available above capacity", summary: nodeSummary("n", 10, 11), wantErr: true},
		{name: "negative available", summary: nodeSummary("n", 10, -1), wantErr: true},
		{name: "fractional bytes", summary: `{"node":{"nodeName":"n","runtime":{"imageFs":{"capacityBytes":1.5,"availableBytes":1}}}}`, wantErr: true},
		{name: "not JSON", summary: `{`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseNodeStatsImageFs([]byte(tt.summary))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseNodeStatsImageFs = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNodeStatsImageFs: %v", err)
			}
			if math.Abs(got.Disk.UsedPct-tt.used) > 1e-6 || math.Abs(got.Disk.FreeGB-tt.free) > 1e-6 {
				t.Errorf("disk = %+v, want %.6f%% used and %.6f GB free", got.Disk, tt.used, tt.free)
			}
		})
	}
}

// The control node is the name the summary gives, so the probe names the node
// it read, not one a caller guessed.
func TestParseNodeStatsImageFsNamesTheNode(t *testing.T) {
	t.Parallel()
	got, err := parseNodeStatsImageFs([]byte(nodeSummary("colima-gt-control", 100, 50)))
	if err != nil {
		t.Fatalf("parseNodeStatsImageFs: %v", err)
	}
	if got.Node != "colima-gt-control" {
		t.Errorf("Node = %q, want colima-gt-control", got.Node)
	}
}

// The read asks every node of the control cluster through the context the
// runner names, and the fullest node answers.
func TestControlNodeDiskReadsTheFullestNode(t *testing.T) {
	t.Parallel()
	cli := newFakeCLIFor(func(c cliCall) cliReply {
		switch cacheTrimVerb(c.args) {
		case "get nodes -o name":
			return cliReply{stdout: "node/colima-gt-control\nnode/colima-gt-worker\n"}
		case "get --raw /api/v1/nodes/colima-gt-control/proxy/stats/summary":
			return cliReply{stdout: nodeSummary("colima-gt-control", 100, 80)}
		case "get --raw /api/v1/nodes/colima-gt-worker/proxy/stats/summary":
			return cliReply{stdout: nodeSummary("colima-gt-worker", 100, 10)}
		}
		return cliReply{}
	})
	d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))

	got, err := d.controlNodeDisk(context.Background())
	if err != nil {
		t.Fatalf("controlNodeDisk: %v", err)
	}
	if got.Node != "colima-gt-worker" || math.Abs(got.Disk.UsedPct-90) > 1e-9 {
		t.Errorf("reading = %+v, want the worker at 90%% used", got)
	}
	calls := cli.recorded()
	if len(calls) != 3 {
		t.Fatalf("kubectl calls = %d, want the list and two reads", len(calls))
	}
	for _, c := range calls {
		if len(c.args) < 4 || c.args[0] != "--kubeconfig" || c.args[2] != "--context" || c.args[3] != ciCacheTestContext {
			t.Errorf("kubectl args = %v, want every call to name the control context", c.args)
		}
	}
}

// One node that cannot be read fails the whole read: the other node's figure is
// not a reading of the control node, and the error carries no stderr text.
func TestControlNodeDiskFailsWholeWhenANodeCannotBeRead(t *testing.T) {
	t.Parallel()
	const secret = "token-from-stderr-must-not-appear"
	cli := newFakeCLIFor(func(c cliCall) cliReply {
		switch cacheTrimVerb(c.args) {
		case "get nodes -o name":
			return cliReply{stdout: "node/colima-gt-control\nnode/colima-gt-worker\n"}
		case "get --raw /api/v1/nodes/colima-gt-control/proxy/stats/summary":
			return cliReply{stdout: nodeSummary("colima-gt-control", 100, 80)}
		case "get --raw /api/v1/nodes/colima-gt-worker/proxy/stats/summary":
			return cliReply{stderr: secret, code: 1}
		}
		return cliReply{}
	})
	d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))

	got, err := d.controlNodeDisk(context.Background())
	if err == nil {
		t.Fatalf("controlNodeDisk = %+v, want an error for the unreadable node", got)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error = %q, want no stderr text in it", err)
	}
	if !strings.Contains(err.Error(), "colima-gt-worker") {
		t.Errorf("error = %q, want the node that failed named", err)
	}
}

// Each refusal of the read fails the read, names what failed, and carries no
// stderr text: a listing that cannot be read, a listing that names no node or
// names something that is not a node, and a summary that cannot be parsed.
func TestControlNodeDiskRefusesAFailedListingOrSummary(t *testing.T) {
	t.Parallel()
	const secret = "credential-from-stderr-must-not-appear"
	const summaryPath = "get --raw /api/v1/nodes/colima-gt-control/proxy/stats/summary"
	tests := []struct {
		name    string
		replies map[string]cliReply
		wantSub string // a fragment the error must carry
	}{
		{"listing fails", map[string]cliReply{"get nodes -o name": {stderr: secret, code: 1}}, "listing the control cluster's nodes"},
		{"listing names no node", map[string]cliReply{"get nodes -o name": {stdout: "\n"}}, "lists no nodes"},
		{"listing names a pod", map[string]cliReply{"get nodes -o name": {stdout: "pod/forgejo-0\n"}}, "want a node/name"},
		{"listing names a node that is not a name", map[string]cliReply{"get nodes -o name": {stdout: "node/Not_A_Node\n"}}, "not a node name"},
		{"a summary cannot be parsed", map[string]cliReply{
			"get nodes -o name": {stdout: "node/colima-gt-control\n"},
			summaryPath:         {stdout: "{"},
		}, "node colima-gt-control"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cli := newFakeCLIFor(func(c cliCall) cliReply { return tt.replies[cacheTrimVerb(c.args)] })
			d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))
			got, err := d.controlNodeDisk(context.Background())
			if err == nil {
				t.Fatalf("controlNodeDisk = %+v, want an error", got)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %q, want it to carry %q", err, tt.wantSub)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error = %q, want no stderr text in it", err)
			}
		})
	}
}

// A town with no control cluster to read is not configured, and an empty
// context the runner names is refused rather than read, so no kubectl runs in
// either case.
func TestControlNodeDiskRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		patrols *DaemonPatrolConfig
		want    error
	}{
		{"no kubernetes runner", nil, errHostNodeNotConfigured},
		{"empty context", nodeDiskPatrol(""), errCICacheTrimNoContext},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cli := newFakeCLIFor(func(cliCall) cliReply { return cliReply{} })
			d := nodeDiskDaemon(t, cli, tt.patrols)
			if _, err := d.controlNodeDisk(context.Background()); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if calls := cli.recorded(); len(calls) != 0 {
				t.Errorf("kubectl ran %d times, want none", len(calls))
			}
		})
	}
}

// The control node's read comes through the daemon's own probe and its item
// reaches the state file, so the collector is wired into the tick.
func TestAttentionHostNodeDiskIsRegistered(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	town := t.TempDir()
	h := healthyHost()
	h.node = hostNodeDisk{Node: "colima-gt-control", Disk: hostDisk{UsedPct: 95, FreeGB: 3}}
	d := &Daemon{
		config:        &Config{TownRoot: town},
		logger:        log.New(io.Discard, "", 0),
		clock:         clockwork.NewFakeClockAt(now),
		hostProbes:    h,
		openWorkBeads: (&labelBeads{}).open,
	}

	d.writeAttention()

	st, err := attention.ReadState(town)
	if err != nil {
		t.Fatal(err)
	}
	itemByKey(t, st, hostRuleNodeDiskCrit)
	if h.nodeCalls != 1 {
		t.Errorf("node disk reads = %d, want one per tick", h.nodeCalls)
	}
}

// A file probe reads the node through the read it was built with, and refuses
// to read it when it was built with none.
func TestFileHostProbesNodeDiskUsesItsReader(t *testing.T) {
	t.Parallel()
	want := hostNodeDisk{Node: "colima-gt-control", Disk: hostDisk{UsedPct: 12, FreeGB: 200}}
	p := newFileHostProbes(t.TempDir(), nil, func(context.Context) (hostNodeDisk, error) { return want, nil })
	got, err := p.NodeDisk(context.Background())
	if err != nil || got != want {
		t.Errorf("NodeDisk = %+v, %v; want %+v, nil", got, err, want)
	}

	unwired := newFileHostProbes(t.TempDir(), nil, nil)
	if _, err := unwired.NodeDisk(context.Background()); err == nil {
		t.Errorf("NodeDisk on a probe built with no reader = nil error, want one")
	}
}

// In container mode the daemon runs in the control cluster, so a town whose
// tier sweep runner names no kubernetes cluster cannot read its node disk. That
// is a failed probe, never the not-configured sentinel that raises nothing.
func TestFileHostProbesNodeDiskNoRunnerIsAFailure(t *testing.T) {
	t.Parallel()
	p := newFileHostProbes(t.TempDir(), nil, func(context.Context) (hostNodeDisk, error) {
		return hostNodeDisk{}, errHostNodeNotConfigured
	})
	_, err := p.NodeDisk(context.Background())
	if err == nil {
		t.Fatalf("NodeDisk = nil error, want a failed read")
	}
	if errors.Is(err, errHostNodeNotConfigured) {
		t.Errorf("NodeDisk err = %v, want a read failure and not the not-configured sentinel", err)
	}
}

// Container mode with no kubernetes runner reads as an info item from the
// production wiring, so a silent probe cannot pass as a healthy disk. The
// daemon makes no kubectl call: the runner names no context to read through.
func TestCollectNodeDiskContainerModeNoRunnerIsAnInfoItem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	cli := newFakeCLIFor(func(cliCall) cliReply { return cliReply{} })
	d := nodeDiskDaemon(t, cli, nil)
	inContainer := func(key string) string {
		if key == agentconfig.ContainerModeEnv {
			return agentconfig.SupervisorContainer
		}
		return ""
	}
	f.src.host = attentionHostProbes(d.config.TownRoot, inContainer, func() (string, error) { return "", nil }, d.controlNodeDisk)

	items := f.collect(t, f.src.collectHostNodeDisk)
	if _, ok := findItem(items, hostProbeFailedPrefix+hostProbeNodeDisk); !ok {
		t.Fatalf("items = %+v, want a failed control node disk probe item", items)
	}
	if calls := cli.recorded(); len(calls) != 0 {
		t.Errorf("kubectl ran %d times, want none", len(calls))
	}
}

// findItem is the item with key among items, if there is one.
func findItem(items []attention.Item, key string) (attention.Item, bool) {
	for _, it := range items {
		if it.Key == key {
			return it, true
		}
	}
	return attention.Item{}, false
}
