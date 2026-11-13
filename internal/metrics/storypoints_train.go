package metrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/storypoints"
)

// The story-points retrain step (gt-5d9k2f SP16). It reads the landings table the
// extract wrote, fits the shape and embedding ridges on the beads that landed with
// a measured time, and writes the proposed weights and a report beside the day's
// tables. It never writes the checked-in weights: a person adopts a proposed file
// by committing it.

// storyPointsProposedName is the proposed weights file. A candidate that does not
// clear storyPointsHoldoutBar is written under the same name with
// storyPointsRejectedSuffix.
const storyPointsProposedName = "storypoints-weights.proposed.json"

// storyPointsRejectedSuffix marks a proposed file whose holdout does not clear the
// bar, or whose holdout correlation cannot be measured.
const storyPointsRejectedSuffix = ".rejected"

// storyPointsTrainReportName is the comparison report beside the proposed file.
const storyPointsTrainReportName = "storypoints-train.txt"

// storyPointsHoldoutBar is the rank correlation a proposed model must reach on its
// own holdout to be adopted: the design's bar.
const storyPointsHoldoutBar = 0.4

// storyPointsEmbedURLEnv names the ollama server the embedder reads. Its default
// is the local server.
const storyPointsEmbedURLEnv = "STORYPOINTS_EMBED_URL"

const storyPointsEmbedDefaultURL = "http://127.0.0.1:11434"

// storyPointsEmbedTimeout bounds one embedding request. A request past it fails
// the retrain rather than being read as a missing vector.
const storyPointsEmbedTimeout = 60 * time.Second

// trainSkips counts the landed rows the retrain did not read, by reason. A row
// skipped for a reason the report names is never read as a fitted bead.
type trainSkips struct {
	seat       int // seat class is neither haiku nor flash
	unmeasured int // no polecat time, or a lower bound
	noBead     int // the bead is not in its rig's store
	noText     int // the bead has neither a Constraints nor an Acceptance section
	badRig     int // the rig names no directory under the town
}

// trainCandidate is one landed row that passed the row filters, waiting for its
// bead's text.
type trainCandidate struct {
	bead    string
	rig     string
	seat    storypoints.Seat
	minutes float64
	landed  time.Time
}

// runStoryPointsTrain is main's storypoints-train step: the proposed weights and
// the report written into the extract's output directory, and the report printed to
// Out. It refuses a run with no landings table, and a table with too few measured
// beads to fit.
func (c Commands) runStoryPointsTrain(ctx context.Context, real RealTown, env Env) error {
	path := filepath.Join(real.Extract.OutDir, landingsName)
	data, err := env.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s (run %s first): %w", path, CommandExtract, err)
	}
	_, rows, err := ReadLandings(bytes.NewReader(data))
	if err != nil {
		return err
	}
	base := storypoints.Default()
	if c.StoryPointsBase != nil {
		base = *c.StoryPointsBase
	}
	embedder := c.Embedder
	if embedder == nil {
		embedder = storyPointsEmbedder(base)
	}
	samples, skips, err := trainingSamples(ctx, env, real.Town, rows, embedder)
	if err != nil {
		return err
	}
	model, err := storypoints.Retrain(base, samples)
	if err != nil {
		return err
	}

	score := holdoutRanks(model.Holdout)
	name := storyPointsProposedName
	if score.proposedErr != nil || score.proposed < storyPointsHoldoutBar {
		name += storyPointsRejectedSuffix
	}
	if err := writeAtomic(real.Extract.OutDir, name, model.Encode); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	report := storyPointsTrainReport(model, skips, name, score)
	if err := writeText(real.Extract.OutDir, storyPointsTrainReportName, strings.TrimSuffix(report, "\n")); err != nil {
		return err
	}
	_, err = io.WriteString(c.Out, report)
	return err
}

// trainingSamples reads the rows the retrain fits. A row is read when its seat
// class is haiku or flash, its polecat time is measured (not a lower bound and not
// absent), its rig names a directory, and its bead is in the store with a
// Constraints or Acceptance section. The others are counted in skips. An embedder
// error fails the whole step: a bead the embedder could not read is not a bead the
// fit may leave out without saying so.
func trainingSamples(ctx context.Context, env Env, town string, rows []Row, embedder storypoints.Embedder) ([]storypoints.Sample, trainSkips, error) {
	var skips trainSkips
	var cands []trainCandidate
	byDir := map[string][]string{}
	for _, row := range rows {
		seat, ok := trainSeat(row["seat_class"])
		if !ok {
			skips.seat++
			continue
		}
		secs, err := strconv.ParseFloat(row["polecat_secs"], 64)
		if row["polecat_lower_bound"] != "0" || err != nil || !(secs > 0) {
			skips.unmeasured++
			continue
		}
		landed, err := ParseTS(row["landed_at"])
		if err != nil {
			return nil, skips, fmt.Errorf("storypoints-train: bead %s: %w", row["bead"], err)
		}
		dir, ok := reviewDir(town, row["rig"])
		if !ok {
			skips.badRig++
			continue
		}
		cands = append(cands, trainCandidate{bead: row["bead"], rig: row["rig"], seat: seat, minutes: secs / 60, landed: landed})
		byDir[dir] = append(byDir[dir], row["bead"])
	}

	issues := map[string]*beads.Issue{}
	for dir, ids := range byDir {
		found, err := showBeads(ctx, env, dir, ids)
		if err != nil {
			return nil, skips, fmt.Errorf("storypoints-train: read beads in %s: %w", dir, err)
		}
		for id, issue := range found {
			issues[id] = issue
		}
	}

	var samples []storypoints.Sample
	for _, c := range cands {
		issue := issues[c.bead]
		if issue == nil {
			skips.noBead++
			continue
		}
		text, err := storypoints.SectionText(issue)
		if err != nil {
			skips.noText++
			continue
		}
		vec, err := embedder.Embed(ctx, text)
		if err != nil {
			return nil, skips, fmt.Errorf("storypoints-train: embed bead %s: %w", c.bead, err)
		}
		samples = append(samples, storypoints.Sample{
			Shape:    storypoints.Features(storypoints.EstimateText(issue), c.rig, c.seat),
			Vector:   vec,
			Minutes:  c.minutes,
			LandedAt: c.landed,
		})
	}
	return samples, skips, nil
}

