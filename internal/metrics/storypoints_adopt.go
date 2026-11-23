package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// The story-points adoption step (gt-y2xi5a). The retrain proposes weights; this
// step decides with storypoints.Adopt and, on adoption, writes the proposal as
// the town's runtime weights — the file the estimator reads — keeping the file it
// replaced as a numbered version beside it. The step never writes a proposal it
// did not adopt, and --revert puts the newest version back.

// storyPointsVersionKeep is how many numbered versions are kept beside the
// runtime weights, so a bad adoption can be undone.
const storyPointsVersionKeep = 8

// storyPointsAdoptReceipt is the prefix of every line this step prints. The
// weekly run reads the outcome it reports from the line it captures, so the two
// steps share the one constant rather than one spelling the other's line.
const storyPointsAdoptReceipt = "storypoints-adopt: "

// storyPointsAdoptHoldoutShare is the share of the samples, the latest in
// landing order, the adoption rule reads. It is storypoints.holdoutShare, which
// Retrain does not export; a change to the retrain's split must change this.
const storyPointsAdoptHoldoutShare = 0.2

// storyPointsStore is the runtime weights store the adoption step lists, writes
// and deletes beside the runtime file. A field left nil is the host filesystem.
// A test fills the one function it needs to refuse, so a write-side refusal is
// driven the same way whatever user the test runs as: a directory mode bit is
// no refusal to root.
type storyPointsStore struct {
	List   func(dir string) ([]DirEntry, error)
	Write  func(dir, name string, write func(io.Writer) error) error
	Remove func(path string) error
}

// withHost fills the fields the caller left nil with the host's.
func (s storyPointsStore) withHost() storyPointsStore {
	if s.List == nil {
		s.List = listDir
	}
	if s.Write == nil {
		s.Write = writeAtomic
	}
	if s.Remove == nil {
		s.Remove = os.Remove
	}
	return s
}

// runStoryPointsAdopt is main's storypoints-adopt step: adopt the day's proposed
// weights, or, with StoryPointsRevert, restore the newest version.
func (c Commands) runStoryPointsAdopt(ctx context.Context, real RealTown, env Env) error {
	if c.StoryPointsRevert {
		return c.revertStoryPoints(real, env)
	}
	return c.adoptStoryPoints(ctx, real, env)
}

// adoptStoryPoints decides the day's proposal by the rank rule and writes it when
// adopted. A rejection is a decision: it prints the rule's reason and returns nil,
// so the hour run carries on.
func (c Commands) adoptStoryPoints(ctx context.Context, real RealTown, env Env) error {
	return c.decideStoryPoints(ctx, real, env, storypoints.Adopt, false)
}

// decideStoryPoints reads the day's proposal and the weights in use, decides it by
// rule and writes it when rule adopts. firstRound says the holdout's first-round
// flags are read from the landing record, which refuses the decision when the
// table lacks those columns or a candidate row's cell is not an integer. Only the
// weekly run's auto-adoption asks for it, so the rank-rule path reads no first-round
// cell.
func (c Commands) decideStoryPoints(ctx context.Context, real RealTown, env Env, rule func([]storypoints.Holdout) storypoints.Adoption, firstRound bool) error {
	_, err := c.decideStoryPointsWrite(ctx, real, env, rule, firstRound)
	return err
}

// storyPointsWritten is what decideStoryPointsWrite wrote or refused: whether it adopted,
// the version it adopted, and the version file the weights it replaced were kept as, empty
// when there were none (the checked-in weights were in use); or, when the rule refused the
// proposal, that it refused and the rule's one-line reason.
type storyPointsWritten struct {
	adopted  bool
	version  string
	previous string
	refused  bool
	reason   string
}

