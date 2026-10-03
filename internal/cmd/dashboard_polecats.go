package cmd

import (
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/landings"
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

// dashBeadInfo reads a bead's priority, labels and title once and keeps them
// for ttl: the polecat panel wants the facts, not a bd read per refresh.
type dashBeadInfo struct {
	read func(rig, id string) *beads.Issue
	now  func() time.Time
	ttl  time.Duration

	mu    sync.Mutex
	cache map[string]dashBeadEntry
}

type dashBeadEntry struct {
	at    time.Time
	issue *beads.Issue // nil: the read failed; retried after ttl, not every refresh
}

func newDashBeadInfo(read func(rig, id string) *beads.Issue) *dashBeadInfo {
	return &dashBeadInfo{read: read, now: time.Now, ttl: 10 * time.Minute, cache: map[string]dashBeadEntry{}}
}

func (c *dashBeadInfo) get(rig, id string) *beads.Issue {
	c.mu.Lock()
	if e, ok := c.cache[id]; ok && c.now().Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.issue
	}
	c.mu.Unlock()
	issue := c.read(rig, id)
	c.mu.Lock()
	c.cache[id] = dashBeadEntry{at: c.now(), issue: issue}
	c.mu.Unlock()
	return issue
}

// dashLabelsShown are the labels worth a badge: the ones that say a bead is
// waiting on someone or somewhere, not the bookkeeping ones.
var dashLabelsShown = map[string]bool{"needs-human": true, "needs-mayor-review": true, "deferred": true, "failed": true}

func dashBadgeLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if n := strings.TrimPrefix(l, "gt:"); dashLabelsShown[n] {
			out = append(out, n)
		}
	}
	return out
}

// dashPolecatInputs is everything the polecat table is built from, so the
// join is a pure function a test can drive.
type dashPolecatInputs struct {
	Now      time.Time
	Seats    []dashSeat           // every polecat the town has, held or not
	Ready    map[string]bool      // beads submitted and waiting to land
	Sessions map[string]time.Time // tmux session -> last window activity
	Known    bool                 // the tmux read worked: a missing session is then meaningful
	Bead     func(rig, id string) *beads.Issue
	Records  []omRecord
}

// dashSeat is one polecat directory and the bead its agent bead hooks.
type dashSeat struct {
	Rig, Name, Session, Bead string
}

// buildDashPolecats joins the seats with their session, bead and record.
func buildDashPolecats(in dashPolecatInputs) []dashboard.Polecat {
	holders := map[string][]string{}
	for _, s := range in.Seats {
		if s.Bead != "" {
			holders[s.Bead] = append(holders[s.Bead], s.Rig+"/"+s.Name)
		}
	}
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
		p := dashboard.Polecat{Rig: s.Rig, Name: s.Name, Bead: s.Bead, State: dashboard.StateIdle}
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
		if s.Bead == "" {
			out = append(out, p)
			continue
		}
		for _, h := range holders[s.Bead] {
			if h != s.Rig+"/"+s.Name {
				p.AlsoHeldBy = append(p.AlsoHeldBy, h)
			}
		}
		var labels []string
		if in.Bead != nil {
			if issue := in.Bead(s.Rig, s.Bead); issue != nil {
				p.Title = issue.Title
				pr := issue.Priority
				p.Priority = &pr
				labels = issue.Labels
				p.BeadStatus = issue.Status
				p.Labels = dashBadgeLabels(labels)
			}
		}
		switch {
		case p.BeadStatus == "closed":
			// the work is done and the hook was never cleared
			p.State = dashboard.StateStale
		case in.Ready[s.Bead]:
			p.State = dashboard.StateQueued
		case hasAnyLabel(labels, "needs-human", "gt:needs-human"):
			p.State = dashboard.StateNeedsHuman
		case in.Known && !p.HasSession:
			p.State = dashboard.StateStale
		default:
			p.State = dashboard.StateWorking
		}
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

func dashStateRank(state string) int {
	switch state {
	case dashboard.StateNeedsHuman:
		return 0
	case dashboard.StateStale:
		return 1
	case dashboard.StateQuiet:
		return 2
	case dashboard.StateWorking:
		return 3
	case dashboard.StateGating:
		return 4
	case dashboard.StateQueued:
		return 5
	}
	return 6
}

// dashSeats lists every polecat directory in every rig with the bead its
// agent bead hooks. A rig whose agent beads cannot be read is an error: its
// polecats would otherwise read as idle, which is the one report a monitor
// must not make for a store that is down.
func dashSeats(townRoot string) ([]dashSeat, error) {
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return nil, err
	}
	reg := townRegistry()
	var seats []dashSeat
	for _, rig := range rigs {
		rigPath := townRoot + "/" + rig
		names, err := listPolecatDirectoryNames(rigPath)
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			continue
		}
		agents, err := tailRigAgentBeads(rigPath)
		if err != nil {
			return nil, err
		}
		prefix := beads.GetPrefixForRig(townRoot, rig)
		hooks := polecatHookBeads(names, func(name string) string { return beads.PolecatBeadIDWithPrefix(prefix, rig, name) }, agents)
		for _, name := range names {
			seats = append(seats, dashSeat{Rig: rig, Name: name, Session: session.PolecatSessionName(reg.PrefixForRig(rig), name), Bead: hooks[name]})
		}
	}
	return seats, nil
}

// dashSessions is the tmux sessions' last activity, and whether the read
// worked. No tmux server is a working read of no sessions.
func dashSessions() (map[string]time.Time, bool) {
	m, err := tmux.NewTmux().ListWindowActivity()
	if err != nil {
		return nil, false
	}
	return m, true
}
