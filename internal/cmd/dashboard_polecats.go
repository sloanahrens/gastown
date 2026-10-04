package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
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

type fileStamp struct {
	size int64
	mod  time.Time
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
		st := fileStamp{}
		if info, err := os.Stat(p); err == nil {
			st = fileStamp{info.Size(), info.ModTime()}
		}
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
var dashLabelsShown = map[string]bool{"needs-human": true, "deferred": true, "failed": true}

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
// bead the queue says is waiting to land, and a bead labeled for a human.
func dashPolecatState(item polecatInventoryItem, hasWork, ready bool, labels []string) (state, reason string) {
	d := item.Disposition
	switch {
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
		p := dashboard.Polecat{Rig: s.Rig, Name: s.Name, RigParked: s.RigParked, CountsTowardCapacity: s.Item.Disposition.CountsTowardCapacity}
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
		}
		var labels []string
		hasWork := s.Issue != nil
		if hasWork {
			p.Bead = s.Issue.ID
			p.Title = s.Issue.Title
			pr := s.Issue.Priority
			p.Priority = &pr
			labels = s.Issue.Labels
			p.Labels = dashBadgeLabels(labels)
		}
		p.State, p.Reason = dashPolecatState(s.Item, hasWork, hasWork && in.Ready[p.Bead], labels)
		if !in.SessionsKnown && p.State == dashboard.StateStalled {
			p.State, p.Reason = dashboard.StateUnknown, "tmux could not be read, so stalled cannot be told from working"
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
	case dashboard.StateNeedsHuman:
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

// dashSeats classifies every polecat the town has with the same inventory and
// environment gt polecat list uses, so the dashboard and the CLI agree on what
// a polecat is doing. (The capacity accounting's counts-only environment
// reports a parked or finished polecat as stalled, which is right for counting
// seats and wrong for describing them.) Sessions are the tmux names read once
// by the caller. A rig whose beads cannot be read is an error, not a rig of
// idle polecats: a store that is down must not read as a quiet town.
func dashSeats(townRoot string, sessionNames []string, sessionsKnown bool, cache *dashSeatCache) ([]dashSeat, error) {
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return nil, err
	}
	reg := townRegistry()
	sessions := newPolecatSessionSet(reg, sessionNames)
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
			key := dashSeatKey(polecat.AgentBeadUpdatedAt(agentBead), now, spawnWindow, activeID, running)
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
			seats = append(seats, dashSeat{Rig: rig, Name: name, Session: session.PolecatSessionName(reg.PrefixForRig(rig), name), RigParked: rigParked, Item: item, Issue: active[name]})
		}
	}
	return seats, nil
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
