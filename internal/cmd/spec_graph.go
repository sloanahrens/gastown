package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specLintGraph is the --graph flag of gt spec lint: read an epic's children
// as a graph instead of linting one bead's shape (gt-hgt9j.16, bead-lint
// design decision 3).
var specLintGraph bool

// specGraphReads is the bead reads the graph command works over. show is the
// reader per-bead lint uses, so the graph and the shape lint read a bead the
// same way; children is bd's parent-child view, which is the only read that
// sees a wisp child (gt-gektq).
type specGraphReads struct {
	show     func(id string) (*beads.Issue, error)
	children func(parentID string) ([]*beads.Issue, error)
}

// specGraphFinding is one finding as --json prints it. The Go field names in
// specdispatch.GraphFinding are not the wire names, so the graph command sets
// them here.
type specGraphFinding struct {
	Kind   string `json:"kind"`
	Bead   string `json:"bead"`
	Detail string `json:"detail"`
}

// specGraphReport is the --json body of gt spec lint --graph.
type specGraphReport struct {
	Epic     string             `json:"epic"`
	Findings []specGraphFinding `json:"findings"`
}

// runSpecGraph is gt spec lint --graph over stores: the production call passes
// slingStores{} (bd), and a test passes fake stores so the reads it makes are
// the ones under test.
func runSpecGraph(out, errOut io.Writer, stores slingStores, townRoot, epicID string, asJSON bool) error {
	return runSpecGraphAt(out, errOut, specGraphReads{
		show:     func(id string) (*beads.Issue, error) { return stores.show(townRoot, id) },
		children: func(parentID string) ([]*beads.Issue, error) { return stores.children(townRoot, parentID) },
	}, epicID, asJSON)
}

// runSpecGraphAt is runSpecGraph with its reads given, so a test can seed a
// graph. It prints one line per finding, or the report with asJSON, and exits
// 1 when any finding but the duplicate-test advisory exists. A read or parse
// failure, or an epic with no children, prints to errOut naming the bead and
// exits 1 as well: a graph that could not be read, or an epic with nothing to
// check, is never reported clean.
func runSpecGraphAt(out, errOut io.Writer, reads specGraphReads, epicID string, asJSON bool) error {
	graph, err := specGraphBeads(reads, epicID)
	if err != nil {
		fmt.Fprintf(errOut, "gt spec lint --graph: %v\n", err)
		return NewSilentExit(specLintExitRefused)
	}
	findings := specdispatch.CheckGraph(graph)
	report := specGraphReport{Epic: epicID, Findings: make([]specGraphFinding, 0, len(findings))}
	refused := false
	for _, finding := range findings {
		report.Findings = append(report.Findings, specGraphFinding{
			Kind:   finding.Kind,
			Bead:   finding.Bead,
			Detail: finding.Detail,
		})
		if finding.Kind != specdispatch.KindDuplicateTest {
			refused = true
		}
	}
	if asJSON {
		if err := json.NewEncoder(out).Encode(report); err != nil {
			return err
		}
	} else {
		for _, finding := range report.Findings {
			fmt.Fprintf(out, "%s: %s: %s\n", finding.Kind, finding.Bead, finding.Detail)
		}
	}
	if refused {
		return NewSilentExit(specLintExitRefused)
	}
	return nil
}

// specGraphBeads reads epicID and its direct children and reduces each child
// to what CheckGraph reads. The epic itself is read first, so an id that does
// not exist is refused rather than answered by a child read that could come
// back empty and pass as a clean graph.
//
// An epic with no children is refused rather than checked: an empty graph has
// no finding and would pass as clean, and a wrong id or a child set the read
// did not see is the likelier cause.
func specGraphBeads(reads specGraphReads, epicID string) ([]specdispatch.GraphBead, error) {
	epic, err := reads.show(epicID)
	if err != nil {
		return nil, fmt.Errorf("cannot read epic %s: %w", epicID, err)
	}
	if epic == nil {
		return nil, fmt.Errorf("cannot read epic %s: the read returned no bead", epicID)
	}
	kids, err := reads.children(epicID)
	if err != nil {
		return nil, fmt.Errorf("cannot read the children of %s: %w", epicID, err)
	}
	if len(kids) == 0 {
		return nil, fmt.Errorf("epic %s has no children to check", epicID)
	}
	graph := make([]specdispatch.GraphBead, 0, len(kids))
	for _, kid := range kids {
		// The children read need not carry the description and dependencies the
		// graph reads, so every child is re-read through the one bead reader.
		issue, err := reads.show(kid.ID)
		if err != nil {
			return nil, fmt.Errorf("cannot read child %s: %w", kid.ID, err)
		}
		if issue == nil {
			return nil, fmt.Errorf("cannot read child %s: the read returned no bead", kid.ID)
		}
		bead, err := graphBead(issue)
		if err != nil {
			return nil, fmt.Errorf("child %s: %w", issue.ID, err)
		}
		graph = append(graph, bead)
	}
	return graph, nil
}

// graphBead reduces one child bead to the fields CheckGraph reads: the names
// its Interfaces section defines and uses, the paths its Files section creates
// and modifies, its direct blocking dependencies, and the TestX name leading
// each acceptance box. A Files or Interfaces section that does not parse is
// returned, never read as an empty one.
func graphBead(issue *beads.Issue) (specdispatch.GraphBead, error) {
	interfaces, _, err := specdispatch.ParseInterfaces(issue.Description)
	if err != nil {
		return specdispatch.GraphBead{}, err
	}
	files, _, err := specdispatch.ParseFiles(issue.Description)
	if err != nil {
		return specdispatch.GraphBead{}, err
	}
	return specdispatch.GraphBead{
		ID:        issue.ID,
		DependsOn: blockingDependencyIDs(issue.Dependencies),
		Defines:   interfaces.Defines,
		Uses:      interfaces.Uses,
		Create:    files.Paths(specdispatch.FileCreate),
		Modify:    files.Paths(specdispatch.FileModify),
		TestNames: acceptanceTestNames(beads.AcceptanceText(issue)),
	}, nil
}

// blockingDependencyIDs returns the ids issue depends on directly, in dependency
// order and without repeats. The parent-child edge is dropped: it names the
// epic, not an ordering constraint among the children. Only blocking relations
// count, since a tracks or related edge orders nothing.
func blockingDependencyIDs(deps []beads.IssueDep) []string {
	var ids []string
	for _, dep := range deps {
		if !beads.IsBlockingDependencyType(dep.DependencyType) {
			continue
		}
		id := beads.ExtractIssueID(dep.ID)
		if id == "" || slices.Contains(ids, id) {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// acceptanceTestRe matches an acceptance box whose text opens with a TestX
// identifier and a colon, as in "- [ ] TestSpecGraphJSON: --json prints the
// report". A box that merely mentions a test name later in its text is not a
// test name.
var acceptanceTestRe = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+\[[ xX]\]\s+(Test[A-Za-z0-9_]+)\s*:`)

// acceptanceTestNames returns the TestX identifier leading each acceptance box
// of text, in order and without repeats.
func acceptanceTestNames(text string) []string {
	var names []string
	for _, item := range beads.AcceptanceItems(text) {
		m := acceptanceTestRe.FindStringSubmatch(item.Line)
		if m == nil || slices.Contains(names, m[1]) {
			continue
		}
		names = append(names, m[1])
	}
	return names
}
