package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// FixtureBattery is the fixture adapter's name. It reads only fixture files
// under the town's runtime directory, which no real battery writes, so tests
// can drive the scorecard end to end. It is test-only, so the battery listings
// and the dashboard never offer it.
const FixtureBattery = "fixture"

func init() {
	RegisterTestOnly(FixtureBattery, func(townRoot string) Adapter { return fixture{town: townRoot} })
}

// FixtureLogPath is the fixture log under townRoot.
func FixtureLogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "fixture.jsonl")
}

// FixtureLabelsPath is the fixture labels under townRoot, a JSON object from
// item id to that item's question labels. An item absent from it is unlabeled.
func FixtureLabelsPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "fixture-labels.json")
}

// FixtureLines is a sample fixture log: four items answering a noul, a choice
// and a score, and one line that does not parse.
var FixtureLines = []string{
	`{"ts":"2026-10-18T09:00:00Z","id":"fx-1","answers":{"needs":{"noul":0.9},"route":{"choice":"yes","probabilities":{"yes":0.8,"no":0.2},"positive":"yes"},"urgency":{"score":3}}}`,
	`{"ts":"2026-10-18T10:00:00Z","id":"fx-2","answers":{"needs":{"noul":0.2},"route":{"choice":"no","probabilities":{"yes":0.3,"no":0.7},"positive":"yes"},"urgency":{"score":0}}}`,
	`{"ts":"2026-10-18T11:00:00Z","id":"fx-3","answers":{"needs":{"noul":0.7},"route":{"choice":"yes","probabilities":{"yes":0.6,"no":0.4},"positive":"yes"},"urgency":{"score":1}}}`,
	`{"ts":"2026-10-18T12:00:00Z","id":"fx-4","answers":{"needs":{"noul":0.6},"route":{"choice":"no","probabilities":{"yes":0.4,"no":0.6},"positive":"yes"},"urgency":{"score":2}}}`,
	`not json`,
}

// FixtureLabels labels the first three of FixtureLines; fx-4 is logged but
// not labeled.
var FixtureLabels = map[string]map[QuestionID]bool{
	"fx-1": {"needs": true, "route": true, "urgency": true},
	"fx-2": {"needs": false, "route": false, "urgency": false},
	"fx-3": {"needs": false, "route": false, "urgency": false},
}

// WriteFixture writes a fixture log and its labels under townRoot, the layout
// the fixture adapter reads.
func WriteFixture(townRoot string, lines []string, labels map[string]map[QuestionID]bool) error {
	if err := os.MkdirAll(filepath.Dir(FixtureLogPath(townRoot)), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(FixtureLogPath(townRoot), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	data, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	return os.WriteFile(FixtureLabelsPath(townRoot), data, 0o600)
}

type fixture struct{ town string }

func (f fixture) Battery() string { return FixtureBattery }
func (f fixture) LogPath() string { return FixtureLogPath(f.town) }

// fixtureLine is one fixture log line: the Item fields, with the answers in
// Answer's own JSON shape.
type fixtureLine struct {
	TS      time.Time             `json:"ts"`
	ID      string                `json:"id"`
	Model   string                `json:"model,omitempty"`
	Answers map[QuestionID]Answer `json:"answers"`
}

func (fixture) Parse(raw []byte) (Item, error) {
	var l fixtureLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.ID == "" || l.TS.IsZero() {
		return Item{}, errors.New("fixture line needs an id and a ts")
	}
	return Item{ID: l.ID, Time: l.TS, Model: l.Model, Answers: l.Answers}, nil
}

func (f fixture) Label(_ context.Context, item Item) (Labeling, error) {
	data, err := os.ReadFile(FixtureLabelsPath(f.town))
	if errors.Is(err, fs.ErrNotExist) {
		return Labeling{}, nil
	}
	if err != nil {
		return Labeling{}, fmt.Errorf("reading the fixture labels: %w", err)
	}
	var labels map[string]map[QuestionID]bool
	if err := json.Unmarshal(data, &labels); err != nil {
		return Labeling{}, fmt.Errorf("parsing the fixture labels: %w", err)
	}
	m, ok := labels[item.ID]
	if !ok {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: m, Rule: "fixture-labels"}, nil
}
