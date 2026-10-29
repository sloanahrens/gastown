package cmd

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// dashLandings reads every rig's landings file and keeps the records until a
// file changes, so the om panel and the polecat panel share one read.
type dashLandings struct {
	townRoot string

	mu     sync.Mutex
	stamps map[string]fileStamp
	recs   []omRecord
}

func newDashLandings(townRoot string) *dashLandings {
	return &dashLandings{townRoot: townRoot, stamps: map[string]fileStamp{}}
}

// get returns every landing record, re-reading only when a file's size or
// modification time moved.
func (l *dashLandings) get() []omRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	rigs, _ := knownRigNames(l.townRoot)
	var paths []string
	for _, rig := range append([]string{"hq"}, rigs...) {
		if path, err := landings.Path(l.townRoot, rig); err == nil {
			paths = append(paths, path)
		}
	}
	changed := len(paths) != len(l.stamps)
	next := make(map[string]fileStamp, len(paths))
	for _, p := range paths {
		st, _ := stampFile(p)
		next[p] = st
		if old, ok := l.stamps[p]; !ok || old != st {
			changed = true
		}
	}
	if changed {
		var recs []omRecord
		for _, p := range paths {
			recs = append(recs, readOMRecords(p)...)
		}
		l.recs, l.stamps = recs, next
	}
	return l.recs
}

// dashLabelsShown are the labels worth a badge: the ones that say a bead is
// waiting on someone or somewhere, not the bookkeeping ones.
var dashLabelsShown = map[string]bool{
	"needs-human": true, "needs-overseer": true, "needs-operator": true,
	"deferred": true, "failed": true,
}

func dashBadgeLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if n := strings.TrimPrefix(l, "gt:"); dashLabelsShown[n] {
			out = append(out, n)
		}
	}
	return out
}

// dashSeat is one polecat as the town's own inventory sees it, with the work
// bead the inventory found assigned to it.
type dashSeat struct {
	Rig, Name string
	Session   string
	RigParked bool // the polecat's rig is parked
	Item      polecatInventoryItem
	Issue     *beads.Issue // the assigned work, nil when it holds none
	// HookedAt is when Issue was dispatched: the same clock the overrun guard
	// judges (events.DispatchStart), so the Age column and the guard's nudge
	// and stop thresholds cannot disagree (gt-tzbtf). It is the bead's first
	// dispatch since its last run-end event (the seat's gt done, a hand close,
	// or the guard's stop), not the sling that reached this seat, so a bead
	// handed between seats keeps one age. A bead the events log does not
	// dispatch falls back to the agent bead's last write, which gt sling makes
	// as it hooks the bead and which can only read short; zero when the agent
	// bead is missing or undated (gt-jwmxo).
	HookedAt time.Time
	// Sandbox is the seat's liveness where the host tmux cannot see it: a
	// sandboxed seat's session lives inside its sandbox (gt-43k7c).
	Sandbox dashSandboxSeat
}

// dashSandboxSeat is a sandboxed seat's liveness as the town answers it. The
// host tmux never lists the seat's session, so absence from the host listing
// says nothing about the seat.
type dashSandboxSeat struct {
	// Seat is true when the agent's seat runs in a sandbox, from the town's seat
	// settings.
	Seat bool
	// Sampled is true once the patrol has recorded a liveness sample for the
	// seat. Before that the town has no answer for it, and a session the host
	// cannot see is a question rather than a death.
	Sampled bool
	// Unknown is true when the town's seat settings could not be read, so
	// nothing says whether the seat is a sandbox seat at all.
	Unknown bool
}

// dashPolecatInputs is everything the polecat table is built from, so the
// join is a pure function a test can drive.
type dashPolecatInputs struct {
	Now      time.Time
	Seats    []dashSeat
	Ready    map[string]bool      // beads submitted and waiting to land
	Sessions map[string]time.Time // tmux session -> last window activity
	// SessionsKnown is false when tmux could not be read. The session list is
	// then empty by failure, not by fact, and a polecat with work and no
	// session is unknown, not stalled.
	SessionsKnown bool
	Records       []omRecord
}