// trainSeat maps a landing's seat_class cell to the seat the shape feature is
// computed with. Only the two classes the weights were fitted on are read.
func trainSeat(class string) (storypoints.Seat, bool) {
	switch class {
	case "haiku":
		return storypoints.SeatHaiku, true
	case "flash":
		return storypoints.SeatFlash, true
	}
	return storypoints.SeatUnknown, false
}

// holdoutScore is the holdout read for the report. proposed is the rank correlation
// of the proposed model's minutes with the measured minutes; proposedErr is set when
// it cannot be computed (no spread in one series), which is never read as clearing
// the bar. checked is the rank correlation of the checked-in and the proposed
// models with each other, and checkedErr is set when that cannot be computed. A
// checked-in model that predicts one value for every bead has no spread, so its
// error reports the comparison as unmeasured without deciding the proposed file.
type holdoutScore struct {
	proposed    float64
	proposedErr error
	checked     float64
	checkedErr  error
}

// holdoutRanks scores the holdout of a retrain.
func holdoutRanks(holdout []storypoints.Holdout) holdoutScore {
	actual := make([]float64, len(holdout))
	prop := make([]float64, len(holdout))
	chk := make([]float64, len(holdout))
	for i, h := range holdout {
		actual[i], prop[i], chk[i] = h.Actual, h.Proposed, h.Checked
	}
	var s holdoutScore
	s.proposed, s.proposedErr = Spearman(actual, prop)
	s.checked, s.checkedErr = Spearman(chk, prop)
	return s
}

// storyPointsTrainReport is the report the step writes and prints: what was read,
// the penalties chosen, the holdout scores, and the verdict on the proposed file.
func storyPointsTrainReport(m storypoints.Model, skips trainSkips, name string, score holdoutScore) string {
	var b strings.Builder
	fmt.Fprintf(&b, "story points retrain: %s\n", m.TrainedOn)
	fmt.Fprintf(&b, "not read: seat class %d, unmeasured %d, bead not in store %d, no Constraints or Acceptance %d, rig without a directory %d\n",
		skips.seat, skips.unmeasured, skips.noBead, skips.noText, skips.badRig)
	fmt.Fprintf(&b, "penalties: shape %g, embedding %g\n", m.ShapeAlpha, m.EmbedAlpha)
	fmt.Fprintf(&b, "holdout: latest %d beads (the earlier beads fit the penalties)\n", len(m.Holdout))
	if score.proposedErr == nil {
		fmt.Fprintf(&b, "holdout rank correlation of the proposed weights: %.3f (bar %.2f)\n", score.proposed, storyPointsHoldoutBar)
	} else {
		fmt.Fprintf(&b, "holdout rank correlation of the proposed weights: not measurable (%v)\n", score.proposedErr)
	}
	if score.checkedErr == nil {
		fmt.Fprintf(&b, "rank correlation of checked-in and proposed weights on the holdout: %.3f\n", score.checked)
	} else {
		fmt.Fprintf(&b, "rank correlation of checked-in and proposed weights on the holdout: not measurable (%v)\n", score.checkedErr)
	}
	if score.proposedErr == nil && score.proposed >= storyPointsHoldoutBar {
		fmt.Fprintf(&b, "verdict: clears the bar; wrote %s, adopt it by committing it as a bead\n", name)
	} else {
		fmt.Fprintf(&b, "verdict: below the bar; wrote %s, not adopted\n", name)
	}
	return b.String()
}

// storyPointsEmbedder is the ollama embedder the checked-in weights name, at the
// URL the environment gives, else the local default.
func storyPointsEmbedder(base storypoints.Weights) storypoints.Embedder {
	url := os.Getenv(storyPointsEmbedURLEnv)
	if url == "" {
		url = storyPointsEmbedDefaultURL
	}
	return storypoints.OllamaEmbedder{
		URL:     url,
		Model:   base.EmbeddingModel(),
		Dim:     base.EmbeddingDimension(),
		Timeout: storyPointsEmbedTimeout,
		HTTP:    &http.Client{Transport: &http.Transport{Proxy: nil}},
	}
}
