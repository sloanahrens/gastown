// Package estimate combines the story-point inputs into one estimate and the
// note that carries it on a bead (gt-5d9k2f.12). It is separate from the core
// storypoints package because it reaches hm and the Jev client, which code that
// only needs the core must not pull into the daemon's import closure.
package estimate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/hmhints"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/storypoints"
	"github.com/steveyegge/gastown/internal/storypoints/nearest"
)

// neighborLimit is the most past beads the note names, the design's hm limit.
const neighborLimit = 5

// ErrNoBead reports an Estimate call with no bead.
var ErrNoBead = errors.New("storypoints/estimate: estimate needs a bead")

// ErrNoBeadText reports a bead with no title, description or acceptance field.
// Such a bead has no text to build the shape input from, so it has no
// estimate; the caller records the rung as none.
var ErrNoBeadText = errors.New("storypoints/estimate: bead has no text to estimate")

// JevAsker asks the dispatch battery about a bead before it has a seat.
// *dispatch.Pass's Ask method fits it: Deps.JevAsk = pass.Ask.
type JevAsker func(ctx context.Context, issue *beads.Issue) (dispatch.Answers, error)

// Deps are the estimate's inputs. Every dependency is optional: a nil one is
// skipped, the reason goes in the result, and the blend falls to the rung its
// remaining inputs make.
type Deps struct {
	// Weights is the model; a non-nil value is used as is and no runtime file
	// is read. Nil falls to WeightsLoader, then to LoadTownWeights.
	Weights *storypoints.Weights
	// WeightsLoader loads the weights for TownRoot when Weights is nil. The
	// LoadTownWeights default loads each town once per process, so a rejected
	// runtime file is reported in one line however many estimates run.
	WeightsLoader *storypoints.TownWeightsLoader
	// TownRoot is the town whose runtime weights are read when Weights is nil.
	// Empty reads none, so the embedded weights are used.
	TownRoot string
	// Embedder turns the section text into the vector the embedding ridge reads.
	Embedder storypoints.Embedder
	// JevAsk asks the battery; wrap a *dispatch.Pass's Ask method.
	JevAsk JevAsker
	// HMRunner runs hm for the nearest past beads. It only names them; it never
	// changes the number.
	HMRunner hmhints.Runner
	// Rig is the bead's rig, which sets the rig_gt and rig_dv shape terms. The
	// caller reads it from the bead's ID prefix (specBeadRig); empty is unknown,
	// as it is for every bead estimated before a rig is known.
	Rig string
	// Prior returns the component minutes an earlier estimate of the bead
	// recorded for the same SectionHash. A nil Prior, or ok false, estimates as
	// usual, and so does a Prior with either minutes value not valid.
	Prior func(beadID, hash string) (PriorMinutes, bool)
}

// PriorMinutes are the component minutes an earlier estimate of one bead text
// recorded: the embedding ridge's minutes and the Jev minutes.
type PriorMinutes struct {
	Embed float64
	Jev   float64
}

// Skip names an input the estimate could not use and why.
type Skip struct {
	Input  string
	Reason string
}

// ComponentMinutes is each input's predicted minutes, as the estimate log
// records them beside the features the inputs were built from.
type ComponentMinutes struct {
	// Shape is the shape model's minutes; a result always has one.
	Shape *float64
	// Embed is the embedding ridge's minutes; nil when the estimate had no
	// embedding input.
	Embed *float64
	// Jev is the minutes the fits_twenty_minutes probability maps to; nil when
	// the estimate had no Jev input.
	Jev *float64
}

// Result is one estimate and everything the note says about it.
type Result struct {
	// Points is the point the blended minutes round to.
	Points int
	// Minutes is the blended estimate before rounding.
	Minutes float64
	// Shape is the twelve shape features the estimate read.
	Shape storypoints.Shape
	// Embedding is the vector the embedding ridge read; nil when the estimate
	// had no embedding input.
	Embedding []float32
	// ComponentMinutes is each input's predicted minutes.
	ComponentMinutes ComponentMinutes
	// Inputs names the inputs the blend averaged, in the blend's names:
	// shape, embed, jev.
	Inputs []string
	// Skipped names each input that was not built, and why, in build order.
	Skipped []Skip
	// Rung names the inputs of this estimate: all, shape-jev, shape-embed or
	// shape.
	Rung string
	// Neighbors are the past beads hm ranked closest. Empty when hm answered
	// with none or was skipped.
	Neighbors []nearest.Neighbor
	// NeighborsSkipped says why there are no neighbors to name, empty when hm
	// answered. The note carries it so the loss is never silent.
	NeighborsSkipped string
	// Hash is SectionHash of the bead the estimate was made from. A later run
	// compares it with the hash in the last note to skip an unchanged bead.
	Hash string
	// WeightsVersion is Weights.Version of the model the estimate used, so a
	// number can be traced to the file that made it. The note carries it.
	WeightsVersion string
}

