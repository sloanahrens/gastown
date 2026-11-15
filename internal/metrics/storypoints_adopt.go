package metrics

import (
	"bytes"
	"context"
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

// adoptStoryPoints decides the day's proposal and writes it when adopted. A
// rejection is a decision: it prints the rule's reason and returns nil, so the
// hour run carries on.
func (c Commands) adoptStoryPoints(ctx context.Context, real RealTown, env Env) error {
	store := c.storyPointsStore.withHost()
	dir, landings, err := storyPointsExtract(env, real.Extract.OutDir)
	if err != nil {
		return err
	}
	proposedPath, proposedData, err := storyPointsProposedFile(env, dir)
	if err != nil {
		return err
	}
	proposed, err := storypoints.LoadWeights(bytes.NewReader(proposedData))
	if err != nil {
		return fmt.Errorf("proposed weights %s: %w", proposedPath, err)
	}
	header, rows, err := ReadLandings(bytes.NewReader(landings))
	if err != nil {
		return err
	}
	if !slices.Contains(header, "polecat_work") {
		return fmt.Errorf("%s: no polecat_work column (run %s first): the table is from an older extract",
			filepath.Join(dir, landingsName), CommandExtract)
	}
	currentPath := storypoints.RuntimeWeightsPath(real.Town)
	if currentPath == "" {
		return errors.New("no town root, so no runtime weights path")
	}
	current, currentData, err := storyPointsWeightsInUse(env, c.StoryPointsBase, currentPath)
	if err != nil {
		return err
	}
	if err := storyPointsComparable(proposed, current); err != nil {
		return err
	}
	embedder := c.Embedder
	if embedder == nil {
		embedder = storyPointsEmbedder(proposed, os.Getenv)
	}
	samples, _, err := trainingSamples(ctx, env, real.Town, rows, proposed, embedder)
	if err != nil {
		return err
	}
	holdout, err := storyPointsHoldout(samples, proposed, current)
	if err != nil {
		return err
	}
	decision := storypoints.Adopt(holdout)
	if !decision.Adopted {
		return c.receipt("storypoints-adopt: %s", decision.Reason)
	}

	runtimeDir := filepath.Dir(currentPath)
	stem := strings.TrimSuffix(filepath.Base(currentPath), ".json")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", runtimeDir, err)
	}
	kept := ""
	if currentData != nil {
		if kept, err = storyPointsSaveVersion(store, runtimeDir, stem, currentData); err != nil {
			return err
		}
	}
	if err := store.Write(runtimeDir, filepath.Base(currentPath), func(w io.Writer) error {
		_, err := w.Write(proposedData)
		return err
	}); err != nil {
		return fmt.Errorf("write %s: %w", currentPath, err)
	}
	// Retention runs after the write: a write that fails must not cost an older
	// version. A trim that fails leaves the extra versions for the next adoption.
	if err := storyPointsTrimVersions(store, runtimeDir, stem, storyPointsVersionKeep); err != nil {
		return fmt.Errorf("wrote %s but could not trim old versions: %w", currentPath, err)
	}
	if kept != "" {
		return c.receipt("storypoints-adopt: %s; wrote %s and kept the weights it replaced as %s",
			decision.Reason, currentPath, kept)
	}
	return c.receipt("storypoints-adopt: %s; wrote %s", decision.Reason, currentPath)
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
			Actual:   s.Minutes,
			Proposed: proposedMinutes,
			Checked:  currentMinutes,
		})
	}
	return holdout, nil
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
	return c.receipt("storypoints-adopt: restored %s from %s", path, name)
}
