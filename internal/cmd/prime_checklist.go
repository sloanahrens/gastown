package cmd

import (
	"fmt"
	"io"
	"sort"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// checklistStep is one step of an agent's checklist. Read from a poured
// molecule, ID is the step bead and Status its live status; from a cook
// (no molecule), ID is the formula's step id and Status is empty.
type checklistStep struct {
	ID          string
	Title       string
	Description string
	Status      string
}

// primeChecklist is the checklist prime renders for an attachment (D6,
// gt-fd2cu.2). A poured molecule is the checklist: its step beads with their
// real statuses, so a formula edited on disk after the pour never changes a
// running molecule. Only an attachment with no molecule is cooked. name is
// the formula, or the molecule when no formula name was stored.
type primeChecklist struct {
	name     string
	molecule string
	steps    []checklistStep
}

// attachmentChecklist reads the checklist for attachment.
func (r RoleInfo) attachmentChecklist(attachment *beads.AttachmentFields) (primeChecklist, error) {
	c := primeChecklist{name: attachment.AttachedFormula, molecule: attachment.AttachedMolecule}
	if c.molecule != "" {
		if c.name == "" {
			c.name = c.molecule
		}
		steps, err := moleculeChecklist(r.moleculeReader(), c.molecule)
		if err != nil {
			return c, fmt.Errorf("read molecule %s: %w", c.molecule, err)
		}
		c.steps = steps
		return c, nil
	}
	f, err := r.formulaCooker().cookForRender(c.name, r.TownRoot, r.Rig, attachmentFormulaVars(attachment))
	if err != nil {
		return c, err
	}
	c.steps = f.checklist()
	return c, nil
}

// moleculeReader is the beads client prime reads poured molecules through.
func (r RoleInfo) moleculeReader() beads.Client {
	if r.molecules != nil {
		return r.molecules
	}
	return beads.New(rigBeadsRoot(r))
}

// currentStep is the 1-based step whose body the checklist shows in full:
// the first step of a molecule not yet closed, else step 1.
func (c primeChecklist) currentStep() int {
	for i, s := range c.steps {
		if s.Status != "" && s.Status != string(beads.StatusClosed) {
			return i + 1
		}
	}
	return 1
}

// showChecklist writes the bounded checklist, or one warning line when it cannot be
// read.
func (r RoleInfo) showChecklist(w io.Writer, attachment *beads.AttachmentFields) {
	c, err := r.attachmentChecklist(attachment)
	if err != nil {
		style.PrintWarning("%v", err)
		return
	}
	if c.molecule != "" && len(c.steps) == 0 {
		_, _ = fmt.Fprintf(w, "Molecule %s has no step beads.\n", c.molecule)
		return
	}
	_, _ = fmt.Fprint(w, renderFormulaChecklist(c.name, c.steps, c.currentStep()))
}

// moleculeChecklist reads the steps poured under molID with their statuses,
// each parent before its children and siblings in the order their blocking
// dependencies allow. A level of the tree is one read and the dependencies
// one more, so a flat molecule costs three bd calls.
func moleculeChecklist(c beads.Client, molID string) ([]checklistStep, error) {
	kids := map[string][]*beads.Issue{}
	parent := map[string]string{}
	var all []string
	for level := []string{molID}; len(level) > 0; {
		byParent, err := c.ChildrenOf(level...)
		if err != nil {
			return nil, err
		}
		var next []string
		for _, p := range level {
			for _, k := range byParent[p] {
				if _, seen := parent[k.ID]; seen || k.ID == molID {
					continue
				}
				parent[k.ID] = p
				kids[p] = append(kids[p], k)
				next = append(next, k.ID)
			}
		}
		all = append(all, next...)
		level = next
	}
	if len(all) == 0 {
		return nil, nil
	}
	shown, err := c.ShowMultiple(all)
	if err != nil {
		return nil, err
	}

	var out []checklistStep
	var walk func(p string)
	walk = func(p string) {
		for _, s := range orderSiblings(p, kids[p], shown, parent) {
			out = append(out, checklistStep{ID: s.ID, Title: s.Title, Description: s.Description, Status: s.Status})
			walk(s.ID)
		}
	}
	walk(molID)
	return out, nil
}

// orderSiblings orders p's child steps so each comes after the siblings it
// is blocked by (a dependency on a sibling's descendant counts as one on the
// sibling). Steps free at the same time keep pour's sequence suffix order,
// then ID order; a cycle appends the rest in that order.
func orderSiblings(p string, sibs []*beads.Issue, shown map[string]*beads.Issue, parent map[string]string) []*beads.Issue {
	sorted := append([]*beads.Issue(nil), sibs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		si, sj := extractStepSequence(sorted[i].ID), extractStepSequence(sorted[j].ID)
		if si != sj {
			return si < sj
		}
		return sorted[i].ID < sorted[j].ID
	})
	underP := func(id string) string {
		for id != "" {
			if parent[id] == p {
				return id
			}
			id = parent[id]
		}
		return ""
	}
	waits := map[string]map[string]bool{}
	for _, s := range sorted {
		is := shown[s.ID]
		if is == nil {
			continue
		}
		for _, d := range is.Dependencies {
			if !isBlockingDepType(d.DependencyType) {
				continue
			}
			if sib := underP(d.ID); sib != "" && sib != s.ID {
				if waits[s.ID] == nil {
					waits[s.ID] = map[string]bool{}
				}
				waits[s.ID][sib] = true
			}
		}
	}

	var out []*beads.Issue
	placed := map[string]bool{}
	for len(out) < len(sorted) {
		progressed := false
		for _, s := range sorted {
			if placed[s.ID] {
				continue
			}
			ready := true
			for dep := range waits[s.ID] {
				if !placed[dep] {
					ready = false
					break
				}
			}
			if ready {
				out = append(out, s)
				placed[s.ID] = true
				progressed = true
				break
			}
		}
		if !progressed {
			for _, s := range sorted {
				if !placed[s.ID] {
					out = append(out, s)
					placed[s.ID] = true
				}
			}
		}
	}
	return out
}
