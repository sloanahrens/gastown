package sling

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/style"
)

// ErrDuplicateContent is the sentinel a caller sees when the pre-dispatch
// content duplicate check refused a dispatch (gt-mcq).
var ErrDuplicateContent = errors.New("duplicate content")

// MatchLimit caps how many overlapping beads a report lists before summarizing
// the remainder. A bead whose description enumerates a whole failing suite can
// overlap many others at once.
const MatchLimit = 5

// ContentRefs is the set of test names and file paths a bead's text names.
type ContentRefs struct {
	Tests []string
	Files []string
}

// Empty reports whether the bead named neither a test nor a file.
func (r ContentRefs) Empty() bool { return len(r.Tests) == 0 && len(r.Files) == 0 }

// Duplicate is one bead reduced to the fields the dedupe compares.
type Duplicate struct {
	ID        string
	Title     string
	Status    string
	ClosedAt  string
	IssueType string
	// Priority and CreatedAt are the bead's dispatch-order keys. The guard
	// ranks an unstarted overlap against the candidate with them, so the bead
	// an all-open cluster lets through is the one the dispatcher would have
	// taken first (gt-nlkba).
	Priority  int
	CreatedAt string
	// Held names why the dispatcher would not sling this bead, empty when it
	// would. The verdict is the caller's, because deciding that a bead is the
	// dispatcher's to run is the dispatcher's own rule (gt-00gee).
	Held string
	// Ready is what that verdict reads off the bead. The pool builder fills it
	// from the rows a fetch already reads, so asking whether the dispatcher
	// would sling a peer costs no read of its own (gt-mknmb).
	Ready Readiness
	Refs  ContentRefs
	// HoldFiles is the file set the dispatcher's overlap hold reads: Refs.Files
	// minus the reference documents the bead only reads (gt-6t44f).
	HoldFiles []string
	// Blockers lists the IDs this bead has a blocking dependency on, as far as
	// the caller could read them from bd. The overlap guard walks these edges
	// to tell a bead of the candidate's own chain from a duplicate: a chain's
	// beads share tests because they are one piece of work (gt-xydyc).
	Blockers []string
}

// IsEpic reports whether the bead is an epic. An epic is a container for other
// work, so its prose enumerates its children's tests and files rather than
// naming one defect; sharing a test with it says nothing about duplication
// (gt-fudap).
func (d Duplicate) IsEpic() bool { return d.IssueType == "epic" }

// Readiness is the half of a pool bead the readiness question reads: the
// fields the spec dispatcher's candidate test asks of a bead before it spends
// a seat. The overlap guard ranks an unstarted peer ahead of a candidate only
// when the dispatcher could itself sling that peer, and the pool builder fills
// this from the rows a fetch already reads, so the fields below cost the guard
// no read of its own (gt-mknmb).
//
// An unread field reads exactly as an empty one: the bead looks like the
// dispatcher's to sling, which leaves the guard refusing — a delayed dispatch,
// never two seats on one defect. That direction is what makes the two reads a
// fetch already makes enough: bd list's row carries the labels, assignee,
// prose and acceptance fields, and the batched show behind it carries the
// design and the dependencies' statuses.
type Readiness struct {
	// Assignee and Labels are the candidate filter's own inputs: a bead
	// another agent already owns, or wearing a label the dispatcher routes
	// elsewhere, is not its to take (specdispatch.Eligible).
	Assignee string
	Labels   []string
	// Design and Notes carry a hold recorded on the bead itself, the half of
	// the candidate filter no label states (dispatch.DispatchHoldFields).
	Design string
	Notes  string
	// Description and Acceptance are the shape lint's inputs: its sections
	// come out of the description unless the bead keeps them in its acceptance
	// field (specdispatch.Lint).
	Description string
	Acceptance  string
	// OpenBlockers are the blocking dependencies that are not resolved. bd's
	// ready query leaves the bead off the board while any of them is open, so
	// however eligible the bead is, the dispatcher never reaches it
	// (gt-mknmb).
	OpenBlockers []string
}

// DuplicateMatch records one existing bead whose content overlaps the
// candidate's, and on what.
type DuplicateMatch struct {
	Bead        Duplicate
	SharedTests []string
	SharedFiles []string
}

