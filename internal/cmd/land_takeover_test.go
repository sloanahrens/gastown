package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	takeoverBeadID = "gt-tk4w1"
	takeoverBranch = "polecat/mica/gt-tk4w1+abc"
	takeoverTip    = "2222222222222222222222222222222222222222"
	takeoverHuman  = "operator-human"
	takeoverCaller = "town/crew/overseer"
	takeoverReason = "escalated by hand before the label existed"
)

// fakeTakeoverRemote answers ListRemoteRefsWithHashes with one branch tip, or
// none when tip is empty, and records what it was asked.
type fakeTakeoverRemote struct {
	tip       string
	err       error
	gotRemote string
	gotRef    string
}

func (f *fakeTakeoverRemote) ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error) {
	f.gotRemote, f.gotRef = remote, prefix
	if f.err != nil {
		return nil, f.err
	}
	if f.tip == "" {
		return nil, nil
	}
	return []git.RemoteRef{{Hash: f.tip, Name: prefix}}, nil
}

// recordingTakeoverBeads records every Update the takeover makes, in order.
// failOn fails the nth Update (1-based) without writing it; dropLabels makes
// a label write report success without persisting it; omitNotesOn makes the
// nth Update report success without persisting its note.
type recordingTakeoverBeads struct {
	*beadsfake.Fake
	updates     []beads.UpdateOptions
	failOn      int
	dropLabels  bool
	omitNotesOn int
}

func (r *recordingTakeoverBeads) Update(id string, opts beads.UpdateOptions) error {
	r.updates = append(r.updates, opts)
	if len(r.updates) == r.failOn {
		return errors.New("bd update refused")
	}
	if r.dropLabels {
		opts.AddLabels, opts.RemoveLabels = nil, nil
	}
	if len(r.updates) == r.omitNotesOn {
		opts.AppendNotes = ""
	}
	return r.Fake.Update(id, opts)
}

func newTakeoverBeads(status beads.IssueStatus, labels []string) *recordingTakeoverBeads {
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	bd.Seed(beads.Issue{
		ID: takeoverBeadID, Title: "the work", Type: "task", Status: string(status),
		Labels: labels,
	})
	return &recordingTakeoverBeads{Fake: bd}
}

func takeoverRequestFor(overrides func(*takeoverRequest)) takeoverRequest {
	req := takeoverRequest{
		BeadID: takeoverBeadID, Branch: takeoverBranch, Target: "main",
		Actor: takeoverCaller, Human: takeoverHuman,
	}
	if overrides != nil {
		overrides(&req)
	}
	return req
}

