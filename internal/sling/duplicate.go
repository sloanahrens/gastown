package sling

import (
	"errors"
	"fmt"
	"strings"
	"time"

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
	ID       string
	Title    string
	Status   string
	ClosedAt string
	Refs     ContentRefs
}

// DuplicateMatch records one existing bead whose content overlaps the
// candidate's, and on what.
type DuplicateMatch struct {
	Bead        Duplicate
	SharedTests []string
	SharedFiles []string
}

// Blocking reports whether the overlap is strong enough to refuse the sling:
// a shared test name against live work, since shared files alone are common
// between beads that are genuinely distinct (gt-4k3fj.5).
func (m DuplicateMatch) Blocking() bool {
	return len(m.SharedTests) > 0 && blockingStatuses[m.Bead.Status]
}

// blockingStatuses are the statuses whose overlap refuses a sling.
var blockingStatuses = map[string]bool{"open": true, "in_progress": true, "hooked": true}

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

// DecideDuplicates turns the overlaps found against a bead into the decision
// the dispatch acts on, and the report it prints either way.
func DecideDuplicates(beadID string, matches []DuplicateMatch) DuplicateDecision {
	if len(matches) == 0 {
		return DuplicateDecision{}
	}

	blocking := make([]DuplicateMatch, 0, len(matches))
	warning := make([]DuplicateMatch, 0, len(matches))
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
		fmt.Fprintf(&b, "%s %s overlaps %d existing bead(s), none of them live work sharing a test.\n",
			style.Warning.Render("⚠"), beadID, len(warning))
		b.WriteString("  Shared files alone, or overlap with closed/parked work, does not block; this sling proceeds.\n\n")
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

// statusPhrase renders a match's state for the report, including how recently a
// closed bead was closed — the age is what tells the operator whether this is
// work that landed an hour ago or a week ago.
func statusPhrase(c Duplicate) string {
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
