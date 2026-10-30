package dataset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// fixtureReading is the parse-and-label pass over a small battery: two labeled
// items answering a noul and a choice, one unlabeled item, a ref the export
// may write and one it may not.
func fixtureReading() scorecard.Reading {
	day := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	choice := func(name string, yes float64) scorecard.Answer {
		return scorecard.Answer{
			Choice:        name,
			Probabilities: map[string]float64{"yes": yes, "no": 1 - yes},
			Positive:      "yes",
		}
	}
	return scorecard.Reading{
		Items: []scorecard.Item{
			{ID: "jb-1", Time: day.Add(9 * time.Hour), Model: "m1",
				Answers: map[scorecard.QuestionID]scorecard.Answer{
					"needs": {Noul: ptr(0.9)},
					"route": choice("yes", 0.8),
				},
				Refs: map[string]string{"rig": "gastown", "agent": "polecat", "head": "a1a1a1a"}},
			{ID: "jb-2", Time: day.Add(10 * time.Hour), Model: "m2",
				Answers: map[scorecard.QuestionID]scorecard.Answer{
					"needs": {Noul: ptr(0.2)},
					"route": choice("no", 0.3),
				},
				Refs: map[string]string{"rig": "gastown", "agent": "crew", "head": "b2b2b2b"}},
			{ID: "jb-3", Time: day.Add(11 * time.Hour), Model: "m1",
				Answers: map[scorecard.QuestionID]scorecard.Answer{
					"needs": {Noul: ptr(0.7)},
					"route": choice("yes", 0.6),
				},
				Refs: map[string]string{"rig": "gastown"}},
		},
		Labelings: []scorecard.Labeling{
			{Labeled: true, Labels: map[scorecard.QuestionID]bool{"needs": true, "route": true}, Rule: "fixture-labels", Source: "overseer"},
			{Labeled: true, Labels: map[scorecard.QuestionID]bool{"needs": false, "route": false}, Rule: "fixture-labels"},
			{},
		},
	}
}

func ptr(v float64) *float64 { return &v }