func TestLandTakeoverWritesTheReadyBlockTheWaiverAndTheLabels(t *testing.T) {
	t.Parallel()
	bd := newTakeoverBeads(beads.StatusOpen, []string{land.LabelNeedsOverseer, land.LabelRework})
	remote := &fakeTakeoverRemote{tip: takeoverTip}
	var out bytes.Buffer

	if err := takeOverLanding(bd, remote, "origin", takeoverRequestFor(nil), &out); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	if len(bd.updates) != 3 {
		t.Fatalf("wrote %d updates, want 3 (the READY block, the waiver, the labels): %+v", len(bd.updates), bd.updates)
	}
	ready, ok := land.ParseReadyNote(bd.updates[0].AppendNotes)
	if !ok {
		t.Fatalf("first write %q is not a READY TO LAND block the landing worker reads", bd.updates[0].AppendNotes)
	}
	if ready.Branch != takeoverBranch || ready.Head != takeoverTip || ready.Target != "main" || ready.Worker != takeoverCaller {
		t.Errorf("READY block reads branch=%q head=%q target=%q worker=%q, want %q %q main %q",
			ready.Branch, ready.Head, ready.Target, ready.Worker, takeoverBranch, takeoverTip, takeoverCaller)
	}
	if ready.Submitted.IsZero() {
		t.Error("READY block carries no submission time; the landing worker orders the queue by it")
	}
	if got := strings.TrimSpace(bd.updates[1].AppendNotes); got != land.OverseerReviewedMarker+" "+takeoverTip {
		t.Errorf("second write %q is not the OVERSEER REVIEWED note for the origin tip", got)
	}
	labels := bd.updates[2]
	for _, want := range []string{land.LabelOverseerReviewed, land.LabelReadyToLand} {
		if !containsString(labels.AddLabels, want) {
			t.Errorf("label write adds %v, want %s", labels.AddLabels, want)
		}
	}
	for _, want := range []string{land.LabelNeedsOverseer, land.LabelRework} {
		if !containsString(labels.RemoveLabels, want) {
			t.Errorf("label write removes %v, want %s", labels.RemoveLabels, want)
		}
	}

	after, err := bd.Show(takeoverBeadID)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !land.OverseerReviewed(after, takeoverTip) {
		t.Errorf("the bead does not carry the waiver for %s after the takeover", takeoverTip)
	}
	if _, err := land.WorkFromBead(after, "gastown"); err != nil {
		t.Errorf("the landing worker cannot read the takeover's request: %v", err)
	}
	if land.HasNeedsPerson(after) || beads.HasLabel(after, land.LabelRework) {
		t.Errorf("labels %v still hold the bead for a person or for rework", after.Labels)
	}

	if remote.gotRemote != "origin" || remote.gotRef != "refs/heads/"+takeoverBranch {
		t.Errorf("read remote=%q ref=%q, want origin refs/heads/%s", remote.gotRemote, remote.gotRef, takeoverBranch)
	}
	if !strings.Contains(out.String(), takeoverTip) {
		t.Errorf("output %q does not print the head it named", out.String())
	}
}

// TestLandTakeoverRefusesWithoutWriting: every refusal comes before the first
// write, so a refused takeover leaves the bead exactly as it was.
func TestLandTakeoverRefusesWithoutWriting(t *testing.T) {
	t.Parallel()
	escalated := []string{land.LabelNeedsOverseer}
	cases := []struct {
		name      string
		status    beads.IssueStatus
		labels    []string
		tip       string
		remoteErr error
		showFails bool
		edit      func(*takeoverRequest)
		wantText  string
	}{
		{name: "branch not on origin", labels: escalated, tip: "", wantText: "is not on origin"},
		{name: "remote read fails", labels: escalated, remoteErr: errors.New("ls-remote timed out"), wantText: "reading refs/heads/"},
		{name: "bead not escalated and no force", labels: nil, tip: takeoverTip, wantText: "pass --force"},
		// A bead held for a person by another spelling is not escalated for
		// this command: gt:needs-overseer is the label the spec names.
		{name: "only gt:needs-human is not escalated", labels: []string{land.LabelNeedsHuman}, tip: takeoverTip, wantText: "pass --force"},
		{name: "force names no reason", labels: nil, tip: takeoverTip, edit: func(r *takeoverRequest) { r.Force = "   " }, wantText: "pass --force"},
		{name: "caller is the human", labels: escalated, tip: takeoverTip, edit: func(r *takeoverRequest) { r.Actor = takeoverHuman }, wantText: "the human's own identity"},
		{name: "human identity is empty", labels: escalated, tip: takeoverTip, edit: func(r *takeoverRequest) { r.Human = "" }, wantText: "human identity is empty"},
		{name: "caller names no identity", labels: escalated, tip: takeoverTip, edit: func(r *takeoverRequest) { r.Actor = "" }, wantText: "names no identity"},
		{name: "branch is empty", labels: escalated, tip: takeoverTip, edit: func(r *takeoverRequest) { r.Branch = "" }, wantText: "names no branch"},
		{name: "target is empty", labels: escalated, tip: takeoverTip, edit: func(r *takeoverRequest) { r.Target = "" }, wantText: "target branch is empty"},
		{name: "bead is closed", status: beads.StatusClosed, labels: escalated, tip: takeoverTip, wantText: "not waiting to land"},
		{name: "bead cannot be read", labels: escalated, tip: takeoverTip, showFails: true, wantText: "reading " + takeoverBeadID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status := tc.status
			if status == "" {
				status = beads.StatusOpen
			}
			bd := newTakeoverBeads(status, tc.labels)
			if tc.showFails {
				bd.Fake.FailWith("show", errors.New("dolt unreachable"))
			}
			remote := &fakeTakeoverRemote{tip: tc.tip, err: tc.remoteErr}

			err := takeOverLanding(bd, remote, "origin", takeoverRequestFor(tc.edit), &bytes.Buffer{})
			if err == nil {
				t.Fatal("takeover succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("refusal %q does not say %q", err, tc.wantText)
			}
			if len(bd.updates) != 0 {
				t.Errorf("a refused takeover wrote %d updates, want none: %+v", len(bd.updates), bd.updates)
			}
		})
	}
}