// dashPolecatState maps the town's inventory state to the dashboard's. The
// inventory decides; the dashboard adds only what the inventory cannot see: a
// bead the queue says is waiting to land, and the person label its bead
// carries. The state names the label, so the row says which person must act
// (gt-slmaz); a bead carrying two of the labels reads as the first step, the
// overseer's.
func dashPolecatState(item polecatInventoryItem, hasWork, ready bool, labels []string) (state, reason string) {
	d := item.Disposition
	switch {
	case hasWork && hasAnyLabel(labels, "needs-overseer", "gt:needs-overseer"):
		return dashboard.StateNeedsOverseer, "the bead is labeled needs-overseer"
	case hasWork && hasAnyLabel(labels, "needs-operator", "gt:needs-operator"):
		return dashboard.StateNeedsOperator, "the bead is labeled needs-operator"
	case hasWork && hasAnyLabel(labels, "needs-human", "gt:needs-human"):
		return dashboard.StateNeedsHuman, "the bead is labeled needs-human"
	case hasWork && ready:
		return dashboard.StateQueued, "submitted for landing"
	}
	switch item.State {
	case polecat.StateWorking:
		return dashboard.StateWorking, d.Reason
	case polecat.StateSubmitted:
		return dashboard.StateQueued, d.Reason
	case polecat.StateSpawning:
		return dashboard.StateSpawning, d.Reason
	case polecat.StateStalled, polecat.StateStuck, polecat.StateZombie:
		return dashboard.StateStalled, "assigned work and no live session"
	case polecat.StateReviewNeeded:
		return dashboard.StateReviewNeeded, d.Reason
	}
	switch {
	case d.ReuseStatus == polecat.WorkstateReuseStatusParked:
		return dashboard.StateParked, d.Reason
	case d.NeedsRecovery:
		return dashboard.StateRecovery, d.Reason
	}
	return dashboard.StateIdle, ""
}

