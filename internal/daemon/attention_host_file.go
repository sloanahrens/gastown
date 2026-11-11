package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/hostprobe"
)

// fileHostProbeMaxAge is three host-agent intervals: the agent writes the file
// every 60 seconds, and three missed writes mean it stopped, so the readings
// are refused rather than served unrefreshed.
const fileHostProbeMaxAge = 180 * time.Second

// cgroupMountDir is the cgroup v2 mount the daemon container sees. In the pod
// that mount is the pod's own cgroup, so its memory.max is the pod's limit.
const cgroupMountDir = "/sys/fs/cgroup"

// cgroupUnlimited is what a cgroup's memory.max reads when it has no limit.
const cgroupUnlimited = "max"

// procMeminfoPath is the kernel's memory table, the reading MemFreePct falls
// back to when the pod's cgroup has no limit. It is a different file from the
// cgroup, so the fallback is a second source and not the first one read again.
const procMeminfoPath = "/proc/meminfo"

// errProbeFileRefused marks a probe file this daemon will not serve. Its cause
// is wrapped alongside it, so the event keeps the reader's reason while a
// caller tells a refused file from a field a probe recorded as failed without
// matching the reader's message.
var errProbeFileRefused = errors.New("host probe file refused")

// fileHostProbes serves the attention host rules for a daemon inside a
// container. The disk comes from the probe file a host agent publishes: the
// Mac's volume is the one the town lives on, and the container cannot measure
// it. Memory is the pod's own cgroup, because the Mac's memory is not the
// pod's. Build it with newFileHostProbes; the zero value has no path or clock.
type fileHostProbes struct {
	// systemHostProbes answers the Forgejo health read: the instance is a
	// network request, which the container makes as the host does.
	systemHostProbes
	path string
	// now is the reader's clock, so a test decides what the file's age is.
	now    func() time.Time
	maxAge time.Duration
	// nodeDisks reads the control nodes' disks through the control cluster. The
	// nodes are not on the host's probe file: the daemon container reads them
	// through the cluster API, as the control-disk patrol does.
	nodeDisks func(context.Context) ([]hostNodeRead, error)
	// cgroupDir is the cgroup whose memory is read, and meminfoPath the memory
	// table the reading falls back to when that cgroup has no limit. They are
	// the cgroup mount and /proc/meminfo in production; a test points them at
	// temp files.
	cgroupDir, meminfoPath string
}

