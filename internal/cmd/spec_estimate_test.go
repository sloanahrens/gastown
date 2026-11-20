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
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/storypoints"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
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

// The command's production wiring, not only its refusal: the town is found, the
// estimator is handed the rig the bead's ID prefix routes to (specBeadRig), and
// the labels and the note land in the store the wiring was given.
func TestSpecEstimateWiringCarriesTheBeadsRigAndWritesTheStore(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(town, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	db := specEstimateSeed()
	stub := specEstimateOK()
	var gotRig, gotTownRoot string
	var storeFor []string
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	err := runSpecEstimateWired(cmd, []string{"gt-est"},
		func() (string, error) { return town, nil },
		func(townRoot, beadID string) specEstimateStore {
			storeFor = []string{townRoot, beadID}
			return db
		},
		func(ctx context.Context, deps estimate.Deps, issue *beads.Issue) (estimate.Result, error) {
			gotRig, gotTownRoot = deps.Rig, deps.TownRoot
			return stub.run(ctx, deps, issue)
		})
	if err != nil {
		t.Fatalf("runSpecEstimateWired: %v", err)
	}
	if gotRig != "gastown" {
		t.Errorf("the estimator got rig %q, want gastown (the rig the gt- prefix routes to)", gotRig)
	}
	// The prior reader reads where the run appends: the same town root.
	if gotTownRoot != town {
		t.Errorf("the estimator got town root %q, want %q", gotTownRoot, town)
	}
	if !slices.Equal(storeFor, []string{town, "gt-est"}) {
		t.Errorf("the store was built for %v, want the town and the bead", storeFor)
	}
	labels := specEstimateLabelsOn(t, db)
	for _, want := range []string{"sp:5", "sp-rung:shape-jev", "sp-minutes:6.3"} {
		if !slices.Contains(labels, want) {
			t.Errorf("labels = %v, want %s", labels, want)
		}
	}
	if comments := specEstimateCommentsOn(t, db); len(comments) != 1 || !strings.HasPrefix(comments[0], "SP ESTIMATE v1 sp:5 rung:shape-jev") {
		t.Errorf("comments = %v, want the one estimate note", comments)
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
	if deps.Weights == nil || deps.Weights.Version() != "embedded v1" {
		t.Errorf("deps.Weights = %v, want the embedded weights a town with no runtime file gets", deps.Weights)
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

// TestSpecEstimateDepsPairTheEmbedderWithTheRuntimeWeights checks the deps the
// estimate runs with pair one model: the embedder's reply dimension and the
// weights the estimate reads come from the town's runtime file, not the
// embedded one.
func TestSpecEstimateDepsPairTheEmbedderWithTheRuntimeWeights(t *testing.T) {
	t.Parallel()
	town := writeSpecEstimateDaemon(t, `{"patrols":{"spec_dispatch":{"story_points":{"embedder_url":"http://host:11434","embedder_model":"qwen3-embedding:4b-q8_0"}}}}`)
	f := specWeightsFile(t)
	setEmbeddingDim(t, f, 3)
	writeSpecEstimateRuntimeWeights(t, town, f)

	deps, err := specEstimateDeps(town, "gastown", io.Discard)
	if err != nil {
		t.Fatalf("specEstimateDeps: %v", err)
	}
	if deps.Weights == nil || deps.Weights.Version() != "runtime v1" {
		t.Fatalf("deps.Weights = %v, want the town's runtime weights", deps.Weights)
	}
	o, ok := deps.Embedder.(storypoints.OllamaEmbedder)
	if !ok {
		t.Fatalf("embedder is %T, want storypoints.OllamaEmbedder", deps.Embedder)
	}
	if o.Dim != 3 {
		t.Errorf("embedder dimension = %d, want the runtime ridge's 3", o.Dim)
	}
}

// TestSpecEstimateDepsRefuseARuntimeModelTheEmbedderCannotPair checks a town
// whose runtime file was fitted on another embedding model refuses the run
// rather than estimating with mixed models.
func TestSpecEstimateDepsRefuseARuntimeModelTheEmbedderCannotPair(t *testing.T) {
	t.Parallel()
	town := writeSpecEstimateDaemon(t, `{"patrols":{"spec_dispatch":{"story_points":{"embedder_url":"http://host:11434","embedder_model":"qwen3-embedding:4b-q8_0"}}}}`)
	f := specWeightsFile(t)
	f["embedding"].(map[string]any)["model"] = "other-embedding:1b"
	writeSpecEstimateRuntimeWeights(t, town, f)

	if _, err := specEstimateDeps(town, "gastown", io.Discard); err == nil {
		t.Fatal("specEstimateDeps accepted a runtime file fitted on another embedding model")
	}
}

// TestSpecEstimateEmbedderFromConfig checks the embedder is built from
// story_points: a URL with a model, nothing with no URL, and a refusal for a
// URL with no model or weights it cannot pair with. The reply dimension is the
// weights' own, so a town's runtime ridge is what the embedder's replies are
// checked against.
func TestSpecEstimateEmbedderFromConfig(t *testing.T) {
	t.Parallel()
	embedded := storypoints.Default()
	small := specWeights(t, specWeightsFile(t), func(f map[string]any) {
		setEmbeddingDim(t, f, 3)
	})
	tests := []struct {
		name    string
		sd      *config.SpecDispatchConfig
		w       storypoints.Weights
		wantNil bool
		wantErr bool
		wantDim int
	}{
		{"no spec_dispatch block", nil, embedded, true, false, 0},
		{"no story_points block", &config.SpecDispatchConfig{}, embedded, true, false, 0},
		{"story_points without a URL", &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{EmbedderModel: "qwen3-embedding:4b-q8_0"}}, embedded, true, false, 0},
		{"URL with no model", &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{EmbedderURL: "http://host:11434"}}, embedded, false, true, 0},
		{"URL and model", &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{EmbedderURL: "http://host:11434", EmbedderModel: "qwen3-embedding:4b-q8_0"}}, embedded, false, false, embedded.EmbeddingDimension()},
		{"URL and model with a runtime ridge of another dimension", &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{EmbedderURL: "http://host:11434", EmbedderModel: "qwen3-embedding:4b-q8_0"}}, small, false, false, 3},
		{"URL and model with weights that have no ridge", &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{EmbedderURL: "http://host:11434", EmbedderModel: "qwen3-embedding:4b-q8_0"}}, storypoints.Weights{}, false, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			emb, err := specEstimateEmbedder(tc.sd, tc.w)
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
			if o.Dim != tc.wantDim || o.Dim == 0 {
				t.Errorf("embedder dimension = %d, want %d", o.Dim, tc.wantDim)
			}
			if o.Timeout <= 0 || o.HTTP == nil {
				t.Errorf("embedder has no timeout or client: %+v", o)
			}
		})
	}
}