// Blocking reports whether the overlap is strong enough to refuse the sling:
// a shared test name against work that is either in flight or the dispatcher's
// next move, since shared files alone are common between beads that are
// genuinely distinct (gt-4k3fj.5). candidate is the bead being slung, which
// the unstarted case ranks the overlap against.
//
// Work in flight on the test refuses the sling whoever the two beads are. An
// unstarted overlap — an open bead, holding no seat — refuses only when both
// of these hold: it precedes the candidate in dispatch order, and it is a bead
// the dispatcher would actually sling (Held empty). One is not enough. Every
// open bead refusing every other refused an all-open cluster forever and left
// the dispatcher nothing to sling (gt-nlkba); and a peer the dispatcher holds
// for a reason of its own — a shape the lint refuses, a hold label, a
// container whose child is open — is never slung at all, so ranking it first
// refused the rest of the cluster for as long as the hold lasted (gt-00gee).
func (m DuplicateMatch) Blocking(candidate Duplicate) bool {
	if len(m.SharedTests) == 0 {
		return false
	}
	if liveOverlapStatuses[m.Bead.Status] {
		return true
	}
	return m.Bead.Status == "open" && m.Bead.Held == "" && Precedes(m.Bead, candidate)
}

// liveOverlapStatuses are the statuses of work already in flight: a bead in
// either name already holds a seat on its defect.
var liveOverlapStatuses = map[string]bool{"in_progress": true, "hooked": true}

// Precedes reports whether a comes before b in dispatch order: lower priority
// number first, then earlier creation, then bead ID. It is the ready board's
// own total order (specdispatch.Order), which is what lets the overlap guard
// let exactly one bead of an all-open cluster through instead of refusing all
// of them (gt-nlkba).
func Precedes(a, b Duplicate) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	ta, tb := parseDispatchTime(a.CreatedAt), parseDispatchTime(b.CreatedAt)
	if !ta.Equal(tb) {
		// An unparseable time sorts after every parseable one.
		if ta.IsZero() {
			return false
		}
		if tb.IsZero() {
			return true
		}
		return ta.Before(tb)
	}
	return a.ID < b.ID
}

// parseDispatchTime reads the creation layouts bd writes — the same set
// specdispatch.parseTime reads, so the guard's ranking cannot drift from the
// board's on a spelling one of them accepted and the other did not.
func parseDispatchTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// DepEdge is one blocking dependency edge as bd records it: From depends on
// To.
type DepEdge struct {
	From string
	To   string
}

// RelatedBeads returns every bead the edges link to root within maxHops hops,
// walked in both directions because the candidate and the overlapping bead can
// be on either side of a blocking edge (gt-xydyc).
func RelatedBeads(root string, edges []DepEdge, maxHops int) map[string]bool {
	if root == "" || maxHops <= 0 {
		return nil
	}

	neighbors := make(map[string][]string, len(edges)*2)
	for _, e := range edges {
		if e.From == "" || e.To == "" || e.From == e.To {
			continue
		}
		neighbors[e.From] = append(neighbors[e.From], e.To)
		neighbors[e.To] = append(neighbors[e.To], e.From)
	}

	related := make(map[string]bool)
	seen := map[string]bool{root: true}
	frontier := []string{root}
	for hop := 0; hop < maxHops && len(frontier) > 0; hop++ {
		var next []string
		for _, id := range frontier {
			for _, neighbor := range neighbors[id] {
				if seen[neighbor] {
					continue
				}
				seen[neighbor] = true
				related[neighbor] = true
				next = append(next, neighbor)
			}
		}
		frontier = next
	}
	if len(related) == 0 {
		return nil
	}
	return related
}

