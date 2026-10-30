package store

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

// The wisp age policy, ported from the internal/reaper rules. Classify holds
// every rule and TestClassifyWisp has one row per rule. ReapPass and PurgeWisps
// only gather facts from the Store and act on the verdicts.

// ReapPolicy holds the two ages the policy measures from now. A zero age takes
// its default.
type ReapPolicy struct {
	// MaxAge closes a stale open wisp: one created before now minus MaxAge.
	MaxAge time.Duration
	// PurgeAge deletes a closed wisp: one closed before now minus PurgeAge.
	PurgeAge time.Duration
}

const (
	// DefaultReapMaxAge is the stale-open age the gt reaper uses (--max-age).
	DefaultReapMaxAge = 24 * time.Hour
	// DefaultReapPurgeAge is the closed-wisp age the gt reaper uses (--purge-age).
	DefaultReapPurgeAge = 168 * time.Hour

	// mrStateProtection is how long a cleanup wisp tracking an outstanding merge
	// stays protected. The tracker has no label-set time, so updated_at stands
	// in for it; updated_at is never earlier, so the protection never ends sooner.
	mrStateProtection = 24 * time.Hour

	// maxReapChunk bounds the ids one close or delete call carries.
	maxReapChunk = 500
)

// openStatuses are the statuses a wisp is reaped from.
var openStatuses = []string{"open", "hooked", "in_progress"}

func (p ReapPolicy) withDefaults() ReapPolicy {
	if p.MaxAge == 0 {
		p.MaxAge = DefaultReapMaxAge
	}
	if p.PurgeAge == 0 {
		p.PurgeAge = DefaultReapPurgeAge
	}
	return p
}

// Action is what one pass does to one wisp.
type Action int

const (
	// Keep leaves the wisp alone.
	Keep Action = iota
	// Close closes the wisp with the verdict's reason.
	Close
	// Purge deletes the wisp.
	Purge
)

// Verdict is the policy's decision on one wisp, with the reason a report names.
type Verdict struct {
	Action Action
	Reason string
}

// WispFacts is what the policy reads of one wisp.
type WispFacts struct {
	Wisp api.Issue
	// Parents are the wisp's parent-child targets, with their status and type.
	Parents []api.IssueDep
	// Referenced is true when a live agent bead names the wisp as its hook_bead
	// or active_mr.
	Referenced bool
}

// Classify decides what the policy does to one wisp at now. It is the whole
// policy: the callers gather facts and act on the verdict, nothing more.
func Classify(f WispFacts, now time.Time, p ReapPolicy) Verdict {
	p = p.withDefaults()
	w := f.Wisp
	switch {
	case isAgentBead(w):
		return keep("agent beads are never reaped")
	case !w.Ephemeral:
		return keep("a durable issue is not a wisp")
	case f.Referenced:
		return keep("a live agent bead names it")
	case w.Status == "closed":
		if w.ClosedAt != nil && w.ClosedAt.Before(now.Add(-p.PurgeAge)) {
			return Verdict{Action: Purge, Reason: fmt.Sprintf("closed past purge-age %s", p.PurgeAge)}
		}
		return keep("closed within purge-age")
	case !slices.Contains(openStatuses, w.Status):
		return keep("status " + w.Status + " is not reaped")
	case hasLabel(w, "gt:merge-request"):
		return keep("a merge request in flight is protected at any age")
	case hasLabel(w, "cleanup") && hasLabel(w, "state:merge-requested") && !w.UpdatedAt.Before(now.Add(-mrStateProtection)):
		return keep("a cleanup wisp tracking a merge is protected for 24h")
	}

	openParent := false
	closedMolecule := false
	for _, par := range f.Parents {
		if slices.Contains(openStatuses, par.Status) {
			openParent = true
		}
		if par.Type == "molecule" && par.Status == "closed" {
			closedMolecule = true
		}
	}
	switch {
	case openParent:
		return keep("an open parent keeps it")
	case closedMolecule:
		return Verdict{Action: Close, Reason: "reaper: parent molecule closed"}
	case w.CreatedAt.Before(now.Add(-p.MaxAge)):
		return Verdict{Action: Close, Reason: fmt.Sprintf("reaper: stale wisp past max-age %s", p.MaxAge)}
	}
	return keep("younger than max-age")
}

