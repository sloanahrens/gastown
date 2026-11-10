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
)

// hostNodeDisk is the control node's image filesystem as the rules read it: the
// node that answered and the volume's usage.
type hostNodeDisk struct {
	Node string
	Disk hostDisk
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
// errHostNodeNotConfigured. A cluster with more than one node reads each, and
// the fullest answers; a node that cannot be read fails the whole read, so the
// probe is an info item and no node is judged on another's reading.
//
// Nothing the kubectl run printed to stderr enters an error: a credential or a
// path in it would reach the queue (rule 11).
func (d *Daemon) controlNodeDisk(ctx context.Context) (hostNodeDisk, error) {
	kubeContext, configured, err := ciCacheTrimContext(d.patrolConfig)
	if !configured {
		return hostNodeDisk{}, errHostNodeNotConfigured
	}
	if err != nil {
		return hostNodeDisk{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, attentionHostProbeTimeout)
	defer cancel()
	kubeconfig := diskMaintKubeconfig(d.config.TownRoot, d.diskMaintenanceSeams().getenv)
	listing, _, err := d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext, "get", "nodes", "-o", "name")
	if err != nil {
		return hostNodeDisk{}, fmt.Errorf("listing the control cluster's nodes: %w", err)
	}
	names, err := parseControlNodeNames(listing)
	if err != nil {
		return hostNodeDisk{}, err
	}
	var (
		worst    hostNodeDisk
		found    bool
		failures []error
	)
	for _, name := range names {
		summary, _, err := d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext,
			"get", "--raw", "/api/v1/nodes/"+name+"/proxy/stats/summary")
		if err != nil {
			failures = append(failures, fmt.Errorf("node %s: %w", name, err))
			continue
		}
		n, err := parseNodeStatsImageFs(summary)
		if err != nil {
			failures = append(failures, fmt.Errorf("node %s: %w", name, err))
			continue
		}
		if !found || n.Disk.UsedPct > worst.Disk.UsedPct {
			worst, found = n, true
		}
	}
	if len(failures) > 0 {
		return hostNodeDisk{}, errors.Join(failures...)
	}
	return worst, nil
}
