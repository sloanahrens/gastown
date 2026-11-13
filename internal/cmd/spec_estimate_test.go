package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/storypoints"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
)

// specEstimateBody is a bead description with the sections the hash covers.
const specEstimateBody = "## Goal\nMake the thing.\n\n## Constraints\n- one file\n\n## Acceptance\n- [ ] it works\n\n## Size\nOne worker.\n"

// specEstimateStub is the estimate a run is given: it counts its calls and
// answers with the result it holds, with the hash of the bead it was asked
// about, as estimate.Estimate does.
type specEstimateStub struct {
	calls  int
	result estimate.Result
	err    error
}

func (s *specEstimateStub) run(_ context.Context, _ estimate.Deps, issue *beads.Issue) (estimate.Result, error) {
	s.calls++
	r := s.result
	r.Hash = estimate.SectionHash(issue)
	return r, s.err
}

func specEstimateRunnerFor(s *specEstimateStub) specEstimateRunner {
	return specEstimateRunner{
		Estimate: s.run,
		Deps:     func() (estimate.Deps, error) { return estimate.Deps{}, nil },
	}
}

// specEstimateOK is a result the blend could make: five points from shape and Jev.
func specEstimateOK() *specEstimateStub {
	return &specEstimateStub{result: estimate.Result{
		Points:  5,
		Minutes: 6.33,
		Inputs:  []string{"shape", "jev"},
		Rung:    "shape-jev",
	}}
}

// specEstimateSeed is a store holding one bead with the given labels.
func specEstimateSeed(labels ...string) *beadsfake.Fake {
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-est", Title: "estimate me", Status: "open", Description: specEstimateBody, Labels: labels})
	return db
}

func specEstimateLabelsOn(t *testing.T, db beads.Client) []string {
	t.Helper()
	issue, err := db.Show("gt-est")
	if err != nil {
		t.Fatal(err)
	}
	return issue.Labels
}

func specEstimateCommentsOn(t *testing.T, db beads.Client) []string {
	t.Helper()
	cs, err := db.Comments("gt-est")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range cs {
		texts = append(texts, c.Text)
	}
	return texts
}

// specEstimateHashOf is the hash of the seeded bead, read the way the command reads it.
func specEstimateHashOf(t *testing.T, db beads.Client) string {
	t.Helper()
	issue, err := db.Show("gt-est")
	if err != nil {
		t.Fatal(err)
	}
	return estimate.SectionHash(issue)
}

// specEstimateFailUpdate is a store whose label write is refused.
type specEstimateFailUpdate struct{ *beadsfake.Fake }

func (specEstimateFailUpdate) Update(string, beads.UpdateOptions) error {
	return errors.New("bd update refused")
}

// specEstimateFailComments is a store whose comment read is refused.
type specEstimateFailComments struct{ *beadsfake.Fake }

func (specEstimateFailComments) Comments(string) ([]beads.Comment, error) {
	return nil, errors.New("bd comments refused")
}

// specEstimateFailComment is a store whose comment write is refused.
type specEstimateFailComment struct{ *beadsfake.Fake }

func (specEstimateFailComment) AddComment(string, string) error {
	return errors.New("bd comment refused")
}

func TestSpecEstimateWritesLabelsAndNote(t *testing.T) {
	t.Parallel()
	db := specEstimateSeed()
	stub := specEstimateOK()
	var out bytes.Buffer
	if err := runSpecEstimateOn(&out, "gt-est", db, specEstimateOpts{}, specEstimateRunnerFor(stub)); err != nil {
		t.Fatalf("runSpecEstimateOn: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("estimate ran %d times, want 1", stub.calls)
	}
	labels := specEstimateLabelsOn(t, db)
	for _, want := range []string{"sp:5", "sp-rung:shape-jev", "sp-minutes:6.3"} {
		if !slices.Contains(labels, want) {
			t.Errorf("labels = %v, want %s", labels, want)
		}
	}
	comments := specEstimateCommentsOn(t, db)
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want the one estimate note", len(comments))
	}
	wantFirst := "SP ESTIMATE v1 sp:5 rung:shape-jev hash:" + specEstimateHashOf(t, db)
	if first, _, _ := strings.Cut(comments[0], "\n"); first != wantFirst {
		t.Errorf("note first line = %q, want %q", first, wantFirst)
	}
	if !strings.Contains(out.String(), "gt-est: sp:5 rung:shape-jev minutes:6.3") {
		t.Errorf("output = %q, want the estimate on one line", out.String())
	}
}

