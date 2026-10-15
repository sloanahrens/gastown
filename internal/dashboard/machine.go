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
	r := hoststat.Sample(context.Background())
	m := Machine{At: time.Now(), Cores: r.Cores, CPUIdle: r.CPUIdle, DoltCores: r.DoltCores}
	for _, p := range r.Top {
		m.Top = append(m.Top, Proc{Name: p.Name, CPU: p.CPU})
	}
	if r.Load1 == nil {
		// A sample that read no load average read nothing at all: the hub
		// ages the tile rather than painting a calm machine (gt-q6h8e).
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
