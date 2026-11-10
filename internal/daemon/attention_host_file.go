package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/hostprobe"
)

// fileHostProbeMaxAge is three host-agent intervals: the agent writes the file
// every 60 seconds, and three missed writes mean it stopped, so the readings
// are refused rather than served unrefreshed.
const fileHostProbeMaxAge = 180 * time.Second

// errProbeFileRefused marks a probe file this daemon will not serve. Its cause
// is wrapped alongside it, so the event keeps the reader's reason while a
// caller tells a refused file from a field a probe recorded as failed without
// matching the reader's message.
var errProbeFileRefused = errors.New("host probe file refused")

// fileHostProbes serves the attention host rules from the probe file a host
// agent publishes, for a daemon inside a container: memory_pressure and docker
// ps name the host, and from the container they would name its own namespace.
// Build it with newFileHostProbes; the zero value has no path or clock.
type fileHostProbes struct {
	// systemHostProbes answers RigParked: the park record is a town config
	// file, which the town root bind mount makes readable from the container.
	systemHostProbes
	path string
	// now is the reader's clock, so a test decides what the file's age is.
	now    func() time.Time
	maxAge time.Duration
	// nodeDisk reads the control node's disk through the control cluster. The
	// node is not on the host's probe file: the daemon container reads it
	// through the cluster API, as the control-disk patrol does.
	nodeDisk func(context.Context) (hostNodeDisk, error)
}

// newFileHostProbes returns the file-backed host reads for the town rooted at
// townRoot. forgejoBaseURL is the town's Forgejo origin, which the health read
// asks over the network because the probe file the host agent publishes has no
// say in it. nodeDisk is the control node's disk read (controlNodeDisk).
func newFileHostProbes(townRoot string, forgejoBaseURL func() (string, error), nodeDisk func(context.Context) (hostNodeDisk, error)) fileHostProbes {
	return fileHostProbes{
		systemHostProbes: systemHostProbes{townRoot: townRoot, forgejoBaseURL: forgejoBaseURL},
		path:             filepath.Join(townRoot, ".runtime", "host", "probes.json"),
		now:              time.Now,
		maxAge:           fileHostProbeMaxAge,
		nodeDisk:         nodeDisk,
	}
}

// probes reads the file as of now. A refusal — missing file, another schema,
// a stamp too old or too far ahead — is errProbeFileRefused, never empty
// readings, since an empty host reads as healthy to the rules.
func (p fileHostProbes) probes() (hostprobe.Probes, error) {
	pr, err := hostprobe.Read(p.path, p.now(), p.maxAge)
	if err != nil {
		return hostprobe.Probes{}, fmt.Errorf("%w: %w", errProbeFileRefused, err)
	}
	return pr, nil
}

// fieldErr turns one probe field's recorded failure into the error the rule
// reports; an empty Err is a reading that happened.
func fieldErr(path, probe, recorded string) error {
	if recorded == "" {
		return nil
	}
	return fmt.Errorf("%s probe in %s: %s", probe, path, recorded)
}

func (p fileHostProbes) Disk() (hostDisk, error) {
	pr, err := p.probes()
	if err != nil {
		return hostDisk{}, err
	}
	if err := fieldErr(p.path, hostProbeDisk, pr.Disk.Err); err != nil {
		return hostDisk{}, err
	}
	return hostDisk{UsedPct: pr.Disk.Value.UsedPct, FreeGB: pr.Disk.Value.FreeGB}, nil
}

// NodeDisk reads the control node's disk through the control cluster. A probe
// built with no node read is a wiring error, never a quiet disk.
func (p fileHostProbes) NodeDisk(ctx context.Context) (hostNodeDisk, error) {
	if p.nodeDisk == nil {
		return hostNodeDisk{}, errors.New("the file host probes were built with no control node read")
	}
	return p.nodeDisk(ctx)
}

func (p fileHostProbes) MemFreePct(context.Context) (float64, error) {
	pr, err := p.probes()
	if err != nil {
		return 0, err
	}
	if err := fieldErr(p.path, hostProbeMem, pr.MemFreePct.Err); err != nil {
		return 0, err
	}
	return pr.MemFreePct.Value, nil
}

// ForgejoHealthy asks the town's Forgejo itself. The health read is one HTTP
// request, which the container makes as the host does, so unlike
// memory_pressure and docker ps it is not the host's namespace the probe is
// about and the published file has nothing to answer for it.
func (p fileHostProbes) ForgejoHealthy(ctx context.Context) error {
	return p.systemHostProbes.ForgejoHealthy(ctx)
}

func (p fileHostProbes) Containers(context.Context) ([]string, error) {
	pr, err := p.probes()
	if err != nil {
		return nil, err
	}
	if err := fieldErr(p.path, hostProbeDocker, pr.Containers.Err); err != nil {
		return nil, err
	}
	return pr.Containers.Value, nil
}