// A bead whose latest estimate note carries its current hash is left alone,
// and so is a bead whose older note matches but whose latest one does not
// (only the latest note counts). A note that names a different hash is
// estimated again.
func TestSpecEstimateSkipsUnchangedBead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		notes     func(hash string) []string
		wantCalls int
	}{
		{"latest note matches", func(h string) []string {
			return []string{"SP ESTIMATE v1 sp:5 rung:shape-jev hash:" + h + "\nminutes: 6.3"}
		}, 0},
		{"a non-estimate comment after the note is ignored", func(h string) []string {
			return []string{"SP ESTIMATE v1 sp:5 rung:shape-jev hash:" + h, "operator: looking at this"}
		}, 0},
		{"latest note names another hash", func(h string) []string {
			return []string{"SP ESTIMATE v1 sp:5 rung:shape-jev hash:" + h, "SP ESTIMATE v1 sp:3 rung:all hash:deadbeef"}
		}, 1},
		{"older note matches but latest does not", func(h string) []string {
			return []string{"SP ESTIMATE v1 sp:5 rung:shape-jev hash:" + h, "SP ESTIMATE v1 sp:3 rung:all hash:deadbeef"}
		}, 1},
		{"note has no hash", func(h string) []string {
			return []string{"SP ESTIMATE v1 sp:5 rung:shape-jev"}
		}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := specEstimateSeed("sp:5", "sp-rung:shape-jev", "sp-minutes:6.3")
			hash := specEstimateHashOf(t, db)
			for _, note := range tc.notes(hash) {
				if err := db.AddComment("gt-est", note); err != nil {
					t.Fatal(err)
				}
			}
			before := specEstimateCommentsOn(t, db)
			stub := specEstimateOK()
			var out bytes.Buffer
			if err := runSpecEstimateOn(&out, "gt-est", db, specEstimateOpts{}, specEstimateRunnerFor(stub)); err != nil {
				t.Fatalf("runSpecEstimateOn: %v", err)
			}
			if stub.calls != tc.wantCalls {
				t.Fatalf("estimate ran %d times, want %d", stub.calls, tc.wantCalls)
			}
			if tc.wantCalls == 0 {
				if !strings.Contains(out.String(), "unchanged") {
					t.Errorf("output = %q, want it to say the bead is unchanged", out.String())
				}
				if got := specEstimateCommentsOn(t, db); len(got) != len(before) {
					t.Errorf("a skipped bead gained comments: %d -> %d", len(before), len(got))
				}
			}
		})
	}
}

// --force estimates an unchanged bead and replaces its labels, including the
// sp-minutes: label the old estimate left.
func TestSpecEstimateForceReplacesOldLabels(t *testing.T) {
	t.Parallel()
	db := specEstimateSeed("sp:3", "sp-rung:shape", "sp-minutes:2.5", "gt:task")
	if err := db.AddComment("gt-est", "SP ESTIMATE v1 sp:3 rung:shape hash:"+specEstimateHashOf(t, db)); err != nil {
		t.Fatal(err)
	}
	stub := specEstimateOK()
	if err := runSpecEstimateOn(&bytes.Buffer{}, "gt-est", db, specEstimateOpts{Force: true}, specEstimateRunnerFor(stub)); err != nil {
		t.Fatalf("runSpecEstimateOn: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("--force estimated %d times, want 1", stub.calls)
	}
	labels := specEstimateLabelsOn(t, db)
	for _, want := range []string{"sp:5", "sp-rung:shape-jev", "sp-minutes:6.3", "gt:task"} {
		if !slices.Contains(labels, want) {
			t.Errorf("labels = %v, want %s", labels, want)
		}
	}
	for _, gone := range []string{"sp:3", "sp-rung:shape", "sp-minutes:2.5"} {
		if slices.Contains(labels, gone) {
			t.Errorf("labels = %v, the old %s survived --force", labels, gone)
		}
	}
	if got := specEstimateCommentsOn(t, db); len(got) != 2 {
		t.Errorf("comments = %d, want the old note and the new one", len(got))
	}
}

// specEstimateRecordUpdate records the label write the command makes.
type specEstimateRecordUpdate struct {
	*beadsfake.Fake
	removed []string
}

func (s *specEstimateRecordUpdate) Update(id string, opts beads.UpdateOptions) error {
	s.removed = append(s.removed, opts.RemoveLabels...)
	return s.Fake.Update(id, opts)
}

// A label the rewrite sets again is not in the remove list: the bead keeps it,
// and the write never names it for removal.
func TestSpecEstimateForceDoesNotRemoveLabelsItSetsAgain(t *testing.T) {
	t.Parallel()
	db := specEstimateSeed("sp:5", "sp-rung:shape-jev", "sp-minutes:6.3", "gt:task")
	store := &specEstimateRecordUpdate{Fake: db}
	if err := runSpecEstimateOn(&bytes.Buffer{}, "gt-est", store, specEstimateOpts{Force: true}, specEstimateRunnerFor(specEstimateOK())); err != nil {
		t.Fatalf("runSpecEstimateOn: %v", err)
	}
	for _, l := range []string{"sp:5", "sp-rung:shape-jev", "sp-minutes:6.3"} {
		if slices.Contains(store.removed, l) {
			t.Errorf("remove list = %v names %s, which the rewrite sets again", store.removed, l)
		}
	}
	if got := specEstimateLabelsOn(t, db); !slices.Contains(got, "sp-minutes:6.3") || !slices.Contains(got, "gt:task") {
		t.Errorf("labels = %v, want the new estimate and the bead's own label", got)
	}
}

func TestSpecEstimateDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		stub     *specEstimateStub
		wantText string
	}{
		{"an estimate", specEstimateOK(), "dry run: nothing written"},
		{"an estimate that fails", &specEstimateStub{err: errors.New("no text")}, "sp-rung:none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := specEstimateSeed("gt:task")
			var out bytes.Buffer
			if err := runSpecEstimateOn(&out, "gt-est", db, specEstimateOpts{DryRun: true}, specEstimateRunnerFor(tc.stub)); err != nil {
				t.Fatalf("runSpecEstimateOn: %v", err)
			}
			if got := specEstimateLabelsOn(t, db); !slices.Equal(got, []string{"gt:task"}) {
				t.Errorf("labels changed on a dry run: %v", got)
			}
			if got := specEstimateCommentsOn(t, db); len(got) != 0 {
				t.Errorf("a dry run wrote %d comments", len(got))
			}
			if !strings.Contains(out.String(), tc.wantText) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tc.wantText)
			}
		})
	}
}