// writeCSV renders a table's CSV as a string.
func writeCSV(t *testing.T, table Table) string {
	t.Helper()
	var b bytes.Buffer
	if err := table.WriteCSV(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The rows hold the labeled answers to the question, the other answers beside
// them and the allow-listed refs, and leave the unlabeled item out.
func TestDatasetRows(t *testing.T) {
	t.Parallel()
	table, err := Build(fixtureReading(), "needs")
	if err != nil {
		t.Fatal(err)
	}
	want := `row_id,day,hour,dow,model,answer,answer_band,confidence,label,label_rule,label_source,ref_rig,ref_agent,ans_route
5de8462c2176,2026-10-18,10,0,m2,0.2,0.2-0.4,,false,fixture-labels,,gastown,crew,no
2cde2b79325a,2026-10-18,9,0,m1,0.9,0.8-1.0,,true,fixture-labels,overseer,gastown,polecat,yes
`
	if got := writeCSV(t, table); got != want {
		t.Errorf("CSV = %q, want %q", got, want)
	}
	for _, row := range table.Rows {
		if row[0] == "bbeed24d02f2" {
			t.Error("the unlabeled item jb-3 is in the file")
		}
	}
}

// A choice question exports the chosen name, its band over the scored
// probability and the confidence in the choice, with the noul beside it.
func TestDatasetChoiceQuestion(t *testing.T) {
	t.Parallel()
	table, err := Build(fixtureReading(), "route")
	if err != nil {
		t.Fatal(err)
	}
	want := `row_id,day,hour,dow,model,answer,answer_band,confidence,label,label_rule,label_source,ref_rig,ref_agent,ans_needs
5de8462c2176,2026-10-18,10,0,m2,no,0.2-0.4,0.7,false,fixture-labels,,gastown,crew,0.2
2cde2b79325a,2026-10-18,9,0,m1,yes,0.8-1.0,0.8,true,fixture-labels,overseer,gastown,polecat,0.9
`
	if got := writeCSV(t, table); got != want {
		t.Errorf("CSV = %q, want %q", got, want)
	}
}

// Two builds of the same reading write the same bytes, in hash order rather
// than the log's order.
func TestDatasetShuffleIsDeterministic(t *testing.T) {
	t.Parallel()
	read := fixtureReading()
	first, err := Build(read, "needs")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(fixtureReading(), "needs")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := writeCSV(t, first), writeCSV(t, second); got != want {
		t.Errorf("two builds differ:\n%s\nwant:\n%s", got, want)
	}

	dayColumn := 1
	var days []string
	for _, row := range first.Rows {
		days = append(days, row[dayColumn])
		want := "2026-10-18"
		if row[dayColumn] != want {
			t.Errorf("row %s day %q, want %q", row[0], row[dayColumn], want)
		}
	}
	if len(first.Rows) != 2 {
		t.Fatalf("table has %d rows, want 2", len(first.Rows))
	}
	// The table holds jb-1 (09:00) before jb-2 (10:00); the file must not.
	if first.Rows[0][0] != "5de8462c2176" || first.Rows[1][0] != "2cde2b79325a" {
		t.Errorf("row order = %s, %s, want jb-2's row before jb-1's by hash",
			first.Rows[0][0], first.Rows[1][0])
	}
	for i := 1; i < len(first.Rows); i++ {
		if key(first.Rows[i-1][0]) > key(first.Rows[i][0]) {
			t.Errorf("rows are not in sha256 order at row %d", i)
		}
	}
}

// key is the hash the spec orders rows by.
func key(rowID string) string {
	sum := sha256.Sum256([]byte(rowID))
	return hex.EncodeToString(sum[:])
}

// A ref the allow-list leaves out never reaches the file, a question named for
// text is an error, and a categorical with too many values is dropped and
// named in the dict.
func TestDatasetRefusesTextColumns(t *testing.T) {
	t.Parallel()

	t.Run("refs outside the allow-list are dropped", func(t *testing.T) {
		t.Parallel()
		read := fixtureReading()
		read.Items[0].Refs["title"] = "a prompt the file must not hold"
		read.Items[0].Refs["samples"] = "a transcript"
		table, err := Build(read, "needs")
		if err != nil {
			t.Fatal(err)
		}
		csv := writeCSV(t, table)
		for _, name := range []string{"ref_title", "ref_samples", "a prompt the file", "a transcript"} {
			if strings.Contains(csv, name) {
				t.Errorf("CSV holds %q:\n%s", name, csv)
			}
		}
	})

	t.Run("a question named for text is an error", func(t *testing.T) {
		t.Parallel()
		read := fixtureReading()
		read.Items[0].Answers["prompt"] = scorecard.Answer{Noul: ptr(0.4)}
		read.Labelings[0].Labels["prompt"] = true
		_, err := Build(read, "needs")
		if err == nil {
			t.Fatal("a prompt question exported cleanly")
		}
		if !strings.Contains(err.Error(), `"prompt"`) {
			t.Errorf("error %q does not name the question", err)
		}
	})

	t.Run("a wide categorical is dropped and named", func(t *testing.T) {
		t.Parallel()
		read := fixtureReading()
		read.Labelings[2] = scorecard.Labeling{
			Labeled: true,
			Labels:  map[scorecard.QuestionID]bool{"needs": true},
			Rule:    "fixture-labels",
		}
		for i := range read.Items {
			read.Items[i].Model = fmt.Sprintf("model-%d", i)
		}
		// Sixteen labeled items, each on its own model, put the column past
		// the fifteen distinct values a categorical may hold.
		for i := len(read.Items); i < 16; i++ {
			read.Items = append(read.Items, scorecard.Item{
				ID:      fmt.Sprintf("jb-%d", i),
				Time:    read.Items[0].Time,
				Model:   fmt.Sprintf("model-%d", i),
				Answers: map[scorecard.QuestionID]scorecard.Answer{"needs": {Noul: ptr(0.5)}},
			})
			read.Labelings = append(read.Labelings, scorecard.Labeling{
				Labeled: true,
				Labels:  map[scorecard.QuestionID]bool{"needs": true},
				Rule:    "fixture-labels",
			})
		}
		table, err := Build(read, "needs")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range table.Columns {
			if c == "model" {
				t.Fatal("the model column survived with 16 distinct values")
			}
		}
		if len(table.Dropped) != 1 || table.Dropped[0].Column != "model" {
			t.Fatalf("dropped = %+v, want the model column", table.Dropped)
		}
		var dict bytes.Buffer
		if err := table.WriteDict(&dict, "fixture", "needs"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(dict.String(), "- model: 16 distinct values") {
			t.Errorf("dict does not name the dropped model column:\n%s", dict.String())
		}
	})
}

// The dict names every column's type and meaning, the row count, the date
// range and the dropped columns.
func TestDatasetDict(t *testing.T) {
	t.Parallel()
	table, err := Build(fixtureReading(), "needs")
	if err != nil {
		t.Fatal(err)
	}
	var dict bytes.Buffer
	if err := table.WriteDict(&dict, "fixture", "needs"); err != nil {
		t.Fatal(err)
	}
	got := dict.String()
	for _, want := range []string{
		"# fixture.needs\n",
		"Rows: 2\n",
		"Date range: 2026-10-18\n",
		"| answer | numeric |",
		"| label | boolean |",
		"| ans_route | text |",
		"| confidence | empty |",
		"Dropped columns: none.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dict does not hold %q:\n%s", want, got)
		}
	}
	for _, column := range table.Columns {
		if !strings.Contains(got, "| "+column+" |") {
			t.Errorf("dict does not list column %q", column)
		}
	}

	t.Run("a repeated name is categorical", func(t *testing.T) {
		t.Parallel()
		// Twenty rows sharing one label source: a twentieth of the values are
		// distinct, under mango's fifth, so the column reads categorical.
		read := scorecard.Reading{}
		for i := range 20 {
			read.Items = append(read.Items, scorecard.Item{
				ID:      fmt.Sprintf("jb-%d", i),
				Time:    time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC),
				Answers: map[scorecard.QuestionID]scorecard.Answer{"needs": {Noul: ptr(0.5)}},
			})
			read.Labelings = append(read.Labelings, scorecard.Labeling{
				Labeled: true,
				Labels:  map[scorecard.QuestionID]bool{"needs": true},
				Rule:    "fixture-labels",
				Source:  "overseer",
			})
		}
		table, err := Build(read, "needs")
		if err != nil {
			t.Fatal(err)
		}
		var dict bytes.Buffer
		if err := table.WriteDict(&dict, "fixture", "needs"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(dict.String(), "| label_source | categorical |") {
			t.Errorf("dict does not read a repeated label source as categorical:\n%s", dict.String())
		}
	})
}

// A battery and question that cannot be one filename are refused.
func TestDatasetPathsRefuseEscapes(t *testing.T) {
	t.Parallel()
	for _, q := range []scorecard.QuestionID{"../escape", "needs/action", "", ".."} {
		if _, _, err := Paths(t.TempDir(), "fixture", q); err == nil {
			t.Errorf("question %q named a path", q)
		}
	}
	if _, _, err := Paths(t.TempDir(), "../fixture", "needs"); err == nil {
		t.Error("a battery named a path")
	}
	csvPath, dictPath, err := Paths("/d", "fixture", "needs.action")
	if err != nil {
		t.Fatal(err)
	}
	if csvPath != "/d/fixture.needs.action.csv" || dictPath != "/d/fixture.needs.action.dict.md" {
		t.Errorf("paths = %q, %q", csvPath, dictPath)
	}
}