func TestLandTakeoverForceTakesOverAnUnescalatedBead(t *testing.T) {
	t.Parallel()
	bd := newTakeoverBeads(beads.StatusOpen, nil)
	remote := &fakeTakeoverRemote{tip: takeoverTip}
	var out bytes.Buffer

	err := takeOverLanding(bd, remote, "origin", takeoverRequestFor(func(r *takeoverRequest) { r.Force = takeoverReason }), &out)
	if err != nil {
		t.Fatalf("takeover with --force: %v", err)
	}
	if len(bd.updates) != 4 {
		t.Fatalf("wrote %d updates, want 4 (the force reason, the READY block, the waiver, the labels)", len(bd.updates))
	}
	if got, want := strings.TrimSpace(bd.updates[0].AppendNotes), landTakeoverForceMarker+": "+takeoverReason; got != want {
		t.Errorf("first write %q is not the forced reason %q", got, want)
	}
	after, err := bd.Show(takeoverBeadID)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !takeoverForceRecorded(after, takeoverReason) {
		t.Errorf("the bead's notes do not record the forced reason: %q", after.Notes)
	}
	if !strings.Contains(out.String(), takeoverReason) {
		t.Errorf("output %q does not print the forced reason", out.String())
	}
}

// TestLandTakeoverStopsWhenTheForceReasonCannotBeWritten: the force note is the
// first write of a forced takeover, so its failure leaves the bead without the
// block or the ready label.
func TestLandTakeoverStopsWhenTheForceReasonCannotBeWritten(t *testing.T) {
	t.Parallel()
	bd := newTakeoverBeads(beads.StatusOpen, nil)
	bd.failOn = 1
	remote := &fakeTakeoverRemote{tip: takeoverTip}

	err := takeOverLanding(bd, remote, "origin", takeoverRequestFor(func(r *takeoverRequest) { r.Force = takeoverReason }), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "recording the --force reason") {
		t.Fatalf("takeover whose force note failed error = %v, want it to name the force reason", err)
	}
	if len(bd.updates) != 1 {
		t.Errorf("made %d writes, want 1: no write runs after the failed one", len(bd.updates))
	}
	after, err := bd.Show(takeoverBeadID)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if beads.HasLabel(after, land.LabelReadyToLand) {
		t.Errorf("labels %v carry %s although the force note failed", after.Labels, land.LabelReadyToLand)
	}
}

// TestLandTakeoverFailsWhenTheForceReasonDoesNotPersist: bd reporting success
// is not proof the note landed, so the read-back checks the forced reason.
func TestLandTakeoverFailsWhenTheForceReasonDoesNotPersist(t *testing.T) {
	t.Parallel()
	bd := newTakeoverBeads(beads.StatusOpen, nil)
	bd.omitNotesOn = 1
	remote := &fakeTakeoverRemote{tip: takeoverTip}

	err := takeOverLanding(bd, remote, "origin", takeoverRequestFor(func(r *takeoverRequest) { r.Force = takeoverReason }), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "does not read back as recording the --force reason") {
		t.Fatalf("takeover whose force note did not persist error = %v, want the read-back to name it", err)
	}
}