// TestSpecEstimateEmbedderRefusesAnotherModel checks a runtime file fitted on
// an embedding model other than the configured one is refused, naming both: the
// embedder that would send the vectors is not the one the ridge was fitted on.
func TestSpecEstimateEmbedderRefusesAnotherModel(t *testing.T) {
	t.Parallel()
	w := specWeights(t, specWeightsFile(t), func(f map[string]any) {
		f["embedding"].(map[string]any)["model"] = "other-embedding:1b"
	})
	sd := &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{
		EmbedderURL:   "http://host:11434",
		EmbedderModel: "qwen3-embedding:4b-q8_0",
	}}
	_, err := specEstimateEmbedder(sd, w)
	if err == nil {
		t.Fatal("specEstimateEmbedder accepted weights fitted on another embedding model")
	}
	if !strings.Contains(err.Error(), "other-embedding:1b") || !strings.Contains(err.Error(), "qwen3-embedding:4b-q8_0") {
		t.Errorf("error %q does not name both embedding models", err)
	}
}

// specWeightsFile decodes the checked-in weights file, so a test can change one
// field and load the result back as a Weights value.
func specWeightsFile(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../storypoints/weights.json")
	if err != nil {
		t.Fatalf("read the weights file: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse the weights file: %v", err)
	}
	return f
}