// decideStoryPointsWrite is decideStoryPoints, and returns what it wrote or refused, so
// the weekly run can log the week's outcome.
func (c Commands) decideStoryPointsWrite(ctx context.Context, real RealTown, env Env, rule func([]storypoints.Holdout) storypoints.Adoption, firstRound bool) (storyPointsWritten, error) {
	store := c.storyPointsStore.withHost()
	dir, landings, err := storyPointsExtract(env, real.Extract.OutDir)
	if err != nil {
		return storyPointsWritten{}, err
	}
	proposedPath, proposedData, err := storyPointsProposedFile(env, dir)
	if err != nil {
		return storyPointsWritten{}, err
	}
	proposed, err := storypoints.LoadWeights(bytes.NewReader(proposedData))
	if err != nil {
		return storyPointsWritten{}, fmt.Errorf("proposed weights %s: %w", proposedPath, err)
	}
	header, rows, err := ReadLandings(bytes.NewReader(landings))
	if err != nil {
		return storyPointsWritten{}, err
	}
	if !slices.Contains(header, "polecat_work") {
		return storyPointsWritten{}, fmt.Errorf("%s: no polecat_work column (run %s first): the table is from an older extract",
			filepath.Join(dir, landingsName), CommandExtract)
	}
	if firstRound {
		if missing := missingFirstRoundColumns(header); len(missing) > 0 {
			return storyPointsWritten{}, fmt.Errorf("%s: no %s column (run %s first): the first-round holdout cannot be read, so the adoption is refused",
				filepath.Join(dir, landingsName), strings.Join(missing, " and "), CommandExtract)
		}
	}
	currentPath := storypoints.RuntimeWeightsPath(real.Town)
	if currentPath == "" {
		return storyPointsWritten{}, errors.New("no town root, so no runtime weights path")
	}
	current, currentData, err := storyPointsWeightsInUse(env, c.StoryPointsBase, currentPath)
	if err != nil {
		return storyPointsWritten{}, err
	}
	if err := storyPointsComparable(proposed, current); err != nil {
		return storyPointsWritten{}, err
	}
	embedder := c.Embedder
	if embedder == nil {
		embedder = storyPointsEmbedder(proposed, os.Getenv)
	}
	samples, _, err := trainingSamplesWith(ctx, env, real.Town, rows, proposed, embedder, firstRound)
	if err != nil {
		return storyPointsWritten{}, err
	}
	holdout, err := storyPointsHoldout(samples, proposed, current)
	if err != nil {
		return storyPointsWritten{}, err
	}
	decision := rule(holdout)
	if !decision.Adopted {
		return storyPointsWritten{refused: true, reason: decision.Reason},
			c.receipt("%s%s", storyPointsAdoptReceipt, decision.Reason)
	}

	runtimeDir := filepath.Dir(currentPath)
	stem := strings.TrimSuffix(filepath.Base(currentPath), ".json")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return storyPointsWritten{}, fmt.Errorf("create %s: %w", runtimeDir, err)
	}
	kept := ""
	if currentData != nil {
		if kept, err = storyPointsSaveVersion(store, runtimeDir, stem, currentData); err != nil {
			return storyPointsWritten{}, err
		}
	}
	if err := store.Write(runtimeDir, filepath.Base(currentPath), func(w io.Writer) error {
		_, err := w.Write(proposedData)
		return err
	}); err != nil {
		return storyPointsWritten{}, fmt.Errorf("write %s: %w", currentPath, err)
	}
	written := storyPointsWritten{adopted: true, version: proposed.Version(), previous: kept}
	// Retention runs after the write: a write that fails must not cost an older
	// version. A trim that fails leaves the extra versions for the next adoption.
	if err := storyPointsTrimVersions(store, runtimeDir, stem, storyPointsVersionKeep); err != nil {
		return written, fmt.Errorf("wrote %s but could not trim old versions: %w", currentPath, err)
	}
	if kept != "" {
		return written, c.receipt("%s%s; wrote %s and kept the weights it replaced as %s",
			storyPointsAdoptReceipt, decision.Reason, currentPath, kept)
	}
	return written, c.receipt("%s%s; wrote %s", storyPointsAdoptReceipt, decision.Reason, currentPath)
}

