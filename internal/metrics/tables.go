package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/storypoints"
)

// explainOnlyColumns are the landings.csv columns known only after the landing:
// never a predictor, only joined back to explain a box. The polecat time and the
// seat are written by the landing record, so they are known only after it too.
// The timestamps are not features either, so sp_estimated_at is explain-only
// beside created_at and dispatched_at. They are kept in the sorted order the
// sidecar writes.
var explainOnlyColumns = []string{
	"agent", "attempts", "created_at", "dispatched_at", "file_entropy", "files_touched",
	"gate_result", "lines_added", "lines_deleted", "lines_total", "om_verdict",
	"package_entropy", "polecat_lower_bound", "polecat_secs", "seat_class", "sp_actual",
	"sp_estimated_at", "submitted_at", "time_to_land_s", "unreadable",
}

// DiffSource is where a landing's size and spread columns come from: Stats
// returns the stats of one commit, and an error wrapping ErrDiffUnavailable
// when the diff cannot be read.
type DiffSource interface {
	Stats(ctx context.Context, rig, commit string) (DiffStat, error)
}

// BuildLandings joins the loaded sources into one landings.csv row per ledger
// row, as tables.py's build_landings does: the ledger's own fields, the
// rework depth, the diff's size and spread, the bead's shape, the regime in
// force when it landed, and the stage stamps and model the join derives.
//
// A landing whose diff cannot be read keeps its row, with empty size and spread
// cells and unreadable=1, and is named in dropped. A landing with no rework
// entry is a first attempt. A stage stamp or model no source supplies is an
// empty cell, counted in the extract's footer rather than dropped. A landing
// whose bead has no line in jev keeps empty Jev cells.
func BuildLandings(ctx context.Context, ledger []LedgerRow, rework map[string]Rework, diffs DiffSource,
	beadsByID map[string]BeadRow, usage map[string]Usage, overrun map[string]bool,
	eras map[string][]OMEra, shapedIDs map[string]bool, stamps StageStamps,
	jev map[string]dispatch.Line, dropped *Dropped) ([]Row, error) {
	rows := make([]Row, 0, len(ledger))
	for _, l := range ledger {
		rw, ok := rework[l.Bead]
		if !ok {
			rw = Rework{Attempts: 1}
		}

		var d *DiffStat
		if l.LandedCommit != "" {
			stats, err := diffs.Stats(ctx, l.Rig, l.LandedCommit)
			if err != nil && !errors.Is(err, ErrDiffUnavailable) {
				return nil, fmt.Errorf("landing %s: %w", l.Bead, err)
			}
			if err == nil {
				d = &stats
			}
		}
		if d == nil {
			dropped.Add("git:"+l.Rig, "unreadable diff for "+l.Bead)
		}

		landed, err := ParseTS(l.LandedAt)
		if err != nil {
			return nil, fmt.Errorf("landing %s: %w", l.Bead, err)
		}
		week := weekOf(landed)
		threshold, err := EraOf(eras[l.Rig], l.LandedAt)
		if err != nil {
			return nil, fmt.Errorf("landing %s: %w", l.Bead, err)
		}

		b := beadsByID[l.Bead]
		model := landingModel(l, usage, stamps.Seats)
		jevFits, jevSize := jevCells(jev, l.Bead)
		sp, spRung, spMinutes := storyPointCells(l.Bead, b.Labels, dropped)
		createdAt := stampCell(b.CreatedAt)
		dispatchedAt := stampCell(stamps.Dispatched[l.Bead])
		submittedAt := landingSubmitted(l.Bead, b, stamps, landed)
		// A nil value is written as an empty cell.
		var linesAdded, linesDeleted, linesTotal, filesTouched, fileEntropy, packageEntropy string
		if d != nil {
			linesAdded = strconv.Itoa(d.LinesAdded)
			linesDeleted = strconv.Itoa(d.LinesDeleted)
			linesTotal = strconv.Itoa(d.LinesAdded + d.LinesDeleted)
			filesTouched = strconv.Itoa(d.FilesTouched)
			fileEntropy = FormatFloat(d.FileEntropy)
			packageEntropy = FormatFloat(d.PackageEntropy)
		}
		rows = append(rows, Row{
			"bead":                    l.Bead,
			"rig":                     l.Rig,
			"landed_at":               l.LandedAt,
			"week":                    week,
			"route":                   l.Route,
			"om_verdict":              l.OMVerdict,
			"gate_result":             l.GateResult,
			"lines_added":             linesAdded,
			"lines_deleted":           linesDeleted,
			"lines_total":             linesTotal,
			"files_touched":           filesTouched,
			"file_entropy":            fileEntropy,
			"package_entropy":         packageEntropy,
			"attempts":                strconv.Itoa(rw.Attempts),
			"rejected_before_landing": boolCell(rw.RejectedBeforeLanding),
			"manual_rescue":           boolCell(l.Route != "daemon"),
			"reached_overrun_stop":    boolCell(overrun[l.Bead]),
			"unreadable":              boolCell(d == nil),
			"type":                    b.Type,
			"priority":                priorityCell(b.Priority),
			"shaped":                  boolCell(shapedIDs[l.Bead]),
			"label_count":             strconv.Itoa(len(b.Labels)),
			"model":                   model,
			"om_threshold":            thresholdCell(threshold),
			"dominant_model":          model,
			"created_at":              createdAt,
			"dispatched_at":           dispatchedAt,
			"submitted_at":            submittedAt,
			"time_to_land_s":          timeToLandSeconds(createdAt, l.LandedAt),
			"polecat_secs":            polecatSecsCell(l),
			"polecat_lower_bound":     polecatLowerBoundCell(l),
			"agent":                   l.Agent,
			"seat_class":              storypoints.SeatClass(l.Agent),
			"sp_actual":               spActualCell(l),
			"jev_fits20":              jevFits,
			"jev_size":                jevSize,
			"sp":                      sp,
			"sp_rung":                 spRung,
			"sp_minutes":              spMinutes,
			"sp_estimated_at":         stampCell(b.EstimatedAt),
		})
	}
	return rows, nil
}

// priorityCell is the bead's priority, or the empty cell of a bead with none.
func priorityCell(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

// thresholdCell is the era's threshold through FormatFloat, or empty when no
// era had begun.
func thresholdCell(v *float64) string {
	if v == nil {
		return ""
	}
	return FormatFloat(*v)
}

// boolCell is a flag landing as the 1 or 0 count landings.csv carries.
func boolCell(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// WriteLandingSidecar writes landings.sidecar.json: the explain-only columns,
// which the mango stage drops from its predictors, in indent=1 JSON bytes.
func WriteLandingSidecar(w io.Writer) error {
	sidecar := struct {
		ExplainOnly []string `json:"explain_only"`
	}{ExplainOnly: explainOnlyColumns}
	data, err := json.MarshalIndent(sidecar, "", " ")
	if err != nil {
		return fmt.Errorf("landings.sidecar.json: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("landings.sidecar.json: %w", err)
	}
	return nil
}
