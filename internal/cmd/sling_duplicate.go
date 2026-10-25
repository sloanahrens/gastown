package cmd

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/style"
)

// Sling-time content dedupe (gt-mcq).
//
// Two polecat sessions were lost in one night to the same-defect double
// dispatch. The mechanism: one defect seen from two vantage points — a
// refinery gate failure and a polecat exploring the code — produces two beads
// whose prose shares no keywords, so similarity on titles and vocabulary finds
// nothing. What the two descriptions *do* share are the things that name the
// work itself: the tests that fail and the files that carry the fix.
//
// The cost of a miss is a whole session (plus, in the gt-3vr/gt-rl0 case, ~3h
// of queue latency and a gate run), so the check runs before dispatch. Three
// moments exist and none subsumes another:
//
//	before dispatch  this file — content dedupe against open + recently-closed rig beads
//	before work      polecat-side check of the bead's paths against main
//	after work       patch-id equivalence, which is definitive but necessarily post-hoc
//
// Overlap policy. A shared *test name* means two beads are almost certainly one
// defect, and the sling is refused until --force. A shared *file* alone is weak
// evidence — plenty of genuinely distinct beads touch internal/cmd/sling.go —
// so it warns and proceeds. Those two cases are the acceptance criteria on
// gt-mcq, and they are the whole of the policy: nothing here inspects titles,
// keywords, or embeddings, because those are exactly what the two vantage
// points defeat. Which overlaps of a shared test refuse is narrower than it
// looks: work in flight always does, but an unstarted peer only when it comes
// first in dispatch order and the dispatcher would sling it — a cluster of
// ready beads naming one test would otherwise refuse each other forever and
// dispatch none of them (gt-nlkba), and a head the dispatcher holds for a
// reason of its own would refuse the rest for as long as the hold lasted
// (gt-00gee). A peer the dispatcher never reaches counts as held too, which is
// the same standoff by another road: a bead blocked by an open dependency is
// not on the ready board at all, so ranking it first refuses the rest until
// that dependency closes (gt-mknmb). The rule lives in sling.Blocking /
// sling.DecideDuplicates, so the CLI and the dispatcher cannot disagree about
// it, and the peer's dispatchability is answered here (specPeerHeldFrom),
// from the peer's own pool row, because deciding it is the dispatcher's rule.

const (
	// duplicateLookback bounds the recently-closed half of the comparison pool.
	// Work closed within this window is still plausibly the same defect being
	// re-filed; beyond it, overlap is more likely to be coincidence.
	duplicateLookback = 72 * time.Hour

	// duplicatePoolTTL is how long a fetched pool is reused. Dispatches arrive
	// in bursts (batch sling, the scheduler's batch) and a pool fetch costs two
	// bd list round-trips, so a short TTL collapses a burst into one fetch. The
	// window is short enough that a bead closed seconds ago is seen by the next
	// burst.
	duplicatePoolTTL = 60 * time.Second
)

// duplicateMatchLimit caps how many overlapping beads a report lists before
// summarizing the remainder. The report is written by internal/sling, which
// owns the limit.
const duplicateMatchLimit = sling.MatchLimit

// duplicateDependencyHops bounds the dependency walk the guard makes before
// each sling. Chains in this town are shallow — a plan's steps depend on the
// plan, siblings on the base they share — so a small bound covers them and
// keeps the guard a fixed cost.
const duplicateDependencyHops = 4