// dashPolecatStateSince is when a seat entered the state it is in, for the one
// state the hook time does not date: a bead submitted for landing entered
// queued at its last write, the label write gt done makes, which is the wait
// the Landings panel ages from the same field. Every other state answers nil,
// and the page reads the hub's hook time (gt-iv7jf). A last write that does not
// parse answers an error, so the caller reports the hook fallback instead of
// letting it happen silently (gt-uqmdg).
func dashPolecatStateSince(state string, issue *beads.Issue) (*time.Time, error) {
	if state != dashboard.StateQueued || issue == nil {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, issue.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &at, nil
}

// buildDashPolecats joins the seats with their session, bead and record.
func buildDashPolecats(in dashPolecatInputs) []dashboard.Polecat {
	type rec struct {
		landed, approved int
		scoreSum         float64
		scoreN           int
	}
	record := map[string]*rec{}
	since := in.Now.Add(-24 * time.Hour)
	for _, r := range in.Records {
		parts := strings.Split(r.Branch, "/")
		if len(parts) < 3 || parts[0] != "polecat" || r.LandedAt.Before(since) {
			continue
		}
		k := r.Rig + "/" + parts[1]
		if record[k] == nil {
			record[k] = &rec{}
		}
		x := record[k]
		x.landed++
		if omOutcome(r.OMVerdict) == "approved" {
			x.approved++
			if r.OMScore > 0 {
				x.scoreSum += r.OMScore
				x.scoreN++
			}
		}
	}

	out := make([]dashboard.Polecat, 0, len(in.Seats))
	for _, s := range in.Seats {
		p := dashboard.Polecat{Rig: s.Rig, Name: s.Name, RigParked: s.RigParked, Agent: s.Item.Agent, CountsTowardCapacity: s.Item.Disposition.CountsTowardCapacity}
		if x := record[s.Rig+"/"+s.Name]; x != nil {
			p.Landed24h, p.Approved24h = x.landed, x.approved
			if x.scoreN > 0 {
				v := x.scoreSum / float64(x.scoreN)
				p.AvgScore24h = &v
			}
		}
		if at, ok := in.Sessions[s.Session]; ok {
			p.HasSession = true
			at := at
			p.LastActive = &at
		} else if s.Item.SessionRunning {
			// The session is up where the host tmux neither lists nor clocks it
			// (inside a sandbox), so the row says it has one and leaves its
			// silence unknown rather than zero (gt-43k7c).
			p.HasSession = true
		}
		var labels []string
		hasWork := s.Issue != nil
		if hasWork {
			p.Bead = s.Issue.ID
			p.Title = s.Issue.Title
			p.Description = truncateRunes(dashBeadSummary(s.Issue.Description), dashboard.DescriptionCap)
			pr := s.Issue.Priority
			p.Priority = &pr
			labels = s.Issue.Labels
			p.Labels = dashBadgeLabels(labels)
			// The seat's record of the hook dates the bead for the pane when
			// the feed has no dispatch line for it (gt-jwmxo).
			if !s.HookedAt.IsZero() {
				at := s.HookedAt
				p.Hooked = &at
			}
		}
		p.State, p.Reason = dashPolecatState(s.Item, hasWork, hasWork && in.Ready[p.Bead], labels)
		at, err := dashPolecatStateSince(p.State, s.Issue)
		if err != nil {
			// The bead's last write could not be read, so the queued wait has no
			// clock of its own and the page dates it from the hook: say so rather
			// than let the fallback pass as the submit time (gt-uqmdg).
			p.Reason = "the bead's last write could not be read, so the queued wait is dated from the hook"
		} else if at != nil {
			p.StateSince = at
		}
		if !in.SessionsKnown && p.State == dashboard.StateStalled {
			p.State, p.Reason = dashboard.StateUnknown, "tmux could not be read, so stalled cannot be told from working"
		}
		if s.Sandbox.Seat && hasWork && !s.Sandbox.Sampled && p.State == dashboard.StateStalled {
			// A sandbox seat the patrol has not sampled yet: no session the host
			// can see says nothing about it, so the seat is starting (gt-43k7c).
			p.State, p.Reason = dashboard.StateSpawning, "sandbox seat starting: no liveness sample yet"
		}
		if s.Sandbox.Unknown && hasWork && p.State == dashboard.StateStalled {
			// The seat settings could not be read: a seat the host tmux cannot see
			// may be a sandbox seat that is alive, so stalled cannot be told apart.
			p.State, p.Reason = dashboard.StateUnknown, "seat settings could not be read, so stalled cannot be told from a sandbox seat starting"
		}
		p.Hints = dashPolecatHints(s.Rig, s.Name, p.Bead, p.State)
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := dashStateRank(out[i].State), dashStateRank(out[j].State); ri != rj {
			return ri < rj
		}
		return out[i].Rig+"/"+out[i].Name < out[j].Rig+"/"+out[j].Name
	})
	return out
}

