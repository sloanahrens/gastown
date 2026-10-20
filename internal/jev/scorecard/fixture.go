package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// FixtureBattery is the fixture adapter's name. It reads only fixture files
// under the town's runtime directory, which no real battery writes, so tests
// can drive the scorecard end to end.
const FixtureBattery = "fixture"

func init() {
	Register(FixtureBattery, func(townRoot string) Adapter { return fixture{town: townRoot} })
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
