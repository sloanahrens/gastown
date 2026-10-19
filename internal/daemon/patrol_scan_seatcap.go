package daemon

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
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
	if pool == nil || (pool.GetMaxTotal() <= 0 && !pool.SeatsCapped() && len(pool.Seats) == 0) {
		return patrolscan.SeatCap{}, nil
	}
	byAgent, total, err := h.livePolecatCensus(rig+"/polecats/"+name, pool)
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
	// The seat map: the restarting polecat answers to the seat it will launch
	// on, the agent its bead recorded, else the role default a restart starts
	// a polecat with no record on. Placing it on no seat would let it past a
	// full one.
	if len(pool.Seats) > 0 {
		agent, err := h.recordedAgent(rig, name)
		if err != nil {
			return patrolscan.SeatCap{}, fmt.Errorf("reading the agent bead of %s/%s: %w", rig, name, err)
		}
		if agent == "" {
			agent = ts.RoleAgents["polecat"]
		}
		if seat, ok := pool.Seats[agent]; ok {
			key := "polecat_pool.seats." + agent + ".cap"
			// A cap of 0 closes the seat (gt pool set <agent>=0), the way
			// specdispatch.Seat.Free reads it: held however few are live.
			// SeatConfig.Cap is a plain int (internal/config/seats.go), so a
			// seat with no cap key is cap 0 and closed too: that is the design,
			// not a missing cap.
			if seat.Cap <= 0 {
				return patrolscan.SeatCap{Live: byAgent[agent], Closed: true, Key: key}, nil
			}
			if byAgent[agent] >= seat.Cap {
				return patrolscan.SeatCap{Live: byAgent[agent], Limit: seat.Cap, Key: key}, nil
			}
		}
	}
	return patrolscan.SeatCap{}, nil
}

// seatLiveAgent is the agent a live polecat is counted on: the agent its agent
// bead recorded when the seat was spawned, which outlives the session; failing
// that, the GT_AGENT the session was launched with (sessionAgent, "" for a seat
// with no session to read); failing that, the pool's UnrecordedAgent. A polecat
// that falls to the last is never counted on a seat it may not hold. A bead
// that cannot be read is an error, not an unrecorded agent: the census would
// otherwise move a polecat between seats on a bd hiccup. A pool with no seat
// map reads no bead: its caps are stated on GT_AGENT alone, and a census that
// never needed bd must not start failing with it.
func (h *patrolScanHost) seatLiveAgent(rig, name, sessionAgent string, pool *agentconfig.PolecatPool) (string, error) {
	if len(pool.Seats) == 0 {
		return strings.TrimSpace(sessionAgent), nil
	}
	recorded, err := h.recordedAgent(rig, name)
	if err != nil {
		return "", fmt.Errorf("reading the agent bead of %s/%s: %w", rig, name, err)
	}
	if recorded != "" {
		return recorded, nil
	}
	if agent := strings.TrimSpace(sessionAgent); agent != "" {
		return agent, nil
	}
	return pool.UnrecordedAgent(), nil
}

// recordedAgent is the agent the polecat's agent bead says its seat was
// spawned on, "" when the bead names none or does not exist.
func (h *patrolScanHost) recordedAgent(rig, name string) (string, error) {
	if h.agentRecord != nil {
		return h.agentRecord(rig, name)
	}
	id := beads.AgentBeadIDFor(constants.RolePolecat, rig, name, h.town())
	if id == "" {
		return "", nil
	}
	_, fields, err := beads.GetAgentBead(h.recoveryBeads(), id)
	if err != nil || fields == nil {
		return "", err
	}
	return strings.TrimSpace(fields.Agent), nil
}

// livePolecatCensus counts the town's live polecat sessions, in total and per
// agent. Poles are identified by the GT_ROLE their session carries
// ("<rig>/polecats/<name>"), the way the pool's own seat listing identifies
// them, so a witness, refinery or crew session on the same server holds no
// seat and is not counted. Each is counted on the agent its seat was spawned on
// (seatLiveAgent), which is what every seat cap is stated per; those that
// record none are listed in the log once per tick (gt-mukgk).
//
// This is the live half of the pool's picture and not all of it: the pool also
// counts the seats a crashed polecat still holds, the seats mid-landing and
// the in-flight claims, because a dispatch must not fill a seat something else
// is about to take. Those cannot be counted here — the seat this check is
// about is one of them, and counting it would read the restart as spending a
// seat it already holds, leaving it to wait on itself forever. The cap bounds
// live sessions, and a restart that replaces one adds none: skipRole, the
// GT_ROLE of the seat being restarted, names the session left out.
func (h *patrolScanHost) livePolecatCensus(skipRole string, pool *agentconfig.PolecatPool) (map[string]int, int, error) {
	names, err := h.d.tmux.ListSessions()
	if err != nil {
		return nil, 0, fmt.Errorf("listing tmux sessions: %w", err)
	}
	byAgent := make(map[string]int, len(names))
	total := 0
	var unrecorded []string
	for _, name := range names {
		role, err := h.d.tmux.GetEnvironment(name, "GT_ROLE")
		if err != nil || !strings.Contains(role, "/polecats/") {
			continue // not a polecat session, or a session that cannot say
		}
		if strings.TrimSpace(role) == skipRole {
			continue // the seat being restarted: its restart replaces this session
		}
		sessionAgent, _ := h.d.tmux.GetEnvironment(name, "GT_AGENT")
		rig, polecatName, _ := strings.Cut(strings.TrimSpace(role), "/polecats/")
		agent, err := h.seatLiveAgent(rig, polecatName, sessionAgent, pool)
		if err != nil {
			return nil, 0, err
		}
		if agent == agentconfig.UnknownSeatAgent {
			unrecorded = append(unrecorded, strings.TrimSpace(role))
		}
		byAgent[agent]++
		total++
	}
	h.logUnrecordedSeats(unrecorded)
	return byAgent, total, nil
}

// logUnrecordedSeats says once per tick which live polecats record no agent and
// so sit on no seat: they count toward max_total and fill no seat's cap.
func (h *patrolScanHost) logUnrecordedSeats(roles []string) {
	if len(roles) == 0 || h.unrecordedLogged || h.d.logger == nil {
		return
	}
	h.unrecordedLogged = true
	h.d.logger.Printf("patrol_scan: seat cap: %d polecat(s) record no agent and count under %q, on no seat: %s",
		len(roles), agentconfig.UnknownSeatAgent, strings.Join(roles, ", "))
}
