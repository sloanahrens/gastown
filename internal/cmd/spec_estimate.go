package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hmhints"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/storypoints"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt spec estimate (gt-5d9k2f.13) writes one bead's story-point estimate: the
// labels sp:N, sp-rung:<rung> and sp-minutes:<n>, and the estimate's note as a
// comment. A bead whose sections match the hash in its latest note is left
// alone unless --force. An estimate that cannot be made writes sp-rung:none and
// a comment saying why, and exits 0, so a bead the shape cannot judge is never
// read as a small one.

// specEstimateNotePrefix begins every comment the estimate writes, and the
// comment the command reads its hash from.
const specEstimateNotePrefix = "SP ESTIMATE v1"

const (
	// specEstimateTimeout bounds one estimate, Jev and the embedder included.
	specEstimateTimeout = 2 * time.Minute
	// specEstimateEmbedTimeout bounds one embedder request.
	specEstimateEmbedTimeout = 30 * time.Second
)

var (
	specEstimateForce  bool
	specEstimateDryRun bool
	specEstimateJSON   bool
)

var specEstimateCmd = &cobra.Command{
	Use:   "estimate <bead-id>",
	Short: "Write a bead's story-point estimate (sp:N labels and a note)",
	Long: `Estimate one work bead before dispatch and write the result to it.

The estimate blends the shape of the bead's text, an embedding of its
Constraints and Acceptance (when patrols.spec_dispatch.story_points names an
embedder), and Jev's fits_twenty_minutes answer (when the battery in
patrols.jev_dispatch opens). An input that cannot be built is left out and
named in the note, and the rung label says which inputs were used.

On success the bead gets sp:N, sp-rung:<rung> and sp-minutes:<n> (the blended
minutes, one decimal), replacing any earlier sp:, sp-rung: and sp-minutes:
labels, and the note as a comment. The note's first line is
"SP ESTIMATE v1 sp:N rung:<rung> hash:<hash>"; the hash covers the bead's
Goal, Constraints, Acceptance and Size sections, so a bead rerun without a
change is skipped.

When no estimate can be made, the bead gets sp-rung:none, loses its old
sp:, sp-rung: and sp-minutes: labels, and a comment says why. The command
exits 0.

--dry-run prints the labels and the note it would write and writes nothing.
--force estimates again even when the hash matches.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runSpecEstimate,
}

func init() {
	specEstimateCmd.Flags().BoolVar(&specEstimateForce, "force", false, "Estimate again even when the bead's sections match the hash in its latest estimate note")
	specEstimateCmd.Flags().BoolVar(&specEstimateDryRun, "dry-run", false, "Print the labels and the note that would be written; write nothing")
	specEstimateCmd.Flags().BoolVar(&specEstimateJSON, "json", false, "Print the outcome as JSON")
	specCmd.AddCommand(specEstimateCmd)
}

// specEstimateStore is the bead access the command needs. Every write goes
// through the bd wrapper beads.Client, as the other spec commands do.
type specEstimateStore interface {
	Show(id string) (*beads.Issue, error)
	Comments(id string) ([]beads.Comment, error)
	Update(id string, opts beads.UpdateOptions) error
	AddComment(id, text string) error
}

// specEstimateRunner is what a run needs from outside the bead. Production
// wires estimate.Estimate and specEstimateDeps; a test wires stubs, so no hm,
// Jev or embedder is reached from a unit test.
type specEstimateRunner struct {
	Estimate func(ctx context.Context, deps estimate.Deps, issue *beads.Issue) (estimate.Result, error)
	Deps     func() (estimate.Deps, error)
}

// specEstimateOpts are the command's flags.
type specEstimateOpts struct {
	Force  bool
	DryRun bool
	JSON   bool
}

// specEstimateOutcome is what one run did, or would do, to one bead.
type specEstimateOutcome struct {
	ID string `json:"id"`
	// Action is estimated, unchanged (the sections match the hash in the latest
	// note, so nothing was estimated) or none (no usable estimate).
	Action string `json:"action"`
	// DryRun is set when nothing was written and the changes are only shown.
	DryRun  bool    `json:"dry_run"`
	Points  int     `json:"sp,omitempty"`
	Rung    string  `json:"rung,omitempty"`
	Minutes float64 `json:"minutes,omitempty"`
	Hash    string  `json:"hash"`
	// Reason says why there is no estimate; empty otherwise.
	Reason       string   `json:"reason,omitempty"`
	AddLabels    []string `json:"add_labels,omitempty"`
	RemoveLabels []string `json:"remove_labels,omitempty"`
	Comment      string   `json:"comment,omitempty"`
	// Neighbors is the nearest-beads line of the estimate note the run wrote,
	// or the one it found current. The split route quotes it. It is not part of
	// --json: Comment is what this run wrote.
	Neighbors string `json:"-"`
}

func runSpecEstimate(cmd *cobra.Command, args []string) error {
	return runSpecEstimateFrom(cmd, args, workspace.FindFromCwdOrError)
}

// runSpecEstimateFrom is runSpecEstimate with the town-root lookup given, so a
// test drives its refusal without depending on the working directory.
func runSpecEstimateFrom(cmd *cobra.Command, args []string, findTown func() (string, error)) error {
	return runSpecEstimateWired(cmd, args, findTown,
		func(townRoot, beadID string) specEstimateStore {
			return beads.New(resolveBeadDirFromTownRoot(townRoot, beadID))
		},
		estimate.Estimate)
}

// runSpecEstimateWired is the command's production wiring with the store and
// the estimator given: it finds the town, builds the inputs from the town's
// settings and the bead's rig, and runs the estimate. A test drives the whole
// wiring through a fake store and a stub estimator.
func runSpecEstimateWired(cmd *cobra.Command, args []string, findTown func() (string, error),
	newStore func(townRoot, beadID string) specEstimateStore,
	estimateFn func(ctx context.Context, deps estimate.Deps, issue *beads.Issue) (estimate.Result, error)) error {
	townRoot, err := findTown()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	beadID := args[0]
	run := specEstimateRunner{
		Estimate: estimateFn,
		Deps: func() (estimate.Deps, error) {
			return specEstimateDeps(townRoot, specBeadRig(townRoot, beadID), cmd.ErrOrStderr())
		},
	}
	opts := specEstimateOpts{Force: specEstimateForce, DryRun: specEstimateDryRun, JSON: specEstimateJSON}
	return runSpecEstimateOn(cmd.OutOrStdout(), beadID, newStore(townRoot, beadID), opts, run)
}

// runSpecEstimateOn is runSpecEstimate with the store and the runner given, so
// a test drives every branch through fakes.
func runSpecEstimateOn(out io.Writer, beadID string, store specEstimateStore, opts specEstimateOpts, run specEstimateRunner) error {
	outcome, err := specEstimateBead(context.Background(), beadID, store, opts, run)
	if err != nil {
		return err
	}
	return writeSpecEstimate(out, outcome, opts.JSON)
}

// specEstimateBead makes one bead's estimate and writes it, as runSpecEstimateOn
// does, under ctx. The estimate itself runs under specEstimateTimeout inside
// ctx, so a caller that gives up early (the dispatcher's shadow estimate)
// cancels it.
func specEstimateBead(ctx context.Context, beadID string, store specEstimateStore, opts specEstimateOpts, run specEstimateRunner) (specEstimateOutcome, error) {
	issue, err := store.Show(beadID)
	if err != nil {
		return specEstimateOutcome{}, fmt.Errorf("reading %s: %w", beadID, err)
	}
	comments, err := store.Comments(beadID)
	if err != nil {
		return specEstimateOutcome{}, fmt.Errorf("reading the comments on %s: %w", beadID, err)
	}
	hash := estimate.SectionHash(issue)
	if !opts.Force && latestEstimateHash(comments) == hash {
		note := latestEstimateNote(comments)
		outcome := specEstimateOutcome{ID: beadID, Action: "unchanged", Hash: hash, Neighbors: specEstimateNoteNeighbors(note)}
		specEstimateFromLabels(&outcome, issue.Labels)
		if outcome.Rung == "none" {
			outcome.Reason = specEstimateNoteReason(note)
		}
		return outcome, nil
	}

	deps, err := run.Deps()
	if err != nil {
		return specEstimateOutcome{}, fmt.Errorf("estimating %s: %w", beadID, err)
	}
	estCtx, cancel := context.WithTimeout(ctx, specEstimateTimeout)
	defer cancel()
	result, estErr := run.Estimate(estCtx, deps, issue)

	outcome := planSpecEstimate(beadID, hash, issue.Labels, result, estErr)
	outcome.DryRun = opts.DryRun
	if opts.DryRun {
		return outcome, nil
	}
	// The labels go first. A failed label write leaves the bead with no note
	// for this hash, so the next run estimates it again. A note written before
	// the labels would make the bead look estimated when its labels are not.
	if err := store.Update(beadID, beads.UpdateOptions{AddLabels: outcome.AddLabels, RemoveLabels: outcome.RemoveLabels}); err != nil {
		return specEstimateOutcome{}, fmt.Errorf("labeling %s: %w", beadID, err)
	}
	if err := store.AddComment(beadID, outcome.Comment); err != nil {
		return specEstimateOutcome{}, fmt.Errorf("commenting on %s: %w", beadID, err)
	}
	return outcome, nil
}

// planSpecEstimate decides what one run writes: the labels to add and remove,
// and the comment. An estimate error, or a result that no label can carry,
// becomes the none outcome.
func planSpecEstimate(beadID, hash string, existing []string, r estimate.Result, estErr error) specEstimateOutcome {
	if estErr == nil {
		estErr = specEstimateLabelable(r)
	}
	if estErr != nil {
		add := []string{storypoints.RungLabel("none")}
		return specEstimateOutcome{
			ID:           beadID,
			Action:       "none",
			Hash:         hash,
			Reason:       estErr.Error(),
			AddLabels:    add,
			RemoveLabels: staleEstimateLabels(existing, add),
			Comment:      specEstimateNoneNote(hash, estErr),
		}
	}
	minutes := specEstimateMinutes(r.Minutes)
	add := []string{storypoints.SPLabel(r.Points), storypoints.RungLabel(r.Rung), storypoints.MinutesLabel(minutes)}
	return specEstimateOutcome{
		ID:           beadID,
		Action:       "estimated",
		Hash:         hash,
		Points:       r.Points,
		Rung:         r.Rung,
		Minutes:      minutes,
		AddLabels:    add,
		RemoveLabels: staleEstimateLabels(existing, add),
		Comment:      estimate.Note(r),
		Neighbors:    estimate.NeighborsLine(r),
	}
}

// specEstimateLabelable refuses a result whose labels would not parse back:
// points off the scale, or minutes that round to zero, infinite or NaN.
func specEstimateLabelable(r estimate.Result) error {
	if !storypoints.Valid(r.Points) {
		return fmt.Errorf("the estimate's %d points are not on the scale", r.Points)
	}
	if m := specEstimateMinutes(r.Minutes); !(m > 0) || math.IsInf(m, 0) {
		return fmt.Errorf("the estimate's %v minutes do not round to a labelable value", r.Minutes)
	}
	return nil
}

// specEstimateMinutes rounds minutes to one decimal, the value sp-minutes
// carries. MinutesLabel writes the shortest form of that value, which is what
// ParseLabel reads back.
func specEstimateMinutes(m float64) float64 {
	return math.Round(m*10) / 10
}

// staleEstimateLabels lists the sp:, sp-rung: and sp-minutes: labels a bead
// carries that the run is about to replace. A label the run adds again is not
// removed. sp-actual: is the landing worker's and is never touched here.
func staleEstimateLabels(existing, add []string) []string {
	var stale []string
	for _, l := range existing {
		if !strings.HasPrefix(l, "sp:") && !strings.HasPrefix(l, "sp-rung:") && !strings.HasPrefix(l, "sp-minutes:") {
			continue
		}
		if slices.Contains(add, l) {
			continue
		}
		stale = append(stale, l)
	}
	return stale
}

// latestEstimateNote is the text of the latest comment that begins SP ESTIMATE
// v1, trimmed, or "" when there is none. Comments are oldest first.
func latestEstimateNote(comments []beads.Comment) string {
	for i := len(comments) - 1; i >= 0; i-- {
		text := strings.TrimSpace(comments[i].Text)
		if strings.HasPrefix(text, specEstimateNotePrefix) {
			return text
		}
	}
	return ""
}

// latestEstimateHash is the hash in the note of the latest comment that begins
// SP ESTIMATE v1, or "" when there is no such comment or it names no hash.
func latestEstimateHash(comments []beads.Comment) string {
	text := latestEstimateNote(comments)
	if text == "" {
		return ""
	}
	first, _, _ := strings.Cut(text, "\n")
	for _, field := range strings.Fields(first) {
		if h, ok := strings.CutPrefix(field, "hash:"); ok {
			return h
		}
	}
	return ""
}

// specEstimateFromLabels fills an outcome whose estimate already stood from the
// labels the command wrote on an earlier run. A value the bead's labels disagree
// on is left unset, so a caller reads the estimate as absent rather than
// guessing between two.
func specEstimateFromLabels(o *specEstimateOutcome, labels []string) {
	if points, ok := storypoints.EstimateOf(labels); ok {
		o.Points = points
	}
	rungs := map[string]bool{}
	minutes := map[float64]bool{}
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if rung, ok := strings.CutPrefix(l, "sp-rung:"); ok {
			rungs[rung] = true
		}
		if parsed, err := storypoints.ParseLabel(l); err == nil && parsed.Kind == storypoints.KindMinutes {
			minutes[parsed.Minutes] = true
		}
	}
	if len(rungs) == 1 {
		for rung := range rungs {
			o.Rung = rung
		}
	}
	if len(minutes) == 1 {
		for m := range minutes {
			o.Minutes = m
		}
	}
}

// specEstimateNoteReason is the reason line of a note the command wrote for an
// estimate it could not make, or "" when the note carries none.
func specEstimateNoteReason(note string) string {
	for _, line := range strings.Split(note, "\n") {
		if reason, ok := strings.CutPrefix(line, "no estimate: "); ok {
			return reason
		}
	}
	return ""
}

// specEstimateNoteNeighbors is the nearest-beads sentence of an estimate note,
// or "" when the note has none.
func specEstimateNoteNeighbors(note string) string {
	for _, line := range strings.Split(note, "\n") {
		if strings.HasPrefix(line, "nearest beads:") || strings.HasPrefix(line, "nearest beads unavailable:") {
			return line
		}
	}
	return ""
}

// specEstimateNoneNote is the comment for an estimate that could not be made.
// It carries the hash, so an unchanged bead is not estimated again, and it
// names the reason on one line.
func specEstimateNoneNote(hash string, cause error) string {
	reason := strings.Join(strings.Fields(cause.Error()), " ")
	return fmt.Sprintf("%s sp:none rung:none hash:%s\nno estimate: %s", specEstimateNotePrefix, hash, reason)
}

// specEstimateDeps reads the estimate's inputs from the town's settings. A
// settings block that does not parse refuses the run, as the dispatcher's tick
// does. A Jev battery that cannot be opened, and an embedder that is not
// configured, are not refusals: each becomes a skipped input, named in the
// note. An embedder URL set without a model is a configuration error and
// refuses the run (specEstimateEmbedder).
func specEstimateDeps(townRoot, rig string, errw io.Writer) (estimate.Deps, error) {
	sd, err := readSpecDispatchConfig(townRoot)
	if err != nil {
		return estimate.Deps{}, fmt.Errorf("reading patrols.spec_dispatch: %w", err)
	}
	// The weights are loaded once here and passed to the estimate, so the
	// embedder's reply check and the ridge that reads its vectors are the same
	// model; a second load in the estimate would let them disagree.
	weights := storypoints.LoadTownWeights(townRoot)
	embedder, err := specEstimateEmbedder(sd, weights)
	if err != nil {
		return estimate.Deps{}, err
	}
	return estimate.Deps{
		Weights:  &weights,
		TownRoot: townRoot,
		Embedder: embedder,
		JevAsk:   specEstimateJev(townRoot, errw),
		HMRunner: hmhints.ExecRunner,
		Rig:      rig,
	}, nil
}

// specEstimateEmbedder builds the embedder from story_points, or returns a nil
// embedder when none is configured. A URL with no model is refused, since the
// embedder would send a request that names no model. The embedder's model and
// its reply dimension are the loaded weights': a runtime file fitted on another
// embedding model, or one with no ridge, would mix sources inside one estimate,
// so each is refused with the reason.
func specEstimateEmbedder(sd *config.SpecDispatchConfig, w storypoints.Weights) (storypoints.Embedder, error) {
	if sd == nil || sd.StoryPoints == nil || sd.StoryPoints.EmbedderURL == "" {
		return nil, nil
	}
	sp := sd.StoryPoints
	if sp.EmbedderModel == "" {
		return nil, errors.New("patrols.spec_dispatch.story_points.embedder_url is set without embedder_model")
	}
	if w.EmbeddingDimension() == 0 {
		return nil, fmt.Errorf("patrols.spec_dispatch.story_points.embedder_url is set but the %s weights have no embedding ridge",
			weightsName(w))
	}
	if model := w.EmbeddingModel(); model != sp.EmbedderModel {
		return nil, fmt.Errorf("the %s weights name embedding model %q, but patrols.spec_dispatch.story_points.embedder_model is %q",
			weightsName(w), model, sp.EmbedderModel)
	}
	return storypoints.OllamaEmbedder{
		URL:     sp.EmbedderURL,
		Model:   sp.EmbedderModel,
		Dim:     w.EmbeddingDimension(),
		Timeout: specEstimateEmbedTimeout,
		HTTP:    &http.Client{},
	}, nil
}

// weightsName names a Weights value in an error: its version, or the zero
// value's lack of one.
func weightsName(w storypoints.Weights) string {
	if v := w.Version(); v != "" {
		return v
	}
	return "loaded"
}

// specEstimateJev is the Jev input. When the battery cannot be opened, the
// returned asker reports why on every call, so the note names the skip.
func specEstimateJev(townRoot string, errw io.Writer) estimate.JevAsker {
	pass, err := openSpecEstimateJev(townRoot, errw)
	if err != nil {
		return func(context.Context, *beads.Issue) (dispatch.Answers, error) {
			return dispatch.Answers{}, err
		}
	}
	return pass.Ask
}

// openSpecEstimateJev opens the battery from patrols.jev_dispatch, the settings
// the dispatch pass reads.
func openSpecEstimateJev(townRoot string, errw io.Writer) (*dispatch.Pass, error) {
	settings, err := jevDispatchSettings(townRoot)
	if err != nil {
		return nil, err
	}
	return dispatch.Open(townRoot, settings, errw)
}

// writeSpecEstimate prints the outcome: one line for a person, the whole record
// for --json.
func writeSpecEstimate(out io.Writer, o specEstimateOutcome, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(o)
	}
	switch o.Action {
	case "unchanged":
		fmt.Fprintf(out, "%s: unchanged since its last estimate; nothing written (--force estimates again)\n", o.ID)
	case "none":
		fmt.Fprintf(out, "%s: no estimate (%s); sp-rung:none\n", o.ID, o.Reason)
	default:
		fmt.Fprintf(out, "%s: sp:%d rung:%s minutes:%s\n", o.ID, o.Points, o.Rung, strconv.FormatFloat(o.Minutes, 'f', -1, 64))
	}
	if o.DryRun {
		fmt.Fprintf(out, "dry run: nothing written\n")
		fmt.Fprintf(out, "add: %s\n", strings.Join(o.AddLabels, ", "))
		if len(o.RemoveLabels) > 0 {
			fmt.Fprintf(out, "remove: %s\n", strings.Join(o.RemoveLabels, ", "))
		}
		fmt.Fprintf(out, "comment:\n%s\n", o.Comment)
	}
	return nil
}

// The estimate-ahead pass (gt-hzb051) gives an a priori estimate to beads the
// dispatcher is not about to sling. After the tick's sling decisions, with
// story_points.shadow on, it estimates at most specEstimateAheadLimit shaped
// candidates the tick left and writes the labels and note gt spec estimate
// writes. It is bounded by the shadow estimate's timeout, and an estimate that
// fails or times out is one logged line.

// specEstimateAheadPass estimates the beads specEstimateAheadPick chooses among
// the shaped candidates the tick did not sling, and returns the lines the tick
// logs. It does nothing with the setting off, and writes nothing in a dry run.
func specEstimateAheadPass(env specDispatchEnv, shaped []string, slung map[string]bool) []string {
	if !env.StoryPointShadow || env.DryRun {
		return nil
	}
	var ids []string
	for _, id := range shaped {
		if !slung[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if env.EstimateAheadRead == nil {
		// A pass wired to no reader must not look like a tick with nothing to
		// estimate: the estimate would silently stop for every bead.
		return []string{"story points estimate ahead: no bead reader is wired; nothing estimated"}
	}
	var cands []specEstimateAheadBead
	var lines []string
	for _, id := range ids {
		issue, comments, err := env.EstimateAheadRead(id)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s: story points estimate ahead: cannot read: %s", id, firstErrLine(err)))
			continue
		}
		cands = append(cands, specEstimateAheadBead{Issue: issue, Comments: comments, CommentsRead: true, Shaped: true})
	}
	for _, id := range specEstimateAheadPick(cands, specEstimateAheadLimit) {
		if line := specEstimateAheadLine(id, specStoryPointRun(env, id)); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// specEstimateAheadRead returns the read the estimate-ahead pass makes for one
// candidate: the bead from this tick's ledger, and its comments read fresh. The
// ledger may already hold the issue from the board read, and that value is the
// one the pass uses; the comments are a new call, so an estimate the sling hook
// wrote this tick is the bead's current one. A failed show or comments read is
// returned unchanged, and the pass logs it as one line rather than reading the
// bead as nothing to estimate.
func specEstimateAheadRead(reads *specTickReads, show func(id string) (*beads.Issue, error), comments func(id string) ([]beads.Comment, error)) func(id string) (*beads.Issue, []beads.Comment, error) {
	return func(id string) (*beads.Issue, []beads.Comment, error) {
		issue, err := reads.show(id, func() (*beads.Issue, error) { return show(id) })
		if err != nil {
			return nil, nil, err
		}
		reads.call()
		cs, err := comments(id)
		if err != nil {
			return nil, nil, err
		}
		return issue, cs, nil
	}
}

// specEstimateAheadLine is the one line a failed or timed-out ahead estimate
// logs, or "" when the estimate wrote its labels and note.
func specEstimateAheadLine(beadID string, answer specStoryPointAnswer) string {
	if answer.timedOut {
		return fmt.Sprintf("%s: story points estimate ahead: no estimate within %s", beadID, specStoryPointTimeout)
	}
	if err := specShadowErr(answer.outcome, answer.err); err != nil {
		return fmt.Sprintf("%s: story points estimate ahead: %s", beadID, firstErrLine(err))
	}
	return ""
}