// storyPointsProposedFile reads the proposed weights the retrain left beside the
// day's tables. A proposal under the rejected suffix is read too, so the rule
// decides and prints its reason rather than the command calling a refused
// proposal missing. The retrain writes one name or the other, never both.
func storyPointsProposedFile(env Env, dir string) (string, []byte, error) {
	names := []string{storyPointsProposedName, storyPointsProposedName + storyPointsRejectedSuffix}
	var found string
	var data []byte
	for _, name := range names {
		path := filepath.Join(dir, name)
		b, err := env.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return "", nil, fmt.Errorf("read %s: %w", path, err)
		}
		if found != "" {
			return "", nil, fmt.Errorf("%s and %s both exist: the retrain writes one proposed file, so the day's proposal is ambiguous",
				found, path)
		}
		found, data = path, b
	}
	if found == "" {
		return "", nil, fmt.Errorf("no %s or %s under %s (run %s first)",
			names[0], names[1], dir, CommandStoryPointsTrain)
	}
	return found, data, nil
}

// storyPointsWeightsInUse reads the weights the estimator reads in town: the
// runtime file when it is there, else base (the checked-in weights). A runtime
// file that exists but does not load is an error rather than a silent comparison
// against the embedded weights: adoption would replace a corrupt file that the
// estimator is already ignoring, and nothing would say so. data is nil when no
// runtime file exists.
func storyPointsWeightsInUse(env Env, base *storypoints.Weights, path string) (storypoints.Weights, []byte, error) {
	data, err := env.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		w := storypoints.Default()
		if base != nil {
			w = *base
		}
		return w, nil, nil
	case err != nil:
		return storypoints.Weights{}, nil, fmt.Errorf("read %s: %w", path, err)
	}
	w, err := storypoints.LoadWeights(bytes.NewReader(data))
	if err != nil {
		return storypoints.Weights{}, nil, fmt.Errorf("runtime weights %s: %w", path, err)
	}
	return w, data, nil
}

// storyPointsComparable refuses two models the holdout blend cannot compare: a
// vector embedded for one model scored by the other is not a prediction.
func storyPointsComparable(proposed, current storypoints.Weights) error {
	if p, c := proposed.EmbeddingDimension(), current.EmbeddingDimension(); p != c {
		return fmt.Errorf("the proposed weights embed %d numbers, the weights in use embed %d, so the holdout blend cannot compare them", p, c)
	}
	if p, c := proposed.EmbeddingModel(), current.EmbeddingModel(); p != c {
		return fmt.Errorf("the proposed weights embed with %q, the weights in use embed with %q, so the holdout vectors are not comparable", p, c)
	}
	return nil
}

// storyPointsHoldout rebuilds the holdout the retrain scored: the latest fifth
// of the samples in landing order, both blends through storypoints.Blend. It is
// Retrain's split, repeated because the retrain does not write its holdout down.
func storyPointsHoldout(samples []storypoints.Sample, proposed, current storypoints.Weights) ([]storypoints.Holdout, error) {
	sorted := slices.Clone(samples)
	slices.SortStableFunc(sorted, func(a, b storypoints.Sample) int { return a.LandedAt.Compare(b.LandedAt) })
	cut := int(math.Round(float64(len(sorted)) * (1 - storyPointsAdoptHoldoutShare)))
	for cut < len(sorted) && sorted[cut].LandedAt.Equal(sorted[cut-1].LandedAt) {
		cut++
	}
	holdout := make([]storypoints.Holdout, 0, len(sorted)-cut)
	for i, s := range sorted[cut:] {
		proposedMinutes, err := storyPointsBlend(proposed, s)
		if err != nil {
			return nil, fmt.Errorf("proposed weights, holdout bead %d: %w", i, err)
		}
		currentMinutes, err := storyPointsBlend(current, s)
		if err != nil {
			return nil, fmt.Errorf("weights in use, holdout bead %d: %w", i, err)
		}
		holdout = append(holdout, storypoints.Holdout{
			Actual:     s.Minutes,
			Proposed:   proposedMinutes,
			Checked:    currentMinutes,
			FirstRound: s.FirstRound,
		})
	}
	return holdout, nil
}

// missingFirstRoundColumns names the landing columns the first-round flag reads
// that header does not carry, in omLandingColumns' order.
func missingFirstRoundColumns(header []string) []string {
	var missing []string
	for _, col := range omLandingColumns {
		if !slices.Contains(header, col) {
			missing = append(missing, col)
		}
	}
	return missing
}