func keep(reason string) Verdict { return Verdict{Action: Keep, Reason: reason} }

// isAgentBead is the identity the old reaper used: an agent issue type, or the
// gt:agent label on a bead of any other type.
func isAgentBead(w api.Issue) bool {
	return w.Type == "agent" || hasLabel(w, "gt:agent")
}

func hasLabel(w api.Issue, label string) bool {
	return slices.Contains(w.Labels, label)
}

var (
	agentStatePattern = regexp.MustCompile(`(?m)^agent_state:\s*(\S+)\s*$`)
	hookBeadPattern   = regexp.MustCompile(`(?m)^hook_bead:\s*(\S+)\s*$`)
	activeMRPattern   = regexp.MustCompile(`(?m)^active_mr:\s*(\S+)\s*$`)
)

// liveAgentRefs maps each wisp id a live agent bead names as hook_bead or
// active_mr. A nuked agent names nothing that protects a wisp. The agent's own
// fields are read first; its description is read for the lines the tracker has
// no field for.
func liveAgentRefs(ctx context.Context, s Store) (map[string]bool, error) {
	agents, err := listAgentBeads(ctx, s)
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for _, a := range agents {
		state := a.AgentState
		if state == "" {
			state = firstMatch(agentStatePattern, a.Description)
		}
		if state == "nuked" {
			continue
		}
		for _, id := range []string{a.HookBead, firstMatch(hookBeadPattern, a.Description), firstMatch(activeMRPattern, a.Description)} {
			if id != "" {
				refs[id] = true
			}
		}
	}
	return refs, nil
}

func firstMatch(p *regexp.Regexp, s string) string {
	if m := p.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// listAgentBeads reads every agent bead, durable and wisp, by issue type and by
// the gt:agent label. A bead with both appears once.
func listAgentBeads(ctx context.Context, s Store) ([]api.Issue, error) {
	seen := map[string]bool{}
	var out []api.Issue
	for _, eph := range []bool{false, true} {
		for _, f := range []api.ListFilter{
			{IssueType: "agent", Ephemeral: eph, Statuses: []string{"all"}, IncludeInfra: true},
			{Labels: []string{"gt:agent"}, Ephemeral: eph, Statuses: []string{"all"}, IncludeInfra: true},
		} {
			issues, err := listAll(ctx, s, f)
			if err != nil {
				return nil, fmt.Errorf("list agent beads: %w", err)
			}
			for _, a := range issues {
				if !seen[a.ID] {
					seen[a.ID] = true
					out = append(out, a)
				}
			}
		}
	}
	return out, nil
}

// listAll walks every page of f. A cursor that comes back unchanged is refused
// rather than read again forever.
func listAll(ctx context.Context, s Store, f api.ListFilter) ([]api.Issue, error) {
	var out []api.Issue
	for {
		page, err := s.ListIssues(ctx, f)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Issues...)
		if page.NextCursor == "" {
			return out, nil
		}
		if page.NextCursor == f.Cursor {
			return nil, fmt.Errorf("list: cursor %q did not advance", f.Cursor)
		}
		f.Cursor = page.NextCursor
	}
}

// PassResult counts what one ReapPass did.
type PassResult struct {
	Closed int
	Purged int
}

