package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/rig"
)

// EditorialRequiredCheck warns when a rig repo carries a rubric (.om.json)
// but merge_queue.editorial.required is not set, so the rubric silently has
// no enforcement — a landed .om.json invites the assumption that reviews are
// gated when they are not.
type EditorialRequiredCheck struct {
	BaseCheck
}

// NewEditorialRequiredCheck creates a new editorial-required check.
func NewEditorialRequiredCheck() *EditorialRequiredCheck {
	return &EditorialRequiredCheck{
		BaseCheck: BaseCheck{
			CheckName:        "editorial-required",
			CheckDescription: "Warn when a rig has an om rubric but editorial review is not required",
			CheckCategory:    CategoryRig,
		},
	}
}

// Run checks whether the rig repo's .om.json rubric is backed by
// merge_queue.editorial.required.
//
// The rubric is read as the rig repo's main has it, so a clone that has not
// pulled since the rubric landed does not read as a rig with no rubric
// (gt-yj6g1); a repo this run cannot read warns rather than reporting none.
func (c *EditorialRequiredCheck) Run(ctx *CheckContext) *CheckResult {
	rigPath := ctx.RigPath()
	if rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rig specified",
		}
	}

	rubric, err := rig.ReadRepoFile(ctx.rigRepoOpener(), ctx.TownRoot, ctx.RigName, ".om.json")
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("cannot read the rig's .om.json rubric: %v", err),
			FixHint: "Verify the rig repo is readable; until then this check cannot tell whether the rig has a rubric",
		}
	}
	if !rubric.Found {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No .om.json rubric in rig repo",
		}
	}

	mq := rig.ResolveMergeQueueConfigIn(ctx.rigRepoOpener(), ctx.TownRoot, ctx.RigName)
	required := mq != nil && mq.Editorial != nil && mq.Editorial.Required
	if required {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "editorial review required and rubric present",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "rig has .om.json but merge_queue.editorial.required is false",
		Details: []string{fmt.Sprintf("rubric: %s", rubric.Source)},
		FixHint: "Set merge_queue.editorial.required=true in config.json, or remove .om.json if editorial review is not intended for this rig",
	}
}
