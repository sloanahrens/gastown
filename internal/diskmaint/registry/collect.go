package registry

import (
	"errors"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// collectTarget is the Target name a collect-only plan carries, the name
// `gt maint` answers to for it.
const collectTarget = "registry-collect"

// actionCollect is the one action kind a collect-only plan carries. It names
// no manifest: the action is the garbage collect itself.
const actionCollect = "collect"

// errCollectMeasure is what a collect-only target answers to Measure: it
// reclaims space and does not hold any of its own to measure, so an empty
// reading would pass for a registry with nothing in it.
var errCollectMeasure = errors.New("registry-collect measures nothing: the registry target reports the registry's bytes")

// NewCollectTarget returns a Target that runs only the registry's garbage
// collect. The collect reclaims the space every delete freed, so a run that
// deleted nothing still has a collect due when an earlier run's deletes left
// space behind. It makes no deletes, and it takes the same lock and push guard
// as the delete path (see Apply).
func NewCollectTarget(cfg Config) *Target {
	t := NewTarget(cfg)
	t.collectOnly = true
	return t
}

// collectPlan is the plan a collect-only target makes: one collect, whatever
// the registry holds, because the collect's own guards decide whether it runs.
func collectPlan(p diskmaint.Policy) diskmaint.Plan {
	return diskmaint.Plan{
		Target: collectTarget,
		Policy: p,
		Actions: []diskmaint.Action{{
			Kind: actionCollect,
			Ref:  planTarget,
			Why:  "reclaims the blobs no kept tag shares, including space an earlier delete freed",
		}},
	}
}

// allCollectActions reports whether every action is a collect, the only kind a
// collect-only target applies.
func allCollectActions(actions []diskmaint.Action) bool {
	for _, action := range actions {
		if action.Kind != actionCollect {
			return false
		}
	}
	return true
}