// dashBeadSummary is what a seat's hover says about its bead: the description
// without the dispatcher's attachment lines, cut to the body of its "## Goal"
// section (up to the next "## " heading) or, with no such section, to its first
// paragraph (gt-dhc6o).
func dashBeadSummary(desc string) string {
	lines := strings.Split(strings.TrimSpace(beads.StripAttachmentFields(desc)), "\n")
	for i, l := range lines {
		if !strings.EqualFold(strings.TrimSpace(l), "## Goal") {
			continue
		}
		rest := lines[i+1:]
		for j, l := range rest {
			if strings.HasPrefix(strings.TrimSpace(l), "## ") {
				rest = rest[:j]
				break
			}
		}
		return strings.TrimSpace(strings.Join(rest, "\n"))
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines = lines[:i]
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func hasAnyLabel(labels []string, want ...string) bool {
	for _, l := range labels {
		for _, w := range want {
			if l == w {
				return true
			}
		}
	}
	return false
}

// dashStateRank orders the table: what needs a look first, idle last.
func dashStateRank(state string) int {
	switch state {
	case dashboard.StateNeedsHuman, dashboard.StateNeedsOverseer, dashboard.StateNeedsOperator:
		return 0
	case dashboard.StateStalled, dashboard.StateUnknown:
		return 1
	case dashboard.StateReviewNeeded, dashboard.StateRecovery:
		return 2
	case dashboard.StateQuiet:
		return 3
	case dashboard.StateWorking, dashboard.StateSpawning:
		return 4
	case dashboard.StateGating:
		return 5
	case dashboard.StateQueued:
		return 6
	case dashboard.StateParked:
		return 7
	}
	return 8
}

// dashSeatKey is what a cached classification depends on: the agent bead's
// last write, the work assigned to the polecat, whether its session is up, and
// whether it is inside its spawn grace window. The last is a function of time
// alone, so without it a polecat that just left its grace would keep the
// "spawning" reading until the ttl.
func dashSeatKey(updatedAt, now time.Time, grace time.Duration, activeID string, running bool) string {
	inGrace := !updatedAt.IsZero() && now.Sub(updatedAt) < grace
	return fmt.Sprintf("%d|%s|%t|%t", updatedAt.UnixNano(), activeID, running, inGrace)
}

// dashSeatCache keeps a polecat's classification between refreshes. The
// classification includes a live git probe of its worktree (the same one gt
// polecat list pays per seat), and an idle polecat's worktree does not change
// on its own, so the result is reused until the polecat's agent bead, its
// assigned work or its session changes, or ttl passes.
type dashSeatCache struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[string]dashSeatCached
}

type dashSeatCached struct {
	key  string
	at   time.Time
	item polecatInventoryItem
}

func newDashSeatCache() *dashSeatCache {
	return &dashSeatCache{ttl: 5 * time.Minute, now: time.Now, m: map[string]dashSeatCached{}}
}

func (c *dashSeatCache) get(id, key string) (polecatInventoryItem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || e.key != key || c.now().Sub(e.at) >= c.ttl {
		return polecatInventoryItem{}, false
	}
	return e.item, true
}

func (c *dashSeatCache) put(id, key string, item polecatInventoryItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[id] = dashSeatCached{key: key, at: c.now(), item: item}
}

// dashHookTime is when the seat's held bead went out: the clock the panel's Age
// and the overrun guard share. It is the bead's dispatch (gt-tzbtf), dated with
// the submission the bead's last READY TO LAND block records when that
// submission ended the run the dispatch began: a rework slung after a landing
// rejection is dated from its re-sling, not from the dispatch it was submitted
// on, and shows the same age the guard judges it by (gt-chd44). A bead the
// event log does not date falls back to agentUpdated, the seat's agent bead.
// The clock is the one dispatches.Starts read for this poll, so an unreadable
// log has already failed the poll. A seat the guard restarted on the run is
// dated from the restart the guard adopted for it (daemon.OverrunRestartClock),
// the same clock the guard stops it on (gt-y9lkx).
func dashHookTime(townRoot string, agentUpdated time.Time, bead *beads.Issue, dispatched map[string]time.Time, dispatches *events.DispatchReader) time.Time {
	at, ok := dispatched[bead.ID]
	if sub := land.ReadyBlockTime(bead.Notes); !sub.IsZero() {
		at, ok = dispatches.StartSince(bead.ID, sub)
	}
	if ok {
		return daemon.OverrunRestartClock(townRoot, bead.ID, at)
	}
	return agentUpdated
}

// dashSeats classifies every polecat the town has with the same inventory and
// environment gt polecat list uses, so the dashboard and the CLI agree on what
// a polecat is doing. (The capacity accounting's counts-only environment
// reports a parked or finished polecat as stalled, which is right for counting
// seats and wrong for describing them.) Sessions are the tmux names read once
// by the caller, plus the sandbox registry's, which the host tmux cannot see
// (gt-43k7c). A rig whose beads cannot be read is an error, not a rig of
// idle polecats: a store that is down must not read as a quiet town.
func dashSeats(townRoot string, sessionNames []string, sessionsKnown bool, cache *dashSeatCache, dispatches *events.DispatchReader, lister sessionLister) ([]dashSeat, error) {
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return nil, err
	}
	// The dispatch clock the overrun guard judges, kept by the panel's reader
	// across polls: an unreadable events log is an error, not a town where no
	// bead was ever dispatched, so the seats read as unavailable rather than as
	// young. It is read before the session and sandbox probes, so a town whose
	// clock cannot be read costs no live read.
	dispatched, err := dispatches.Starts()
	if err != nil {
		return nil, err
	}
	reg := townRegistry()
	// The lister reads each live session's recorded GT_AGENT, which the pane
	// names as the polecat's model.
	host := newPolecatSessionSetFromNames(reg, lister, sessionNames)
	sessions := dashSeatSessions(host, reg, session.ListSandboxSessions(townRoot), sandboxSeatDeathsFor(townRoot))
	sandboxAgents, sandboxErr := dashSandboxSeatAgents(townRoot)
	spawnWindow := polecatSpawnGraceWindow(townRoot)
	now := time.Now()
	var seats []dashSeat
	for _, rig := range rigs {
		rigPath := filepath.Join(townRoot, rig)
		rigParked := IsRigParked(townRoot, rig)
		names, err := listPolecatDirectoryNames(rigPath)
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			continue
		}
		rigBeads := beads.New(rigPath)
		agents, err := beads.ListAgentBeads(rigBeads)
		if err != nil {
			return nil, err
		}
		prefix := beads.GetPrefixForRig(townRoot, rig)
		agentBeadID := func(name string) string { return beads.PolecatBeadIDWithPrefix(prefix, rig, name) }
		active, err := listActivePolecatWorkByName(rigBeads, rig, polecatHookBeads(names, agentBeadID, agents))
		if err != nil {
			return nil, err
		}
		// The merge-request index is one query per rig, and only a polecat
		// that must be re-classified needs it.
		var mrIndex polecatMRIndex
		mrLoaded := false
		for _, name := range names {
			agentBead := agents[agentBeadID(name)]
			_, running := sessions.session(rig, name)
			activeID := ""
			if active[name] != nil {
				activeID = active[name].ID
			}
			// The agent bead's last write is the spawn grace's clock. It is
			// also the dispatch time's fallback: the seat's bead clock stands in
			// when the events log holds no dispatch for the bead (gt-tzbtf).
			updatedAt := polecat.AgentBeadUpdatedAt(agentBead)
			var hookedAt time.Time
			if activeID != "" {
				hookedAt = dashHookTime(townRoot, updatedAt, active[name], dispatched, dispatches)
			}
			key := dashSeatKey(updatedAt, now, spawnWindow, activeID, running)
			id := rig + "/" + name
			// A classification made without a session list is not the polecat's:
			// it is neither read from nor written to the cache.
			item, ok := polecatInventoryItem{}, false
			if sessionsKnown {
				item, ok = cache.get(id, key)
			}
			if !ok {
				if !mrLoaded {
					mrIndex, _ = loadPolecatMRIndex(rigBeads, rig)
					mrLoaded = true
				}
				env := polecatListInventoryEnv(rigPath, rig, name, mrIndex, polecatActiveMRReader{index: mrIndex, bd: rigBeads},
					polecatSpawnFacts{UpdatedAt: polecat.AgentBeadUpdatedAt(agentBead), Grace: spawnWindow, Now: now})
				item = buildPolecatInventoryItem(rig, name, parsePolecatAgentFields(agentBead), active[name], sessions, env)
				if sessionsKnown {
					cache.put(id, key, item)
				}
			}
			// The agent is the live session's own record, read on every poll
			// and never taken from the cache, so a session left over from
			// before a model switch keeps showing its own model. With no
			// session (the seat ran gt done and waits for its bead to land)
			// the agent the sling recorded on the seat's agent bead stands in;
			// that bead is already in hand, so it costs no read.
			item.Agent = dashSeatAgent(host, rig, name, agentBead)
			sandbox := dashSandboxStatus(sandboxAgents, sandboxErr, item.Agent, func() bool { return dashSandboxSampled(townRoot, rig, name) })
			seats = append(seats, dashSeat{Rig: rig, Name: name, Session: session.PolecatSessionName(reg.PrefixForRig(rig), name), RigParked: rigParked, Item: item, Issue: active[name], HookedAt: hookedAt, Sandbox: sandbox})
		}
	}
	return seats, nil
}

