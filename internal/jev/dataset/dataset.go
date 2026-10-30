// Package dataset turns a battery's labeled answers into the flat CSV the
// mango analysis loads: one row per labeled item that answered a question,
// holding ids and features and never the text Jev judged.
package dataset

import (
	"cmp"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// The fixed columns, in written order. The ref_ and ans_ columns follow them.
const (
	colRowID       = "row_id"
	colDay         = "day"
	colHour        = "hour"
	colDow         = "dow"
	colModel       = "model"
	colAnswer      = "answer"
	colAnswerBand  = "answer_band"
	colConfidence  = "confidence"
	colLabel       = "label"
	colLabelRule   = "label_rule"
	colLabelSource = "label_source"
)

// refAllow lists the ref names an export may write, in column order: the
// adapter's own refs name the beads, sections and commits the labels were read
// from, and only these few are features.
var refAllow = []string{"rig", "agent", "attempt", "target"}

// denyNames refuses a column source that may carry the text Jev judged: a
// question named here would put prompts or passages in the file.
var denyNames = map[string]bool{
	"text": true, "title": true, "sample": true, "samples": true,
	"excerpt": true, "passage": true, "transcript": true, "prompt": true,
	"body": true, "description": true,
}

const (
	// rowIDLen is how many hex characters of the item id's sha256 a row id
	// keeps: short enough to read, long enough that a battery's dataset will
	// not collide.
	rowIDLen = 12
	// maxDistinct is how many distinct values a categorical column may hold
	// before the export drops it: mango reads a wide column as an id, and it
	// would only overfit.
	maxDistinct = 15
	// sampleRows is how many leading rows mango reads to infer a column's
	// type, so the dict reports the type mango will see, not the whole file's.
	sampleRows = 100
)

// Table is one export: the columns in written order, the rows in written
// order, the columns left out of the file, and every question the log answers.
type Table struct {
	Columns []string
	Rows    [][]string
	Dropped []Dropped
	// Questions names the questions the log answers, in id order, whether or
	// not an item labels them.
	Questions []scorecard.QuestionID
}

// Dropped names a column left out of the file and why.
type Dropped struct {
	Column string
	Reason string
}

// row is one exported row: the hash of its row id, which orders the file, and
// its cells by column name.
type row struct {
	key   string
	cells map[string]string
}

// Build turns a scorecard reading into the dataset for one battery question: a
// row per labeled item that answered the question, ids and features only. An
// error refuses a column whose name may carry text.
func Build(read scorecard.Reading, question scorecard.QuestionID) (Table, error) {
	kinds := map[scorecard.QuestionID]string{}
	for _, it := range read.Items {
		for id, ans := range it.Answers {
			if _, seen := kinds[id]; !seen {
				if k := ans.Kind(); k != "" {
					kinds[id] = k
				}
			}
		}
	}

	var (
		rows     []row
		refsUsed = map[string]bool{}
		others   = map[scorecard.QuestionID]bool{}
	)
	for i, it := range read.Items {
		lab := read.Labelings[i]
		if !lab.Labeled {
			continue
		}
		label, labeled := lab.Labels[question]
		if !labeled {
			continue
		}
		ans, answered := it.Answers[question]
		kind := ans.Kind()
		if !answered || kind == "" {
			continue
		}
		when := it.Time.UTC()
		cells := map[string]string{
			colRowID:       rowID(it.ID),
			colDay:         when.Format("2006-01-02"),
			colHour:        strconv.Itoa(when.Hour()),
			colDow:         strconv.Itoa(int(when.Weekday())),
			colModel:       it.Model,
			colAnswer:      answerText(ans, kind),
			colAnswerBand:  answerBand(ans, kind),
			colConfidence:  confidence(ans),
			colLabel:       labelText(label),
			colLabelRule:   lab.Rule,
			colLabelSource: lab.Source,
		}
		for name, v := range it.Refs {
			if v == "" || !slices.Contains(refAllow, name) {
				continue
			}
			refsUsed[name] = true
			cells["ref_"+name] = refText(v)
		}
		for id, other := range it.Answers {
			if id == question {
				continue
			}
			k := other.Kind()
			if k == "" {
				continue
			}
			others[id] = true
			cells[answerColumn(id)] = answerText(other, k)
		}
		rows = append(rows, row{key: shuffleKey(cells[colRowID]), cells: cells})
	}

	columns := []string{colRowID, colDay, colHour, colDow, colModel, colAnswer, colAnswerBand, colConfidence, colLabel, colLabelRule, colLabelSource}
	for _, name := range refAllow {
		if refsUsed[name] {
			columns = append(columns, "ref_"+name)
		}
	}
	otherIDs := make([]scorecard.QuestionID, 0, len(others))
	for id := range others {
		otherIDs = append(otherIDs, id)
	}
	slices.Sort(otherIDs)
	for _, id := range otherIDs {
		if err := refuseName(string(id)); err != nil {
			return Table{}, err
		}
		columns = append(columns, answerColumn(id))
	}

	// Sorted by the hash of the row id, so any leading slice of the file is a
	// sample of the whole, and the same reading always writes the same order.
	slices.SortStableFunc(rows, func(a, b row) int { return cmp.Compare(a.key, b.key) })

	kept := make([]string, 0, len(columns))
	var dropped []Dropped
	for _, c := range columns {
		if categorical(c, question, kinds) {
			if n := distinctCount(rows, c); n > maxDistinct {
				dropped = append(dropped, Dropped{
					Column: c,
					Reason: fmt.Sprintf("%d distinct values, more than the %d a categorical column may hold", n, maxDistinct),
				})
				continue
			}
		}
		kept = append(kept, c)
	}

	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		values := make([]string, 0, len(kept))
		for _, c := range kept {
			values = append(values, r.cells[c])
		}
		out = append(out, values)
	}

	questions := make([]scorecard.QuestionID, 0, len(kinds))
	for id := range kinds {
		questions = append(questions, id)
	}
	slices.Sort(questions)

	return Table{Columns: kept, Rows: out, Dropped: dropped, Questions: questions}, nil
}

