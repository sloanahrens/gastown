package metrics

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// legacyLandingColumns is the header landings.csv was written with before the
// stage join and the polecat time appended theirs.
var legacyLandingColumns = []string{
	"bead", "rig", "landed_at", "week", "route", "om_verdict", "gate_result",
	"lines_added", "lines_deleted", "lines_total", "files_touched",
	"file_entropy", "package_entropy", "attempts", "rejected_before_landing",
	"manual_rescue", "reached_overrun_stop", "unreadable", "type", "priority",
	"shaped", "label_count", "model", "om_threshold", "dominant_model",
}

// landingStageStamps are the columns a landing's stage clock fills, in the
// order the footer counts them.
var landingStageStamps = []string{"created_at", "dispatched_at", "submitted_at"}

// appendedLandingColumns are the columns the stage join and the polecat time
// add after the ported extract's header. The Python reference stays at that
// header: a script does not take the join logic Go owns, so the contract
// extends it append-only.
var appendedLandingColumns = []string{
	"created_at", "dispatched_at", "submitted_at", "time_to_land_s",
	"polecat_secs", "polecat_lower_bound", "agent", "seat_class", "sp_actual",
}

// jevLandingColumns are the Jev dispatch answers a landing carries, from the
// first line the dispatch log holds for its bead. They are not in
// appendedLandingColumns: Jev answers before the landing, so they stay
// predictors and are not explain-only.
var jevLandingColumns = []string{"jev_fits20", "jev_size"}

// storyPointLandingColumns are the bead's story-point labels as the landing
// carries them: sp is the estimate (sp:N) and sp_rung names the inputs it was
// made from (sp-rung:<rung>). They are read from the bead's labels, so a bead
// the estimate never labelled keeps both cells empty. They come after the Jev
// columns, append-only.
var storyPointLandingColumns = []string{"sp", "sp_rung"}

// landingColumns is the header landings.csv is written with: the legacy header
// then the appended columns, then the Jev columns, then the story-point
// columns. Columns are append-only: an existing column keeps its name and its
// place, so a reader of an older file reads the new one under the same names.
var landingColumns = slices.Concat(legacyLandingColumns, appendedLandingColumns, jevLandingColumns, storyPointLandingColumns)

// polecatSecsCell is a landing's polecat time in seconds, "" when the ledger
// line carries none. The writer omits a zero time, so a zero or negative value
// is a line from before the field or a corrupt one, and both read as absent.
func polecatSecsCell(l LedgerRow) string {
	if l.PolecatSecs <= 0 {
		return ""
	}
	return strconv.FormatFloat(l.PolecatSecs, 'f', -1, 64)
}

// polecatLowerBoundCell is the lower-bound flag of a landing's polecat time. A
// line with no time and no bound is a line from before the fields existed (or a
// landing with no seat), so its cell is empty rather than a false flag.
func polecatLowerBoundCell(l LedgerRow) string {
	if l.PolecatSecs <= 0 && !l.PolecatLowerBound {
		return ""
	}
	return boolCell(l.PolecatLowerBound)
}

// spActualCell is the points a landing's measured polecat time is worth, the
// sp_actual cell. A lower-bound time, zero seconds and a time FromMinutes
// refuses have no points, so the cell is empty.
func spActualCell(l LedgerRow) string {
	if l.PolecatSecs <= 0 || l.PolecatLowerBound {
		return ""
	}
	points, err := storypoints.FromMinutes(l.PolecatSecs / 60)
	if err != nil {
		return ""
	}
	return strconv.Itoa(points)
}

// rungLabelNone is the label an estimate carries when no input made it. It is
// not a rung ParseLabel accepts, so the cell reads it by its spelling.
const rungLabelNone = "sp-rung:" + rungNone

// rungNone is the rung value of an estimate no input made.
const rungNone = "none"

