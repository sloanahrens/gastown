package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
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

// The all-questions export writes every question the fixture battery answers,
// and a battery that is not registered is one failure that names it, while the
// fixture battery still exports.
func TestExportAllJevDatasetsContinuesPastAFailedBattery(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeScorecardFixture(t, town)
	out := filepath.Join(town, "dataset")

	written, failures := exportAllJevDatasets(context.Background(), town, out, []string{scorecard.FixtureBattery, "no-such-battery"}, time.Time{})
	if written == 0 {
		t.Fatal("no dataset was written for the fixture battery")
	}
	if _, err := os.Stat(filepath.Join(out, "fixture.needs.csv")); err != nil {
		t.Errorf("fixture.needs.csv was not written beside the failure: %v", err)
	}
	var named bool
	for _, err := range failures {
		if strings.Contains(err.Error(), "no-such-battery") {
			named = true
		}
		if strings.Contains(err.Error(), "fixture.needs") {
			t.Errorf("fixture.needs failed: %v", err)
		}
	}
	if !named {
		t.Errorf("failures %v do not name no-such-battery", failures)
	}
}

// A question with no labeled rows fails its own pair, and the questions after it
// in name order still export: route has no labels here and sorts between needs
// and urgency, which both have them.
func TestExportAllJevDatasetsGoesPastAFailedQuestion(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	labels := map[string]map[scorecard.QuestionID]bool{
		"fx-1": {"needs": true, "urgency": true},
		"fx-2": {"needs": false, "urgency": false},
		"fx-3": {"needs": false, "urgency": false},
	}
	if err := scorecard.WriteFixture(town, scorecard.FixtureLines, labels); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(town, "dataset")

	written, failures := exportAllJevDatasets(context.Background(), town, out, []string{scorecard.FixtureBattery}, time.Time{})
	if _, err := os.Stat(filepath.Join(out, "fixture.urgency.csv")); err != nil {
		t.Errorf("fixture.urgency.csv was not written after the failed route question: %v", err)
	}
	if written != 2 {
		t.Errorf("written = %d, want 2 (needs and urgency)", written)
	}
	if len(failures) != 1 || !strings.HasPrefix(failures[0].Error(), "fixture.route: ") {
		t.Errorf("failures = %v, want exactly one naming fixture.route", failures)
	}
}

// A battery whose log cannot be read is one failure that names the read, and
// the battery writes nothing: a directory where the log should be fails the
// scan, which is not the same as an empty log.
func TestExportAllJevDatasetsNamesAnUnreadableLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(scorecard.FixtureLogPath(town), 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(town, "dataset")

	written, failures := exportAllJevDatasets(context.Background(), town, out, []string{scorecard.FixtureBattery}, time.Time{})
	if written != 0 {
		t.Errorf("written = %d for an unreadable log, want 0", written)
	}
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "reading its log") {
		t.Errorf("failures = %v, want one naming the unreadable log", failures)
	}
}

// A battery whose log is missing has no questions to export, so it is named as
// a failure rather than read as a battery with nothing to write.
func TestExportAllJevDatasetsNamesAMissingLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	out := filepath.Join(town, "dataset")

	written, failures := exportAllJevDatasets(context.Background(), town, out, []string{scorecard.FixtureBattery}, time.Time{})
	if written != 0 {
		t.Errorf("written = %d for a battery with no log, want 0", written)
	}
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "log is missing") {
		t.Errorf("failures = %v, want one naming the missing log", failures)
	}
}

// An unreadable patrol config refuses the hour's refresh with one failure that
// names the config, and writes nothing: it is not read as "no batteries on".
func TestJevDatasetHourRefusesAnUnreadablePatrolConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cfgPath := daemon.PatrolConfigFile(town)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	written, failures := jevDatasetHour(context.Background(), town)
	if written != 0 {
		t.Errorf("written = %d with an unreadable patrol config, want 0", written)
	}
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "reading the patrol config") {
		t.Errorf("failures = %v, want one naming the patrol config", failures)
	}
}

// The enabled batteries are the ones whose patrol the town turned on, in
// health's table order, so the refresh covers what health reports as running.
func TestEnabledJevBatteriesFollowsThePatrolConfig(t *testing.T) {
	t.Parallel()
	if got := enabledJevBatteries(nil); len(got) != 0 {
		t.Errorf("enabledJevBatteries(nil) = %v, want none: every jev patrol is opt-in", got)
	}
	cfg := &daemon.DaemonPatrolConfig{Patrols: &daemon.PatrolsConfig{
		JevAttention: &daemon.JevAttentionConfig{Enabled: true},
		JevLanding:   &daemon.JevLandingConfig{Enabled: true},
	}}
	got := enabledJevBatteries(cfg)
	want := []string{"attention", "landing"}
	if !slices.Equal(got, want) {
		t.Errorf("enabledJevBatteries = %v, want %v", got, want)
	}
}