// firstRoundCells reads one landing's first-round flag: the landing took one om
// round and one polecat attempt. A cell that is not an integer is an error rather
// than a landing that is not first round, so a table the holdout cannot be read
// from is refused and not read as all later rounds.
func firstRoundCells(row Row) (bool, error) {
	rounds, err := strconv.Atoi(row["om_rounds"])
	if err != nil {
		return false, fmt.Errorf("om_rounds %q is not an integer", row["om_rounds"])
	}
	attempts, err := strconv.Atoi(row["polecat_attempts"])
	if err != nil {
		return false, fmt.Errorf("polecat_attempts %q is not an integer", row["polecat_attempts"])
	}
	return rounds == 1 && attempts == 1, nil
}

// storyPointsBlend is one model's prediction for a sample: the shape and
// embedding blend Retrain scores, which is the value storypoints.Holdout
// carries.
func storyPointsBlend(w storypoints.Weights, s storypoints.Sample) (float64, error) {
	shape, err := w.ShapeMinutes(s.Shape)
	if err != nil {
		return 0, err
	}
	embed, err := w.EmbeddingMinutes(s.Vector)
	if err != nil {
		return 0, err
	}
	blended, err := storypoints.Blend(storypoints.Inputs{ShapeMinutes: &shape, EmbedMinutes: &embed, Weights: w})
	if err != nil {
		return 0, err
	}
	return blended.Minutes, nil
}

// storyPointsSaveVersion writes data as the next numbered version beside the
// runtime file and returns its name. The next number is the highest one there
// plus one. A --revert consumes the newest version, so the number it frees can
// be handed out again: the name is not a version's identity over the town's
// life, only the order the versions are restored in.
func storyPointsSaveVersion(store storyPointsStore, dir, stem string, data []byte) (string, error) {
	numbers, err := storyPointsVersions(store, dir, stem)
	if err != nil {
		return "", err
	}
	next := 1
	if len(numbers) > 0 {
		next = numbers[0] + 1
	}
	name := storyPointsVersionName(stem, next)
	if err := store.Write(dir, name, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}); err != nil {
		return "", fmt.Errorf("write %s: %w", filepath.Join(dir, name), err)
	}
	return name, nil
}

// storyPointsTrimVersions deletes the oldest numbered versions until keep
// remain. It runs after the runtime file is written, so a write that fails
// leaves every older version in place; a trim that fails leaves more than keep
// versions beside the adopted weights for the next adoption to trim.
func storyPointsTrimVersions(store storyPointsStore, dir, stem string, keep int) error {
	numbers, err := storyPointsVersions(store, dir, stem)
	if err != nil {
		return err
	}
	if len(numbers) <= keep {
		return nil
	}
	for _, n := range numbers[keep:] {
		path := filepath.Join(dir, storyPointsVersionName(stem, n))
		if err := store.Remove(path); err != nil {
			return fmt.Errorf("remove old version %s: %w", path, err)
		}
	}
	return nil
}

// storyPointsVersions lists the numbered versions beside the runtime file,
// highest first. A name that is not a version is left alone: it is neither a
// version to restore nor one for the retention count to delete.
func storyPointsVersions(store storyPointsStore, dir, stem string) ([]int, error) {
	entries, err := store.List(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	var numbers []int
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		if n, ok := storyPointsVersionNumber(stem, entry.Name); ok {
			numbers = append(numbers, n)
		}
	}
	slices.Sort(numbers)
	slices.Reverse(numbers)
	return numbers, nil
}

// storyPointsVersionName is the name a runtime weights file's previous contents
// are kept under: weights.v3.json beside weights.json.
func storyPointsVersionName(stem string, n int) string {
	return fmt.Sprintf("%s.v%d.json", stem, n)
}