// An estimate error writes sp-rung:none and no sp: or sp-minutes: label, drops
// the old ones, and says why in a note that carries the hash, so the bead is
// not estimated again until its sections change.
func TestSpecEstimateErrorWritesNoneRung(t *testing.T) {
	t.Parallel()
	db := specEstimateSeed("sp:3", "sp-rung:shape", "sp-minutes:2.5")
	stub := &specEstimateStub{err: errors.New("storypoints/estimate: bead has no text to estimate")}
	var out bytes.Buffer
	if err := runSpecEstimateOn(&out, "gt-est", db, specEstimateOpts{}, specEstimateRunnerFor(stub)); err != nil {
		t.Fatalf("an estimate error must exit 0, got %v", err)
	}
	labels := specEstimateLabelsOn(t, db)
	if !slices.Contains(labels, "sp-rung:none") {
		t.Errorf("labels = %v, want sp-rung:none", labels)
	}
	for _, l := range labels {
		if strings.HasPrefix(l, "sp:") || strings.HasPrefix(l, "sp-minutes:") || (strings.HasPrefix(l, "sp-rung:") && l != "sp-rung:none") {
			t.Errorf("labels = %v, %s should not be there after an error", labels, l)
		}
	}
	comments := specEstimateCommentsOn(t, db)
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want the reason note", len(comments))
	}
	first, _, _ := strings.Cut(comments[0], "\n")
	if want := "SP ESTIMATE v1 sp:none rung:none hash:" + specEstimateHashOf(t, db); first != want {
		t.Errorf("note first line = %q, want %q", first, want)
	}
	if !strings.Contains(comments[0], "no estimate: storypoints/estimate: bead has no text to estimate") {
		t.Errorf("note = %q, want the reason", comments[0])
	}

	// The second run sees the hash in the none note and does not estimate again.
	if err := runSpecEstimateOn(&bytes.Buffer{}, "gt-est", db, specEstimateOpts{}, specEstimateRunnerFor(stub)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("the none note did not stop a rerun: estimate ran %d times", stub.calls)
	}
}