// dashSeatAgent is the agent a seat's row names: its live session's own record
// when a session exists, else the agent the sling stored on the seat's agent
// bead, else "" (an older agent bead carries no agent line).
func dashSeatAgent(sessions polecatSessionSet, rig, name string, agentBead *beads.Issue) string {
	if entry, ok := sessions.session(rig, name); ok {
		return entry.Agent
	}
	if fields := parsePolecatAgentFields(agentBead); fields != nil {
		return fields.Agent
	}
	return ""
}

// dashSeatSessions is the live-session set the polecat table classifies seats
// with: the host tmux's sessions plus the sandbox registry's, minus the seats
// the patrol has recorded dead. gt polecat list counts the registry's
// sessions, and the host listing cannot see a session inside a sandbox, so a
// set built from the host names alone reads every live sandbox seat as
// sessionless (gt-43k7c).
//
// The registry's entries carry liveness alone — no agent, no creation time — so
// a poll costs no exec into a sandbox. The seat's agent is read from host
// sessions only (see dashSeatAgent), so a sandboxed seat keeps the agent its
// sling recorded.
func dashSeatSessions(host polecatSessionSet, reg *session.PrefixRegistry, sandboxNames []string, dead sandboxSeatDeadFunc) polecatSessionSet {
	sessions := make(polecatSessionSet, len(host)+len(sandboxNames))
	for key, entry := range host {
		sessions[key] = entry
	}
	for key, entry := range newPolecatSessionSet(reg, sandboxNames) {
		if _, ok := sessions[key]; !ok {
			sessions[key] = entry
		}
	}
	if dead == nil {
		return sessions
	}
	for key, entry := range sessions {
		rigName, polecatName, ok := strings.Cut(key, polecatSessionKeySep)
		if ok && dead(entry.Name, rigName, polecatName) {
			delete(sessions, key)
		}
	}
	return sessions
}