// storyPointsVersionNumber reads the number out of a version file's name. The
// digits must be the canonical spelling storyPointsVersionName writes: a name
// like weights.v03.json is a file beside the versions rather than one of them,
// because restoring from it would go through weights.v3.json, which need not
// exist.
func storyPointsVersionNumber(stem, name string) (int, bool) {
	prefix, suffix := stem+".v", ".json"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if digits == "" {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || strconv.Itoa(n) != digits {
		return 0, false
	}
	return n, true
}

// revertStoryPoints is main's storypoints-adopt --revert: the newest version
// becomes the runtime file and is consumed, so the next --revert restores the
// one before it. A version that does not load is refused and left in place.
// With no version to restore there is nothing to undo, and the step fails rather
// than reporting the success of a change it did not make.
func (c Commands) revertStoryPoints(real RealTown, env Env) error {
	store := c.storyPointsStore.withHost()
	path := storypoints.RuntimeWeightsPath(real.Town)
	if path == "" {
		return errors.New("no town root, so no runtime weights path")
	}
	dir := filepath.Dir(path)
	stem := strings.TrimSuffix(filepath.Base(path), ".json")
	numbers, err := storyPointsVersions(store, dir, stem)
	if err != nil {
		return err
	}
	if len(numbers) == 0 {
		return fmt.Errorf("no earlier version to restore under %s; nothing changed", dir)
	}
	name := storyPointsVersionName(stem, numbers[0])
	versionPath := filepath.Join(dir, name)
	data, err := env.ReadFile(versionPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", versionPath, err)
	}
	if _, err := storypoints.LoadWeights(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("version %s: %w", versionPath, err)
	}
	if err := os.Rename(versionPath, path); err != nil {
		return fmt.Errorf("restore %s from %s: %w", path, versionPath, err)
	}
	return c.receipt("%srestored %s from %s", storyPointsAdoptReceipt, path, name)
}

// revertToCheckedInStoryPoints reverts a town's first adoption: it keeps the runtime weights as
// the next numbered version and removes the runtime file, so the estimator runs the checked-in
// weights again, as a town that adopted none does.
func (c Commands) revertToCheckedInStoryPoints(real RealTown, env Env) error {
	path := storypoints.RuntimeWeightsPath(real.Town)
	data, err := env.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	kept, err := storyPointsSaveVersion(c.storyPointsStore.withHost(), dir, strings.TrimSuffix(filepath.Base(path), ".json"), data)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return c.receipt("%srestored the checked-in weights; kept the adopted weights as %s", storyPointsAdoptReceipt, kept)
}

// storyPointsAdoptionsName is the log, beside the runtime weights, of each adoption
// the weekly run makes, for the week-after check of the next bead (gt-q2rrvb).
const storyPointsAdoptionsName = "adoptions.jsonl"

// storyPointsAdoption is one line of the adoptions log: the adoption's time, the
// version it was kept as, and Previous, the version file the weights it replaced
// were kept as (empty when the checked-in weights were in use).
type storyPointsAdoption struct {
	Event    string    `json:"event"`
	At       time.Time `json:"at"`
	Version  string    `json:"version,omitempty"`
	Previous string    `json:"previous,omitempty"`
	// Adopted, Outcome and Reason are a "checked" line's: the time of the adoption it
	// judged, "kept" or "reverted", and why.
	Adopted *time.Time `json:"adopted_at,omitempty"`
	Outcome string     `json:"outcome,omitempty"`
	Reason  string     `json:"reason,omitempty"`
}

// readStoryPointsAdoptions reads the adoptions log in order. No file is no adoption
// yet. A file that cannot be read, or a line that does not parse, is an error: the
// check does not read it as an empty log.
func readStoryPointsAdoptions(env Env, path string) ([]storyPointsAdoption, error) {
	data, err := env.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var log []storyPointsAdoption
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var a storyPointsAdoption
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		log = append(log, a)
	}
	return log, nil
}

// storyPointsAdoptionsPath is the adoptions log beside the runtime weights of town.
func storyPointsAdoptionsPath(town string) string {
	return filepath.Join(filepath.Dir(storypoints.RuntimeWeightsPath(town)), storyPointsAdoptionsName)
}

// appendStoryPointsAdoption appends a line to the adoptions log. The file is
// rewritten whole through the store, as the other writes here are, so a write that
// fails leaves the log as it was.
func (c Commands) appendStoryPointsAdoption(env Env, path string, a storyPointsAdoption) error {
	prior, err := env.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	line, err := json.Marshal(a)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.Write(prior)
	buf.Write(line)
	buf.WriteByte('\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return c.storyPointsStore.withHost().Write(dir, filepath.Base(path), func(w io.Writer) error {
		_, err := w.Write(buf.Bytes())
		return err
	})
}

