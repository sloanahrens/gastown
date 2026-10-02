package specdispatch

import (
	"encoding/json"
	"strings"
)

// LabelRedMain marks a bead the red-main owner filed for a package that stayed
// red on main (landworker.LabelRedMain). The dispatcher reads it to hold that
// bead back while the owner's revert of the same breakage is in flight.
const LabelRedMain = "red-main"

// Revert is the revert a rig's red-main owner has in flight: the culprit
// landing being reverted, and the revert work bead once it is filed (empty
// while the branch is still being built). The landing worker writes it in the
// rig's red-main state file under "revert" (landworker.MainState, gt-zkdwt).
type Revert struct {
	Culprit string `json:"culprit"`
	Bead    string `json:"bead,omitempty"`
}

// ParseRevert reads the revert a rig's red-main state file records. raw is the
// file's bytes; the result is nil when the state records no revert, and also
// when it is malformed. A dispatcher that cannot read the state must not
// invent a revert: the state is written only while one is building or queued,
// so reading silence as a hold would keep a red-main bead from a seat forever.
func ParseRevert(raw []byte) *Revert {
	var st struct {
		Revert *Revert `json:"revert"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return st.Revert
}

// RedMainHold reports why the dispatcher must leave spec to the red-main
// owner: spec is a red-main bead on a rig whose revert rv is in flight, so
// spending a seat on it starts a fix forward the revert supersedes (gt-zkdwt).
// A nil or culprit-less rv, or any bead without the label, is no hold.
func RedMainHold(spec Spec, rv *Revert) string {
	if rv == nil || strings.TrimSpace(rv.Culprit) == "" || !spec.HasLabel(LabelRedMain) {
		return ""
	}
	return "red-main revert of " + strings.TrimSpace(rv.Culprit) + " in flight"
}