// Estimate builds every input it can, blends them into one estimate, and names
// the nearest past beads. An input whose dependency is nil or fails is left
// out of the blend and its reason recorded in the result, so a missing
// dependency lowers the rung instead of failing the estimate. A bead with no
// text, or a canceled context, is an error and no result.
func Estimate(ctx context.Context, deps Deps, issue *beads.Issue) (Result, error) {
	return estimateWith(ctx, deps, issue, storypoints.Blend)
}

// estimateWith is Estimate with the blend step passed in. Every input Estimate
// builds is checked before the blend, so a valid set of inputs cannot make
// storypoints.Blend fail; the seam drives the refusal that would otherwise be
// unreachable.
func estimateWith(ctx context.Context, deps Deps, issue *beads.Issue, blend func(storypoints.Inputs) (storypoints.Blended, error)) (Result, error) {
	if issue == nil {
		return Result{}, ErrNoBead
	}
	if beadTextEmpty(issue) {
		return Result{}, ErrNoBeadText
	}

	weights := weightsFor(deps)

	// The seat is unknown before dispatch, so the shape input neutralizes its
	// term with the training mean.
	shape := storypoints.Features(storypoints.EstimateText(issue), deps.Rig, storypoints.SeatUnknown, weights)
	shapeMinutes, err := weights.ShapeMinutes(shape)
	if err != nil {
		return Result{}, fmt.Errorf("storypoints/estimate: shape input: %w", err)
	}
	componentMinutes := ComponentMinutes{Shape: &shapeMinutes}

	in := storypoints.Inputs{ShapeMinutes: &shapeMinutes, Weights: weights}
	var skipped []Skip
	var vector []float32
	hash := SectionHash(issue)

	// An unchanged bead reuses the embed and Jev minutes its earlier estimate
	// recorded, so the embedder and the Jev battery are not asked again.
	if prior, ok := reusablePrior(deps, issue, hash); ok {
		in.EmbedMinutes, in.JevMinutes = &prior.Embed, &prior.Jev
		componentMinutes.Embed = &prior.Embed
	} else {
		embedMinutes, v, skip := embedInput(ctx, deps, weights, issue)
		if skip != nil {
			skipped = append(skipped, *skip)
		} else {
			vector = v
			in.EmbedMinutes = &embedMinutes
			componentMinutes.Embed = &embedMinutes
		}

		jevFits, skip := jevInput(ctx, deps, issue)
		if skip != nil {
			skipped = append(skipped, *skip)
		} else {
			in.JevFits = &jevFits
		}
	}

	blended, err := blend(in)
	if err != nil {
		return Result{}, fmt.Errorf("storypoints/estimate: %w", err)
	}
	// The log records the minutes the blend mapped the probability to, so the
	// line cannot disagree with the number the average used.
	componentMinutes.Jev = blended.JevMinutes

	// The size input caps the blend: a bead that lists the work it asks for in a
	// fenced list is no larger than that list says, whatever the three averaged
	// inputs read from its prose. It lowers an estimate and never raises one,
	// and the note's inputs line names it when it did (sizeMinutes).
	if size, ok := sizeMinutes(issue); ok && size < blended.Minutes {
		points, perr := storypoints.FromMinutes(size)
		if perr != nil {
			return Result{}, fmt.Errorf("storypoints/estimate: size input: %w", perr)
		}
		blended.Minutes, blended.Points = size, points
		blended.Inputs = append(append([]string(nil), blended.Inputs...), "size")
	}

	neighbors, neighborsSkipped := neighborsOf(ctx, deps.HMRunner, issue)

	// A dependency that saw the context end returns an error, which the input
	// builders above record as a skip. Reporting that as a lower-rung result
	// would hand the caller an estimate from a run it abandoned, so a canceled
	// context refuses the result.
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("storypoints/estimate: %w", err)
	}

	return Result{
		Points:           blended.Points,
		Minutes:          blended.Minutes,
		Shape:            shape,
		Embedding:        vector,
		ComponentMinutes: componentMinutes,
		Inputs:           blended.Inputs,
		Skipped:          skipped,
		Rung:             blended.Rung,
		Neighbors:        neighbors,
		NeighborsSkipped: neighborsSkipped,
		Hash:             hash,
		WeightsVersion:   weights.Version(),
	}, nil
}

