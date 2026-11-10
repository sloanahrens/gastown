package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"strings"
	"sync"
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

// nodeRead is a node whose read succeeded with disk.
func nodeRead(node string, disk hostDisk) hostNodeRead {
	return hostNodeRead{hostNodeDisk: hostNodeDisk{Node: node, Disk: disk}}
}

// nodeReadFailed is a node whose read failed with its own error.
func nodeReadFailed(node string) hostNodeRead {
	return hostNodeRead{hostNodeDisk: hostNodeDisk{Node: node}, Err: errors.New("reading the stats summary: exit status 1")}
}

// One node that cannot be read is its own info item, and the other node is
// still ruled on its own figures: a critical neighbour is not withheld because
// the unreadable node failed, in either listing order.
func TestCollectNodeDiskOneUnreadableNodeStillRulesTheOthers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	crit := hostDisk{UsedPct: 95, FreeGB: 100}
	quiet := hostDisk{UsedPct: 40, FreeGB: 300}
	const worker = "colima-gt-worker"
	const control = "colima-gt-control"
	probeKey := hostProbeFailedPrefix + hostProbeNodeDisk + " " + worker
	controlProbeKey := hostProbeFailedPrefix + hostProbeNodeDisk + " " + control
	held := attention.Item{Key: hostRuleNodeDiskCrit, Kind: attention.KindHostNodeDisk, Severity: attention.SeverityHigh, Summary: "held"}
	tests := []struct {
		name  string
		reads []hostNodeRead
		prev  []attention.Item
		want  []string // the keys raised, in any order
	}{
		{
			name:  "critical node beside an unreadable one",
			reads: []hostNodeRead{nodeRead(control, crit), nodeReadFailed(worker)},
			want:  []string{hostRuleNodeDiskCrit, probeKey},
		},
		{
			name:  "unreadable node listed first",
			reads: []hostNodeRead{nodeReadFailed(worker), nodeRead(control, crit)},
			want:  []string{hostRuleNodeDiskCrit, probeKey},
		},
		{
			name:  "quiet node beside an unreadable one raises only the probe",
			reads: []hostNodeRead{nodeRead(control, quiet), nodeReadFailed(worker)},
			want:  []string{probeKey},
		},
		{
			name:  "held critical kept while the node that held it is unreadable",
			reads: []hostNodeRead{nodeRead(control, quiet), nodeReadFailed(worker)},
			prev:  []attention.Item{held},
			want:  []string{hostRuleNodeDiskCrit, probeKey},
		},
		{
			name:  "fresh critical replaces the held one of the same key, not a second item",
			reads: []hostNodeRead{nodeRead(control, crit), nodeReadFailed(worker)},
			prev:  []attention.Item{held},
			want:  []string{hostRuleNodeDiskCrit, probeKey},
		},
		{
			name:  "two unreadable nodes raise one probe item each",
			reads: []hostNodeRead{nodeReadFailed(worker), nodeReadFailed(control)},
			want:  []string{probeKey, controlProbeKey},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			h := healthyHost()
			h.nodeReads = tt.reads
			f.src.host = h
			f.src.prev = attention.State{Items: tt.prev}

			items := f.collect(t, f.src.collectHostNodeDisk)
			if len(items) != len(tt.want) {
				t.Fatalf("items = %+v, want the keys %v", items, tt.want)
			}
			for _, key := range tt.want {
				if _, ok := findItem(items, key); !ok {
					t.Errorf("items = %+v, want %s", items, key)
				}
			}
		})
	}
}