// specWeights loads f back into a Weights value after mutate has changed it.
func specWeights(t *testing.T, f map[string]any, mutate func(map[string]any)) storypoints.Weights {
	t.Helper()
	mutate(f)
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("encode the weights: %v", err)
	}
	w, err := storypoints.LoadWeights(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("load the weights: %v", err)
	}
	return w
}

// setEmbeddingDim gives f an embedding ridge of dim coefficients, so the loaded
// weights answer EmbeddingDimension dim.
func setEmbeddingDim(t *testing.T, f map[string]any, dim int) {
	t.Helper()
	embedding, ok := f["embedding"].(map[string]any)
	if !ok {
		t.Fatal("the weights file has no embedding block")
	}
	coef := make([]any, dim)
	for i := range coef {
		coef[i] = 0.1
	}
	embedding["dim"] = dim
	embedding["coef"] = coef
}

// specEstimateLogTime is the fixed instant the estimate-log appender stamps its
// records with in a test.
func specEstimateLogTime() time.Time {
	return time.Date(2026, 11, 14, 12, 30, 0, 0, time.UTC)
}

// specEstimateWithFeatures is an estimate whose log line can be checked in every
// field: a distinct shape, embedding and component minutes.
func specEstimateWithFeatures() *specEstimateStub {
	stub := specEstimateOK()
	stub.result.Shape = storypoints.Shape{
		Boxes: 3, Paths: 4, Pkgs: 2, DLen: 7.6, NCons: 2, NLab: 5,
		PodEng: 1, Tests: 3, NBt: 13, RigGT: 1, RigDV: 0, IsHaiku: 0.5,
	}
	stub.result.Embedding = []float32{0.25, -1.5, 3}
	shape, embed, jev := 5.5, 7.25, 6.0
	stub.result.ComponentMinutes = estimate.ComponentMinutes{Shape: &shape, Embed: &embed, Jev: &jev}
	stub.result.WeightsVersion = "embedded v1"
	return stub
}

// specEstimateLogRunner is the runner a log test drives: the stub estimate and
// the production appender under town, stamped with the fixed time.
func specEstimateLogRunner(stub *specEstimateStub, town string) specEstimateRunner {
	run := specEstimateRunnerFor(stub)
	run.Log = specEstimateAppendLog(town, specEstimateLogTime)
	return run
}