// ReapPass runs the policy once at now: it closes the stale open wisps the
// policy names, then deletes the expired closed wisps. A failed read or write
// stops the pass with the counts so far and the error.
func ReapPass(ctx context.Context, s Store, now time.Time, p ReapPolicy) (PassResult, error) {
	p = p.withDefaults()
	var res PassResult
	refs, err := liveAgentRefs(ctx, s)
	if err != nil {
		return res, fmt.Errorf("reap: %w", err)
	}

	// Every open wisp is read, not only the stale ones: a molecule closed at any
	// age closes its open steps.
	open, err := listAll(ctx, s, api.ListFilter{Ephemeral: true, Statuses: openStatuses})
	if err != nil {
		return res, fmt.Errorf("reap: list open wisps: %w", err)
	}
	byReason := map[string][]string{}
	for _, w := range open {
		parents, err := s.Dependencies(ctx, w.ID, "parent-child")
		if err != nil {
			return res, fmt.Errorf("reap: parents of %s: %w", w.ID, err)
		}
		v := Classify(WispFacts{Wisp: w, Parents: parents, Referenced: refs[w.ID]}, now, p)
		if v.Action == Close {
			byReason[v.Reason] = append(byReason[v.Reason], w.ID)
		}
	}
	for _, reason := range sortedKeys(byReason) {
		if err := eachChunk(byReason[reason], func(ids []string) error {
			return s.CloseIssues(ctx, ids, reason, true)
		}); err != nil {
			return res, fmt.Errorf("reap: close: %w", err)
		}
		res.Closed += len(byReason[reason])
	}

	closed, err := listAll(ctx, s, api.ListFilter{Ephemeral: true, Statuses: []string{"closed"}, ClosedBefore: now.Add(-p.PurgeAge)})
	if err != nil {
		return res, fmt.Errorf("reap: list closed wisps: %w", err)
	}
	var doomed []string
	for _, w := range closed {
		if Classify(WispFacts{Wisp: w, Referenced: refs[w.ID]}, now, p).Action == Purge {
			doomed = append(doomed, w.ID)
		}
	}
	if err := eachChunk(doomed, func(ids []string) error {
		return s.DeleteIssues(ctx, ids)
	}); err != nil {
		return res, fmt.Errorf("reap: purge: %w", err)
	}
	res.Purged = len(doomed)
	return res, nil
}

// PurgeWisps deletes the wisps among ids that the policy purges at now, and
// keeps the rest with the policy's reason. An id that names no issue is kept as
// not found. A failed read or delete returns an error and no result.
func PurgeWisps(ctx context.Context, s Store, ids []string, now time.Time, p ReapPolicy) (api.PurgeWispsResult, error) {
	p = p.withDefaults()
	found, err := s.GetIssues(ctx, ids)
	if err != nil {
		return api.PurgeWispsResult{}, fmt.Errorf("purge wisps: read: %w", err)
	}
	refs, err := liveAgentRefs(ctx, s)
	if err != nil {
		return api.PurgeWispsResult{}, fmt.Errorf("purge wisps: %w", err)
	}
	res := api.PurgeWispsResult{Purged: []string{}, Kept: []api.KeptWisp{}}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		w, ok := found[id]
		if !ok {
			res.Kept = append(res.Kept, api.KeptWisp{ID: id, Reason: "not found"})
			continue
		}
		v := Classify(WispFacts{Wisp: w, Referenced: refs[id]}, now, p)
		if v.Action != Purge {
			res.Kept = append(res.Kept, api.KeptWisp{ID: id, Reason: v.Reason})
			continue
		}
		res.Purged = append(res.Purged, id)
	}
	if err := eachChunk(res.Purged, func(ids []string) error {
		return s.DeleteIssues(ctx, ids)
	}); err != nil {
		return api.PurgeWispsResult{}, fmt.Errorf("purge wisps: delete: %w", err)
	}
	return res, nil
}

// eachChunk calls fn with ids in chunks of maxReapChunk, stopping at the first
// error.
func eachChunk(ids []string, fn func([]string) error) error {
	for len(ids) > 0 {
		n := min(len(ids), maxReapChunk)
		if err := fn(ids[:n]); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