// newFileHostProbes returns the file-backed host reads for the town rooted at
// townRoot. forgejoBaseURL is the town's Forgejo origin, which the health read
// asks over the network because the probe file the host agent publishes has no
// say in it. nodeDisks is the control nodes' disk read (controlNodeDisk).
func newFileHostProbes(townRoot string, forgejoBaseURL func() (string, error), nodeDisks func(context.Context) ([]hostNodeRead, error)) fileHostProbes {
	return fileHostProbes{
		systemHostProbes: systemHostProbes{townRoot: townRoot, forgejoBaseURL: forgejoBaseURL},
		path:             filepath.Join(townRoot, ".runtime", "host", "probes.json"),
		now:              time.Now,
		maxAge:           fileHostProbeMaxAge,
		nodeDisks:        nodeDisks,
		cgroupDir:        cgroupMountDir,
		meminfoPath:      procMeminfoPath,
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

// NodeDisks reads the control nodes' disks through the control cluster. A probe
// built with no node read is a wiring error, never a quiet disk. Container mode
// runs in the control cluster, so a town with no kubernetes runner to read them
// through is a failed read here, not the not-configured sentinel that raises
// nothing (gt-x1po96).
func (p fileHostProbes) NodeDisks(ctx context.Context) ([]hostNodeRead, error) {
	if p.nodeDisks == nil {
		return nil, errors.New("the file host probes were built with no control node read")
	}
	reads, err := p.nodeDisks(ctx)
	if errors.Is(err, errHostNodeNotConfigured) {
		return nil, errors.New("container mode reads the control node's disk, but the tier sweep runner is not a kubernetes runner, so there is no control cluster to read it through")
	}
	return reads, err
}

// ForgejoHealthy asks the town's Forgejo itself. The health read is one HTTP
// request, which the container makes as the host does, so unlike the disk read
// it is not the host's namespace the probe is about and the published file has
// nothing to answer for it.
func (p fileHostProbes) ForgejoHealthy(ctx context.Context) error {
	return p.systemHostProbes.ForgejoHealthy(ctx)
}

// MemFreePct is the free share of the pod's own memory (memFreePctFromCgroup).
// The probe file's memory field describes the Mac, not the pod, so container
// mode does not read it; the pod's limit is what the daemon can be short of.
func (p fileHostProbes) MemFreePct(context.Context) (float64, error) {
	if p.cgroupDir == "" || p.meminfoPath == "" {
		return 0, errors.New("the file host probes were built with no cgroup or meminfo to read the memory from")
	}
	return memFreePctFromCgroup(p.cgroupDir, p.meminfoPath)
}

// memFreePctFromCgroup is the free share of the pod's memory as a percentage of
// its limit: the limit less memory.current, over the limit, read from the
// cgroup in cgroupDir. When the cgroup's memory.max reads max it has no limit,
// and the share is taken from meminfoPath instead: MemAvailable over MemTotal,
// which is the memory the kernel reports as free for new work. A file that is
// unreadable, or that holds neither a byte count nor max, is an error, never a
// free share of 0 or 100.
func memFreePctFromCgroup(cgroupDir, meminfoPath string) (float64, error) {
	limitPath := filepath.Join(cgroupDir, "memory.max")
	limit, unlimited, err := readCgroupValue(limitPath)
	if err != nil {
		return 0, err
	}
	if unlimited {
		return memFreePctFromMeminfo(meminfoPath)
	}
	current, err := readCgroupBytes(filepath.Join(cgroupDir, "memory.current"))
	if err != nil {
		return 0, err
	}
	if limit == 0 {
		return 0, fmt.Errorf("%s reads a zero limit, so there is no free share to measure", limitPath)
	}
	if current >= limit {
		return 0, nil
	}
	return float64(limit-current) / float64(limit) * 100, nil
}

// memFreePctFromMeminfo is the free share of memory in a meminfo-format file:
// MemAvailable over MemTotal, as a percentage. Both fields must be present and
// MemTotal must be positive; the error names the file and the field, never the
// file's contents.
func memFreePctFromMeminfo(path string) (float64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading the memory table %s: %w", path, err)
	}
	total, err := meminfoKiB(path, raw, "MemTotal")
	if err != nil {
		return 0, err
	}
	avail, err := meminfoKiB(path, raw, "MemAvailable")
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, fmt.Errorf("%s reports MemTotal as zero, so there is no free share to measure", path)
	}
	return float64(avail) / float64(total) * 100, nil
}

// meminfoKiB is the value of one field of a meminfo-format file, in kibibytes.
// A field the file does not carry, or a value that is not a number, is an error.
func meminfoKiB(path string, raw []byte, field string) (uint64, error) {
	for _, line := range strings.Split(string(raw), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != field {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s holds a %s that is not a kibibyte count", path, field)
		}
		return n, nil
	}
	return 0, fmt.Errorf("%s has no %s field", path, field)
}

// readCgroupBytes reads a cgroup file that must hold a byte count. max is an
// error here: memory.current is never unlimited.
func readCgroupBytes(path string) (uint64, error) {
	n, unlimited, err := readCgroupValue(path)
	if err != nil {
		return 0, err
	}
	if unlimited {
		return 0, fmt.Errorf("%s reads max, want a byte count", path)
	}
	return n, nil
}

// readCgroupValue reads a cgroup memory file: a byte count, or max for no
// limit. The error names the file and never its contents, so whatever the file
// holds cannot reach an error or a log.
func readCgroupValue(path string) (uint64, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false, fmt.Errorf("reading the cgroup memory file %s: %w", path, err)
	}
	s := strings.TrimSpace(string(raw))
	if s == cgroupUnlimited {
		return 0, true, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%s holds neither a byte count nor max", path)
	}
	return n, false, nil
}