// storyPointCells reads a bead's sp and sp_rung cells from its labels. A label
// that starts sp: or sp-rung: and does not parse is named in dropped, not
// ignored: a bead whose estimate cannot be read is unknown, not unlabelled. Two
// different values of one kind are also unknown, so that cell is empty.
func storyPointCells(bead string, labels []string, dropped *Dropped) (sp, rung string) {
	var points, rungs []string
	for _, label := range labels {
		if !strings.HasPrefix(label, "sp:") && !strings.HasPrefix(label, "sp-rung:") {
			continue
		}
		parsed, err := storypoints.ParseLabel(label)
		switch {
		case err == nil && parsed.Kind == storypoints.KindEstimate:
			points = append(points, strconv.Itoa(parsed.Points))
		case err == nil && parsed.Kind == storypoints.KindRung:
			rungs = append(rungs, parsed.Rung)
		case label == rungLabelNone:
			rungs = append(rungs, rungNone)
		default:
			dropped.Add("labels:"+bead, "unreadable story-point label "+label)
		}
	}
	sp, err := singleLabelValue(points)
	if err != nil {
		dropped.Add("labels:"+bead, "conflicting sp labels: "+strings.Join(points, ","))
	}
	rung, err = singleLabelValue(rungs)
	if err != nil {
		dropped.Add("labels:"+bead, "conflicting sp-rung labels: "+strings.Join(rungs, ","))
	}
	return sp, rung
}

// singleLabelValue is the one value a bead's labels of a kind agree on: "" when
// it has none, and an error when they disagree.
func singleLabelValue(values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	for _, v := range values[1:] {
		if v != values[0] {
			return "", errors.New("labels disagree")
		}
	}
	return values[0], nil
}

// LandingGaps is what the joined landings do not cover: the rows no seat record
// gave a model, and the rows whose stage stamp is empty, by stamp name.
type LandingGaps struct {
	ModelUnknown      int
	StageStampMissing map[string]int
}

// CountLandingGaps counts live from the rows' cells, so a footer count cannot
// disagree with the column it describes.
func CountLandingGaps(rows []Row) LandingGaps {
	gaps := LandingGaps{StageStampMissing: make(map[string]int, len(landingStageStamps))}
	for _, row := range rows {
		if row["model"] == "" {
			gaps.ModelUnknown++
		}
		for _, name := range landingStageStamps {
			if row[name] == "" {
				gaps.StageStampMissing[name]++
			}
		}
	}
	return gaps
}

// landingModel is the model a landing ran on, "" when no usage record names
// one. The seat that wrote the landed branch is AuthorSeat(branch): the branch
// is what landed, so it names the landed attempt even when an earlier seat
// worked the same bead.
//
// The bead's own usage record answers the model when it names no seat or that
// seat, because it measured the run. When it names another seat its models are
// that seat's work, so the landed seat's own model answers instead
// (UsageSeats.SeatModels). A landing whose bead and seat no record names keeps
// the cell empty and is counted model_unknown, never dropped.
func landingModel(l LedgerRow, usage map[string]Usage, seats UsageSeats) string {
	model := usage[l.Bead].Model
	landed := AuthorSeat(l.Branch)
	if landed == "" {
		return model
	}
	if recordSeat := seats.BeadSeats[l.Bead]; recordSeat != "" && recordSeat != landed {
		return seats.SeatModels[l.Rig+"/"+landed]
	}
	if model != "" {
		return model
	}
	return seats.SeatModels[l.Rig+"/"+landed]
}

// landingSubmitted is when a landing was submitted: the READY TO LAND block's
// own stamp, which gt done writes with the submission and no comment moves
// (gt-t2jhf), else the bead's latest done event. A stamp after the landing
// belongs to a run that did not produce it, so it is not this landing's stamp,
// whether the notes or the feed carries it: a later run that resubmitted the
// bead rewrites the note too.
func landingSubmitted(bead string, b BeadRow, stamps StageStamps, landedAt time.Time) string {
	if note := b.SubmittedAt; note != "" {
		if when, err := ParseTS(note); err == nil && !when.After(landedAt) {
			return note
		}
	}
	at, ok := stamps.Submitted[bead]
	if !ok {
		return ""
	}
	when, err := ParseTS(at)
	if err != nil || when.After(landedAt) {
		return ""
	}
	return at
}