// TestLandTakeoverStopsWhenAWriteFails: a failed write ends the takeover at
// that write, so no later write runs. The label write is last, so a failure
// before it leaves the bead without the ready label.
func TestLandTakeoverStopsWhenAWriteFails(t *testing.T) {
	t.Parallel()
	for _, failOn := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("write %d fails", failOn), func(t *testing.T) {
			t.Parallel()
			bd := newTakeoverBeads(beads.StatusOpen, []string{land.LabelNeedsOverseer})
			bd.failOn = failOn
			remote := &fakeTakeoverRemote{tip: takeoverTip}

			if err := takeOverLanding(bd, remote, "origin", takeoverRequestFor(nil), &bytes.Buffer{}); err == nil {
				t.Fatal("takeover succeeded after a failed write")
			}
			if len(bd.updates) != failOn {
				t.Errorf("made %d writes, want %d: no write runs after the failed one", len(bd.updates), failOn)
			}
			after, err := bd.Show(takeoverBeadID)
			if err != nil {
				t.Fatalf("Show: %v", err)
			}
			if beads.HasLabel(after, land.LabelReadyToLand) {
				t.Errorf("labels %v carry %s although write %d failed", after.Labels, land.LabelReadyToLand, failOn)
			}
		})
	}
}

// absentTakeoverBeads answers Show with no record and no error, as a store
// that found nothing under the id would.
type absentTakeoverBeads struct{}

func (absentTakeoverBeads) Show(string) (*beads.Issue, error) { return nil, nil }

func (absentTakeoverBeads) Update(string, beads.UpdateOptions) error {
	return errors.New("update on an absent bead")
}

func TestLandTakeoverRefusesAMissingBeadRecord(t *testing.T) {
	t.Parallel()
	remote := &fakeTakeoverRemote{tip: takeoverTip}

	err := takeOverLanding(absentTakeoverBeads{}, remote, "origin", takeoverRequestFor(nil), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no such bead") {
		t.Fatalf("takeover of an absent bead error = %v, want a no-such-bead refusal", err)
	}
}

// readBackFailsBeads fails its second Show, the read-back after the writes; the
// first Show, the read before them, succeeds.
type readBackFailsBeads struct {
	*recordingTakeoverBeads
	shows int
}

func (r *readBackFailsBeads) Show(id string) (*beads.Issue, error) {
	r.shows++
	if r.shows == 2 {
		return nil, errors.New("dolt unreachable")
	}
	return r.recordingTakeoverBeads.Show(id)
}

func TestLandTakeoverFailsWhenTheReadBackFails(t *testing.T) {
	t.Parallel()
	bd := &readBackFailsBeads{recordingTakeoverBeads: newTakeoverBeads(beads.StatusOpen, []string{land.LabelNeedsOverseer})}
	remote := &fakeTakeoverRemote{tip: takeoverTip}

	err := takeOverLanding(bd, remote, "origin", takeoverRequestFor(nil), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "reading "+takeoverBeadID+" back") {
		t.Fatalf("takeover with a failed read-back error = %v, want it to name the read-back", err)
	}
	if len(bd.updates) != 3 {
		t.Errorf("made %d writes, want 3 before the read-back", len(bd.updates))
	}
}

func TestLandTakeoverFailsWhenTheLabelsDoNotPersist(t *testing.T) {
	t.Parallel()
	bd := newTakeoverBeads(beads.StatusOpen, []string{land.LabelNeedsOverseer})
	bd.dropLabels = true
	remote := &fakeTakeoverRemote{tip: takeoverTip}

	err := takeOverLanding(bd, remote, "origin", takeoverRequestFor(nil), &bytes.Buffer{})
	if err == nil {
		t.Fatal("takeover reported success although the labels did not persist")
	}
	if len(bd.updates) != 3 {
		t.Errorf("made %d writes, want 3 before the read-back", len(bd.updates))
	}
}