// DropRelatedMatches removes the matches the candidate's dependency chain
// already links to it: counting them as overlaps refuses the base of a planned
// chain and leaves an automatic dispatcher nothing it can sling without
// --force (gt-xydyc).
func DropRelatedMatches(matches []DuplicateMatch, related map[string]bool) []DuplicateMatch {
	if len(related) == 0 {
		return matches
	}
	kept := make([]DuplicateMatch, 0, len(matches))
	for _, m := range matches {
		if related[m.Bead.ID] {
			continue
		}
		kept = append(kept, m)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// packageFixtureTests are test identifiers that name a package's own
// scaffolding rather than a defect. TestMain is Go's package entrypoint: every
// test binary defines it, so two beads that cite it share no work at all, yet
// the guard — which sees no package context — read it as a shared failing test
// and refused unrelated slings (gt-a0gk). A name joins this set when it proves
// per-package rather than defect-naming.
var packageFixtureTests = map[string]bool{
	"TestMain": true,
}

// IsPackageFixtureTest reports whether an extracted test identifier names
// per-package scaffolding rather than one test; callers drop these from a
// bead's refs. The trailing "_" of a wildcard citation ("TestMain_*") is
// ignored, so the same fixture is not readable two ways.
func IsPackageFixtureTest(name string) bool {
	return packageFixtureTests[strings.TrimRight(name, "_")]
}

// DuplicateDecision is the outcome of a pre-dispatch dedupe check: whether to
// refuse, and the report to print either way. Message is empty when nothing
// overlapped.
type DuplicateDecision struct {
	Blocked bool
	Message string
}

// Err renders the decision as the error a caller returns for a refused sling,
// or nil when it refused nothing. The error reports ErrDuplicateContent through
// errors.Is, so a caller that must tell a refusal from a failure — the CLI
// suppressing its usage block, a dispatcher deciding defer or fail — does not
// have to parse the report (gt-fudap).
func (d DuplicateDecision) Err() error {
	if !d.Blocked {
		return nil
	}
	return &duplicateRefusal{report: d.Message}
}

// duplicateRefusal is a refused sling's error. Report is the multi-line
// explanation; Is carries the sentinel.
type duplicateRefusal struct{ report string }

func (e *duplicateRefusal) Error() string { return e.report }

func (e *duplicateRefusal) Is(target error) bool { return target == ErrDuplicateContent }

// DecideDuplicates turns the overlaps found against a bead into the decision
// the dispatch acts on, and the report it prints either way. candidate is the
// bead being slung: it names the bead in the report and it is what each
// unstarted overlap is ranked against.
func DecideDuplicates(candidate Duplicate, matches []DuplicateMatch) DuplicateDecision {
	if len(matches) == 0 {
		return DuplicateDecision{}
	}
	beadID := candidate.ID

	blocking := make([]DuplicateMatch, 0, len(matches))
	warning := make([]DuplicateMatch, 0, len(matches))
	for _, m := range matches {
		if m.Blocking(candidate) {
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
		b.WriteString("  keywords, but they do name the same failing tests (gt-mcq).\n")
		b.WriteString("  Work in flight on that test refuses the sling; unstarted work refuses only\n")
		b.WriteString("  when it comes first in dispatch order AND is one the dispatcher would\n")
		b.WriteString("  sling, so a cluster of ready beads dispatches one at a time rather than\n")
		b.WriteString("  none of them, even when the bead ahead of this one is held.\n\n")
		writeMatchList(&b, blocking)
		// One line, and the report's last: an automatic dispatcher logs only
		// the last line of a failed sling, so it has to carry both the reason
		// and the remediation or the dispatcher log is blank again (gt-fudap).
		fmt.Fprintf(&b, "\n%s; re-sling with --force if this is genuinely distinct: gt sling %s <target> --force\n",
			dispatch.DuplicateRefusalClosing(beadID, len(blocking)), beadID)
	} else {
		fmt.Fprintf(&b, "%s %s overlaps %d existing bead(s), none of them in flight or next for the dispatcher.\n",
			style.Warning.Render("⚠"), beadID, len(warning))
		b.WriteString("  Shared files alone, closed or parked work, unstarted work this bead would\n")
		b.WriteString("  reach first, and unstarted work the dispatcher holds for a reason of its\n")
		b.WriteString("  own do not block; this sling proceeds.\n\n")
		writeMatchList(&b, warning)
	}

	return DuplicateDecision{Blocked: len(blocking) > 0, Message: b.String()}
}

func writeMatchList(b *strings.Builder, matches []DuplicateMatch) {
	shown := matches
	if len(shown) > MatchLimit {
		shown = shown[:MatchLimit]
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

// statusPhrase renders a match's state for the report: the hold that keeps an
// unstarted bead from refusing the sling, and how recently a closed bead was
// closed — the age is what tells the operator whether this is work that landed
// an hour ago or a week ago.
func statusPhrase(c Duplicate) string {
	if c.Status == "open" && c.Held != "" {
		return "open, held from dispatch: " + c.Held
	}
	if c.Status != "closed" {
		return c.Status
	}
	closedAt, err := time.Parse(time.RFC3339, c.ClosedAt)
	if err != nil {
		return "closed"
	}
	return "closed " + formatAge(closedAt)
}

// formatAge returns a human-readable age string.
func formatAge(t time.Time) string {
	d := time.Since(t)

	if d < time.Hour {
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", days)
}
