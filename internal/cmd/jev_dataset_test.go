package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// The command writes the CSV and the dict over a fixture town, one row per
// labeled answer to the question and nothing for the unlabeled item or the
// line that did not parse.
func TestJevDatasetCommand(t *testing.T) {
	t.Parallel()
	if found, _, err := jevCmd.Find([]string{"dataset"}); err != nil || found.Name() != "dataset" {
		t.Fatalf("gt jev does not offer dataset: %v", err)
	}
	town := t.TempDir()
	writeScorecardFixture(t, town)
	out := filepath.Join(town, "dataset")
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	var stdout bytes.Buffer
	if err := jevDatasetWithFlags(context.Background(), &stdout, town, scorecard.FixtureBattery, "needs", "", out, now); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(out, "fixture.needs.csv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	wantHeader := "row_id,day,hour,dow,model,answer,answer_band,confidence,label,label_rule,label_source,ans_route,ans_urgency"
	if lines[0] != wantHeader {
		t.Errorf("header = %q, want %q", lines[0], wantHeader)
	}
	if len(lines) != 4 {
		t.Fatalf("file has %d lines, want a header and 3 labeled rows:\n%s", len(lines), data)
	}
	for _, line := range lines[1:] {
		cells := strings.Split(line, ",")
		if label := cells[8]; label != "true" && label != "false" {
			t.Errorf("row %q has label %q, want true or false", line, label)
		}
	}
	if !strings.Contains(stdout.String(), filepath.Join(out, "fixture.needs.csv")) {
		t.Errorf("output %q does not name the CSV it wrote", stdout.String())
	}

	dict, err := os.ReadFile(filepath.Join(out, "fixture.needs.dict.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# fixture.needs\n", "Rows: 3\n", "Date range: 2026-10-18\n"} {
		if !strings.Contains(string(dict), want) {
			t.Errorf("dict does not hold %q:\n%s", want, dict)
		}
	}
}

// Without --out the files land in the town's jev-dataset runtime directory.
func TestJevDatasetCommandDefaultDir(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)

	var stdout bytes.Buffer
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := jevDatasetWithFlags(context.Background(), &stdout, town, scorecard.FixtureBattery, "route", "", "", now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fixture.route.csv", "fixture.route.dict.md"} {
		if _, err := os.Stat(filepath.Join(town, ".runtime", "jev-dataset", name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A question with no labeled answer is an error that names the questions the
// log does answer, and nothing is written.
func TestJevDatasetCommandUnknownQuestion(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)
	out := filepath.Join(town, "dataset")
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	var stdout bytes.Buffer
	err := jevDatasetWithFlags(context.Background(), &stdout, town, scorecard.FixtureBattery, "nope", "", out, now)
	if err == nil {
		t.Fatal("a question with no rows exported cleanly")
	}
	if !strings.Contains(err.Error(), "needs, route, urgency") {
		t.Errorf("error %q does not name the questions the log answers", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, "fixture.nope.csv")); statErr == nil {
		t.Error("the refused export wrote a file")
	}
}

// A bad window, an unknown battery, an unreadable log and a refusing output
// all fail the command rather than writing a partial export.
func TestJevDatasetCommandErrors(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	t.Run("a bad window", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeScorecardFixture(t, town)
		err := jevDatasetWithFlags(context.Background(), io.Discard, town, scorecard.FixtureBattery, "needs", "nope", filepath.Join(town, "dataset"), now)
		if err == nil || !strings.Contains(err.Error(), "--since") {
			t.Errorf("err = %v, want the window refused", err)
		}
	})

	t.Run("an unknown battery", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		err := jevDatasetWithFlags(context.Background(), io.Discard, town, "no-such-battery", "needs", "", filepath.Join(town, "dataset"), now)
		if err == nil || !strings.Contains(err.Error(), "no-such-battery") {
			t.Errorf("err = %v, want the battery refused", err)
		}
	})

	t.Run("an unreadable log", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeScorecardFixture(t, town)
		if err := os.Remove(scorecard.FixtureLogPath(town)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(scorecard.FixtureLogPath(town), 0o700); err != nil {
			t.Fatal(err)
		}
		err := jevDatasetWithFlags(context.Background(), io.Discard, town, scorecard.FixtureBattery, "needs", "", filepath.Join(town, "dataset"), now)
		if err == nil || !strings.Contains(err.Error(), "reading the log") {
			t.Errorf("err = %v, want the log refused", err)
		}
	})

	t.Run("a refusing output", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeScorecardFixture(t, town)
		err := jevDatasetWithFlags(context.Background(), failingWriter{}, town, scorecard.FixtureBattery, "needs", "", filepath.Join(town, "dataset"), now)
		if err == nil {
			t.Error("a refused write reported success")
		}
	})
}

// The command needs a town to find the battery in, and reports the lookup's
// failure without writing anything.
func TestJevDatasetCommandNeedsTown(t *testing.T) {
	t.Parallel()
	cmd := newJevDatasetCmd(jevDatasetDeps{findTownRoot: func() (string, error) { return "", errors.New("no town here") }})
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--battery", "fixture", "--question", "needs"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()

	if err == nil || !strings.Contains(err.Error(), "finding the town root") {
		t.Errorf("err = %v, want the missing town refused", err)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing", out.String())
	}
}