// A result no label can carry is the none outcome: points off the scale, and
// minutes that round to zero, to infinity or to NaN. Each would write a label
// ParseLabel refuses.
func TestSpecEstimateUnlabelableResultIsNone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		points  int
		minutes float64
	}{
		{"points off the scale", 4, 6.3},
		{"minutes round to zero", 1, 0.01},
		{"minutes zero", 1, 0},
		{"minutes NaN", 3, math.NaN()},
		{"minutes infinite", 89, math.Inf(1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := specEstimateSeed()
			stub := &specEstimateStub{result: estimate.Result{Points: tc.points, Minutes: tc.minutes, Rung: "shape"}}
			if err := runSpecEstimateOn(&bytes.Buffer{}, "gt-est", db, specEstimateOpts{}, specEstimateRunnerFor(stub)); err != nil {
				t.Fatalf("runSpecEstimateOn: %v", err)
			}
			labels := specEstimateLabelsOn(t, db)
			if !slices.Contains(labels, "sp-rung:none") {
				t.Errorf("labels = %v, want sp-rung:none", labels)
			}
			for _, l := range labels {
				if strings.HasPrefix(l, "sp:") || strings.HasPrefix(l, "sp-minutes:") {
					t.Errorf("labels = %v, %s written for an unlabelable result", labels, l)
				}
			}
		})
	}
}

// A store failure is an error, and the write after it does not happen. A label
// write that fails writes no note; a note that fails leaves the labels set, and
// a rerun writes the same labels and the note again.
func TestSpecEstimateStoreFailuresAreErrors(t *testing.T) {
	t.Parallel()
	t.Run("the bead does not read", func(t *testing.T) {
		db := specEstimateSeed()
		db.FailWith("show", errors.New("bd show refused"))
		stub := specEstimateOK()
		if err := runSpecEstimateOn(&bytes.Buffer{}, "gt-est", db, specEstimateOpts{}, specEstimateRunnerFor(stub)); err == nil {
			t.Fatal("a bead that does not read returned nil")
		}
		if stub.calls != 0 {
			t.Errorf("estimate ran %d times for a bead it could not read", stub.calls)
		}
	})
	t.Run("the label write fails", func(t *testing.T) {
		db := specEstimateSeed()
		store := specEstimateFailUpdate{db}
		if err := runSpecEstimateOn(&bytes.Buffer{}, "gt-est", store, specEstimateOpts{}, specEstimateRunnerFor(specEstimateOK())); err == nil {
			t.Fatal("a refused label write returned nil")
		}
		if got := specEstimateCommentsOn(t, db); len(got) != 0 {
			t.Errorf("a refused label write still wrote %d comments", len(got))
		}
	})
	t.Run("the note write fails", func(t *testing.T) {
		db := specEstimateSeed()
		store := specEstimateFailComment{db}
		if err := runSpecEstimateOn(&bytes.Buffer{}, "gt-est", store, specEstimateOpts{}, specEstimateRunnerFor(specEstimateOK())); err == nil {
			t.Fatal("a refused note write returned nil")
		}
		if got := specEstimateLabelsOn(t, db); !slices.Contains(got, "sp:5") {
			t.Errorf("labels = %v, want the labels that did land", got)
		}
	})
	t.Run("the comments do not read", func(t *testing.T) {
		db := specEstimateSeed()
		stub := specEstimateOK()
		if err := runSpecEstimateOn(&bytes.Buffer{}, "gt-est", specEstimateFailComments{db}, specEstimateOpts{}, specEstimateRunnerFor(stub)); err == nil {
			t.Fatal("a bead whose comments do not read returned nil")
		}
		if stub.calls != 0 {
			t.Errorf("estimate ran %d times without reading the last hash", stub.calls)
		}
		if got := specEstimateLabelsOn(t, db); len(got) != 0 {
			t.Errorf("labels = %v written after a failed read", got)
		}
	})
}

// A patrol config that will not load refuses the run before any estimate or
// write. The estimate is not asked to run on inputs it could not read.
func TestSpecEstimateDepsFailureRefusesTheRun(t *testing.T) {
	t.Parallel()
	db := specEstimateSeed()
	stub := specEstimateOK()
	run := specEstimateRunner{
		Estimate: stub.run,
		Deps:     func() (estimate.Deps, error) { return estimate.Deps{}, errors.New("settings do not parse") },
	}
	if err := runSpecEstimateOn(&bytes.Buffer{}, "gt-est", db, specEstimateOpts{}, run); err == nil {
		t.Fatal("a settings read failure returned nil")
	}
	if stub.calls != 0 {
		t.Errorf("estimate ran %d times with settings it could not read", stub.calls)
	}
	if got := specEstimateCommentsOn(t, db); len(got) != 0 {
		t.Errorf("a refused run wrote %d comments", len(got))
	}
}

