package hostprobe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/exectax"
	"github.com/steveyegge/gastown/internal/hoststat"
	"github.com/steveyegge/gastown/internal/util"
)

// Runner runs argv and returns its stdout; an error means the command failed.
// It is the seam every host command goes through, so a test answers each one
// from a fake and only the production runner starts a process.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// dockerContext is the Docker context every Docker call names. The global
// context is whatever the Mac last selected and can point at another engine,
// so the collector never reads it or relies on it.
const dockerContext = "desktop-linux"

// topProcs is how many busy processes a reading keeps, the number hoststat keeps.
const topProcs = 6

// The macOS commands hoststat's Sample runs for the idle share and the process
// table. The host agent is a launchd job on the Mac, so these are the forms it
// reads.
var (
	topArgv = []string{"/usr/bin/top", "-l", "2", "-n", "0", "-s", "1"}
	psArgv  = []string{"ps", "-axo", "pcpu=,command="}
)

// collectors are the reads one Collect makes. Disk and exec tax are not
// commands the Runner can run, so they are fields too: a test swaps the one it
// is about and nothing touches the host.
type collectors struct {
	run     Runner
	disk    func(path string) (*util.DiskSpaceInfo, error)
	execTax func(ctx context.Context) (time.Duration, error)
}

// Collect reads every probe field once and stamps the reading now. Each field
// is its own read, so a read that fails sets that field's Err and leaves the
// rest filled. townRoot is the town volume the disk reading is taken from.
func Collect(ctx context.Context, run Runner, townRoot string, now time.Time) Probes {
	return collect(ctx, collectors{
		run:  run,
		disk: util.GetDiskSpace,
		execTax: func(ctx context.Context) (time.Duration, error) {
			return probeExecTax(ctx, run)
		},
	}, townRoot, now)
}

// collect is Collect over its reads.
func collect(ctx context.Context, c collectors, townRoot string, now time.Time) Probes {
	return Probes{
		V:            SchemaVersion,
		At:           now,
		MemFreePct:   fieldOf(memFree(ctx, c.run)),
		Disk:         fieldOf(diskUse(c.disk, townRoot)),
		Containers:   fieldOf(containers(ctx, c.run)),
		CPUIdlePct:   fieldOf(cpuIdle(ctx, c.run)),
		TopConsumers: fieldOf(topConsumers(ctx, c.run)),
		ExecTaxMS:    fieldOf(execTaxMS(ctx, c.execTax)),
	}
}

// fieldOf is a reading's Field: its value when the read succeeded, its error
// text when it did not. The error names the read and never carries command
// output, so a secret in a command's output cannot reach the file.
func fieldOf[T any](v T, err error) Field[T] {
	if err != nil {
		return Field[T]{Err: err.Error()}
	}
	return Field[T]{Value: v}
}

// dockerArgv is a Docker command against the desktop-linux context.
func dockerArgv(args ...string) []string {
	return append([]string{"docker", "--context", dockerContext}, args...)
}

// memFree reads the free-memory percentage from memory_pressure.
func memFree(ctx context.Context, run Runner) (float64, error) {
	out, err := run(ctx, []string{"memory_pressure"})
	if err != nil {
		return 0, fmt.Errorf("memory_pressure: %w", err)
	}
	pct, err := ParseMemoryPressure(string(out))
	if err != nil {
		return 0, fmt.Errorf("memory_pressure: %w", err)
	}
	return pct, nil
}

// diskUse reads the town volume's use the way the daemon's disk rule does: the
// same statfs reading, so the two cannot disagree about the same volume.
func diskUse(disk func(string) (*util.DiskSpaceInfo, error), townRoot string) (Disk, error) {
	if townRoot == "" {
		return Disk{}, errors.New("town volume: no town root to read")
	}
	info, err := disk(townRoot)
	if err != nil {
		return Disk{}, fmt.Errorf("town volume: %w", err)
	}
	if info == nil {
		return Disk{}, errors.New("town volume: no reading")
	}
	return Disk{UsedPct: info.UsedPercent, FreeGB: info.AvailableGB()}, nil
}

// containers lists the running containers' names on the desktop-linux context.
func containers(ctx context.Context, run Runner) ([]string, error) {
	out, err := run(ctx, dockerArgv("ps", "--format", "{{.Names}}"))
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	return parseContainers(string(out)), nil
}

// parseContainers reads one container name per line. No containers is an
// empty list, not an error: a host running nothing is a reading.
func parseContainers(out string) []string {
	names := []string{}
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// cpuIdle reads the CPU idle share from one two-sample top, the reading
// hoststat's Sample takes.
func cpuIdle(ctx context.Context, run Runner) (float64, error) {
	out, err := run(ctx, topArgv)
	if err != nil {
		return 0, fmt.Errorf("top: %w", err)
	}
	idle := hoststat.ParseCPUIdle(string(out))
	if idle == nil {
		return 0, errors.New("top: no CPU idle share in its output")
	}
	return *idle, nil
}

// topConsumers reads the busiest processes from the process table, the reading
// hoststat's Sample takes.
func topConsumers(ctx context.Context, run Runner) ([]Consumer, error) {
	out, err := run(ctx, psArgv)
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	consumers := []Consumer{}
	for _, p := range hoststat.ParseTop(string(out), topProcs) {
		consumers = append(consumers, Consumer{Name: p.Name, CPU: p.CPU})
	}
	return consumers, nil
}

// execTaxMS is the exec tax in milliseconds.
func execTaxMS(ctx context.Context, execTax func(context.Context) (time.Duration, error)) (float64, error) {
	d, err := execTax(ctx)
	if err != nil {
		return 0, fmt.Errorf("exec tax: %w", err)
	}
	return float64(d) / float64(time.Millisecond), nil
}

// probeExecTax is the median cost of exec'ing a freshly written program, timed
// through the Runner so it is the exec a run pays. exectax writes the samples
// into a temporary directory and removes it.
func probeExecTax(ctx context.Context, run Runner) (time.Duration, error) {
	res, err := exectax.Probe(ctx, exectax.Options{
		Exec: func(ctx context.Context, path string) (time.Duration, error) {
			start := time.Now()
			if _, err := run(ctx, []string{path}); err != nil {
				return 0, err
			}
			return time.Since(start), nil
		},
	})
	return res.Median, err
}