// The one rule item names the node at the highest level, and the fullest of
// those: a node under the free-space floor is critical though it is less full
// than a warning node beside it.
func TestCollectNodeDiskRulesTheWorstNode(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	const worker = "colima-gt-worker"
	const control = "colima-gt-control"
	tests := []struct {
		name     string
		reads    []hostNodeRead
		wantKey  string // "" for no rule item
		wantNode string
	}{
		{"critical by free space beats a fuller warning",
			[]hostNodeRead{nodeRead(control, hostDisk{UsedPct: 85, FreeGB: 100}), nodeRead(worker, hostDisk{UsedPct: 10, FreeGB: 4})},
			hostRuleNodeDiskCrit, worker},
		{"the fuller of two critical nodes",
			[]hostNodeRead{nodeRead(control, hostDisk{UsedPct: 92, FreeGB: 100}), nodeRead(worker, hostDisk{UsedPct: 95, FreeGB: 50})},
			hostRuleNodeDiskCrit, worker},
		{"two quiet nodes raise nothing",
			[]hostNodeRead{nodeRead(control, hostDisk{UsedPct: 40, FreeGB: 300}), nodeRead(worker, hostDisk{UsedPct: 10, FreeGB: 200})},
			"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			h := healthyHost()
			h.nodeReads = tt.reads
			f.src.host = h

			items := f.collect(t, f.src.collectHostNodeDisk)
			if tt.wantKey == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != tt.wantKey {
				t.Fatalf("items = %+v, want one %s", items, tt.wantKey)
			}
			if !strings.Contains(items[0].Summary, tt.wantNode) {
				t.Errorf("summary = %q, want the node %s named", items[0].Summary, tt.wantNode)
			}
		})
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
// Each node is read on its own: every node gets a read in the listed order, a
// reading carries that node's own figures, and a node that cannot be read
// carries its own error without the other nodes' reads being lost. The stderr
// text of a failed read never enters the error.
func TestControlNodeDiskReadsEachNodeOnItsOwn(t *testing.T) {
	t.Parallel()
	const secret = "token-from-stderr-must-not-appear"
	tests := []struct {
		name    string
		replies map[string]cliReply
		// want is each node's read, in listing order: the used share, or -1
		// for a read that must fail.
		want    []float64
		wantSub string // a fragment the failed read's error must carry
	}{
		{
			name: "both nodes read",
			replies: map[string]cliReply{
				"get --raw /api/v1/nodes/colima-gt-control/proxy/stats/summary": {stdout: nodeSummary("colima-gt-control", 100, 80)},
				"get --raw /api/v1/nodes/colima-gt-worker/proxy/stats/summary":  {stdout: nodeSummary("colima-gt-worker", 100, 10)},
			},
			want: []float64{20, 90},
		},
		{
			name: "one node unreadable",
			replies: map[string]cliReply{
				"get --raw /api/v1/nodes/colima-gt-control/proxy/stats/summary": {stdout: nodeSummary("colima-gt-control", 100, 80)},
				"get --raw /api/v1/nodes/colima-gt-worker/proxy/stats/summary":  {stderr: secret, code: 1},
			},
			want:    []float64{20, -1},
			wantSub: "reading the stats summary",
		},
		{
			name: "a summary cannot be parsed",
			replies: map[string]cliReply{
				"get --raw /api/v1/nodes/colima-gt-control/proxy/stats/summary": {stdout: "{"},
				"get --raw /api/v1/nodes/colima-gt-worker/proxy/stats/summary":  {stdout: nodeSummary("colima-gt-worker", 100, 10)},
			},
			want:    []float64{-1, 90},
			wantSub: "node stats summary",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cli := newFakeCLIFor(func(c cliCall) cliReply {
				if cacheTrimVerb(c.args) == "get nodes -o name" {
					return cliReply{stdout: "node/colima-gt-control\nnode/colima-gt-worker\n"}
				}
				return tt.replies[cacheTrimVerb(c.args)]
			})
			d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))

			reads, err := d.controlNodeDisk(context.Background())
			if err != nil {
				t.Fatalf("controlNodeDisk: %v, want the reads of both nodes and no whole-read error", err)
			}
			if len(reads) != len(tt.want) {
				t.Fatalf("reads = %+v, want %d", reads, len(tt.want))
			}
			for i, r := range reads {
				if tt.want[i] < 0 {
					if r.Err == nil {
						t.Errorf("read %s = %+v, want its own error", r.Node, r)
					}
					if strings.Contains(r.Err.Error(), secret) {
						t.Errorf("read %s error = %q, want no stderr text in it", r.Node, r.Err)
					}
					if !strings.Contains(r.Err.Error(), tt.wantSub) {
						t.Errorf("read %s error = %q, want it to carry %q", r.Node, r.Err, tt.wantSub)
					}
					continue
				}
				if r.Err != nil || math.Abs(r.Disk.UsedPct-tt.want[i]) > 1e-9 {
					t.Errorf("read %s = %+v, want %.0f%% used and no error", r.Node, r, tt.want[i])
				}
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
		})
	}
}