// weightsFor picks the model the estimate runs on: the caller's override, which
// reads no runtime file, else the caller's loader, else the process-wide loader
// LoadTownWeights shares.
func weightsFor(deps Deps) storypoints.Weights {
	switch {
	case deps.Weights != nil:
		return *deps.Weights
	case deps.WeightsLoader != nil:
		return deps.WeightsLoader.Load(deps.TownRoot)
	default:
		return storypoints.LoadTownWeights(deps.TownRoot)
	}
}

// Note renders the comment that carries r. The first line identifies the
// estimate, its rung and the hash of the bead text it was made from; the rest
// gives the minutes, the weights the estimate used, the inputs used with each
// component's minutes, each skipped input with its reason, and the nearest past
// beads, or why they are missing. The weights line is written when r names a
// weights version.
func Note(r Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SP ESTIMATE v1 sp:%d rung:%s hash:%s\n", r.Points, r.Rung, r.Hash)
	fmt.Fprintf(&b, "minutes: %s\n", strconv.FormatFloat(r.Minutes, 'f', 1, 64))
	if r.WeightsVersion != "" {
		fmt.Fprintf(&b, "weights: %s\n", r.WeightsVersion)
	}
	fmt.Fprintf(&b, "inputs: %s\n", inputsLine(r))
	for _, s := range r.Skipped {
		fmt.Fprintf(&b, "skipped %s: %s\n", s.Input, oneLine(s.Reason))
	}
	b.WriteString(NeighborsLine(r) + "\n")
	return strings.TrimSuffix(b.String(), "\n")
}

// inputsLine renders the inputs the blend averaged, each component followed by
// the minutes it contributed: "shape 11.0, embed 7.5, jev 5.5". An input that
// has no minutes of its own, as size caps the blend without one, is named alone;
// so is every input of a result built without component minutes.
func inputsLine(r Result) string {
	parts := make([]string, 0, len(r.Inputs))
	for _, input := range r.Inputs {
		m := minutesForInput(r.ComponentMinutes, input)
		if m == nil {
			parts = append(parts, input)
			continue
		}
		parts = append(parts, input+" "+strconv.FormatFloat(*m, 'f', 1, 64))
	}
	return strings.Join(parts, ", ")
}

// minutesForInput returns the minutes the named blend input contributed, or nil
// for an input that has none.
func minutesForInput(c ComponentMinutes, input string) *float64 {
	switch input {
	case "shape":
		return c.Shape
	case "embed":
		return c.Embed
	case "jev":
		return c.Jev
	default:
		return nil
	}
}

// NeighborsLine is Note's nearest-beads sentence: the past beads hm ranked
// closest, or why there are none. The split route quotes it when it holds a
// bead for splitting, so a person sees the comparison without the whole note
// (gt-5d9k2f.18).
func NeighborsLine(r Result) string {
	switch {
	case r.NeighborsSkipped != "":
		return "nearest beads unavailable: " + r.NeighborsSkipped
	case len(r.Neighbors) == 0:
		return "nearest beads: none"
	default:
		return "nearest beads: " + neighborList(r.Neighbors)
	}
}

// sectionKeys are the sections SectionHash covers, in the order it writes them.
var sectionKeys = []string{"goal", "constraints", "acceptance", "size"}

// SectionHash is a stable hash of the bead text an estimate is made from: the
// Goal, Constraints, Acceptance and Size sections of the description, then the
// bead's Acceptance field, which EstimateText carries too. The title, labels,
// notes and comments are outside it. A command rerunning an unchanged bead
// compares this with the hash in the last note (gt-5d9k2f.13).
func SectionHash(issue *beads.Issue) string {
	if issue == nil {
		return ""
	}
	sections := specdispatch.Sections(issue.Description)
	h := sha256.New()
	for _, key := range sectionKeys {
		writeHashField(h, key, sections[key])
	}
	writeHashField(h, "acceptance-field", issue.AcceptanceCriteria)
	return hex.EncodeToString(h.Sum(nil))
}