// refuseName refuses a question whose name may carry text: a question reaches
// the file as an ans_ column, so the deny-list is checked against what the
// name costs, and silently dropping the column would hide a battery that names
// its prompts.
func refuseName(name string) error {
	if denyNames[strings.ToLower(name)] {
		return fmt.Errorf("refusing question %q: it would be an ans_ column for text, and the dataset holds ids and features only", name)
	}
	return nil
}

// categorical reports whether a column's values are names rather than numbers.
func categorical(column string, question scorecard.QuestionID, kinds map[scorecard.QuestionID]string) bool {
	switch {
	case column == colModel, column == colAnswerBand, column == colLabelRule, column == colLabelSource:
		return true
	case column == colAnswer:
		return kinds[question] == scorecard.KindChoice
	case strings.HasPrefix(column, "ref_"):
		return true
	case strings.HasPrefix(column, "ans_"):
		return kinds[scorecard.QuestionID(strings.TrimPrefix(column, "ans_"))] == scorecard.KindChoice
	}
	return false
}

// distinctCount counts a column's distinct non-empty values.
func distinctCount(rows []row, column string) int {
	seen := map[string]bool{}
	for _, r := range rows {
		if v := r.cells[column]; v != "" {
			seen[v] = true
		}
	}
	return len(seen)
}

// answerColumn names the column that carries the answer to question.
func answerColumn(question scorecard.QuestionID) string { return "ans_" + string(question) }

// rowID is the opaque id a row carries: a prefix of the item id's sha256, so
// the file names no bead and the analysis still joins rows across questions.
func rowID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:rowIDLen]
}

// shuffleKey is what the rows are ordered by: the hash of the row id, never
// the time, so a battery that logged its items in any order exports the same
// file.
func shuffleKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// labelText spells the target the way mango's boolean column expects.
func labelText(label bool) string {
	if label {
		return "true"
	}
	return "false"
}

// answerText writes Jev's answer: a noul's probability, a score's level or a
// choice's name.
func answerText(ans scorecard.Answer, kind string) string {
	switch kind {
	case scorecard.KindNoul:
		if ans.Noul != nil {
			return number(*ans.Noul)
		}
	case scorecard.KindScore:
		if ans.Score != nil {
			return number(*ans.Score)
		}
	case scorecard.KindChoice:
		return ans.Choice
	}
	return ""
}

// answerBand writes the scorecard's calibration band of the value it scores
// the answer by, or the score level, which has no band.
func answerBand(ans scorecard.Answer, kind string) string {
	if kind == scorecard.KindScore {
		if ans.Score != nil {
			return number(*ans.Score)
		}
		return ""
	}
	v, _, ok := ans.Value(kind)
	if !ok {
		return ""
	}
	return scorecard.Band(v)
}

// confidence writes the probability of the choice Jev picked; an answer with
// no choice behind it carries none.
func confidence(ans scorecard.Answer) string {
	if ans.Kind() != scorecard.KindChoice || ans.Choice == "" {
		return ""
	}
	p, ok := ans.Probabilities[ans.Choice]
	if !ok {
		return ""
	}
	return number(p)
}

// refText writes a ref's value: a boolean is written 1 or 0 so mango reads the
// column as a feature, and anything else keeps the adapter's words.
func refText(v string) string {
	switch v {
	case "true":
		return "1"
	case "false":
		return "0"
	}
	return v
}

