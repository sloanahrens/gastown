package doctor

import (
	"fmt"
	"os"
	"path/filepath"

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
func (c *EditorialRequiredCheck) Run(ctx *CheckContext) *CheckResult {
	rigPath := ctx.RigPath()
	if rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rig specified",
		}
	}

	rubricPath := filepath.Join(rigPath, "mayor", "rig", ".om.json")
	if _, err := os.Stat(rubricPath); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No .om.json rubric in rig repo",
		}
	}

	mq := rig.ResolveMergeQueueConfig(ctx.TownRoot, ctx.RigName)
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
		Details: []string{fmt.Sprintf("rubric: %s", rubricPath)},
		FixHint: "Set merge_queue.editorial.required=true in config.json, or remove .om.json if editorial review is not intended for this rig",
	}
}
