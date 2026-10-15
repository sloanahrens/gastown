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
// the hub ages the tile rather than reading it as a calm machine (gt-q6h8e).
func sampleMachine(sample func(context.Context) hoststat.Reading) (Machine, error) {
	r := sample(context.Background())
	m := Machine{At: time.Now(), Cores: r.Cores, CPUIdle: r.CPUIdle, DoltCores: r.DoltCores}
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