// TestSpecEstimateAppendsOneLogLine: an estimate appends one estimate-log line
// carrying the hash of the text it read, the features and the blended minutes.
func TestSpecEstimateAppendsOneLogLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	db := specEstimateSeed()
	stub := specEstimateWithFeatures()

	var out bytes.Buffer
	if err := runSpecEstimateOn(&out, "gt-est", db, specEstimateOpts{}, specEstimateLogRunner(stub, town)); err != nil {
		t.Fatalf("runSpecEstimateOn: %v", err)
	}

	records, malformed, err := estlog.Read(town)
	if err != nil {
		t.Fatalf("read the estimate log: %v", err)
	}
	if malformed != 0 {
		t.Fatalf("malformed lines = %d, want none", malformed)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want the one estimate", len(records))
	}
	got := records[0]
	if got.BeadID != "gt-est" {
		t.Errorf("BeadID = %q, want gt-est", got.BeadID)
	}
	if want := specEstimateHashOf(t, db); got.DescriptionHash != want {
		t.Errorf("DescriptionHash = %q, want %q", got.DescriptionHash, want)
	}
	wantShape := estlog.Shape{
		Boxes: 3, Paths: 4, Pkgs: 2, DLen: 7.6, NCons: 2, NLab: 5,
		PodEng: 1, Tests: 3, NBt: 13, RigGT: 1, RigDV: 0, IsHaiku: 0.5,
	}
	if got.Shape != wantShape {
		t.Errorf("Shape = %+v, want %+v", got.Shape, wantShape)
	}
	if !slices.Equal(got.Embedding, stub.result.Embedding) {
		t.Errorf("Embedding = %v, want %v", got.Embedding, stub.result.Embedding)
	}
	for name, pair := range map[string]struct{ got, want *float64 }{
		"shape": {got.Minutes.Shape, stub.result.ComponentMinutes.Shape},
		"embed": {got.Minutes.Embed, stub.result.ComponentMinutes.Embed},
		"jev":   {got.Minutes.Jev, stub.result.ComponentMinutes.Jev},
	} {
		if pair.got == nil || pair.want == nil || *pair.got != *pair.want {
			t.Errorf("%s minutes = %v, want %v", name, pair.got, pair.want)
		}
	}
	if got.BlendedMinutes != stub.result.Minutes {
		t.Errorf("BlendedMinutes = %v, want %v", got.BlendedMinutes, stub.result.Minutes)
	}
	if got.WeightsVersion != "embedded v1" {
		t.Errorf("WeightsVersion = %q, want embedded v1", got.WeightsVersion)
	}
	if !got.Time.Equal(specEstimateLogTime()) {
		t.Errorf("Time = %v, want %v", got.Time, specEstimateLogTime())
	}
}

// TestSpecEstimateLogsOneLineForEachBeadAndHash: a forced second estimate of
// the same text appends nothing, so the log keeps one line per bead and hash
// however often the bead is re-estimated.
func TestSpecEstimateLogsOneLineForEachBeadAndHash(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	db := specEstimateSeed()
	stub := specEstimateOK()
	run := specEstimateLogRunner(stub, town)
	for i := 0; i < 2; i++ {
		if err := runSpecEstimateOn(io.Discard, "gt-est", db, specEstimateOpts{Force: true}, run); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	if stub.calls != 2 {
		t.Fatalf("the estimate ran %d time(s), want 2: a forced rerun must reach the log", stub.calls)
	}
	records, _, err := estlog.Read(town)
	if err != nil {
		t.Fatalf("read the estimate log: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want the one line for the bead and hash", len(records))
	}
}

// TestSpecEstimateLogFailureIsOneLineAndKeepsTheEstimate: a log that cannot be
// written is one reported line; the estimate's labels are on the bead, its
// note is left for the retry, and the run succeeds.
func TestSpecEstimateLogFailureIsOneLineAndKeepsTheEstimate(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A regular file where the log's directory goes makes every append fail.
	if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write %s: %v", filepath.Join(town, ".runtime"), err)
	}
	db := specEstimateSeed()
	stub := specEstimateOK()

	var out bytes.Buffer
	if err := runSpecEstimateOn(&out, "gt-est", db, specEstimateOpts{}, specEstimateLogRunner(stub, town)); err != nil {
		t.Fatalf("runSpecEstimateOn: %v", err)
	}
	labels := specEstimateLabelsOn(t, db)
	for _, want := range []string{"sp:5", "sp-rung:shape-jev", "sp-minutes:6.3"} {
		if !slices.Contains(labels, want) {
			t.Errorf("labels = %v, want %s", labels, want)
		}
	}
	// A note naming the hash would record the missed line as appended, so the
	// bead keeps no note until the retry writes one with the line (gt-l5buel).
	if comments := specEstimateCommentsOn(t, db); len(comments) != 0 {
		t.Fatalf("comments = %d, want none: the note waits for the log line", len(comments))
	}
	var logLines []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "story points estimate log") {
			logLines = append(logLines, line)
		}
	}
	if len(logLines) != 1 {
		t.Fatalf("estimate-log lines = %v, want exactly one", logLines)
	}
}