var (
	// testNameRe matches a Go test identifier as it appears in bead prose.
	// The trailing character class includes "_" so the wildcard spelling used
	// in this repo ("TestRunPrimeExternalTools_*") is captured whole; trimming
	// happens in specdispatch.TestNamesOverlap.
	testNameRe = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

	// pathTokenRe matches one non-space token, which is the largest unit that
	// can carry a file path in prose.
	pathTokenRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]*`)

	// sourceFileExtRe is the extension set a token must end with to count as a
	// referenced file. Deliberately narrow: prose is full of dotted words
	// (v1.2, 6.30pm, yaml) that would otherwise look like paths.
	sourceFileExtRe = regexp.MustCompile(`\.(?:go|md|sh|bash|zsh|ya?ml|toml|sql|ts|tsx|js|jsx|py|rs|tmpl|conf)$`)

	// bareFileExtRe restricts extension-only matches (no directory component)
	// to extensions that are unambiguous file references on their own.
	bareFileExtRe = regexp.MustCompile(`\.(?:go|md|sh|ya?ml|toml|sql)$`)

	// domainLikeRe matches a leading host component such as "github.com", so a
	// pasted URL is not mistaken for a path inside the repo.
	domainLikeRe = regexp.MustCompile(`^(?:[a-z0-9-]+\.)+(?:com|org|io|dev|net|ai|gov|edu)$`)
)

// contentRefs is the set of test names and file paths a bead's text names.
type contentRefs = sling.ContentRefs

// duplicateCandidate is one bead reduced to the fields the dedupe compares.
type duplicateCandidate = sling.Duplicate

// depEdge is one blocking dependency edge; internal/sling owns the shape and
// the walk over it.
type depEdge = sling.DepEdge

// duplicateMatch records one existing bead whose content overlaps the
// candidate's, and on what. Which overlaps refuse a sling is sling.Blocking's
// decision, so the dispatch engine and this check cannot disagree.
type duplicateMatch = sling.DuplicateMatch

// slingDuplicateDecision is the outcome of a pre-sling dedupe check: whether to
// refuse, and the report to print either way.
type slingDuplicateDecision = sling.DuplicateDecision

// decideSlingDuplicates turns the overlaps found against a bead into the
// decision the sling acts on, plus the report it prints. candidate is the
// bead being slung, which the guard ranks each unstarted overlap against.
func decideSlingDuplicates(candidate duplicateCandidate, matches []duplicateMatch) slingDuplicateDecision {
	return sling.DecideDuplicates(candidate, matches)
}

// extractContentRefs pulls test names and file paths out of free-form bead
// prose. Callers pass every text field a bead carries (title, description,
// design, notes). Extraction is purely lexical — no index, no vocabulary
// model — because the two vantage points share none of the latter. A
// per-package fixture is not a ref: it names scaffolding every package has,
// not a defect (sling.IsPackageFixtureTest).
func extractContentRefs(parts ...string) contentRefs {
	tests := map[string]bool{}
	files := map[string]bool{}
	for _, part := range parts {
		for _, name := range testNameRe.FindAllString(part, -1) {
			if sling.IsPackageFixtureTest(name) {
				continue
			}
			tests[name] = true
		}
		for _, tok := range pathTokenRe.FindAllString(part, -1) {
			if path, ok := normalizeFileToken(tok); ok {
				files[path] = true
			}
		}
	}
	return contentRefs{Tests: sortedKeys(tests), Files: sortedKeys(files)}
}

// planDocPrefix is the docs tree the overlap guard reads as context, not work:
// every child of a plan cites it, so it must not hold siblings apart (gt-4wb27).
const planDocPrefix = "docs/plans/"

// extractOverlapRefs is extractContentRefs for the content-overlap guard, minus
// the plan documents a bead cites. A code path named beside the plan still
// counts.
func extractOverlapRefs(parts ...string) contentRefs {
	refs := extractContentRefs(parts...)
	files := make([]string, 0, len(refs.Files))
	for _, path := range refs.Files {
		if isPlanDocPath(path) {
			continue
		}
		files = append(files, path)
	}
	refs.Files = files
	return refs
}

// isPlanDocPath reports whether a normalized file ref names a plan document,
// whatever prefix the prose spelled it with: normalizeFileToken keeps a home
// path ("~/gt/docs/plans/x.md") and an absolute one ("/Users/me/gt/docs/plans/x.md")
// whole, so the plan tree is matched at a directory boundary anywhere in it.
// The match is wider than those two spellings on purpose: a rig-relative ref
// ("gastown/docs/plans/x.md") names the same tree. The cost is that any path
// with a docs/plans/ directory in it is read as plan context too (gt-upi7n).
func isPlanDocPath(path string) bool {
	return strings.HasPrefix(path, planDocPrefix) || strings.Contains(path, "/"+planDocPrefix)
}

// normalizeFileToken reduces one prose token to a repo-relative file path, or
// reports that it is not a file reference at all.
func normalizeFileToken(raw string) (string, bool) {
	// "." and "-" belong to a path token, so a path that ends a sentence keeps
	// its full stop (".../hermetic_enforce_test.go."). Trim those before the
	// extension check, which is what makes the token a file reference.
	raw = strings.TrimRight(strings.TrimSpace(raw), "./-")
	if raw == "" || strings.Contains(raw, "://") {
		return "", false
	}

	segments := strings.Split(raw, "/")
	// Trim leading "./", "../", and "/" so relative and absolute spellings of
	// the same file compare equal.
	for len(segments) > 1 && (segments[0] == "." || segments[0] == ".." || segments[0] == "") {
		segments = segments[1:]
	}
	// Trim a trailing separator ("internal/cmd/").
	for len(segments) > 1 && segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}
	if len(segments) == 0 || segments[len(segments)-1] == "" {
		return "", false
	}
	// A token with a directory component whose first element is a hostname is
	// a URL path, not a repo path.
	if len(segments) > 1 && domainLikeRe.MatchString(segments[0]) {
		return "", false
	}

	last := segments[len(segments)-1]
	if len(segments) > 1 {
		if !sourceFileExtRe.MatchString(last) {
			return "", false
		}
	} else if !bareFileExtRe.MatchString(last) {
		return "", false
	}
	if dot := strings.LastIndex(last, "."); dot <= 0 {
		return "", false
	}
	return strings.Join(segments, "/"), true
}

// findDuplicateMatches compares one candidate against the pool, returning every
// overlap, blocking ones first. Epics are not candidates: an epic's prose
// enumerates its children's tests, so a shared name says nothing about
// duplication, and every task filed under it would otherwise refuse against it
// (gt-fudap, the gt-idv8s/gt-ik4a1 seat-refill failure).
func findDuplicateMatches(candidate duplicateCandidate, pool []duplicateCandidate) []duplicateMatch {
	var matches []duplicateMatch
	for _, other := range pool {
		if other.ID == candidate.ID || other.Refs.Empty() || other.IsEpic() {
			continue
		}
		sharedTests := intersectTests(candidate.Refs.Tests, other.Refs.Tests)
		sharedFiles := intersectStrings(candidate.Refs.Files, other.Refs.Files)
		if len(sharedTests) == 0 && len(sharedFiles) == 0 {
			continue
		}
		matches = append(matches, duplicateMatch{
			Bead:        other,
			SharedTests: sharedTests,
			SharedFiles: sharedFiles,
		})
	}
	sortDuplicateMatches(candidate, matches)
	return matches
}

// sortDuplicateMatches orders a match list for the report: the ones that
// refuse the sling first, then by bead ID. A caller that stamps the peers with
// the dispatcher's verdict (resolvePeerHolds) sorts again afterwards, since
// that verdict is what decides which ones refuse.
func sortDuplicateMatches(candidate duplicateCandidate, matches []duplicateMatch) {
	sort.Slice(matches, func(i, j int) bool {
		bi, bj := matches[i].Blocking(candidate), matches[j].Blocking(candidate)
		if bi != bj {
			return bi
		}
		return matches[i].Bead.ID < matches[j].Bead.ID
	})
}

// checkSlingDuplicates compares a bead's own content against its rig's open and
// recently-closed beads.
//
// It returns the candidate it inspected (for registration once the sling
// actually lands), every overlap found, and an error only when the pool could
// not be read. The check is advisory: a caller must never refuse a sling
// because of a check error, since a bd hiccup is not evidence of a duplicate.
// A nil candidate means the check did not run — either the bead names no tests
// or files, or the rig's bead directory could not be resolved.
func checkSlingDuplicates(townRoot, beadID string, info *beadInfo) (*duplicateCandidate, []duplicateMatch, error) {
	return slingDuplicatePools.check(townRoot, beadID, info)
}

// check is checkSlingDuplicates against p's pools.
func (p *duplicatePools) check(townRoot, beadID string, info *beadInfo) (*duplicateCandidate, []duplicateMatch, error) {
	if info == nil {
		return nil, nil, nil
	}
	refs := extractOverlapRefs(info.Title, info.Description, info.Design, info.Notes)
	if refs.Empty() {
		return nil, nil, nil
	}

	candidate := &duplicateCandidate{
		ID:        beadID,
		Title:     info.Title,
		Status:    info.Status,
		IssueType: info.IssueType,
		Priority:  info.Priority,
		CreatedAt: info.CreatedAt,
		Blockers:  blockingDepIDs(info.Dependencies),
		Ready:     readinessOfBeadInfo(info),
		Refs:      refs,
	}

	beadsDir := duplicateBeadsDir(townRoot, beadID)
	if beadsDir == "" {
		return candidate, nil, nil
	}
	pool, err := p.poolFor(beadsDir)
	if err != nil {
		return candidate, nil, fmt.Errorf("duplicate check skipped: %w", err)
	}
	matches := findDuplicateMatches(*candidate, pool)
	matches = dropRelatedMatches(*candidate, pool, matches)
	return candidate, p.resolvePeerHolds(townRoot, beadsDir, *candidate, matches), nil
}

// resolvePeerHolds stamps each unstarted peer the guard's rank is about to
// weigh with the dispatcher's verdict on it (sling.Duplicate.Held): the reason
// the dispatcher would not sling that bead, or "" when it would. Only a peer
// that is open and ahead of the candidate is asked — those are the only ones
// an answer changes (sling.Blocking) — so the question costs one read per peer
// that could refuse rather than one per pool bead. What the verdict is read
// from is the peer's own pool row, so the question itself costs no bd read.
//
// A pool built without a resolver leaves every peer un-asked, which is the
// conservative reading of the same silence: each still holds the sling on its
// rank alone.
func (p *duplicatePools) resolvePeerHolds(townRoot, beadsDir string, candidate duplicateCandidate, matches []duplicateMatch) []duplicateMatch {
	if p.held == nil || len(matches) == 0 {
		return matches
	}
	asked := false
	for i := range matches {
		peer := matches[i].Bead
		if peer.Status != "open" || !sling.Precedes(peer, candidate) {
			continue
		}
		matches[i].Bead.Held = p.held(townRoot, beadsDir, peer)
		asked = true
	}
	if asked {
		sortDuplicateMatches(candidate, matches)
	}
	return matches
}

// dropRelatedMatches removes the overlaps the candidate's dependency chain
// already links to it. The chain is read from the edges this check has in hand
// — the candidate's own blocking dependencies and each pool bead's — because a
// refusal arrives from the dependents side too (gt-xydyc).
func dropRelatedMatches(candidate duplicateCandidate, pool []duplicateCandidate, matches []duplicateMatch) []duplicateMatch {
	if len(matches) == 0 {
		return matches
	}
	related := sling.RelatedBeads(candidate.ID, dependencyEdges(candidate, pool), duplicateDependencyHops)
	return sling.DropRelatedMatches(matches, related)
}

// dependencyEdges is every blocking dependency edge this check knows.
func dependencyEdges(candidate duplicateCandidate, pool []duplicateCandidate) []depEdge {
	edges := make([]depEdge, 0, len(candidate.Blockers))
	for _, blocker := range candidate.Blockers {
		edges = append(edges, depEdge{From: candidate.ID, To: blocker})
	}
	for _, other := range pool {
		for _, blocker := range other.Blockers {
			edges = append(edges, depEdge{From: other.ID, To: blocker})
		}
	}
	return edges
}

// blockingDepIDs reduces a bead's dependency list to the IDs it blocks on,
// dropping relations that are not blocking and bd's external: refs.
func blockingDepIDs(deps []beads.IssueDep) []string {
	var ids []string
	seen := make(map[string]bool, len(deps))
	for _, dep := range deps {
		if !beads.IsBlockingDependencyType(dep.DependencyType) {
			continue
		}
		id := beads.ExtractIssueID(dep.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// noteSlingCandidateDispatched adds a bead to the cached pool once its sling
// landed, so the next dispatch in the same burst compares against it too.
// Without this, two duplicates dispatched in one batch — the pattern behind
// gt-3vr/gt-rl0 — would both miss each other, because the pool snapshot
// predates both. A nil candidate (the check was skipped) is a no-op.
func noteSlingCandidateDispatched(townRoot string, candidate *duplicateCandidate) {
	slingDuplicatePools.noteSlingCandidateDispatched(townRoot, candidate)
}

// noteSlingCandidateDispatched is the package function of that name against
// p's pools.
func (p *duplicatePools) noteSlingCandidateDispatched(townRoot string, candidate *duplicateCandidate) {
	if candidate == nil {
		return
	}
	if beadsDir := duplicateBeadsDir(townRoot, candidate.ID); beadsDir != "" {
		p.noteDispatched(beadsDir, *candidate)
	}
}

// noteDispatched records one already-dispatched bead in a rig's cached pool as
// hooked, whether the pool already holds a row for it or the row is appended.
// The sling's hook write has landed, so the bead holds a seat, and the cached
// row predates that write. Left open, it reads as unstarted work that may not
// precede the peer judging it, and sling.Blocking then lets that peer past the
// guard and onto a second seat for one defect (gt-dzjtz).
//
// The caller records this at its commit point, not beside the hook write: the
// note outlives a rollback, so a sling that is later unwound would otherwise
// leave its bead reading as hooked work the tick had already given back
// (gt-fvak3).
//
// It is a no-op when the pool was never fetched: a fresh fetch would find the
// bead in the database anyway, now that its hook has landed.
func (p *duplicatePools) noteDispatched(beadsDir string, candidate duplicateCandidate) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.cache[beadsDir]
	if !ok {
		return
	}
	candidate.Status = beads.StatusHooked
	for i, existing := range entry.candidates {
		if existing.ID == candidate.ID {
			// A slice poolFor already handed out is shared with readers, so the
			// corrected row goes into a copy rather than in place. The whole row
			// is replaced: the candidate is the bead as the sling's own show read
			// it, while every other field of the snapshot's row is as stale as
			// the status this corrects (gt-fvak3).
			updated := make([]duplicateCandidate, len(entry.candidates))
			copy(updated, entry.candidates)
			updated[i] = candidate
			entry.candidates = updated
			return
		}
	}
	entry.candidates = append(entry.candidates, candidate)
}

// duplicateBeadsDir resolves the directory to run bd in for a bead's rig.
// The pool must come from the rig that owns the bead, not from the town
// database: the town's bd search only reaches hq beads, which is how a rig
// duplicate went unseen (gt-mcq).
func duplicateBeadsDir(townRoot, beadID string) string {
	if townRoot == "" || beadID == "" {
		return ""
	}
	dir := resolveBeadDirFromTownRoot(townRoot, beadID)
	if dir == "" || dir == "." {
		return ""
	}
	return dir
}

// duplicatePoolStatuses are the statuses that count as live work: a bead in any
// of them is either already dispatched or waiting to be.
var duplicatePoolStatuses = []string{"open", "in_progress", "hooked", "pinned", "blocked", "deferred"}

// duplicatePoolEntry is a cached pool snapshot for one rig's bead database.
type duplicatePoolEntry struct {
	fetchedAt  time.Time
	candidates []duplicateCandidate
}

// duplicatePools caches one comparison pool per rig bead database, read by
// fetch. The process shares slingDuplicatePools; a test builds its own.
type duplicatePools struct {
	fetch func(beadsDir string) ([]duplicateCandidate, error)
	// held reports why the dispatcher would not sling a bead, "" when it would
	// (sling.Duplicate.Held). It is a parameter of the check rather than part
	// of the pool fetch because it is a question about the dispatcher's own
	// knobs — the priority ceiling, the shape gate, the child set — asked of
	// the few peers the rank is weighing, and it reads the peer's pool row
	// rather than asking bd about the bead again (gt-mknmb). A nil resolver is
	// the conservative reading: no peer is known to be held, so each still
	// holds the sling on its rank.
	held  func(townRoot, beadsDir string, peer duplicateCandidate) string
	mu    sync.Mutex
	cache map[string]*duplicatePoolEntry
}

func newDuplicatePools(fetch func(beadsDir string) ([]duplicateCandidate, error), held func(townRoot, beadsDir string, peer duplicateCandidate) string) *duplicatePools {
	return &duplicatePools{fetch: fetch, held: held, cache: map[string]*duplicatePoolEntry{}}
}

// slingDuplicatePools is the process-wide cache, read from the bd on PATH.
var slingDuplicatePools = newDuplicatePools(fetchDuplicatePool, specPeerHeldPinned)

// poolFor returns the comparison pool for a rig, reusing a snapshot taken
// within duplicatePoolTTL. Returns a slice shared with the cache; callers must
// not mutate it.
func (p *duplicatePools) poolFor(beadsDir string) ([]duplicateCandidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if entry, ok := p.cache[beadsDir]; ok && time.Since(entry.fetchedAt) < duplicatePoolTTL {
		return entry.candidates, nil
	}

	candidates, err := p.fetch(beadsDir)
	if err != nil {
		return nil, err
	}
	p.cache[beadsDir] = &duplicatePoolEntry{fetchedAt: time.Now(), candidates: candidates}
	return candidates, nil
}

// resetDuplicatePoolCache clears the process-wide pool cache. Tests call it to
// isolate one another; production code never needs to.
func resetDuplicatePoolCache() {
	slingDuplicatePools.mu.Lock()
	defer slingDuplicatePools.mu.Unlock()
	slingDuplicatePools.cache = map[string]*duplicatePoolEntry{}
}

// fetchDuplicatePool reads the live and recently-closed beads of one rig.
func fetchDuplicatePool(beadsDir string) ([]duplicateCandidate, error) {
	return fetchDuplicatePoolWith(slingStores{}.pinnedAt(beadsDir), warnDuplicateEnrichmentFailed, beadsDir, specDetailCache.Load())
}

// fetchDuplicatePoolFrom is fetchDuplicatePool read from store, with a
// failed design/notes enrichment reported to warn. The pool is cached for
// duplicatePoolTTL, so warn fires once per fetch, not once per sling.
func fetchDuplicatePoolFrom(store beads.Client, warn func(beadsDir string, err error), beadsDir string) ([]duplicateCandidate, error) {
	return fetchDuplicatePoolWith(store, warn, beadsDir, nil)
}

// fetchDuplicatePoolWith is fetchDuplicatePoolFrom reading each bead's design
// and notes through details, which shows only the beads whose list row changed
// since it last did (gt-l0vlf). A nil details shows every bead of the pool.
//
// Both lists are read before either is enriched: a cached blocker's open status
// is read from the pool's rows, and a blocker that closed sits in the closed
// list while the bead it blocks sits in the active one. The tick's unread count
// and any enrichment failure are reported once, however many lists they came
// from.
func fetchDuplicatePoolWith(store beads.Client, warn func(beadsDir string, err error), beadsDir string, details *duplicateDetailCache) ([]duplicateCandidate, error) {
	activeRows, err := listDuplicateRows(store, duplicatePoolStatuses, time.Time{})
	if err != nil {
		return nil, err
	}
	closedRows, err := listDuplicateRows(store, []string{"closed"}, time.Now().Add(-duplicateLookback))
	if err != nil {
		return nil, err
	}
	// A bead in a custom status is not compared as a peer, but a blocker in one
	// is still open: listing it here keeps the cache from reading an edge to it
	// as resolved because the pool does not list its target (gt-rhr2t). The
	// lookup is a refinement of the cached blocker read, so a failure is
	// reported and the pool is built without it, as a failed enrichment is; it
	// stays its own list because folding it into the active one would let a bd
	// that rejects a custom status fail that list too. With no custom status
	// registered there is nothing to ask: an empty status filter is not "none".
	var customRows []*beads.Issue
	if custom := constants.BeadsCustomStatusesList(); len(custom) > 0 {
		var customErr error
		if customRows, customErr = listDuplicateRows(store, custom, time.Time{}); customErr != nil {
			warn(beadsDir, customErr)
		}
	}
	known := append(append(append([]*beads.Issue(nil), activeRows...), closedRows...), customRows...)
	active, activeDeferred, activeErr := duplicateCandidatesOf(store, beadsDir, activeRows, details, known)
	closed, closedDeferred, closedErr := duplicateCandidatesOf(store, beadsDir, closedRows, details, known)
	if details != nil {
		listed := make(map[string]bool, len(known))
		for _, row := range known {
			listed[row.ID] = true
		}
		details.retain(beadsDir, listed)
		if deferred := activeDeferred + closedDeferred; deferred > 0 {
			warn(beadsDir, fmt.Errorf("design/notes of %d of %d bead(s) not read this tick, over its limit of %d; they are read on the next ticks",
				deferred, len(known), details.limit))
		}
	}
	if err := errors.Join(activeErr, closedErr); err != nil {
		warn(beadsDir, err)
	}
	return append(active, closed...), nil
}

// listDuplicateCandidates lists one status set from store and reduces every
// row to its comparison fields, then enriches every row with a batched show
// for design and notes text, reporting a failed enrichment to warn. bd list
// --json never carries those two fields, and a closed-after filter excludes
// rows that were never closed, so live and closed work take separate list
// queries; neither can be folded into the other.
func listDuplicateCandidates(store beads.Client, warn func(beadsDir string, err error), beadsDir string, statuses []string, closedAfter time.Time) ([]duplicateCandidate, error) {
	rows, err := listDuplicateRows(store, statuses, closedAfter)
	if err != nil {
		return nil, err
	}
	candidates, _, err := duplicateCandidatesOf(store, beadsDir, rows, nil, nil)
	if err != nil {
		warn(beadsDir, err)
	}
	return candidates, nil
}

// listDuplicateRows is the bd list behind a pool read.
func listDuplicateRows(store beads.Client, statuses []string, closedAfter time.Time) ([]*beads.Issue, error) {
	rows, err := store.List(beads.ListOptions{Status: strings.Join(statuses, ","), Priority: -1, ClosedAfter: closedAfter})
	if err != nil {
		return nil, fmt.Errorf("listing %s beads: %w", strings.Join(statuses, ","), err)
	}
	return rows, nil
}

// duplicateCandidatesOf reduces listed rows to their comparison fields, with
// the design/notes enrichment described on listDuplicateCandidates. details,
// when not nil, is the cache that enrichment reads through for the rig at
// beadsDir, and known the rows its cached blockers are read against. It returns
// how many rows the cache left unread for the tick's limit, and the enrichment's
// error; the candidates are built either way, on list text where the show
// failed.
func duplicateCandidatesOf(store beads.Client, beadsDir string, rows []*beads.Issue, details *duplicateDetailCache, known []*beads.Issue) ([]duplicateCandidate, int, error) {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ID != "" {
			ids = append(ids, row.ID)
		}
	}
	// A bead whose overlap signal lives only in design or notes — the real
	// gt-g6b case (gt-mcq): the failing test names arrived as the polecat
	// worked, in design and notes, never in title or description — would
	// otherwise be invisible on the pool side forever, since bd list cannot
	// carry those fields (gt-hgvu). A failed enrichment degrades to
	// list-only refs rather than failing the whole pool fetch: the same
	// bd-hiccup-is-not-a-duplicate contract fetchDuplicatePool already holds.
	// The degrade is reported, though: a pool without design/notes text is
	// blind to exactly the overlap this check was extended to catch, and the
	// operator must be able to tell that apart from "no duplicates found".
	var detailsByID map[string]duplicateDetail
	var deferred int
	var detailErr error
	if details != nil {
		detailsByID, deferred, detailErr = details.read(beadsDir, store, rows, known)
	} else {
		detailsByID, detailErr = fetchDuplicateDetails(store, ids)
		if detailErr != nil {
			detailsByID = nil
		}
	}

	candidates := make([]duplicateCandidate, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		detail := detailsByID[row.ID]
		candidates = append(candidates, duplicateCandidate{
			ID:        row.ID,
			Title:     row.Title,
			Status:    row.Status,
			ClosedAt:  row.ClosedAt,
			IssueType: row.Type,
			Priority:  row.Priority,
			CreatedAt: row.CreatedAt,
			Blockers:  detail.Blockers,
			Ready:     readinessOf(row, detail),
			Refs: extractOverlapRefs(row.Title, row.Description, row.CloseReason,
				detail.Design, detail.Notes),
		})
	}
	return candidates, deferred, detailErr
}

// readinessOf reduces one listed bead to the fields the readiness question
// reads off an unstarted peer — whether the dispatcher could sling it — so the
// overlap guard can ask that of a pool row instead of a bd read per peer
// (gt-mknmb).
//
// The list row carries most of it. The two fields it does not are the ones the
// batched show is for: the design, and the blocking dependencies with the
// statuses that say which of them are still open. A row the show did not
// answer leaves those two unread, which reads as the dispatcher's to sling —
// the guard keeps refusing, and the pool warns that its comparison is degraded.
func readinessOf(row *beads.Issue, detail duplicateDetail) sling.Readiness {
	return sling.Readiness{
		Assignee:     row.Assignee,
		Labels:       row.Labels,
		Design:       detail.Design,
		Notes:        detail.Notes,
		Description:  row.Description,
		Acceptance:   row.AcceptanceCriteria,
		OpenBlockers: detail.OpenBlockers,
	}
}

// readinessOfBeadInfo is readinessOf for the bead the check is about to sling,
// from the show the sling guards read it through. That candidate joins the pool
// once its hook lands (noteSlingCandidateDispatched) and can be a peer to the
// next bead of the same burst, so it has to answer the hold question as the
// record does — the halved row a pool fetch degrades to is not the one to
// answer it from (gt-mknmb).
func readinessOfBeadInfo(info *beadInfo) sling.Readiness {
	return sling.Readiness{
		Assignee:     info.Assignee,
		Labels:       info.Labels,
		Design:       info.Design,
		Notes:        info.Notes,
		Description:  info.Description,
		Acceptance:   info.Acceptance,
		OpenBlockers: beads.UnresolvedBlockerIDs(info.Dependencies),
	}
}

// warnDuplicateEnrichmentFailed tells the operator the duplicate check is
// running on list-only text. It writes to stderr so it cannot corrupt a
// caller's stdout report.
func warnDuplicateEnrichmentFailed(beadsDir string, err error) {
	fmt.Fprintf(os.Stderr, "%s duplicate check degraded for %s: design/notes enrichment incomplete, comparing title/description only where it is: %v\n",
		style.Dim.Render("Warning:"), beadsDir, err)
}

// duplicateDetail is what one batched show recovers for a pool bead beyond its
// bd list row: the design and notes text, which bd list's JSON never carries,
// the blocking dependency edges, which the guard walks to tell a bead in the
// candidate's chain from a duplicate (gt-xydyc), and the blockers that are
// still open, which only a show's dependency statuses can name (gt-mknmb).
type duplicateDetail struct {
	Design   string
	Notes    string
	Blockers []string
	// OpenBlockers is Blockers less the dependencies that are resolved: the
	// edges bd's ready query holds the bead off the board for.
	OpenBlockers []string
}

// fetchDuplicateDetails recovers that detail for a pool of beads with a single
// batched show, so the pool comparison sees the same fields the sling-time
// candidate already does (checkSlingDuplicates reads its candidate via bd
// show, which carries design, notes and dependencies; bd list does not).
// Returns nil, nil for an empty pool — nothing to enrich.
func fetchDuplicateDetails(store beads.Client, ids []string) (map[string]duplicateDetail, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	issues, err := store.ShowMultiple(ids)
	if err != nil {
		return nil, fmt.Errorf("fetching design/notes for %d bead(s): %w", len(ids), err)
	}
	details := make(map[string]duplicateDetail, len(issues))
	for id, is := range issues {
		details[id] = duplicateDetail{
			Design:       is.Design,
			Notes:        is.Notes,
			Blockers:     blockingDepIDs(is.Dependencies),
			OpenBlockers: beads.UnresolvedBlockerIDs(is.Dependencies),
		}
	}
	return details, nil
}

// intersectTests returns the entries of a that name the same test as some entry
// of b.
func intersectTests(a, b []string) []string {
	found := map[string]bool{}
	for _, x := range a {
		for _, y := range b {
			if specdispatch.TestNamesOverlap(x, y) {
				found[x] = true
			}
		}
	}
	return sortedKeys(found)
}

// intersectStrings returns the entries common to a and b.
func intersectStrings(a, b []string) []string {
	found := map[string]bool{}
	other := make(map[string]bool, len(b))
	for _, y := range b {
		other[y] = true
	}
	for _, x := range a {
		if other[x] {
			found[x] = true
		}
	}
	return sortedKeys(found)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// errSlingDuplicateContent is the sentinel for a refused sling, so callers that
// need to distinguish the refusal from a bd failure can.
var errSlingDuplicateContent = sling.ErrDuplicateContent
