package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The candidate scan decides two things about every candidate before any sling:
// whether its labels admit it (specdispatch.Eligible) and whether its shape
// passes the lint (specdispatch.Lint). Both read the bead's own fields, which a
// tick gets from the ready board and from one details read per rig. Nearly every
// candidate repeats the verdict of the last tick, so the scan keeps each
// verdict in the annotations file, keyed by the board row it was decided from,
// and reads a candidate again only when that row has moved (gt-km9jt).
//
// The key is (bead id, updated_at, dependency_count) plus the knobs the verdict
// was decided under (specCandidateInputs): a bead's own edit moves updated_at,
// an edge added or removed moves dependency_count, and a change to the template,
// the gate, the priority ceiling or the target branch reads the candidate again.
// Staleness bound: a change to the bead is seen at the next tick, because it
// moves updated_at. A blocking edge that closes does not move the dependent's
// row, so a refused candidate is never recorded at all: it is read again every
// tick, and the edge's status is only kept for a bead the read found free to
// go. The children are read fresh every tick for the same reason (their change
// does not move the parent's row either).

// specCandidateInputs are the knobs a verdict was decided under. They are part
// of the record, not the key alone, so a tick whose template, gate, ceiling or
// target branch differs re-reads the candidate instead of serving a verdict
// decided under other rules.
type specCandidateInputs struct {
	// Template is a digest of the sections the lint checked.
	Template string `json:"template"`
	// ShapeGate is the gate the verdict was reached under.
	ShapeGate string `json:"shape_gate"`
	// MaxPriority is the operator's ceiling.
	MaxPriority int `json:"max_priority"`
	// Target is a digest of the branch inventory the lint checked Constraints
	// paths against.
	Target string `json:"target"`
}

// specCandidateVerdict is one candidate's shape and label verdict, as the tick
// that read it decided. It keeps the admission edges beside the verdict, so a
// later tick can check admission without the details read that supplied them.
type specCandidateVerdict struct {
	// UpdatedAt and DepCount are the board row the verdict was decided from:
	// the next tick serves it only while both still match.
	UpdatedAt string              `json:"updated_at"`
	DepCount  int                 `json:"dep_count"`
	Inputs    specCandidateInputs `json:"inputs"`

	// Eligible is specdispatch.Eligible's answer for the row, and Skip its
	// refusal. A retry a killed sling left labeled is decided before this
	// point, so the row Eligible saw is the one the tick sees.
	Eligible bool   `json:"eligible"`
	Skip     string `json:"skip,omitempty"`
	// Shaped reports that Shape is the lint's verdict for this row. A record an
	// earlier skip wrote (an ineligible bead) has no shape: the tick never
	// reached the lint.
	Shaped bool `json:"shaped,omitempty"`
	// Shape is specdispatch.Lint's verdict.
	Shape specdispatch.LintVerdict `json:"shape"`
	// Blockers are the bead's blocking edges as the details read returned them,
	// kept so a sling can still check admission without the read.
	Blockers []beads.IssueDep `json:"blockers,omitempty"`
}

// specCandidateVerdicts is the candidate scan's verdict memory: the annotated
// file's store, and the inputs factory that names the knobs the tick decides
// under.
type specCandidateVerdicts struct {
	memory *specAnnotated
	inputs func(rig string) specCandidateInputs
}

// newSpecCandidateVerdicts wires the verdict memory to an annotations file and
// the input factory. A nil memory answered no verdicts and remembered none, so
// a caller with none (a tick that keeps nothing) decides every candidate.
func newSpecCandidateVerdicts(memory *specAnnotated, inputs func(rig string) specCandidateInputs) *specCandidateVerdicts {
	return &specCandidateVerdicts{memory: memory, inputs: inputs}
}

// specCandidateInputsFor is the inputs factory a tick uses: the loaded
// template, the town's gate and ceiling, and the rig's target inventory.
// target is the tick's own per-rig cache, so a digest is computed once a tick.
func specCandidateInputsFor(tmpl specdispatch.Template, shapeGate string, maxPriority int, target func(rig string) *specdispatch.TargetTree) func(rig string) specCandidateInputs {
	template := specTemplateKey(tmpl)
	targets := map[string]string{}
	return func(rig string) specCandidateInputs {
		key, ok := targets[rig]
		if !ok {
			key = specTargetKey(target(rig))
			targets[rig] = key
		}
		return specCandidateInputs{Template: template, ShapeGate: shapeGate, MaxPriority: maxPriority, Target: key}
	}
}

// lookUp answers c's stored verdict when the tick's row and knobs still match
// the ones it was decided from, and false otherwise: a changed bead, an edge
// added or removed, a different template, gate, ceiling or target branch, or a
// record with no shape, which is what a blocked read leaves.
func (v *specCandidateVerdicts) lookUp(c specCandidate) (specCandidateVerdict, bool) {
	if v == nil || v.memory == nil || c.UpdatedAt == "" {
		return specCandidateVerdict{}, false
	}
	v.memory.mu.Lock()
	entry, ok := v.memory.verdicts[c.Spec.ID]
	v.memory.mu.Unlock()
	if !ok || entry.UpdatedAt != c.UpdatedAt || entry.DepCount != c.DepCount {
		return specCandidateVerdict{}, false
	}
	// An eligible candidate's shape was decided with its labels, so a record
	// without one never got past the admission check: the bead was blocked, or
	// the file is not one this code wrote. Both are read again, because a
	// blocker that closes does not move the dependent's row.
	if entry.Eligible && !entry.Shaped {
		return specCandidateVerdict{}, false
	}
	if entry.Inputs != v.inputs(c.Rig) {
		return specCandidateVerdict{}, false
	}
	return entry, true
}

// record keeps the verdict the tick reached for c, at the row it decided from.
// A row with no updated_at is not remembered: the next tick would have nothing
// to compare it against.
func (v *specCandidateVerdicts) record(c specCandidate, verdict specCandidateVerdict) {
	if v == nil || v.memory == nil || c.UpdatedAt == "" {
		return
	}
	verdict.UpdatedAt = c.UpdatedAt
	verdict.DepCount = c.DepCount
	verdict.Inputs = v.inputs(c.Rig)
	v.memory.mu.Lock()
	v.memory.verdicts[c.Spec.ID] = verdict
	v.memory.dirty = true
	v.memory.mu.Unlock()
}

// specTemplateKey digests the lint template: the sections it requires, and the
// source they were loaded from.
func specTemplateKey(t specdispatch.Template) string {
	h := sha256.New()
	h.Write([]byte(t.Source))
	for _, name := range t.Sections {
		h.Write([]byte{0})
		h.Write([]byte(name))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// specTargetKey digests a target inventory: the branch, the error that stopped
// the read, and the files it holds. No inventory is its own digest, so a tick
// that could not read one does not serve a verdict decided against another.
func specTargetKey(t *specdispatch.TargetTree) string {
	if t == nil {
		return "none"
	}
	files := make([]string, 0, len(t.Files))
	for path := range t.Files {
		files = append(files, path)
	}
	sort.Strings(files)
	h := sha256.New()
	h.Write([]byte(t.Branch))
	h.Write([]byte{0})
	h.Write([]byte(t.Err))
	for _, path := range files {
		h.Write([]byte{0})
		h.Write([]byte(path))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
