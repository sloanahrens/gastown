package cmd

import (
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// checklistStep is one step of an agent's checklist: the formula's step id,
// title and body as bd cooks them.
type checklistStep struct {
	ID          string
	Title       string
	Description string
}

// primeChecklist is the checklist prime renders for an attachment: the
// attached formula as bd cooks it (D6, gt-fd2cu.2). Steps poured for the same
// attachment are not read, so the checklist is always the formula as it now
// reads. name is the formula.
type primeChecklist struct {
	name  string
	steps []checklistStep
}

// attachmentChecklist cooks the attachment's formula and returns its steps. An
// attachment that names no formula has nothing to cook, which is an error
// rather than a guess.
func (r RoleInfo) attachmentChecklist(attachment *beads.AttachmentFields) (primeChecklist, error) {
	if attachment.AttachedFormula == "" {
		return primeChecklist{}, fmt.Errorf("attachment names no formula to cook")
	}
	c := primeChecklist{name: attachment.AttachedFormula}
	f, err := r.formulaCooker().cookForRender(c.name, r.TownRoot, r.Rig, attachmentFormulaVars(attachment))
	if err != nil {
		return c, err
	}
	c.steps = f.checklist()
	return c, nil
}

// showChecklist writes the bounded checklist, or one warning line when it cannot be
// read.
func (r RoleInfo) showChecklist(w io.Writer, attachment *beads.AttachmentFields) {
	c, err := r.attachmentChecklist(attachment)
	if err != nil {
		style.PrintWarning("%v", err)
		return
	}
	_, _ = fmt.Fprint(w, renderFormulaChecklist(c.name, c.steps, 1))
}
