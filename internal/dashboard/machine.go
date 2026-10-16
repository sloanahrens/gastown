package dashboard

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/hoststat"
)

// SampleMachine reads the host's busyness — the CPU idle reading and the Dolt
// server's CPU, with the load averages kept as the number macOS inflates
// (gt-auiji) — and the busiest processes.
func SampleMachine() (Machine, error) {
	return sampleMachine(hoststat.Sample)
}

// sampleMachine is SampleMachine with its probe passed in, so a test can drive
// the branch a host that reads nothing takes.
//
// The load average is one instant counter read, so what is left that fails is
// the host answering none of its probes — a sandbox, or a platform with no
// probe to run. That sample carries nothing to paint, so it is an error and
// the hub ages the strip's host reading rather than reading it as a calm
// machine (gt-q6h8e).
func sampleMachine(sample func(context.Context) hoststat.Reading) (Machine, error) {
	r := sample(context.Background())
	m := Machine{At: time.Now(), Cores: r.Cores, CPUIdle: r.CPUIdle, DoltCores: r.DoltCores, HostState: hostState(r.CPUIdle)}
	for _, p := range r.Top {
		m.Top = append(m.Top, Proc{Name: p.Name, CPU: p.CPU})
	}
	if r.Load1 == nil {
		return m, errors.New("hoststat: no load average")
	}
	m.Load1 = *r.Load1
	if r.Load5 != nil {
		m.Load5 = *r.Load5
	}
	if r.Load15 != nil {
		m.Load15 = *r.Load15
	}
	return m, nil
}

// hostState is the strip's verdict on the host's reading of record, the CPU
// idle share: "bad" below hoststat.CPUIdleCritical idle, "warn" below
// hoststat.CPUIdleScarce, "" above them. The page draws that verdict rather
// than comparing thresholds of its own, so what the strip calls a strained
// host is the same reading a slow exec probe calls a saturated one
// (gt-auiji, gt-v50sn). A host that reported no idle reading gets no verdict:
// the strip shows a dash, never a calm machine.
func hostState(idle *float64) string {
	switch {
	case idle == nil:
		return ""
	case *idle < hoststat.CPUIdleCritical:
		return "bad"
	case *idle < hoststat.CPUIdleScarce:
		return "warn"
	}
	return ""
}