// --all takes the town and the whole battery set, so --battery and --question
// beside it are refused rather than one of the two silently winning.
func TestJevDatasetAllRefusesABatteryFlag(t *testing.T) {
	t.Parallel()
	d := jevDatasetDeps{findTownRoot: func() (string, error) { return t.TempDir(), nil }}
	f := jevDatasetFlags{all: true, battery: scorecard.FixtureBattery}
	err := runJevDatasetAll(context.Background(), io.Discard, d, f, time.Now())
	if err == nil || !strings.Contains(err.Error(), "drop --battery and --question") {
		t.Errorf("runJevDatasetAll with --battery = %v, want a refusal naming the flags", err)
	}
}

// --all needs a town to name, so a town that cannot be found is refused before
// any battery is read.
func TestJevDatasetAllRefusesAnUnfoundTown(t *testing.T) {
	t.Parallel()
	d := jevDatasetDeps{findTownRoot: func() (string, error) { return "", errors.New("no town here") }}
	err := runJevDatasetAll(context.Background(), io.Discard, d, jevDatasetFlags{all: true}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "finding the town root") {
		t.Errorf("runJevDatasetAll with no town = %v, want a refusal naming the town root", err)
	}
}

// --since is parsed before anything is read, so a window it cannot read is
// refused.
func TestJevDatasetAllRefusesABadWindow(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d := jevDatasetDeps{findTownRoot: func() (string, error) { return town, nil }}
	err := runJevDatasetAll(context.Background(), io.Discard, d, jevDatasetFlags{all: true, since: "soon"}, time.Now())
	if err == nil {
		t.Error("runJevDatasetAll with --since soon returned no error")
	}
}

// An unreadable patrol config refuses --all rather than exporting as though no
// battery were on.
func TestJevDatasetAllRefusesAnUnreadablePatrolConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cfgPath := daemon.PatrolConfigFile(town)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := jevDatasetDeps{findTownRoot: func() (string, error) { return town, nil }}
	err := runJevDatasetAll(context.Background(), io.Discard, d, jevDatasetFlags{all: true}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "reading the patrol config") {
		t.Errorf("runJevDatasetAll with an unreadable patrol config = %v, want a refusal naming it", err)
	}
}

// An enabled battery with no log fails the command: the receipt still prints,
// and the error names the battery and its missing log.
func TestJevDatasetAllExitsWithTheFailuresItNamed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cfg := &daemon.DaemonPatrolConfig{Patrols: &daemon.PatrolsConfig{
		JevAttention: &daemon.JevAttentionConfig{Enabled: true},
	}}
	if err := daemon.SavePatrolConfig(town, cfg); err != nil {
		t.Fatal(err)
	}
	d := jevDatasetDeps{findTownRoot: func() (string, error) { return town, nil }}
	var out bytes.Buffer
	err := runJevDatasetAll(context.Background(), &out, d, jevDatasetFlags{all: true}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "attention") || !strings.Contains(err.Error(), "log is missing") {
		t.Errorf("runJevDatasetAll = %v, want the attention battery's missing log named", err)
	}
	if !strings.Contains(out.String(), "wrote 0 dataset(s)") {
		t.Errorf("output %q does not print the receipt", out.String())
	}
}

// closedSink is a receipt sink that cannot be written to.
type closedSink struct{}

func (closedSink) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

// A receipt that cannot be written fails the command rather than being dropped.
func TestJevDatasetAllReportsAReceiptWriteFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d := jevDatasetDeps{findTownRoot: func() (string, error) { return town, nil }}
	err := runJevDatasetAll(context.Background(), closedSink{}, d, jevDatasetFlags{all: true}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "stdout closed") {
		t.Errorf("runJevDatasetAll with a failing receipt = %v, want the write error", err)
	}
}

// Without --all the command still needs the battery and the question.
func TestJevDatasetCommandRequiresBatteryAndQuestionWithoutAll(t *testing.T) {
	t.Parallel()
	cmd := newJevDatasetCmd(jevDatasetDeps{findTownRoot: func() (string, error) { return t.TempDir(), nil }})
	cmd.SetArgs([]string{"--battery", scorecard.FixtureBattery})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "required unless --all") {
		t.Errorf("Execute with only --battery = %v, want a refusal naming --all", err)
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
