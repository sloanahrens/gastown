package daemon

// The control node's data disk for the host attention rules (gt-x1po96). The
// town-root rule reads the volume the town lives on, and the control-disk
// patrol reads the node's root filesystem (node.fs). Neither reads the volume
// the kubelet evicts on: the image filesystem, which holds the container
// runtime's store and the local-path claims. That figure is node.runtime.imageFs
// in the node's stats summary, read through the control cluster's kubectl, the
// way the cache trims read it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// controlNodeNameReuse is how long the control cluster's node names are reused
// before the next listing. Only the names are held, never a reading: a stale
// node list costs a node until the window ends, and a stale disk reading would
// hide the fill the rules exist to catch (gt-x1po96).
const controlNodeNameReuse = time.Hour

// hostNodeDisk is the control node's image filesystem as the rules read it: the
// node that answered and the volume's usage.
type hostNodeDisk struct {
	Node string
	Disk hostDisk
}

// hostNodeRead is one control node's read: its reading, or the error that
// stopped it. Each node is ruled on its own figures, and a node that could not
// be read is its own info item, so one node's failure never withholds another
// node's alarm (gt-x1po96).
type hostNodeRead struct {
	hostNodeDisk
	Err error
}

// nodeStatsImageFs is the part of the kubelet stats summary the node disk
// reading takes: the node's name and the image filesystem its container runtime
// reports. The byte counts stay json.Number, so an absent field is not read as
// a zero count.
type nodeStatsImageFs struct {
	Node struct {
		NodeName string `json:"nodeName"`
		Runtime  *struct {
			ImageFs *struct {
				CapacityBytes  json.Number `json:"capacityBytes"`
				AvailableBytes json.Number `json:"availableBytes"`
			} `json:"imageFs"`
		} `json:"runtime"`
	} `json:"node"`
}

// parseNodeStatsImageFs reads the node's image filesystem from a kubelet stats
// summary. The used share is the capacity less the bytes available, not the
// usedBytes the kubelet reports: usedBytes is the kubelet's own accounting and
// does not say how full the volume is. The free space is in GiB, as the
// town-root rule reads it.
//
// A summary without the node's name, without node.runtime.imageFs, without
// either byte count, with a capacity that is not positive, or with an available
// count outside 0 to the capacity is an error and never a reading: a disk the
// probe cannot measure must not read as an empty one.
func parseNodeStatsImageFs(summary []byte) (hostNodeDisk, error) {
	var stats nodeStatsImageFs
	if err := json.Unmarshal(summary, &stats); err != nil {
		return hostNodeDisk{}, fmt.Errorf("node stats summary: %w", err)
	}
	name := stats.Node.NodeName
	if name == "" {
		return hostNodeDisk{}, errors.New("node stats summary names no node")
	}
	if stats.Node.Runtime == nil || stats.Node.Runtime.ImageFs == nil {
		return hostNodeDisk{}, fmt.Errorf("node %s stats summary names no node.runtime.imageFs", name)
	}
	fs := stats.Node.Runtime.ImageFs
	capacity, err := imageFsBytes(name, "capacityBytes", fs.CapacityBytes)
	if err != nil {
		return hostNodeDisk{}, err
	}
	available, err := imageFsBytes(name, "availableBytes", fs.AvailableBytes)
	if err != nil {
		return hostNodeDisk{}, err
	}
	if capacity <= 0 {
		return hostNodeDisk{}, fmt.Errorf("node %s imageFs capacityBytes = %d, want a positive capacity", name, capacity)
	}
	// Compare the counts as integers before any division, as disks.ParseNodeStats
	// does, so a count above 2^53 bytes cannot round to a share over 100.
	if available < 0 || available > capacity {
		return hostNodeDisk{}, fmt.Errorf("node %s imageFs availableBytes = %d, outside 0 to capacityBytes = %d", name, available, capacity)
	}
	used := capacity - available
	return hostNodeDisk{
		Node: name,
		Disk: hostDisk{
			UsedPct: float64(used) / float64(capacity) * 100,
			FreeGB:  float64(available) / (1 << 30),
		},
	}, nil
}

// imageFsBytes requires a byte count the node's imageFs names, whole and not
// absent.
func imageFsBytes(node, field string, value json.Number) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("node %s imageFs names no %s", node, field)
	}
	n, err := value.Int64()
	if err != nil {
		return 0, fmt.Errorf("node %s imageFs %s = %q: %w", node, field, value, err)
	}
	return n, nil
}

// controlNodeNames holds the node names last listed through one kubernetes
// context, for controlNodeNameReuse. The attention tick runs one at a time, so
// the mutex only guards against a second caller.
type controlNodeNames struct {
	mu      sync.Mutex
	context string
	names   []string
	listed  time.Time
}