// TestSpecEstimateLogFailureIsRetriedOnTheNextRun: a failed append leaves the
// bead without its note, so the next estimate appends the line it missed and
// then records the hash. A note written on the failed run would read as
// unchanged forever (gt-l5buel).
func TestSpecEstimateLogFailureIsRetriedOnTheNextRun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	blocked := filepath.Join(town, ".runtime")
	// A regular file where the log's directory goes makes every append fail.
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write %s: %v", blocked, err)
	}
	db := specEstimateSeed()
	stub := specEstimateOK()
	run := specEstimateLogRunner(stub, town)

	if err := runSpecEstimateOn(io.Discard, "gt-est", db, specEstimateOpts{}, run); err != nil {
		t.Fatalf("run with the log blocked: %v", err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatalf("unblock the log: %v", err)
	}
	if err := runSpecEstimateOn(io.Discard, "gt-est", db, specEstimateOpts{}, run); err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("the estimate ran %d time(s), want 2: the retry estimates the bead again", stub.calls)
	}
	records, _, err := estlog.Read(town)
	if err != nil {
		t.Fatalf("read the estimate log: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want the one line the retry appended", len(records))
	}
	if comments := specEstimateCommentsOn(t, db); len(comments) != 1 {
		t.Fatalf("comments after the retry = %d, want the one estimate note", len(comments))
	}
}

// TestSpecEstimateChangedTextAppendsASecondLine: a bead whose sections changed
// since its note is estimated again and appends a second line, so the log keeps
// the features of the text the second estimate read.
func TestSpecEstimateChangedTextAppendsASecondLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	db := specEstimateSeed()
	stub := specEstimateWithFeatures()
	run := specEstimateLogRunner(stub, town)
	if err := runSpecEstimateOn(io.Discard, "gt-est", db, specEstimateOpts{}, run); err != nil {
		t.Fatalf("first run: %v", err)
	}
	changed := strings.Replace(specEstimateBody, "One worker.", "Two workers.", 1)
	if err := db.Update("gt-est", beads.UpdateOptions{Description: &changed}); err != nil {
		t.Fatalf("change the bead's size section: %v", err)
	}
	if err := runSpecEstimateOn(io.Discard, "gt-est", db, specEstimateOpts{}, run); err != nil {
		t.Fatalf("run after the change: %v", err)
	}
	records, _, err := estlog.Read(town)
	if err != nil {
		t.Fatalf("read the estimate log: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want one line per hash", len(records))
	}
	if records[0].DescriptionHash == records[1].DescriptionHash {
		t.Fatalf("both lines hash %q, want the changed text's own hash", records[0].DescriptionHash)
	}
	if want := specEstimateHashOf(t, db); records[1].DescriptionHash != want {
		t.Errorf("the second line's hash = %q, want the changed text's %q", records[1].DescriptionHash, want)
	}
}

// specEstimatePriorStub answers from deps.Prior when the run wired one, as
// estimate.Estimate does, summing the logged component minutes, the seed's 6.3.
type specEstimatePriorStub struct {
	calls      int
	priorWired bool
}

func (s *specEstimatePriorStub) run(_ context.Context, deps estimate.Deps, issue *beads.Issue) (estimate.Result, error) {
	s.calls++
	s.priorWired = deps.Prior != nil
	r := estimate.Result{Points: 8, Minutes: 20, Inputs: []string{"shape"}, Rung: "shape"}
	if s.priorWired {
		if prior, ok := deps.Prior(issue.ID, estimate.SectionHash(issue)); ok {
			r = estimate.Result{Points: 5, Minutes: prior.Embed + prior.Jev, Inputs: []string{"shape", "embed", "jev"}, Rung: "all"}
		}
	}
	r.Hash = estimate.SectionHash(issue)
	return r, nil
}