// storyPointsCheckAge is how old an adoption is before the weekly run checks it: a
// week after it, the next weekly run.
const storyPointsCheckAge = 6 * 24 * time.Hour

// storyPointsReadWeights loads the weights file at path. A file that cannot be read
// or does not load is an error.
func storyPointsReadWeights(env Env, path string) (storypoints.Weights, error) {
	data, err := env.ReadFile(path)
	if err != nil {
		return storypoints.Weights{}, fmt.Errorf("read %s: %w", path, err)
	}
	w, err := storypoints.LoadWeights(bytes.NewReader(data))
	if err != nil {
		return storypoints.Weights{}, fmt.Errorf("weights %s: %w", path, err)
	}
	return w, nil
}

// firstAdoptionRevertedUnrecorded reports whether the missing runtime file of a first
// adoption is its revert's doing: the revert keeps the adopted weights as the next
// numbered version and removes the runtime file, so the newest version file must be
// there and its parsed version label must match the log's (the label is all that is
// compared: not the weights' bytes, which the log does not hold).
// Anything else is the missing file it is: false, and the caller's error stands. A
// version file that is there and does not load is an error naming it.
func (c Commands) firstAdoptionRevertedUnrecorded(env Env, runtimePath string, adopted *storyPointsAdoption) (bool, error) {
	dir := filepath.Dir(runtimePath)
	stem := strings.TrimSuffix(filepath.Base(runtimePath), ".json")
	numbers, err := storyPointsVersions(c.storyPointsStore.withHost(), dir, stem)
	if err != nil {
		return false, fmt.Errorf("%s is missing and the version files cannot be listed, so the revert it may have left cannot be recognized: %w", runtimePath, err)
	}
	if len(numbers) == 0 {
		return false, nil
	}
	kept, err := storyPointsReadWeights(env, filepath.Join(dir, storyPointsVersionName(stem, numbers[0])))
	if err != nil {
		return false, fmt.Errorf("%s is missing and the newest version file does not load, so the revert it may have left cannot be recognized: %w", runtimePath, err)
	}
	return kept.Version() == adopted.Version, nil
}

// recordRevertFound records the check of an adoption whose revert happened and whose
// check line did not get written, as reverted, and says so: the next weekly run then
// finds the adoption checked and goes on to the retrain (gt-skeyaw).
func (c Commands) recordRevertFound(env Env, logPath string, adopted *storyPointsAdoption, now time.Time, inferred string) error {
	at := adopted.At
	check := storyPointsAdoption{Event: "checked", At: now, Adopted: &at, Outcome: "reverted",
		Reason: "recorded after the revert: the revert had happened and its check line had not been written (" + inferred + ")"}
	if err := c.appendStoryPointsAdoption(env, logPath, check); err != nil {
		return fmt.Errorf("the adoption of %s was found reverted, but the check was not recorded: %w", adopted.Version, err)
	}
	return c.receipt("story points weekly: the adoption of %s was found reverted, its check line never written: recorded as reverted", adopted.Version)
}