// stampCell is a stage stamp as the cell carries it, "" when the source text is
// not a timestamp. A stamp the join cannot place on a clock is missing, so the
// footer counts it and time_to_land_s stays empty, rather than the cell carrying
// text no reader can use.
func stampCell(at string) string {
	if at == "" {
		return ""
	}
	if _, err := ParseTS(at); err != nil {
		return ""
	}
	return at
}

// timeToLandSeconds is a landing's created_at to landed_at in seconds, "" when
// either stamp is missing or unreadable, or when the bead was created after it
// landed. An empty cell is the question the report cannot answer; a zero would
// read as an instant landing.
func timeToLandSeconds(createdAt, landedAt string) string {
	if createdAt == "" || landedAt == "" {
		return ""
	}
	from, err := ParseTS(createdAt)
	if err != nil {
		return ""
	}
	to, err := ParseTS(landedAt)
	if err != nil || to.Before(from) {
		return ""
	}
	return strconv.FormatFloat(to.Sub(from).Seconds(), 'f', -1, 64)
}

// Row is one landing: the CSV's header mapped to its cells, with a cell the
// input left empty read back as "".
type Row map[string]string

// ReadLandings reads landings.csv: its first record is the header and every
// later record is a Row keyed by it, so a column this package has never heard
// of is carried rather than dropped. The header is returned as written, in its
// own order.
//
// A record whose width differs from the header is an error, not a short row:
// the extract writes every column of every landing, so a ragged record means
// the file is not the contract and its cells cannot be attributed.
//
// A CRLF pair inside a quoted cell is read as the LF that encoding/csv makes
// of a line ending inside a field; every other cell byte is kept.
func ReadLandings(r io.Reader) (header []string, rows []Row, err error) {
	cr := csv.NewReader(r)
	header, err = cr.Read()
	if err == io.EOF {
		return nil, nil, fmt.Errorf("landings.csv: empty file")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("landings.csv header: %w", err)
	}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return header, rows, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("landings.csv row %d: %w", len(rows)+2, err)
		}
		row := make(Row, len(header))
		for i, name := range header {
			row[name] = rec[i]
		}
		rows = append(rows, row)
	}
}

// WriteLandings writes landings.csv in the extract's format: the
// header, then each row's cells in header order, for a column the row does not
// carry an empty cell, and CRLF line endings.
//
// The line ending is the extract's, not Go's csv default, so a file read by
// ReadLandings and written back with no columns added is byte for byte the
// file the extract wrote, as long as no cell carries the CRLF pair ReadLandings
// folds to an LF.
func WriteLandings(w io.Writer, header []string, rows []Row) error {
	if err := writeRecord(w, header); err != nil {
		return fmt.Errorf("landings.csv header: %w", err)
	}
	rec := make([]string, len(header))
	for i, row := range rows {
		for j, name := range header {
			rec[j] = row[name]
		}
		if err := writeRecord(w, rec); err != nil {
			return fmt.Errorf("landings.csv row %d: %w", i+2, err)
		}
	}
	return nil
}

// writeRecord writes one record the way the extract's csv writer does: a cell
// carrying a comma, a quote, a CR or an LF is quoted with its quotes doubled,
// and the record ends CRLF.
//
// The cells are copied verbatim, which encoding/csv's CRLF mode does not do: it
// drops a bare CR and rewrites every LF in a cell as CRLF, so a value a
// snapshot carried would not reach the file as written (gt-yr3kn).
func writeRecord(w io.Writer, cells []string) error {
	var b strings.Builder
	for i, c := range cells {
		if i > 0 {
			b.WriteByte(',')
		}
		if strings.ContainsAny(c, ",\"\r\n") {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(c, `"`, `""`))
			b.WriteByte('"')
			continue
		}
		b.WriteString(c)
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}
