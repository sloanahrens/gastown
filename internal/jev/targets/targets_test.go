package targets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// fakeSource returns the file texts it holds and records every read.
type fakeSource struct {
	files map[string]string
	err   error
	reads []string
}

func (f *fakeSource) ReadFile(_ context.Context, path string) (string, error) {
	f.reads = append(f.reads, path)
	if f.err != nil {
		return "", f.err
	}
	text, ok := f.files[path]
	if !ok {
		return "", errors.New("no such file")
	}
	return text, nil
}

// fakeAsker answers every Ask with the same answers, or fails with errs in
// order, and records every state and question set.
type fakeAsker struct {
	answers *jev.Answers
	errs    []error
	states  []string
	asked   []map[string]jev.Question
}

func (f *fakeAsker) Ask(_ context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	f.states = append(f.states, state.(string))
	f.asked = append(f.asked, qs)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	return f.answers, nil
}

func goodAnswers() *jev.Answers {
	return &jev.Answers{
		Model: "jev-1.13.0",
		Noul: map[string]float64{
			QLikelyDefect: 0.62,
			QLogicChurn:   0.44,
			QUnderTested:  0.18,
		},
		Score: map[string]jev.ScoreAnswer{QReviewPriority: {Score: 2, Confidence: 0.5}},
	}
}

// rig is a pass over fakes in a temp dir.
type rig struct {
	pass   *Pass
	source *fakeSource
	asker  *fakeAsker
	allows func() (bool, error)
}