// held returns the names listed through kubeContext that are still inside the
// reuse window at now, or nil when none are held: the context changed, the
// window ended, or nothing was listed.
func (c *controlNodeNames) held(kubeContext string, now time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names == nil || c.context != kubeContext || now.Sub(c.listed) >= controlNodeNameReuse {
		return nil
	}
	return c.names
}

// hold keeps the names a listing through kubeContext at now returned.
func (c *controlNodeNames) hold(kubeContext string, names []string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.context, c.names, c.listed = kubeContext, names, now
}

// forget drops the held names, so the next read lists the cluster again.
func (c *controlNodeNames) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = nil
}

// parseControlNodeNames reads the names from kubectl get nodes -o name, one
// node/<name> line each. A line in any other form, or a name that is not a node
// name, is an error, because the name becomes part of a request path. A listing
// with no node is an error too: the nodes whose disk to read are unknown.
func parseControlNodeNames(out []byte) ([]string, error) {
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, ok := strings.CutPrefix(line, "node/")
		if !ok {
			return nil, fmt.Errorf("kubectl get nodes named %q, want a node/name", line)
		}
		if !validSandboxNodeName(name) {
			return nil, fmt.Errorf("kubectl get nodes named %q, which is not a node name", name)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("the control cluster lists no nodes")
	}
	return names, nil
}

// controlNodeDisk reads the control node's image filesystem through the control
// cluster the tier sweep runner names: the same context the CI cache trim
// resolves. A town with no kubernetes runner has no control node and reports
// errHostNodeNotConfigured. The read returns one hostNodeRead per node: a node
// whose summary cannot be read carries its own error, and the other nodes'
// readings are still returned, so each node is ruled on its own figures.
// Only a failed listing fails the whole read.
//
// The node names are listed once per controlNodeNameReuse and held between
// ticks; every tick reads each node's summary fresh. A tick in which any node's
// read fails drops the held names, so a node that left the cluster is not read
// again for the rest of the window.
//
// Every kubectl run, the listing and each node's summary, gets its own
// deadline from ctx (hostNodeReadContext): a node that hangs spends only its
// own budget, and the nodes read after it still get theirs.
//
// Nothing the kubectl run printed to stderr enters an error, so a credential or
// a path in it never reaches the queue.
func (d *Daemon) controlNodeDisk(ctx context.Context) ([]hostNodeRead, error) {
	kubeContext, configured, err := ciCacheTrimContext(d.patrol())
	if !configured {
		return nil, errHostNodeNotConfigured
	}
	if err != nil {
		return nil, err
	}
	kubeconfig := diskMaintKubeconfig(d.config.TownRoot, d.diskMaintenanceSeams().getenv)
	now := d.clk().Now()
	names := d.controlNodes.held(kubeContext, now)
	if names == nil {
		lctx, lcancel := d.hostNodeReadContext(ctx)
		listing, _, err := d.ciCacheTrimKubectlRun(lctx, kubeconfig, kubeContext, "get", "nodes", "-o", "name")
		lcancel()
		if err != nil {
			return nil, fmt.Errorf("listing the control cluster's nodes: %w", err)
		}
		names, err = parseControlNodeNames(listing)
		if err != nil {
			return nil, err
		}
		d.controlNodes.hold(kubeContext, names, now)
	}
	reads := make([]hostNodeRead, 0, len(names))
	failed := false
	for _, name := range names {
		n, err := d.readNodeDisk(ctx, kubeconfig, kubeContext, name)
		if err != nil {
			failed = true
			reads = append(reads, hostNodeRead{hostNodeDisk: hostNodeDisk{Node: name}, Err: err})
			continue
		}
		reads = append(reads, hostNodeRead{hostNodeDisk: n})
	}
	if failed {
		d.controlNodes.forget()
	}
	return reads, nil
}

// readNodeDisk reads one node's image filesystem: its stats summary through the
// control context, parsed.
func (d *Daemon) readNodeDisk(ctx context.Context, kubeconfig, kubeContext, name string) (hostNodeDisk, error) {
	ctx, cancel := d.hostNodeReadContext(ctx)
	defer cancel()
	summary, _, err := d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext,
		"get", "--raw", "/api/v1/nodes/"+name+"/proxy/stats/summary")
	if err != nil {
		return hostNodeDisk{}, fmt.Errorf("reading the stats summary: %w", err)
	}
	return parseNodeStatsImageFs(summary)
}

// hostNodeReadContext bounds one kubectl run of the node-disk read with its own
// attentionHostProbeTimeout from parent. One deadline per run, never one shared
// across the nodes: a node that hangs cannot spend the budget of the nodes read
// after it (gt-x1po96). hostNodeReadContextFn replaces it in a test.
func (d *Daemon) hostNodeReadContext(parent context.Context) (context.Context, context.CancelFunc) {
	if d.hostNodeReadContextFn != nil {
		return d.hostNodeReadContextFn(parent)
	}
	return context.WithTimeout(parent, attentionHostProbeTimeout)
}