// checkStoryPointsAdoption is the week-after check of the newest adoption. Once that
// adoption is storyPointsCheckAge old and has no check, it judges the weights it
// adopted against the ones they replaced on the landings since
// (judgeStoryPointsAdoption), reverts the adoption as --revert does when they did
// worse, and records the check as kept or reverted. A holdout too small to rank
// waits: the receipt says so and nothing is recorded, so a later week checks again.
func (c Commands) checkStoryPointsAdoption(ctx context.Context, real RealTown, env Env, embedder storypoints.Embedder, now time.Time) error {
	logPath := storyPointsAdoptionsPath(real.Town)
	log, err := readStoryPointsAdoptions(env, logPath)
	if err != nil {
		return err
	}
	var adopted *storyPointsAdoption
	for i := len(log) - 1; i >= 0 && adopted == nil; i-- {
		if log[i].Event == "adopted" {
			adopted = &log[i]
		}
	}
	if adopted == nil || now.Sub(adopted.At) < storyPointsCheckAge {
		return nil
	}
	for _, a := range log {
		if a.Event == "checked" && a.Adopted != nil && a.Adopted.Equal(adopted.At) {
			return nil
		}
	}
	dir, landings, err := storyPointsExtract(env, real.Extract.OutDir)
	if err != nil {
		return err
	}
	header, rows, err := ReadLandings(bytes.NewReader(landings))
	if err != nil {
		return err
	}
	var missing []string
	if !slices.Contains(header, "polecat_work") {
		missing = append(missing, "polecat_work")
	}
	missing = append(missing, missingFirstRoundColumns(header)...)
	if len(missing) > 0 {
		return fmt.Errorf("%s: no %s column (run %s first): the adoption cannot be checked on its holdout",
			filepath.Join(dir, landingsName), strings.Join(missing, " and "), CommandExtract)
	}
	runtimePath := storypoints.RuntimeWeightsPath(real.Town)
	adoptedWeights, err := storyPointsReadWeights(env, runtimePath)
	if err != nil {
		// A first adoption's revert removes the runtime file: when that is the file
		// missing and the revert's own version file is there, the revert happened
		// and its check line was not written (gt-skeyaw).
		if adopted.Previous == "" && errors.Is(err, fs.ErrNotExist) {
			if reverted, rerr := c.firstAdoptionRevertedUnrecorded(env, runtimePath, adopted); rerr != nil {
				return rerr
			} else if reverted {
				return c.recordRevertFound(env, logPath, adopted, now, "inferred: the runtime weights file is missing and the newest version file carries the adopted version")
			}
		}
		return err
	}
	previous, previousName := storypoints.Default(), "the checked-in weights"
	if c.StoryPointsBase != nil {
		previous = *c.StoryPointsBase
	}
	if adopted.Previous != "" {
		if p := adopted.Previous; filepath.Base(p) != p || p == "." || p == ".." {
			return fmt.Errorf("%s: the previous version %q is not a bare file name, so it is not read", logPath, p)
		}
		previousName = adopted.Previous
		if previous, err = storyPointsReadWeights(env, filepath.Join(filepath.Dir(runtimePath), adopted.Previous)); err != nil {
			// A later adoption's revert renames the version file it replaced over the
			// runtime file, so the version file the log names is gone and the runtime
			// file, which loaded, holds the restored weights (gt-skeyaw).
			if errors.Is(err, fs.ErrNotExist) {
				return c.recordRevertFound(env, logPath, adopted, now, "inferred: the version file "+adopted.Previous+" is missing and the runtime weights file loads")
			}
			return err
		}
	}
	samples, _, err := trainingSamplesWith(ctx, env, real.Town, rows, adoptedWeights, embedder, true)
	if err != nil {
		return err
	}
	verdict, err := judgeStoryPointsAdoption(samples, adoptedWeights, previous, adopted.At)
	if err != nil {
		return fmt.Errorf("judging the adoption of %s against %s: %w", adopted.Version, previousName, err)
	}
	if verdict.Waits {
		return c.receipt("story points weekly: the adoption of %s waits: %s, so it is not checked yet", adopted.Version, verdict.Why)
	}
	outcome := "kept"
	if verdict.Revert {
		outcome = "reverted"
		quiet := c
		quiet.Out = io.Discard
		// A first adoption replaced the checked-in weights, which are no file to restore: the
		// way back is the town with no runtime weights, so the adopted file is kept as a
		// version and removed.
		revert := quiet.revertStoryPoints
		if adopted.Previous == "" {
			revert = quiet.revertToCheckedInStoryPoints
		}
		if err := revert(real, env); err != nil {
			return fmt.Errorf("the adoption of %s is %s and could not be reverted: %w", adopted.Version, verdict.Why, err)
		}
	}
	at := adopted.At
	check := storyPointsAdoption{Event: "checked", At: now, Adopted: &at, Outcome: outcome, Reason: verdict.Why}
	if err := c.appendStoryPointsAdoption(env, logPath, check); err != nil {
		return fmt.Errorf("the adoption of %s was checked and %s, but the check was not recorded: %w", adopted.Version, outcome, err)
	}
	return c.receipt("story points weekly: checked the adoption of %s: %s: %s", adopted.Version, outcome, verdict.Why)
}