func TestSpecEstimateJSONReportsTheOutcome(t *testing.T) {
	t.Parallel()
	db := specEstimateSeed()
	var out bytes.Buffer
	if err := runSpecEstimateOn(&out, "gt-est", db, specEstimateOpts{JSON: true}, specEstimateRunnerFor(specEstimateOK())); err != nil {
		t.Fatalf("runSpecEstimateOn: %v", err)
	}
	var got specEstimateOutcome
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out.String())
	}
	if got.Action != "estimated" || got.Points != 5 || got.Rung != "shape-jev" || got.Minutes != 6.3 {
		t.Errorf("outcome = %+v", got)
	}
	if !slices.Contains(got.AddLabels, "sp:5") || !slices.Contains(got.AddLabels, "sp-minutes:6.3") {
		t.Errorf("add_labels = %v", got.AddLabels)
	}
}

// The command refuses to run outside a town, before it reads any bead.
func TestSpecEstimateRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	err := runSpecEstimateFrom(&cobra.Command{}, []string{"gt-est"}, func() (string, error) {
		return "", errors.New("no workspace")
	})
	if err == nil {
		t.Fatal("a run with no town returned nil")
	}
}

// writeSpecEstimateDaemon makes a temp town whose patrol config is body:
// mayor/daemon.json, the file daemon.ReadPatrolConfig loads.
func writeSpecEstimateDaemon(t *testing.T, body string) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "daemon.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// The estimate's inputs come from the town's settings. Settings that do not
// parse refuse the run, and so does an embedder URL with no model. A town with
// no settings gets no embedder, and its Jev asker reports that the battery is
// not configured, so the note names the skip.
func TestSpecEstimateDepsReadTheTownSettings(t *testing.T) {
	t.Parallel()
	refused := []struct {
		name string
		body string
	}{
		{"settings do not parse", `{not json`},
		{"embedder URL with no model", `{"patrols":{"spec_dispatch":{"story_points":{"embedder_url":"http://host:11434"}}}}`},
	}
	for _, tc := range refused {
		town := writeSpecEstimateDaemon(t, tc.body)
		if _, err := specEstimateDeps(town, "gastown", io.Discard); err == nil {
			t.Errorf("%s: specEstimateDeps returned nil for settings it cannot use", tc.name)
		}
	}

	deps, err := specEstimateDeps(t.TempDir(), "gastown", io.Discard)
	if err != nil {
		t.Fatalf("specEstimateDeps with no settings: %v", err)
	}
	if deps.Embedder != nil {
		t.Errorf("embedder = %T, want none without an embedder URL", deps.Embedder)
	}
	if deps.HMRunner == nil || deps.JevAsk == nil || deps.Rig != "gastown" {
		t.Errorf("deps = %+v, want hm, a Jev asker and the rig", deps)
	}
	if _, err := deps.JevAsk(context.Background(), &beads.Issue{ID: "gt-est"}); err == nil {
		t.Error("the Jev asker answered with no jev_dispatch settings, want its reason as an error")
	}

	town := writeSpecEstimateDaemon(t, `{not json`)
	if _, err := specEstimateJev(town, io.Discard)(context.Background(), &beads.Issue{ID: "gt-est"}); err == nil {
		t.Error("the Jev asker answered from settings it could not read")
	}
}

// The embedder is built from story_points: a URL with a model, nothing with no
// URL, and a refusal for a URL with no model.
func TestSpecEstimateEmbedderFromConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sd      *config.SpecDispatchConfig
		wantNil bool
		wantErr bool
	}{
		{"no spec_dispatch block", nil, true, false},
		{"no story_points block", &config.SpecDispatchConfig{}, true, false},
		{"story_points without a URL", &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{EmbedderModel: "qwen3-embedding:4b-q8_0"}}, true, false},
		{"URL with no model", &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{EmbedderURL: "http://host:11434"}}, false, true},
		{"URL and model", &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{EmbedderURL: "http://host:11434", EmbedderModel: "qwen3-embedding:4b-q8_0"}}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			emb, err := specEstimateEmbedder(tc.sd)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if (emb == nil) != tc.wantNil {
				t.Fatalf("embedder nil = %v, want %v", emb == nil, tc.wantNil)
			}
			if emb == nil {
				return
			}
			o, ok := emb.(storypoints.OllamaEmbedder)
			if !ok {
				t.Fatalf("embedder is %T, want storypoints.OllamaEmbedder", emb)
			}
			if o.URL != "http://host:11434" || o.Model != "qwen3-embedding:4b-q8_0" {
				t.Errorf("embedder = %+v, want the configured URL and model", o)
			}
			if o.Dim != storypoints.Default().EmbeddingDimension() || o.Dim == 0 {
				t.Errorf("embedder dimension = %d, want the weights' dimension", o.Dim)
			}
			if o.Timeout <= 0 || o.HTTP == nil {
				t.Errorf("embedder has no timeout or client: %+v", o)
			}
		})
	}
}