func TestLandTakeoverHumanReadsTheTownsIdentity(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if _, err := loadTakeoverHuman(townRoot); err == nil {
		t.Fatal("loadTakeoverHuman with no overseer config returned no error")
	}

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"overseer","version":1,"name":"operator-human","source":"test"}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "overseer.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	name, err := loadTakeoverHuman(townRoot)
	if err != nil {
		t.Fatalf("loadTakeoverHuman: %v", err)
	}
	if name != takeoverHuman {
		t.Errorf("human = %q, want %q", name, takeoverHuman)
	}
}

func TestLandTakeoverActorIsTheCrewSession(t *testing.T) {
	t.Parallel()
	crew, err := takeoverActor(func(key string) string {
		if key == "BEADS_ACTOR" {
			return takeoverCaller
		}
		return ""
	})
	if err != nil || crew != takeoverCaller {
		t.Errorf("takeoverActor with BEADS_ACTOR = %q, %v; want %q", crew, err, takeoverCaller)
	}
	if _, err := takeoverActor(func(string) string { return "" }); err == nil {
		t.Error("takeoverActor with no crew identity returned no error")
	}
	if _, err := takeoverActor(func(key string) string {
		if key == "GT_POLECAT" {
			return "nux"
		}
		return ""
	}); err == nil {
		t.Error("takeoverActor named a polecat; only a crew session takes a bead over")
	}
}

func TestLandTakeoverTargetDefaultsToTheRigsBranch(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")

	if got, err := takeoverTarget(townRoot, "gastown", " trunk "); err != nil || got != "trunk" {
		t.Errorf("takeoverTarget with --target = %q, %v; want trunk", got, err)
	}
	if got, err := takeoverTarget(townRoot, "gastown", ""); err != nil || got != "main" {
		t.Errorf("takeoverTarget with no rig config = %q, %v; want main", got, err)
	}

	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"name":"gastown","default_branch":"trunk"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := takeoverTarget(townRoot, "gastown", ""); err != nil || got != "trunk" {
		t.Errorf("takeoverTarget with default_branch trunk = %q, %v; want trunk", got, err)
	}

	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"default_branch":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := takeoverTarget(townRoot, "gastown", ""); err == nil {
		t.Error("takeoverTarget with an unreadable rig config returned no error")
	}
}

// takeoverFixture is a town the command can take over in: a routed bead, the
// human's identity, a crew caller, an escalated bead and a branch on origin.
// Each refusal test breaks one of those inputs.
type takeoverFixture struct {
	town      string
	beadID    string
	townErr   error
	cwdErr    error
	remoteErr error
	env       map[string]string
	bd        *recordingTakeoverBeads
	remote    *fakeTakeoverRemote
	repoRig   string
}

func newTakeoverFixture(t *testing.T) *takeoverFixture {
	t.Helper()
	town := t.TempDir()
	writeTakeoverFile(t, filepath.Join(town, ".beads", "routes.jsonl"), `{"prefix":"gt-","path":"gastown/.beads"}`+"\n")
	writeTakeoverFile(t, filepath.Join(town, "mayor", "overseer.json"), `{"type":"overseer","version":1,"name":"operator-human","source":"test"}`)
	return &takeoverFixture{
		town:   town,
		beadID: takeoverBeadID,
		env:    map[string]string{"BEADS_ACTOR": takeoverCaller},
		bd:     newTakeoverBeads(beads.StatusOpen, []string{land.LabelNeedsOverseer}),
		remote: &fakeTakeoverRemote{tip: takeoverTip},
	}
}