func newRig(t *testing.T, source *fakeSource) *rig {
	t.Helper()
	r := &rig{
		source: source,
		asker:  &fakeAsker{answers: goodAnswers()},
		allows: func() (bool, error) { return true, nil },
	}
	r.pass = &Pass{
		Asker:   r.asker,
		Source:  r.source,
		LogPath: filepath.Join(t.TempDir(), "targets.jsonl"),
		Allows:  func() (bool, error) { return r.allows() },
		Now:     func() time.Time { return t0 },
	}
	return r
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fixtureRig is a rig holding the three fixture files the golden log names.
func fixtureRig(t *testing.T) *rig {
	t.Helper()
	return newRig(t, &fakeSource{files: map[string]string{
		"internal/a/one.go":   readTestFile(t, "testdata/internal/a/one.go"),
		"internal/b/two.go":   readTestFile(t, "testdata/internal/b/two.go"),
		"internal/c/three.go": readTestFile(t, "testdata/internal/c/three.go"),
	}})
}

// The command on the fixture ranked and history files appends one line per
// slice file, in rank order, with the four answers.
func TestJevTargetsPassLogsOneLinePerSliceFile(t *testing.T) {
	t.Parallel()
	rows, hist, err := ReadSlice(filepath.Join("testdata", "ranked.txt"), filepath.Join("testdata", "history.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("parsed %d rows, want 3", len(rows))
	}
	r := fixtureRig(t)

	sum, err := r.pass.Run(context.Background(), "gt-run1", rows, hist)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 3 || sum.Asked != 3 || sum.Skipped != 0 || sum.CapReached {
		t.Errorf("summary = %+v, want all three files asked", sum)
	}
	if got, want := readTestFile(t, r.pass.LogPath), readTestFile(t, "testdata/targets.golden.jsonl"); got != want {
		t.Errorf("log:\n%s\nwant:\n%s", got, want)
	}
}

// The state holds exactly the fields the goal names, each value JSON-quoted,
// and never the file body.
func TestJevTargetsStateHoldsOnlyTheGoalFields(t *testing.T) {
	t.Parallel()
	r := fixtureRig(t)
	rows := []Row{
		{Tier: 0, LastReview: "0000-00-00", RiskScore: 12.5, Bytes: 1234, Path: "internal/a/one.go"},
		{Tier: 0, LastReview: "2026-09-01", RiskScore: 8, Bytes: 800, Path: "internal/b/two.go"},
	}
	hist := History{
		"internal/a/one.go": {"fix: one (gt-1)"},
		"internal/b/two.go": {"cq3", "cq2", "cq1"},
	}
	if _, err := r.pass.Run(context.Background(), "gt-run1", rows, hist); err != nil {
		t.Fatal(err)
	}

	first := strings.Split(strings.TrimSuffix(r.asker.states[0], "\n"), "\n")
	wantFields := []string{"path", "tier", "last_review", "risk_score", "line_count", "commit_subjects", "exported"}
	if len(first) != len(wantFields) {
		t.Fatalf("state holds %d fields, want %d:\n%s", len(first), len(wantFields), r.asker.states[0])
	}
	for i, want := range wantFields {
		name, value, ok := strings.Cut(first[i], ": ")
		if !ok || name != want {
			t.Fatalf("field %d = %q, want %q", i, first[i], want)
		}
		if !json.Valid([]byte(value)) {
			t.Errorf("field %s value %q is not JSON", name, value)
		}
	}
	if got := strings.Count(r.asker.states[0], "BODY-TEXT-THAT-MUST-NOT-LEAK"); got != 0 {
		t.Error("the state carries file body text")
	}
	lines := map[string]string{}
	for _, l := range strings.Split(r.asker.states[1], "\n") {
		if name, value, ok := strings.Cut(l, ": "); ok {
			lines[name] = value
		}
	}
	if lines["line_count"] != "4" {
		t.Errorf("line_count = %s, want 4", lines["line_count"])
	}
	if lines["commit_subjects"] != `["cq3","cq2","cq1"]` {
		t.Errorf("commit_subjects = %s, want the three subjects in order", lines["commit_subjects"])
	}
	if lines["exported"] != `["Two"]` {
		t.Errorf("exported = %s, want only Two", lines["exported"])
	}
	if _, ok := lines["body"]; ok {
		t.Error("the state names a body field")
	}
}

// One file the source cannot read is skipped with no line and does not hold up
// the files behind it.
func TestJevTargetsSkipsAnUnreadableFile(t *testing.T) {
	t.Parallel()
	src := &fakeSource{files: map[string]string{"internal/b/two.go": "package b\n"}}
	r := newRig(t, src)
	rows := []Row{
		{Path: "internal/a/one.go"},
		{Path: "internal/b/two.go"},
	}
	sum, err := r.pass.Run(context.Background(), "gt-run1", rows, History{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 2 || sum.Asked != 1 || sum.Skipped != 1 {
		t.Errorf("summary = %+v, want one asked and one skipped", sum)
	}
	if got := strings.Count(readTestFile(t, r.pass.LogPath), "\n"); got != 1 {
		t.Errorf("log holds %d lines, want 1", got)
	}
}

// An unusable Jev response skips the file; another failure stops the run, so
// the next one asks the file again.
func TestJevTargetsAskFailures(t *testing.T) {
	t.Parallel()
	rows := []Row{{Path: "internal/a/one.go"}, {Path: "internal/b/two.go"}}
	src := &fakeSource{files: map[string]string{
		"internal/a/one.go": "package a\n",
		"internal/b/two.go": "package b\n",
	}}
	r := newRig(t, src)
	r.asker.errs = []error{jev.ErrBadResponse, errors.New("network down")}
	sum, err := r.pass.Run(context.Background(), "gt-run1", rows, History{})
	if err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("err = %v, want the second ask's failure", err)
	}
	if sum.Skipped != 1 || sum.Asked != 0 {
		t.Errorf("summary = %+v, want the bad response skipped and the run stopped", sum)
	}
}

// The daily cap stops the run before another Ask, and the files it did not
// reach are named in the summary.
func TestJevTargetsCapStopsTheRun(t *testing.T) {
	t.Parallel()
	r := fixtureRig(t)
	asked := 0
	r.allows = func() (bool, error) { asked++; return asked <= 2, nil }
	rows := []Row{
		{Path: "internal/a/one.go"},
		{Path: "internal/b/two.go"},
		{Path: "internal/c/three.go"},
	}
	sum, err := r.pass.Run(context.Background(), "gt-run1", rows, History{})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.CapReached || sum.Asked != 2 || sum.Files != 3 {
		t.Errorf("summary = %+v, want two asked before the cap", sum)
	}
}

// A slice wider than MaxFiles asks about the first MaxFiles, in rank order.
func TestJevTargetsCapsTheSliceAtSixty(t *testing.T) {
	t.Parallel()
	src := &fakeSource{files: map[string]string{}}
	var rows []Row
	for i := 0; i < MaxFiles+1; i++ {
		path := fmt.Sprintf("internal/x/f%02d.go", i)
		rows = append(rows, Row{Path: path})
		src.files[path] = "package x\n"
	}
	r := newRig(t, src)
	sum, err := r.pass.Run(context.Background(), "gt-run1", rows, History{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != MaxFiles || sum.Asked != MaxFiles || len(src.reads) != MaxFiles {
		t.Errorf("summary = %+v with %d reads, want %d files", sum, len(src.reads), MaxFiles)
	}
	if src.reads[0] != "internal/x/f00.go" {
		t.Errorf("first read = %q, want the top of the rank", src.reads[0])
	}
}

// A run with no bead id is refused before it spends anything.
func TestJevTargetsNeedsARunBead(t *testing.T) {
	t.Parallel()
	r := fixtureRig(t)
	_, err := r.pass.Run(context.Background(), " ", []Row{{Path: "internal/a/one.go"}}, History{})
	if err == nil || !strings.Contains(err.Error(), "run bead") {
		t.Fatalf("err = %v, want the missing run bead named", err)
	}
	if _, statErr := os.Stat(r.pass.LogPath); !os.IsNotExist(statErr) {
		t.Errorf("a refused run wrote a log: %v", statErr)
	}
}