// writeHashField writes one length-prefixed field, so a section body can never
// be read as another field's name or value.
func writeHashField(h hash.Hash, name, value string) {
	value = strings.TrimSpace(value)
	fmt.Fprintf(h, "%d:%s%d:%s", len(name), name, len(value), value)
}

// reusablePrior returns the component minutes an earlier estimate recorded for
// this bead text, when deps.Prior answers ok with both values valid minutes. A
// nil Prior, a miss, or a single valid value estimates as usual.
func reusablePrior(deps Deps, issue *beads.Issue, hash string) (PriorMinutes, bool) {
	if deps.Prior == nil {
		return PriorMinutes{}, false
	}
	prior, ok := deps.Prior(issue.ID, hash)
	if !ok || !usableMinutes(prior.Embed) || !usableMinutes(prior.Jev) {
		return PriorMinutes{}, false
	}
	return prior, true
}

// usableMinutes reports whether m is a finite, positive number of minutes, the
// same test storypoints.Blend applies to each input.
func usableMinutes(m float64) bool {
	return m > 0 && !math.IsInf(m, 1)
}

// embedInput builds the embedding minutes and the vector the ridge read, or the
// skip that says why not. A nil embedder, a bead with no section text, an
// embedder failure and a ridge that refuses the vector are all skips: the blend
// drops the input.
func embedInput(ctx context.Context, deps Deps, w storypoints.Weights, issue *beads.Issue) (float64, []float32, *Skip) {
	if deps.Embedder == nil {
		return 0, nil, &Skip{Input: "embed", Reason: "no embedder"}
	}
	text, err := storypoints.SectionText(issue)
	if err != nil {
		return 0, nil, &Skip{Input: "embed", Reason: err.Error()}
	}
	vec, err := deps.Embedder.Embed(ctx, text)
	if err != nil {
		return 0, nil, &Skip{Input: "embed", Reason: err.Error()}
	}
	minutes, err := w.EmbeddingMinutes(vec)
	if err != nil {
		return 0, nil, &Skip{Input: "embed", Reason: err.Error()}
	}
	return minutes, vec, nil
}

// jevInput asks the battery for the fits_twenty_minutes probability, or returns
// the skip that says why not. A probability outside 0 to 1 is a failed input,
// not a failed estimate: the blend drops it as it drops an asker error.
func jevInput(ctx context.Context, deps Deps, issue *beads.Issue) (float64, *Skip) {
	if deps.JevAsk == nil {
		return 0, &Skip{Input: "jev", Reason: "no Jev asker"}
	}
	ans, err := deps.JevAsk(ctx, issue)
	if err != nil {
		return 0, &Skip{Input: "jev", Reason: err.Error()}
	}
	p := ans.FitsTwentyMinutes
	if !(p >= 0 && p <= 1) {
		return 0, &Skip{Input: "jev", Reason: fmt.Sprintf("fits_twenty_minutes %v is outside 0 to 1", p)}
	}
	return p, nil
}

// neighborsOf asks hm for the past beads nearest the issue. A nil runner or an
// hm failure returns no neighbors and the reason, and the estimate proceeds
// without them.
func neighborsOf(ctx context.Context, run hmhints.Runner, issue *beads.Issue) ([]nearest.Neighbor, string) {
	if run == nil {
		return nil, "no hm runner"
	}
	found, err := nearest.Nearest(ctx, run, issue, neighborLimit)
	if err != nil {
		return nil, err.Error()
	}
	return found, ""
}

// neighborList renders neighbors as "id (rig) title" entries on one line.
func neighborList(found []nearest.Neighbor) string {
	parts := make([]string, 0, len(found))
	for _, n := range found {
		parts = append(parts, fmt.Sprintf("%s (%s) %s", n.ID, n.Rig, oneLine(n.Title)))
	}
	return strings.Join(parts, "; ")
}

// oneLine collapses whitespace in a dependency's text, so a title or an error
// from outside cannot add a line to the note.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// beadTextEmpty reports a bead with no title, description or acceptance field:
// EstimateText would hold only the layout scaffolding, which carries no shape
// features to estimate from.
func beadTextEmpty(issue *beads.Issue) bool {
	return strings.TrimSpace(issue.Title) == "" &&
		strings.TrimSpace(issue.Description) == "" &&
		strings.TrimSpace(issue.AcceptanceCriteria) == ""
}