// number writes a float without an exponent and without trailing zeros, the
// shape mango parses as a number.
func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// WriteCSV writes the header and the rows as CSV.
func (t Table) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(t.Columns); err != nil {
		return err
	}
	for _, r := range t.Rows {
		if err := cw.Write(r); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteDict writes the dict that explains the file: each column, the type
// mango will infer it as from the leading rows, what it means, the row count,
// the rows' date range and the columns left out.
func (t Table) WriteDict(w io.Writer, battery string, question scorecard.QuestionID) error {
	first, last := t.dayRange()
	var b strings.Builder
	fmt.Fprintf(&b, "# %s.%s\n\n", battery, question)
	fmt.Fprintf(&b, "Rows: %d\n", len(t.Rows))
	if first == "" {
		b.WriteString("Date range: none\n")
	} else if first == last {
		fmt.Fprintf(&b, "Date range: %s\n", first)
	} else {
		fmt.Fprintf(&b, "Date range: %s to %s\n", first, last)
	}
	b.WriteString("An empty cell is a missing value.\n\n")

	b.WriteString("| column | type | meaning |\n| --- | --- | --- |\n")
	for i, c := range t.Columns {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c, t.typeOf(i), meaning(c, question))
	}

	if len(t.Dropped) == 0 {
		b.WriteString("\nDropped columns: none.\n")
	} else {
		b.WriteString("\nDropped columns:\n")
		for _, d := range t.Dropped {
			fmt.Fprintf(&b, "- %s: %s\n", d.Column, d.Reason)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// dayRange returns the earliest and latest day the rows carry.
func (t Table) dayRange() (first, last string) {
	i := slices.Index(t.Columns, colDay)
	if i < 0 {
		return "", ""
	}
	for _, r := range t.Rows {
		day := r[i]
		if day == "" {
			continue
		}
		if first == "" || day < first {
			first = day
		}
		if day > last {
			last = day
		}
	}
	return first, last
}

// typeOf reports the type mango infers for a column from the leading rows: a
// number whenever every value is one (so 0 and 1 read numeric, not boolean), a
// boolean of true and false, a categorical when fewer than a fifth of the
// values are distinct, and text otherwise.
func (t Table) typeOf(column int) string {
	values := make([]string, 0, min(len(t.Rows), sampleRows))
	for _, r := range t.Rows[:min(len(t.Rows), sampleRows)] {
		values = append(values, r[column])
	}
	distinct := map[string]bool{}
	n := 0
	for _, v := range values {
		if v == "" {
			continue
		}
		n++
		distinct[v] = true
	}
	switch {
	case n == 0:
		return "empty"
	case allOf(values, isNumber):
		return "numeric"
	case allOf(values, isBool):
		return "boolean"
	case len(distinct)*5 < n:
		return "categorical"
	}
	return "text"
}

// allOf reports whether every non-empty value satisfies f.
func allOf(values []string, f func(string) bool) bool {
	for _, v := range values {
		if v != "" && !f(v) {
			return false
		}
	}
	return true
}

func isNumber(v string) bool {
	_, err := strconv.ParseFloat(v, 64)
	return err == nil
}

func isBool(v string) bool { return v == "true" || v == "false" }

// meaning explains a column to the reader of the dict.
func meaning(column string, question scorecard.QuestionID) string {
	switch column {
	case colRowID:
		return fmt.Sprintf("opaque row id: the first %d hex characters of the sha256 of the item id", rowIDLen)
	case colDay:
		return "day of the answer, UTC"
	case colHour:
		return "hour of the answer, UTC, 0 to 23"
	case colDow:
		return "day of the week of the answer, 0 is Sunday"
	case colModel:
		return "the model that answered"
	case colAnswer:
		return fmt.Sprintf("Jev's answer to %s: a noul probability, a score or the choice name", question)
	case colAnswerBand:
		return "the scorecard's calibration band of the scored value, 0.0-0.2 through 0.8-1.0, or the score level"
	case colConfidence:
		return "the probability of the choice Jev picked, empty when the answer carries none"
	case colLabel:
		return "mango's target: the labeled truth for the question, true or false"
	case colLabelRule:
		return "the scorecard rule that set the label"
	case colLabelSource:
		return "where the label came from, such as the overseer's review, empty when the adapter names none"
	}
	if name, ok := strings.CutPrefix(column, "ref_"); ok {
		return fmt.Sprintf("the %s the item's log line carried", name)
	}
	if id, ok := strings.CutPrefix(column, "ans_"); ok {
		return fmt.Sprintf("Jev's answer to %s on the same item", id)
	}
	return ""
}

// Dir is the town's default dataset directory, under its runtime state.
func Dir(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev-dataset")
}

// Paths names the CSV and the dict for one battery question under dir. An
// error refuses a name that cannot be a filename: the battery and the question
// come from the log, and the export must not write outside dir.
func Paths(dir, battery string, question scorecard.QuestionID) (csvPath, dictPath string, err error) {
	base := battery + "." + string(question)
	if !safeName(battery) || !safeName(string(question)) {
		return "", "", fmt.Errorf("refusing the name %q: a battery and a question name letters, digits, dots, dashes and underscores only", base)
	}
	return filepath.Join(dir, base+".csv"), filepath.Join(dir, base+".dict.md"), nil
}

// safeName reports whether name is one path element with no separator or
// parent reference.
func safeName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