// dashSandboxSeatAgents names the agents whose seats run in a sandbox, from the
// town's seat settings — the read the inventory's sandbox git probe makes. When
// the settings cannot be read it returns the error, and the caller marks every
// seat's sandbox status unknown rather than deriving a state from a failed read.
func dashSandboxSeatAgents(townRoot string) (map[string]bool, error) {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return nil, err
	}
	seats, err := config.Seats(ts, nil)
	if err != nil {
		return nil, err
	}
	agents := map[string]bool{}
	for _, seat := range seats {
		if seat.Backend == config.BackendOpenShell {
			agents[seat.Agent] = true
		}
	}
	return agents, nil
}

// dashSandboxStatus is a seat's sandbox standing from the town's seat settings:
// unknown when they could not be read, a sandbox seat (sampled or not) when its
// agent is one, otherwise a host seat. sampled is asked only for a sandbox seat.
func dashSandboxStatus(agents map[string]bool, agentsErr error, agent string, sampled func() bool) dashSandboxSeat {
	st := dashSandboxSeat{Unknown: agentsErr != nil}
	if agents[agent] {
		st.Seat, st.Sampled = true, sampled()
	}
	return st
}

// dashSandboxSampled reports whether the patrol has recorded a liveness sample
// for a sandbox seat. A missing record is "no sample yet" (the intent reader
// returns an empty record for it), and until a sample exists a session the host
// cannot see is not yet a stall. A record that exists but cannot be read counts
// as sampled: the seat must not read as starting forever on a bad file.
func dashSandboxSampled(townRoot, rigName, polecatName string) bool {
	rec, err := intent.Read(townRoot, polecatIntentSeat(rigName, polecatName))
	if err != nil {
		return true
	}
	return rec.Progress != nil
}

// dashSessions is the tmux sessions' last activity, and whether the read
// worked. A town with no tmux server is a working read of no sessions; only a
// failed read is not known.
func dashSessions() (map[string]time.Time, bool) {
	m, err := tmux.NewTmux().ListWindowActivity()
	if err != nil {
		return nil, false
	}
	return m, true
}
