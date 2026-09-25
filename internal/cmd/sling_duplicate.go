package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

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
// points defeat.

const (
	// duplicateLookback bounds the recently-closed half of the comparison pool.
	// Work closed within this window is still plausibly the same defect being
	// re-filed; beyond it, overlap is more likely to be coincidence.
	duplicateLookback = 72 * time.Hour

	// duplicatePoolTTL is how long a fetched pool is reused. Dispatches arrive
	// in bursts (batch sling, convoy feeds) and a pool fetch costs two bd list
	// round-trips, so a short TTL collapses a burst into one fetch. The window
	// is short enough that a bead closed seconds ago is seen by the next burst.
	duplicatePoolTTL = 60 * time.Second

	// duplicateMatchLimit caps how many overlapping beads a report lists before
	// summarizing the remainder. A bead whose description enumerates a whole
	// failing suite can overlap many others at once.
	duplicateMatchLimit = 5
)

var (
	// testNameRe matches a Go test identifier as it appears in bead prose.
	// The trailing character class includes "_" so the wildcard spelling used
	// in this repo ("TestRunPrimeExternalTools_*") is captured whole; trimming
	// happens in testNamesOverlap.
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
type contentRefs struct {
	Tests []string
	Files []string
}

func (r contentRefs) empty() bool { return len(r.Tests) == 0 && len(r.Files) == 0 }

// duplicateCandidate is one bead reduced to the fields the dedupe compares.
type duplicateCandidate struct {
	ID       string
	Title    string
	Status   string
	ClosedAt string
	Refs     contentRefs
}

// duplicateMatch records one existing bead whose content overlaps the
// candidate's, and on what.
type duplicateMatch struct {
	Bead        duplicateCandidate
	SharedTests []string
	SharedFiles []string
}

// Blocking reports whether the overlap is strong enough to refuse the sling.
// Shared test names mean two beads very probably describe one defect; a shared
// file alone is common between beads that are genuinely distinct work.
func (m duplicateMatch) Blocking() bool { return len(m.SharedTests) > 0 }

// slingDuplicateDecision is the outcome of a pre-sling dedupe check: whether to
// refuse, and the report to print either way. Message is empty when nothing
// overlapped.
type slingDuplicateDecision struct {
	Blocked bool
	Message string
}

// extractContentRefs pulls test names and file paths out of free-form bead
// prose. Callers pass every text field a bead carries (title, description,
// design, notes). Extraction is purely lexical — no index, no vocabulary
// model — because the two vantage points share none of the latter.
func extractContentRefs(parts ...string) contentRefs {
	tests := map[string]bool{}
	files := map[string]bool{}
	for _, part := range parts {
		for _, name := range testNameRe.FindAllString(part, -1) {
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

// testNamesOverlap reports whether two extracted identifiers name the same test
// or the same test family. Beads often cite a prefix rather than each member
// ("TestRunPrimeExternalTools_* (3)" against
// "TestRunPrimeExternalTools_BoundsSlowMailCheck"), so a prefix that ends on an
// identifier boundary counts as the same family; a prefix that ends mid-word
// does not.
func testNamesOverlap(a, b string) bool {
	a = strings.TrimRight(a, "_")
	b = strings.TrimRight(b, "_")
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	if shorter == "" || !strings.HasPrefix(longer, shorter) {
		return false
	}
	rest := longer[len(shorter):]
	return strings.HasPrefix(rest, "_")
}

// findDuplicateMatches compares one candidate against the pool, returning every
// overlap, blocking ones first.
func findDuplicateMatches(candidate duplicateCandidate, pool []duplicateCandidate) []duplicateMatch {
	var matches []duplicateMatch
	for _, other := range pool {
		if other.ID == candidate.ID || other.Refs.empty() {
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
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Blocking() != matches[j].Blocking() {
			return matches[i].Blocking()
		}
		return matches[i].Bead.ID < matches[j].Bead.ID
	})
	return matches
}

// decideSlingDuplicates renders the matches into a verdict and a report.
func decideSlingDuplicates(beadID string, matches []duplicateMatch) slingDuplicateDecision {
	if len(matches) == 0 {
		return slingDuplicateDecision{}
	}

	blocking := make([]duplicateMatch, 0, len(matches))
	warning := make([]duplicateMatch, 0, len(matches))
	for _, m := range matches {
		if m.Blocking() {
			blocking = append(blocking, m)
		} else {
			warning = append(warning, m)
		}
	}

	var b strings.Builder
	if len(blocking) > 0 {
		fmt.Fprintf(&b, "%s Refusing to sling %s: its content overlaps %d existing bead(s).\n",
			style.Error.Render("✗"), beadID, len(blocking))
		b.WriteString("  Two beads describing one defect from different vantage points share no\n")
		b.WriteString("  keywords, but they do name the same failing tests (gt-mcq).\n\n")
		writeMatchList(&b, blocking)
		fmt.Fprintf(&b, "\nIf this is genuinely distinct work, re-sling with:\n  gt sling %s <target> --force\n", beadID)
	} else {
		fmt.Fprintf(&b, "%s %s overlaps %d existing bead(s) on file paths only.\n",
			style.Warning.Render("⚠"), beadID, len(warning))
		b.WriteString("  Distinct beads touch the same files all the time, so this sling proceeds.\n\n")
		writeMatchList(&b, warning)
	}

	return slingDuplicateDecision{Blocked: len(blocking) > 0, Message: b.String()}
}

func writeMatchList(b *strings.Builder, matches []duplicateMatch) {
	shown := matches
	if len(shown) > duplicateMatchLimit {
		shown = shown[:duplicateMatchLimit]
	}
	for _, m := range shown {
		fmt.Fprintf(b, "  %s  %s\n", m.Bead.ID, statusPhrase(m.Bead))
		if len(m.SharedTests) > 0 {
			fmt.Fprintf(b, "      shared test: %s\n", strings.Join(m.SharedTests, ", "))
		}
		if len(m.SharedFiles) > 0 {
			fmt.Fprintf(b, "      shared file: %s\n", strings.Join(m.SharedFiles, ", "))
		}
	}
	if remaining := len(matches) - len(shown); remaining > 0 {
		fmt.Fprintf(b, "  ... and %d more\n", remaining)
	}
}

// statusPhrase renders a match's state for the report, including how recently a
// closed bead was closed — the age is what tells the operator whether this is
// work that landed an hour ago or a week ago.
func statusPhrase(c duplicateCandidate) string {
	if c.Status != "closed" {
		return c.Status
	}
	closedAt, err := time.Parse(time.RFC3339, c.ClosedAt)
	if err != nil {
		return "closed"
	}
	return "closed " + formatAge(closedAt)
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
	if info == nil {
		return nil, nil, nil
	}
	refs := extractContentRefs(info.Title, info.Description, info.Design, info.Notes)
	if refs.empty() {
		return nil, nil, nil
	}

	candidate := &duplicateCandidate{
		ID:     beadID,
		Title:  info.Title,
		Status: info.Status,
		Refs:   refs,
	}

	beadsDir := duplicateBeadsDir(townRoot, beadID)
	if beadsDir == "" {
		return candidate, nil, nil
	}
	pool, err := duplicatePoolFor(beadsDir)
	if err != nil {
		return candidate, nil, fmt.Errorf("duplicate check skipped: %w", err)
	}
	return candidate, findDuplicateMatches(*candidate, pool), nil
}

// noteSlingCandidateDispatched adds a bead to the cached pool once its sling
// landed, so the next dispatch in the same burst compares against it too.
// Without this, two duplicates dispatched in one batch — the pattern behind
// gt-3vr/gt-rl0 — would both miss each other, because the pool snapshot
// predates both. A nil candidate (the check was skipped) is a no-op.
func noteSlingCandidateDispatched(townRoot string, candidate *duplicateCandidate) {
	if candidate == nil {
		return
	}
	if beadsDir := duplicateBeadsDir(townRoot, candidate.ID); beadsDir != "" {
		noteDispatchedCandidate(beadsDir, *candidate)
	}
}

// noteDispatchedCandidate appends one already-dispatched bead to a rig's cached
// pool. It is a no-op when the pool was never fetched: a fresh fetch would find
// the bead in the database anyway, now that its hook has landed.
func noteDispatchedCandidate(beadsDir string, candidate duplicateCandidate) {
	duplicatePoolMu.Lock()
	defer duplicatePoolMu.Unlock()

	entry, ok := duplicatePoolCache[beadsDir]
	if !ok {
		return
	}
	for _, existing := range entry.candidates {
		if existing.ID == candidate.ID {
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

var (
	// fetchDuplicatePoolFn is the pool reader, swappable in tests.
	fetchDuplicatePoolFn = fetchDuplicatePool

	duplicatePoolMu    sync.Mutex
	duplicatePoolCache = map[string]*duplicatePoolEntry{}
)

// duplicatePoolFor returns the comparison pool for a rig, reusing a snapshot
// taken within duplicatePoolTTL. Returns a slice shared with the cache; callers
// must not mutate it.
func duplicatePoolFor(beadsDir string) ([]duplicateCandidate, error) {
	duplicatePoolMu.Lock()
	defer duplicatePoolMu.Unlock()

	if entry, ok := duplicatePoolCache[beadsDir]; ok && time.Since(entry.fetchedAt) < duplicatePoolTTL {
		return entry.candidates, nil
	}

	candidates, err := fetchDuplicatePoolFn(beadsDir)
	if err != nil {
		return nil, err
	}
	duplicatePoolCache[beadsDir] = &duplicatePoolEntry{fetchedAt: time.Now(), candidates: candidates}
	return candidates, nil
}

// resetDuplicatePoolCache clears the process-wide pool cache. Tests call it to
// isolate one another; production code never needs to.
func resetDuplicatePoolCache() {
	duplicatePoolMu.Lock()
	defer duplicatePoolMu.Unlock()
	duplicatePoolCache = map[string]*duplicatePoolEntry{}
}

// fetchDuplicatePool reads the live and recently-closed beads of one rig.
func fetchDuplicatePool(beadsDir string) ([]duplicateCandidate, error) {
	active, err := listDuplicateCandidates(beadsDir, duplicatePoolStatuses, time.Time{})
	if err != nil {
		return nil, err
	}
	closed, err := listDuplicateCandidates(beadsDir, []string{"closed"}, time.Now().Add(-duplicateLookback))
	if err != nil {
		return nil, err
	}
	return append(active, closed...), nil
}

// listDuplicateCandidates runs one bd list and reduces every row to its
// comparison fields, then enriches every row with a batched bd show for
// design and notes text. bd list --json never carries those two fields, and
// --closed-after excludes rows that were never closed, so live and closed
// work take separate list queries; neither can be folded into the other.
func listDuplicateCandidates(beadsDir string, statuses []string, closedAfter time.Time) ([]duplicateCandidate, error) {
	args := []string{
		"list",
		"--status=" + strings.Join(statuses, ","),
		"--json",
		"--flat",
		"-n", "0",
		"--no-pager",
	}
	if !closedAfter.IsZero() {
		args = append(args, "--closed-after="+closedAfter.UTC().Format(time.RFC3339))
	}

	out, err := runBdJSONAllowStale(beadsDir, args...)
	if err != nil {
		return nil, fmt.Errorf("listing %s beads: %w", strings.Join(statuses, ","), err)
	}

	var rows []struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Status      string `json:"status"`
		ClosedAt    string `json:"closed_at"`
		CloseReason string `json:"close_reason"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}

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
	fullText, ftErr := fetchDuplicateFullText(beadsDir, ids)
	if ftErr != nil {
		fullText = nil
	}

	candidates := make([]duplicateCandidate, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		text := fullText[row.ID]
		candidates = append(candidates, duplicateCandidate{
			ID:       row.ID,
			Title:    row.Title,
			Status:   row.Status,
			ClosedAt: row.ClosedAt,
			Refs: extractContentRefs(row.Title, row.Description, row.CloseReason,
				text.Design, text.Notes),
		})
	}
	return candidates, nil
}

// duplicateFullText holds the design and notes text bd show returns for one
// bead — the two fields bd list never carries.
type duplicateFullText struct {
	Design string
	Notes  string
}

// fetchDuplicateFullText recovers design and notes text for a pool of beads
// with a single batched "bd show <ids...>" call, so the pool comparison sees
// the same fields the sling-time candidate already does (checkSlingDuplicates
// reads its candidate via bd show, which carries design and notes; bd list
// does not). Returns nil, nil for an empty pool — nothing to enrich.
func fetchDuplicateFullText(beadsDir string, ids []string) (map[string]duplicateFullText, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"show"}, ids...)
	args = append(args, "--json")

	out, err := runBdJSONAllowStale(beadsDir, args...)
	if err != nil {
		return nil, fmt.Errorf("fetching design/notes for %d bead(s): %w", len(ids), err)
	}

	var rows []struct {
		ID     string `json:"id"`
		Design string `json:"design"`
		Notes  string `json:"notes"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parsing bd show output: %w", err)
	}

	text := make(map[string]duplicateFullText, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		text[row.ID] = duplicateFullText{Design: row.Design, Notes: row.Notes}
	}
	return text, nil
}

// intersectTests returns the entries of a that name the same test as some entry
// of b.
func intersectTests(a, b []string) []string {
	found := map[string]bool{}
	for _, x := range a {
		for _, y := range b {
			if testNamesOverlap(x, y) {
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
var errSlingDuplicateContent = errors.New("duplicate content")
