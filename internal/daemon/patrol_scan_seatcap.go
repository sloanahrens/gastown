package daemon

import (
	"fmt"
	"strings"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// The pool's seat cap for a crash restart (gt-02tcp).
//
// patrol_scan restarts a dead polecat holding work, which raises a live
// session: a restart spends a seat exactly as a sling does, so this file reads
// the cap a dispatch answers to and hands it to patrolscan, which holds the
// seat hooked while the town is at it.
//
// The cap is read the way the spec dispatcher reads its budget: the pool's own
// seat (polecat_pool.agent, capped by max_seats) and the town's total across
// every seat (polecat_pool.max_total). A pool that leaves max_seats unset is
// uncapped here, as it is to a sling; the dispatcher's defensive default for
// its own seat is the dispatcher's rule, not a town cap.

// seatCap implements patrolscan's seat-cap read over the town's settings and
// its live sessions, for a restart of rig/name. A pool with neither cap set
// answers "no cap" without listing tmux: there is nothing to count it against.
//
// The census leaves out rig/name's own session. A polecat whose agent died but
// whose session stayed up (liveness ReasonAgentGone) is restarted in that
// session, so the restart adds no live polecat; counting it would hold a full
// pool's own crashed seat on itself, every tick (gt-zwiww). A seat whose
// session is gone has nothing to leave out, and its restart still answers to
// the cap.
func (h *patrolScanHost) seatCap(rig, name string) (patrolscan.SeatCap, error) {
	ts, err := agentconfig.LoadOrCreateTownSettings(agentconfig.TownSettingsPath(h.town()))
	if err != nil {
		return patrolscan.SeatCap{}, fmt.Errorf("reading town settings: %w", err)
	}
	pool := ts.PolecatPool
	if pool == nil || (pool.GetMaxTotal() <= 0 && !pool.SeatsCapped()) {
		return patrolscan.SeatCap{}, nil
	}
	byAgent, total, err := h.livePolecatCensus(rig + "/polecats/" + name)
	if err != nil {
		return patrolscan.SeatCap{}, err
	}
	// The total is checked first, the order choosePoolAgent decides a sling
	// in: a town over its total is over it whichever seat the polecats sit on.
	if totalCap := pool.GetMaxTotal(); totalCap > 0 && total >= totalCap {
		return patrolscan.SeatCap{Live: total, Limit: totalCap, Key: "polecat_pool.max_total"}, nil
	}
	if pool.SeatsCapped() && byAgent[pool.Agent] >= pool.MaxSeats {
		return patrolscan.SeatCap{Live: byAgent[pool.Agent], Limit: pool.MaxSeats, Key: "polecat_pool.max_seats"}, nil
	}
	return patrolscan.SeatCap{}, nil
}

// livePolecatCensus counts the town's live polecat sessions, in total and per
// agent. Poles are identified by the GT_ROLE their session carries
// ("<rig>/polecats/<name>"), the way the pool's own seat listing identifies
// them, so a witness, refinery or crew session on the same server holds no
// seat and is not counted; GT_AGENT is what every seat cap is stated per.
//
// This is the live half of the pool's picture and not all of it: the pool also
// counts the seats a crashed polecat still holds, the seats mid-landing and
// the in-flight claims, because a dispatch must not fill a seat something else
// is about to take. Those cannot be counted here — the seat this check is
// about is one of them, and counting it would read the restart as spending a
// seat it already holds, leaving it to wait on itself forever. The cap bounds
// live sessions, and a restart that replaces one adds none: skipRole, the
// GT_ROLE of the seat being restarted, names the session left out.
func (h *patrolScanHost) livePolecatCensus(skipRole string) (map[string]int, int, error) {
	names, err := h.d.tmux.ListSessions()
	if err != nil {
		return nil, 0, fmt.Errorf("listing tmux sessions: %w", err)
	}
	byAgent := make(map[string]int, len(names))
	total := 0
	for _, name := range names {
		role, err := h.d.tmux.GetEnvironment(name, "GT_ROLE")
		if err != nil || !strings.Contains(role, "/polecats/") {
			continue // not a polecat session, or a session that cannot say
		}
		if strings.TrimSpace(role) == skipRole {
			continue // the seat being restarted: its restart replaces this session
		}
		agent, _ := h.d.tmux.GetEnvironment(name, "GT_AGENT")
		byAgent[strings.TrimSpace(agent)]++
		total++
	}
	return byAgent, total, nil
}
