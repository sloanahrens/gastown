package daemon

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// slowSeatCheck is the duration from which one seat's health check logs a line
// of its own (gt-2a5z6).
const slowSeatCheck = time.Second

// slowestSeatsLogged is how many seats the step's summary line names.
const slowestSeatsLogged = 3

type seatCallKind int

const (
	seatTmux seatCallKind = iota
	seatBD
)

// seatTimer collects, for one seat's check, how long its tmux and bd calls took
// and how many there were. It is local to the check, never shared.
type seatTimer struct {
	tmux, bd           time.Duration
	tmuxCalls, bdCalls int
}

// timeSeatCall runs f and charges its duration, by the daemon clock, to kind in
// tm.
func (d *Daemon) timeSeatCall(tm *seatTimer, kind seatCallKind, f func()) {
	start := d.clk().Now()
	f()
	took := d.clk().Since(start)
	switch kind {
	case seatTmux:
		tm.tmux += took
		tm.tmuxCalls++
	case seatBD:
		tm.bd += took
		tm.bdCalls++
	}
}

// seatTime is one seat and how long its check took.
type seatTime struct {
	rig, polecat string
	took         time.Duration
}

// seatTimes collects the seats of one heartbeat's health step; the rigs run on
// the rig pool, so adds are locked.
type seatTimes struct {
	mu    sync.Mutex
	seats []seatTime
	rigs  map[string]bool
}

func (s *seatTimes) add(rig, polecat string, took time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rigs == nil {
		s.rigs = map[string]bool{}
	}
	s.rigs[rig] = true
	s.seats = append(s.seats, seatTime{rig: rig, polecat: polecat, took: took})
}

// summary is the step's one line: the seat and rig counts and the three
// slowest seats, longest first. Seats of equal length keep the order they
// finished in.
func (s *seatTimes) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ranked := append([]seatTime(nil), s.seats...)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].took > ranked[j].took })
	if len(ranked) > slowestSeatsLogged {
		ranked = ranked[:slowestSeatsLogged]
	}
	parts := make([]string, 0, len(ranked))
	for _, r := range ranked {
		parts = append(parts, fmt.Sprintf("%s/%s %s", r.rig, r.polecat, r.took.Round(time.Millisecond)))
	}
	return fmt.Sprintf("polecat-health: %d seats in %d rigs; slowest %s", len(s.seats), len(s.rigs), strings.Join(parts, ", "))
}

// slowSeatLine is the line for one seat that took slowSeatCheck or more, with
// the split between its tmux and its bd calls.
func slowSeatLine(rig, polecat string, took time.Duration, tm *seatTimer) string {
	return fmt.Sprintf("polecat-health: %s/%s took %s (tmux %s in %d calls, bd %s in %d calls)",
		rig, polecat, took.Round(time.Millisecond),
		tm.tmux.Round(time.Millisecond), tm.tmuxCalls, tm.bd.Round(time.Millisecond), tm.bdCalls)
}