func (f *takeoverFixture) host() landTakeoverHost {
	return landTakeoverHost{
		findTown:      func() (string, error) { return f.town, f.townErr },
		getwd:         func() (string, error) { return f.town, f.cwdErr },
		getenv:        func(key string) string { return f.env[key] },
		landingRemote: func(string, string) (string, error) { return "origin", f.remoteErr },
		repo: func(_, rigName string) landTakeoverRemote {
			f.repoRig = rigName
			return f.remote
		},
		beads: func(string, string) landTakeoverBeads { return f.bd },
	}
}

func writeTakeoverFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLandTakeoverCommandWritesThroughItsHost: the command resolves the rig,
// its remote and repo, the human and the caller from its host, then takes the
// bead over with the takeover's writes.
func TestLandTakeoverCommandWritesThroughItsHost(t *testing.T) {
	t.Parallel()
	f := newTakeoverFixture(t)
	var out bytes.Buffer

	if err := landTakeoverCommand(&out, takeoverBeadID, takeoverBranch, "", "", f.host()); err != nil {
		t.Fatalf("landTakeoverCommand: %v", err)
	}
	if f.repoRig != "gastown" {
		t.Errorf("read the repo of rig %q, want gastown", f.repoRig)
	}
	if len(f.bd.updates) != 3 {
		t.Fatalf("wrote %d updates, want 3", len(f.bd.updates))
	}
	ready, ok := land.ParseReadyNote(f.bd.updates[0].AppendNotes)
	if !ok || ready.Worker != takeoverCaller || ready.Target != "main" || ready.Head != takeoverTip {
		t.Errorf("READY block = %+v (ok=%v), want worker %q target main head %q", ready, ok, takeoverCaller, takeoverTip)
	}
	after, err := f.bd.Show(takeoverBeadID)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if _, err := land.WorkFromBead(after, "gastown"); err != nil {
		t.Errorf("the landing worker cannot read the takeover's request: %v", err)
	}
	if !strings.Contains(out.String(), takeoverTip) {
		t.Errorf("output %q does not print the head it named", out.String())
	}
}

// TestLandTakeoverCommandRefusesBeforeWriting: each input the command reads
// from its host is refused with a message naming it, and nothing is written.
func TestLandTakeoverCommandRefusesBeforeWriting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(t *testing.T, f *takeoverFixture)
		want string
	}{
		{name: "no town", edit: func(_ *testing.T, f *takeoverFixture) { f.townErr = errors.New("no town here") }, want: "resolving the town"},
		{name: "bead prefix maps to no rig", edit: func(_ *testing.T, f *takeoverFixture) { f.beadID = "zz-tk4w1" }, want: "cannot resolve the rig"},
		{name: "landing remote unreadable", edit: func(_ *testing.T, f *takeoverFixture) { f.remoteErr = errors.New("forgejo block unreadable") }, want: "forgejo block unreadable"},
		{name: "working directory unreadable", edit: func(_ *testing.T, f *takeoverFixture) { f.cwdErr = errors.New("cwd gone") }, want: "resolving the working directory"},
		{name: "no human identity", edit: func(t *testing.T, f *takeoverFixture) {
			if err := os.Remove(filepath.Join(f.town, "mayor", "overseer.json")); err != nil {
				t.Fatal(err)
			}
		}, want: "human identity cannot be read"},
		{name: "rig config unreadable", edit: func(t *testing.T, f *takeoverFixture) {
			writeTakeoverFile(t, filepath.Join(f.town, "gastown", "config.json"), `{"default_branch":`)
		}, want: "default branch cannot be read"},
		{name: "no crew identity", edit: func(_ *testing.T, f *takeoverFixture) { f.env = map[string]string{} }, want: "cannot name the caller"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTakeoverFixture(t)
			tc.edit(t, f)

			err := landTakeoverCommand(&bytes.Buffer{}, f.beadID, takeoverBranch, "", "", f.host())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("landTakeoverCommand error = %v, want it to name %q", err, tc.want)
			}
			if len(f.bd.updates) != 0 {
				t.Errorf("a refused takeover wrote %d updates, want none", len(f.bd.updates))
			}
		})
	}
}