// Each refusal of the node listing fails the whole read, names what failed, and
// carries no stderr text: a listing that cannot be read, a listing that names no
// node or names something that is not a node. No node is read after it.
func TestControlNodeDiskRefusesAFailedListing(t *testing.T) {
	t.Parallel()
	const secret = "credential-from-stderr-must-not-appear"
	tests := []struct {
		name    string
		replies map[string]cliReply
		wantSub string // a fragment the error must carry
	}{
		{"listing fails", map[string]cliReply{"get nodes -o name": {stderr: secret, code: 1}}, "listing the control cluster's nodes"},
		{"listing names no node", map[string]cliReply{"get nodes -o name": {stdout: "\n"}}, "lists no nodes"},
		{"listing names a pod", map[string]cliReply{"get nodes -o name": {stdout: "pod/forgejo-0\n"}}, "want a node/name"},
		{"listing names a node that is not a name", map[string]cliReply{"get nodes -o name": {stdout: "node/Not_A_Node\n"}}, "not a node name"},
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
			if calls := cli.recorded(); len(calls) != 1 {
				t.Errorf("kubectl ran %d times, want the listing only", len(calls))
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

// nodeDiskTwoNodes answers the node listing and the two node summaries a
// two-node control cluster gives, and counts the listings it served.
func nodeDiskTwoNodes(listings *int) func(cliCall) cliReply {
	return func(c cliCall) cliReply {
		switch cacheTrimVerb(c.args) {
		case "get nodes -o name":
			*listings++
			return cliReply{stdout: "node/colima-gt-control\nnode/colima-gt-worker\n"}
		case "get --raw /api/v1/nodes/colima-gt-control/proxy/stats/summary":
			return cliReply{stdout: nodeSummary("colima-gt-control", 100, 80)}
		case "get --raw /api/v1/nodes/colima-gt-worker/proxy/stats/summary":
			return cliReply{stdout: nodeSummary("colima-gt-worker", 100, 10)}
		}
		return cliReply{}
	}
}

// Steady state is one summary read per node per tick: the names are listed once
// and reused, and every tick still reads each node's summary, so the figures
// are fresh.
func TestNodeDiskTickRunsOneSummaryReadPerNode(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	listings := 0
	cli := newFakeCLIFor(nodeDiskTwoNodes(&listings))
	d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))
	fc := clockwork.NewFakeClockAt(now)
	d.clock = fc

	if _, err := d.controlNodeDisk(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if got := len(cli.recorded()); got != 3 {
		t.Fatalf("first tick kubectl runs = %d, want the list and two summaries", got)
	}
	fc.Advance(3 * time.Minute)
	if _, err := d.controlNodeDisk(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := len(cli.recorded()); got != 5 {
		t.Errorf("two ticks kubectl runs = %d, want 3 + 2 summaries", got)
	}
	if listings != 1 {
		t.Errorf("listings = %d over two ticks, want the one listing", listings)
	}
}

// The held names are dropped after controlNodeNameReuse, so a node added to
// the cluster is read within the hour.
func TestNodeDiskNodeNamesExpireAfterTheReuseWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	listings := 0
	cli := newFakeCLIFor(nodeDiskTwoNodes(&listings))
	d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))
	fc := clockwork.NewFakeClockAt(now)
	d.clock = fc

	if _, err := d.controlNodeDisk(context.Background()); err != nil {
		t.Fatalf("first read: %v", err)
	}
	fc.Advance(controlNodeNameReuse - time.Minute)
	if _, err := d.controlNodeDisk(context.Background()); err != nil {
		t.Fatalf("read inside the window: %v", err)
	}
	if listings != 1 {
		t.Errorf("listings inside the window = %d, want 1", listings)
	}
	fc.Advance(time.Minute)
	if _, err := d.controlNodeDisk(context.Background()); err != nil {
		t.Fatalf("read at the window's end: %v", err)
	}
	if listings != 2 {
		t.Errorf("listings at the window's end = %d, want 2", listings)
	}
}