// TestSpecEstimateForceReusesLoggedMinutes: a forced re-estimate of text whose
// latest note already names its hash hands the estimator the logged embed and
// Jev minutes, so it returns the minutes the earlier estimate logged.
func TestSpecEstimateForceReusesLoggedMinutes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	db := specEstimateSeed()
	hash := specEstimateHashOf(t, db)
	if err := db.AddComment("gt-est", "SP ESTIMATE v1 sp:5 rung:all hash:"+hash); err != nil {
		t.Fatal(err)
	}
	embed, jev := 4.0, 2.3
	if err := estlog.Append(town, estlog.Record{BeadID: "gt-est", DescriptionHash: hash, Minutes: estlog.Minutes{Embed: &embed, Jev: &jev}, BlendedMinutes: 6.3}); err != nil {
		t.Fatalf("append the logged estimate: %v", err)
	}
	stub := &specEstimatePriorStub{}
	run := specEstimateRunner{Estimate: stub.run, Deps: func() (estimate.Deps, error) { return estimate.Deps{TownRoot: town}, nil }}
	var out bytes.Buffer
	if err := runSpecEstimateOn(&out, "gt-est", db, specEstimateOpts{Force: true}, run); err != nil {
		t.Fatalf("runSpecEstimateOn: %v", err)
	}
	if stub.calls != 1 || !stub.priorWired {
		t.Fatalf("estimate calls = %d, prior wired = %v: a forced re-estimate must reuse the log", stub.calls, stub.priorWired)
	}
	if got := specEstimateLabelsOn(t, db); !strings.Contains(out.String(), "gt-est: sp:5 rung:all minutes:6.3") || !slices.Contains(got, "sp-minutes:6.3") {
		t.Errorf("output = %q, labels = %v, want the logged estimate", out.String(), got)
	}
}

// TestSpecEstimateNewTextReadsNoLog: a bead whose latest note names another
// hash, or that has no note, is estimated afresh and its deps carry no Prior.
func TestSpecEstimateNewTextReadsNoLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	embed, jev := 4.0, 2.3
	if err := estlog.Append(town, estlog.Record{BeadID: "gt-est", DescriptionHash: "deadbeef", Minutes: estlog.Minutes{Embed: &embed, Jev: &jev}}); err != nil {
		t.Fatalf("append the logged estimate: %v", err)
	}
	for name, note := range map[string]string{"no note": "", "another hash": "SP ESTIMATE v1 sp:5 rung:all hash:deadbeef"} {
		t.Run(name, func(t *testing.T) {
			db := specEstimateSeed()
			if note != "" {
				if err := db.AddComment("gt-est", note); err != nil {
					t.Fatal(err)
				}
			}
			stub := &specEstimatePriorStub{}
			run := specEstimateRunner{Estimate: stub.run, Deps: func() (estimate.Deps, error) { return estimate.Deps{TownRoot: town}, nil }}
			var out bytes.Buffer
			if err := runSpecEstimateOn(&out, "gt-est", db, specEstimateOpts{}, run); err != nil {
				t.Fatalf("runSpecEstimateOn: %v", err)
			}
			if stub.calls != 1 || stub.priorWired {
				t.Fatalf("estimate calls = %d, prior wired = %v: new text is estimated afresh with no Prior", stub.calls, stub.priorWired)
			}
			if !strings.Contains(out.String(), "gt-est: sp:8 rung:shape minutes:20") {
				t.Errorf("output = %q, want the fresh estimate", out.String())
			}
		})
	}
}

// writeSpecEstimateRuntimeWeights writes f as town's runtime weights file.
func writeSpecEstimateRuntimeWeights(t *testing.T, town string, f map[string]any) {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("encode the runtime weights: %v", err)
	}
	dir := filepath.Join(town, ".runtime", "storypoints")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "weights.json"), data, 0o644); err != nil {
		t.Fatalf("write the runtime weights: %v", err)
	}
}