// A failed summary drops the held names, so the next tick lists the cluster
// again and a node that left it is not failed on every tick until the window
// ends.
func TestNodeDiskFailedSummaryDropsTheNodeNames(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	listings := 0
	failWorker := true
	cli := newFakeCLIFor(func(c cliCall) cliReply {
		if failWorker && cacheTrimVerb(c.args) == "get --raw /api/v1/nodes/colima-gt-worker/proxy/stats/summary" {
			return cliReply{code: 1}
		}
		return nodeDiskTwoNodes(&listings)(c)
	})
	d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))
	d.clock = clockwork.NewFakeClockAt(now)

	reads, err := d.controlNodeDisk(context.Background())
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(reads) != 2 || reads[1].Err == nil {
		t.Fatalf("first read = %+v, want the worker's own failed read", reads)
	}
	failWorker = false
	if _, err := d.controlNodeDisk(context.Background()); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if listings != 2 {
		t.Errorf("listings = %d, want the names listed again after the failure", listings)
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
	want := hostNodeRead{hostNodeDisk: hostNodeDisk{Node: "colima-gt-control", Disk: hostDisk{UsedPct: 12, FreeGB: 200}}}
	p := newFileHostProbes(t.TempDir(), nil, func(context.Context) ([]hostNodeRead, error) { return []hostNodeRead{want}, nil })
	got, err := p.NodeDisks(context.Background())
	if err != nil || len(got) != 1 || got[0] != want {
		t.Errorf("NodeDisks = %+v, %v; want [%+v], nil", got, err, want)
	}

	unwired := newFileHostProbes(t.TempDir(), nil, nil)
	if _, err := unwired.NodeDisks(context.Background()); err == nil {
		t.Errorf("NodeDisks on a probe built with no reader = nil error, want one")
	}
}

// In container mode the daemon runs in the control cluster, so a town whose
// tier sweep runner names no kubernetes cluster cannot read its node disk. That
// is a failed probe, never the not-configured sentinel that raises nothing.
func TestFileHostProbesNodeDiskNoRunnerIsAFailure(t *testing.T) {
	t.Parallel()
	p := newFileHostProbes(t.TempDir(), nil, func(context.Context) ([]hostNodeRead, error) {
		return nil, errHostNodeNotConfigured
	})
	_, err := p.NodeDisks(context.Background())
	if err == nil {
		t.Fatalf("NodeDisks = nil error, want a failed read")
	}
	if errors.Is(err, errHostNodeNotConfigured) {
		t.Errorf("NodeDisks err = %v, want a read failure and not the not-configured sentinel", err)
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

// TestControlNodeDiskGivesEachRunItsOwnDeadline: the listing and each node's
// summary read get a deadline of their own, every one derived from the caller's
// context and none from another run's, so a node that hangs spends only its
// own budget (gt-x1po96, review round 3).
func TestControlNodeDiskGivesEachRunItsOwnDeadline(t *testing.T) {
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
		return cliReply{code: 1}
	})
	d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))
	type parentKey struct{}
	parent := context.WithValue(context.Background(), parentKey{}, "caller")
	var mu sync.Mutex
	var derivedFromCaller, derivedFromAnother int
	d.hostNodeReadContextFn = func(p context.Context) (context.Context, context.CancelFunc) {
		mu.Lock()
		defer mu.Unlock()
		if p == parent {
			derivedFromCaller++
		} else {
			derivedFromAnother++
		}
		return context.WithTimeout(p, attentionHostProbeTimeout)
	}

	reads, err := d.controlNodeDisk(parent)
	if err != nil {
		t.Fatalf("controlNodeDisk: %v", err)
	}
	if len(reads) != 2 || reads[0].Err != nil || reads[1].Err != nil {
		t.Fatalf("reads = %+v, want two clean reads", reads)
	}
	mu.Lock()
	defer mu.Unlock()
	if derivedFromCaller != 3 || derivedFromAnother != 0 {
		t.Errorf("deadlines: %d from the caller's context, %d from another run's; want 3 (the listing and two reads) and 0", derivedFromCaller, derivedFromAnother)
	}
}
