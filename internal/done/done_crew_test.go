package done

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
)

// crewSubmittedRE is the landing worker's submission-comment pattern
// (landworker.submittedRE); a crew comment must satisfy it.
var crewSubmittedRE = regexp.MustCompile(`Submitted for landing: (\S+) @ ([0-9a-fA-F]{4,40})(?: onto (\S+))?`)

const (
	crewTestBranch = "crew/operator/crew-done-submit"
	crewTestHead   = "c0ffee1234abcdef"
	// crewTestActor is the crew seat the env and assignee cases name: it is
	// both the identity the test configures and the one it expects back.
	crewTestActor = "gastown/crew/operator"
)

// newCrewSubmitHarness is the submit harness as a crew run: a crew branch
// already pushed at HEAD, no polecat, no seat.
func newCrewSubmitHarness(t *testing.T) *submitHarness {
	t.Helper()
	h := newSubmitHarness(t)
	writeTownHuman(t, h.townRoot, crewTestHumanName, crewTestHumanEmail)
	h.r.branch = crewTestBranch
	h.r.polecatName = ""
	h.r.sender = "Test Operator"
	h.r.opts = doneOptions{}
	// A hex head, so the comment is one the worker's pattern can read.
	h.repo.head = crewTestHead
	h.repo.ancestors = map[string]bool{"main1": true, crewTestHead: true}
	h.repo.origin[crewTestBranch] = crewTestHead
	return h
}

// TestCrewSubmitMarksPushedBranchReadyToLand: a crew branch pushed at HEAD
// is gated at HEAD, then the bead gets the worker's submission comment, a
// READY TO LAND block naming exactly that head and the ready label. The
// comment is by the crew member. Nothing is pushed, rebased or closed.
func TestCrewSubmitMarksPushedBranchReadyToLand(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	head := crewTestHead
	if len(h.gate.heads) != 1 || h.gate.heads[0] != head {
		t.Errorf("gate ran on %v, want exactly %s", h.gate.heads, head)
	}
	if len(h.repo.pushes) != 0 || len(h.stages) != 0 {
		t.Errorf("crew submit pushed %v / ran stages %v; crew push their own branch", h.repo.pushes, h.stages)
	}
	issue := h.source(t)
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("labels %v lack %s", issue.Labels, land.LabelReadyToLand)
	}
	if issue.Status == string(beads.StatusClosed) {
		t.Error("crew submit closed the bead; only the landing worker may")
	}
	w, err := land.WorkFromBead(issue, "gastown")
	if err != nil || w.Branch != crewTestBranch || w.Head != head || w.Target != "main" || w.Worker != "" {
		t.Errorf("landing request = %+v (%v) from notes %q", w, err, issue.Notes)
	}
	comments, err := h.bd.Comments("bd-source")
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments = %v (%v), want one submission comment", comments, err)
	}
	if comments[0].Author != h.r.sender {
		t.Errorf("submission comment by %q, want the crew member %q (gt-0wkug)", comments[0].Author, h.r.sender)
	}
	m := crewSubmittedRE.FindStringSubmatch(comments[0].Text)
	if m == nil || m[1] != crewTestBranch || m[2] != head || m[3] != "main" {
		t.Errorf("submission comment %q does not parse as %s @ %s onto main (got %v)", comments[0].Text, crewTestBranch, head, m)
	}
}

// TestCrewSubmitPrintsNextStep: a crew submission that marks the bead ready
// ends its output with the line naming gt land wait, so a crew session that
// has forgotten its workflow still learns what to do next (gt-2ass0).
func TestCrewSubmitPrintsNextStep(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	var out bytes.Buffer
	h.r.out = &out
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if last := lines[len(lines)-1]; last != crewNextStepLine {
		t.Errorf("last crew output line = %q, want %q", last, crewNextStepLine)
	}
}

// TestCrewSubmitPreVerifiedSkipsGate: --pre-verified skips only the local
// gate; the bead is still marked ready.
func TestCrewSubmitPreVerifiedSkipsGate(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	h.r.opts.preVerified = true
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	if len(h.gate.heads) != 0 {
		t.Errorf("gate ran on %v despite --pre-verified", h.gate.heads)
	}
	if !beads.HasLabel(h.source(t), land.LabelReadyToLand) {
		t.Error("pre-verified crew submit did not mark the bead ready")
	}
}

// TestCrewSubmitRefusals: every refusal leaves the bead unmarked.
func TestCrewSubmitRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(h *submitHarness)
		code  int // 0 = plain error
		text  string
	}{
		{"not pushed", func(h *submitHarness) { delete(h.repo.origin, crewTestBranch) }, doneExitPushUnverified, "push it first"},
		{"origin behind HEAD", func(h *submitHarness) { h.repo.origin[crewTestBranch] = "older" }, doneExitPushUnverified, "is not at HEAD"},
		{"gate red", func(h *submitHarness) { h.gate.result = land.GateResult{Passed: false} }, doneExitGateFailed, "local gate failed"},
		{"nothing ahead", func(h *submitHarness) { h.repo.ahead = 0 }, 0, "nothing to land"},
		{"no bead", func(h *submitHarness) { h.r.issueID = "" }, 0, "pass --bead"},
		{"main branch", func(h *submitHarness) { h.r.branch = "main" }, 0, "cannot submit the main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCrewSubmitHarness(t)
			tc.setup(h)
			err := submitCrewForLanding(h.r)
			if err == nil {
				t.Fatal("crew submit succeeded, want a refusal")
			}
			if tc.code != 0 {
				wantDoneExit(t, err, tc.code, tc.text)
			} else if !strings.Contains(err.Error(), tc.text) {
				t.Errorf("error %q lacks %q", err, tc.text)
			}
			issue := h.source(t)
			if beads.HasLabel(issue, land.LabelReadyToLand) || strings.Contains(issue.Notes, land.ReadyNoteMarker) {
				t.Errorf("refused crew submit still marked the bead: labels %v notes %q", issue.Labels, issue.Notes)
			}
		})
	}
}

// TestCrewDoneClaimsUnassignedBead: a crew submission of a bead nobody holds
// leaves it in progress under the submitter, so a rejection keeps it with the
// bird instead of releasing it to dispatch (gt-sa0bna).
func TestCrewDoneClaimsUnassignedBead(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	issue := h.source(t)
	if issue.Assignee != h.r.sender || issue.Status != string(beads.StatusInProgress) {
		t.Errorf("after the submission the bead is %q held by %q, want in_progress held by %q", issue.Status, issue.Assignee, h.r.sender)
	}
	if seats := land.HolderSeats(issue.Notes); len(seats) != 0 {
		t.Errorf("an unassigned bead's claim recorded holder seats %v, want none", seats)
	}
	if strings.Contains(issue.Notes, land.HolderSeatNoteMarker) {
		t.Errorf("an unassigned bead's claim wrote a holder line: notes %q", issue.Notes)
	}
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("labels %v lack %s: the claim must not cost the submission", issue.Labels, land.LabelReadyToLand)
	}
}

// TestCrewDoneClaimOverPolecatHolderRecordsHolderSeat: a crew submission that
// claims a bead a polecat seat holds records that seat in the same write, so
// the landing can still name the seat its assignee overwrote (gt-bulryp).
func TestCrewDoneClaimOverPolecatHolderRecordsHolderSeat(t *testing.T) {
	t.Parallel()
	const holder = "gastown/polecats/agate"
	h := newCrewSubmitHarness(t)
	setCrewAssignee(t, h, holder)
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	issue := h.source(t)
	if issue.Status != string(beads.StatusInProgress) || issue.Assignee != h.r.sender {
		t.Errorf("after the submission the bead is %q held by %q, want in_progress held by %q", issue.Status, issue.Assignee, h.r.sender)
	}
	if got := land.HolderSeats(issue.Notes); len(got) != 1 || got[0] != holder {
		t.Errorf("holder seats read back %v from notes %q, want [%s]", got, issue.Notes, holder)
	}
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("labels %v lack %s: the holder note must not cost the submission", issue.Labels, land.LabelReadyToLand)
	}
}

// TestCrewDoneClaimsFailureWritesNoRecords: a claim the beads store refuses
// ends the submission with the ready-failed exit before any record is written,
// so no comment and no ready label announce a bead nobody holds.
func TestCrewDoneClaimsFailureWritesNoRecords(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	h.client = &failingBD{Client: h.bd, updateErr: errors.New("store refused the write")}
	err := submitCrewForLanding(h.r)
	if err == nil {
		t.Fatal("crew submit succeeded although the claim failed")
	}
	wantDoneExit(t, err, doneExitReadyFailed, "could not claim")
	issue := h.source(t)
	if issue.Assignee != "" || beads.HasLabel(issue, land.LabelReadyToLand) || strings.Contains(issue.Notes, land.ReadyNoteMarker) {
		t.Errorf("a failed claim left the bead held by %q, labels %v, notes %q", issue.Assignee, issue.Labels, issue.Notes)
	}
	if comments, cerr := h.bd.Comments("bd-source"); cerr != nil || len(comments) != 0 {
		t.Errorf("comments = %v (%v), want none: the claim comes before the submission comment", comments, cerr)
	}
}

// failCommentBD fails the submission comment and passes the rest to the
// wrapped fake.
type failCommentBD struct {
	beads.Client
}

func (failCommentBD) AddCommentAs(string, string, string) error {
	return errors.New("store refused the comment")
}

// TestCrewDoneClaimsKeepsTheClaimWhenALaterWriteFails: the claim comes first,
// so a submission whose comment write fails ends with the ready-failed exit
// and no ready label, but the bird still holds the bead and can submit again.
func TestCrewDoneClaimsKeepsTheClaimWhenALaterWriteFails(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	h.client = failCommentBD{Client: h.bd}
	err := submitCrewForLanding(h.r)
	if err == nil {
		t.Fatal("crew submit succeeded although the comment write failed")
	}
	wantDoneExit(t, err, doneExitReadyFailed, "could not record the submission")
	issue := h.source(t)
	if issue.Assignee != h.r.sender || issue.Status != string(beads.StatusInProgress) {
		t.Errorf("after the failed submission the bead is %q held by %q, want in_progress still held by %q", issue.Status, issue.Assignee, h.r.sender)
	}
	if beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("labels %v carry %s although the comment write failed", issue.Labels, land.LabelReadyToLand)
	}
}

// crewEnv is a getenv over a fixed map.
func crewEnv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestCrewActorFromEnv: a crew submission writes as the session's agent
// identity, never as the git committer the shell carries, which is the human
// when the human runs gt done by hand — a submission the landing worker
// refuses (gt-i817y). A session naming no identity names "" here, so gt done
// keeps the committer it always fell back to.
func TestCrewActorFromEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"BEADS_ACTOR is the session's identity", map[string]string{"BEADS_ACTOR": "town/crew/overseer"}, "town/crew/overseer"},
		{"BEADS_ACTOR leads BD_ACTOR", map[string]string{"BD_ACTOR": crewTestActor, "BEADS_ACTOR": "town/crew/overseer"}, "town/crew/overseer"},
		{"BD_ACTOR is the identity when BEADS_ACTOR is unset", map[string]string{"BD_ACTOR": crewTestActor}, crewTestActor},
		{"GT_ROLE names the crew member", map[string]string{"GT_ROLE": "town/crew/overseer"}, "town/crew/overseer"},
		{"a bare crew role is its own identity", map[string]string{"GT_ROLE": "crew"}, "crew"},
		{"a blank BEADS_ACTOR names no one", map[string]string{"BEADS_ACTOR": "  "}, ""},
		{"no identity at all", map[string]string{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CrewActorFromEnv(crewEnv(tc.env)); got != tc.want {
				t.Errorf("CrewActorFromEnv(%v) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// gitName is the committer a checkout's git config names, as newCrewDoneRun
// reads it on the fallback path.
func gitName(name string) func() string { return func() string { return name } }

// crewRunFromEnv builds the run a crew session submits as through the same
// constructor runDoneCrew calls, then gives it the submit harness's seams.
func crewRunFromEnv(t *testing.T, h *submitHarness, env map[string]string, committer string) *doneRun {
	t.Helper()
	r := newCrewDoneRun(crewEnv(env), gitName(committer), h.r.cwd, h.townRoot, "gastown", crewTestBranch, "bd-source")
	r.deps = h.r.deps
	r.opts = h.r.opts
	return r
}

// TestCrewSubmitCommentAuthoredBySessionActor: the Submitted-for-landing
// comment the landing worker reads is added under BEADS_ACTOR when the
// session has one, not under the git user.name the human's checkout carries
// (gt-i817y).
func TestCrewSubmitCommentAuthoredBySessionActor(t *testing.T) {
	t.Parallel()
	const actor = "town/crew/overseer"
	h := newCrewSubmitHarness(t)
	r := crewRunFromEnv(t, h, map[string]string{"BEADS_ACTOR": actor}, "Test Operator")
	if err := submitCrewForLanding(r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	comments, err := h.bd.Comments("bd-source")
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments = %v (%v), want one submission comment", comments, err)
	}
	if comments[0].Author != actor {
		t.Errorf("submission comment by %q, want the session's BEADS_ACTOR %q", comments[0].Author, actor)
	}
}

// TestCrewSubmitWithNoSessionActorKeepsCommitter: a session naming no agent
// still records the submission under the git committer, as it always did.
func TestCrewSubmitWithNoSessionActorKeepsCommitter(t *testing.T) {
	t.Parallel()
	const committer = "Test Operator"
	h := newCrewSubmitHarness(t)
	r := crewRunFromEnv(t, h, map[string]string{}, committer)
	if err := submitCrewForLanding(r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	comments, err := h.bd.Comments("bd-source")
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments = %v (%v), want one submission comment", comments, err)
	}
	if comments[0].Author != committer {
		t.Errorf("submission comment by %q, want the git committer %q", comments[0].Author, committer)
	}
}

// TestCrewDoneRunSenderPrefersSessionActor: the run's sender is the session's
// identity, and the git committer is read only when the session names none.
// A session with neither has an empty sender, which submit refuses.
func TestCrewDoneRunSenderPrefersSessionActor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		env       map[string]string
		committer string
		want      string
	}{
		{"session actor wins over the committer", map[string]string{"BEADS_ACTOR": "town/crew/overseer"}, "Test Operator", "town/crew/overseer"},
		{"no session actor falls back to the committer", map[string]string{}, "Test Operator", "Test Operator"},
		{"no session actor and no committer names no one", map[string]string{}, "  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCrewDoneRun(crewEnv(tc.env), gitName(tc.committer), t.TempDir(), t.TempDir(), "gastown", crewTestBranch, "bd-source")
			if r.sender != tc.want {
				t.Errorf("sender = %q, want %q", r.sender, tc.want)
			}
		})
	}
}

// TestCrewSubmitRefusesWithNoIdentity: a submission nobody can be attributed
// to is refused before any gate runs or any comment is written (gt-i817y).
func TestCrewSubmitRefusesWithNoIdentity(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	r := crewRunFromEnv(t, h, map[string]string{}, "  ")
	if err := submitCrewForLanding(r); err == nil {
		t.Fatal("crew submit with no session actor and no committer succeeded, want a refusal")
	}
	if len(h.gate.heads) != 0 {
		t.Errorf("gate ran on %v for an unattributed submission, want none", h.gate.heads)
	}
	if comments, _ := h.bd.Comments("bd-source"); len(comments) != 0 {
		t.Errorf("unattributed submission wrote comments %v, want none", comments)
	}
}

// TestDoneIsCrewRun: crew is detected positively (a crew role, actor or
// worktree path) and any polecat trace rules it out (gt-avwp2).
func TestDoneIsCrewRun(t *testing.T) {
	t.Parallel()
	const (
		crewDir    = "/town/gastown/crew/operator"
		polecatDir = "/town/gastown/polecats/refuge/gastown"
		townDir    = "/town"
	)
	cases := []struct {
		env  map[string]string
		cwd  string
		want bool
	}{
		{map[string]string{}, crewDir, true},
		{map[string]string{}, "/town/gastown/crew/operator-cmd-sling/internal/cmd", true},
		{map[string]string{"BD_ACTOR": crewTestActor}, townDir, true},
		{map[string]string{"GT_ROLE": "crew"}, townDir, true},
		{map[string]string{"BD_ACTOR": crewTestActor, "GT_ROLE": crewTestActor}, crewDir, true},
		// No identity anywhere is not crew.
		{map[string]string{}, townDir, false},
		{map[string]string{}, "", false},
		// A polecat that lost its env stays a polecat.
		{map[string]string{}, polecatDir, false},
		{map[string]string{"GT_ROLE": "crew"}, polecatDir, false},
		{map[string]string{"BD_ACTOR": "gastown/polecats/refuge"}, crewDir, false},
		{map[string]string{"GT_POLECAT": "refuge"}, crewDir, false},
		{map[string]string{"GT_ROLE": "gastown/polecats/refuge"}, crewDir, false},
		{map[string]string{"GT_ROLE": "polecat"}, crewDir, false},
		{map[string]string{"BD_ACTOR": "gastown/witness"}, crewDir, false},
		{map[string]string{"GT_ROLE": "mayor"}, crewDir, false},
	}
	for _, tc := range cases {
		getenv := func(k string) string { return tc.env[k] }
		if got := IsCrewRun(getenv, tc.cwd); got != tc.want {
			t.Errorf("IsCrewRun(%v, %q) = %v, want %v", tc.env, tc.cwd, got, tc.want)
		}
	}
}

// standDownRecorder is what the crew stand-down seams saw: the live polecat
// sessions and the nudges enqueued for them.
type standDownRecorder struct {
	sessions map[string]bool
	nudges   []struct {
		townRoot string
		session  string
		nudge    nudge.QueuedNudge
	}
	nudgeErr error
}

// installStandDown wires a crew harness to the real assignee parser over a
// one-rig registry, the recorder's live-session set, and a captured nudge
// seam. The prefix matches the harness's gastown rig.
func installStandDown(h *submitHarness, sessions map[string]bool, nudgeErr error) *standDownRecorder {
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	rec := &standDownRecorder{sessions: sessions, nudgeErr: nudgeErr}
	h.r.deps.polecatSeat = func(assignee string) (string, bool) {
		return polecatAssigneeSeat(assignee, reg)
	}
	h.r.deps.sessionAlive = func(name string) bool { return rec.sessions[name] }
	h.r.deps.enqueueNudge = func(townRoot, sessionName string, n nudge.QueuedNudge) error {
		rec.nudges = append(rec.nudges, struct {
			townRoot string
			session  string
			nudge    nudge.QueuedNudge
		}{townRoot, sessionName, n})
		return rec.nudgeErr
	}
	return rec
}

// setCrewAssignee assigns the source bead to a seat.
func setCrewAssignee(t *testing.T, h *submitHarness, assignee string) {
	t.Helper()
	if err := h.bd.Update("bd-source", beads.UpdateOptions{Assignee: &assignee}); err != nil {
		t.Fatalf("assigning bd-source to %q: %v", assignee, err)
	}
}

// TestClaimForCrewSubmit: a crew submission claims a bead nobody else is
// building for the submitter, so the landing worker's rejection path keeps it
// with that owner instead of releasing it to dispatch and getting the branch
// built a second time (gt-42cryp). An unassigned bead and a polecat-held bead
// are claimed; a bead another crew member holds is left as it is. A claim that
// replaces a polecat seat records that seat as a holder note (gt-bulryp); an
// unassigned or bird-held bead gets none. A failed write is returned, never
// read as a successful claim.
func TestClaimForCrewSubmit(t *testing.T) {
	t.Parallel()
	const sender = "gastown/crew/operator"
	cases := []struct {
		name        string
		assignee    string
		updateErr   error
		wantClaimed bool
		wantHolder  []string
	}{
		{"unassigned", "", nil, true, nil},
		{"polecat seat holder", "gastown/polecats/agate", nil, true, []string{"gastown/polecats/agate"}},
		{"another crew member", "gastown/crew/wren", nil, false, nil},
		{"failed write", "", errors.New("database is locked"), false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			if tc.assignee != "" {
				setCrewAssignee(t, h, tc.assignee)
			}
			before := h.source(t)
			var bd beads.Client = h.bd
			if tc.updateErr != nil {
				bd = &failingBD{Client: h.bd, updateErr: tc.updateErr}
			}
			err := claimForCrewSubmit(bd, before, sender)
			if tc.updateErr != nil {
				if !errors.Is(err, tc.updateErr) {
					t.Fatalf("claim error = %v, want the write error %v", err, tc.updateErr)
				}
				if got := land.HolderSeats(h.source(t).Notes); len(got) != 0 {
					t.Errorf("a failed claim recorded holder seats %v, want none", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("claimForCrewSubmit: %v", err)
			}
			after := h.source(t)
			if got := land.HolderSeats(after.Notes); strings.Join(got, "|") != strings.Join(tc.wantHolder, "|") {
				t.Errorf("holder seats read back %v, want %v", got, tc.wantHolder)
			}
			if len(tc.wantHolder) == 0 && strings.Contains(after.Notes, land.HolderSeatNoteMarker) {
				t.Errorf("notes %q carry a holder line, want none", after.Notes)
			}
			if tc.wantClaimed {
				if after.Status != string(beads.StatusInProgress) || after.Assignee != sender {
					t.Errorf("after claim status %q assignee %q, want in_progress/%q", after.Status, after.Assignee, sender)
				}
				return
			}
			if after.Status != before.Status || after.Assignee != tc.assignee {
				t.Errorf("held bead changed to status %q assignee %q, want %q/%q", after.Status, after.Assignee, before.Status, tc.assignee)
			}
		})
	}
}

// TestCrewSubmitStandsDownPolecatHolder: a crew submission of a bead a live
// polecat still holds nudges that seat once, names the crew branch and head,
// says not to submit, and tells it to defer. One takeover comment records it.
func TestCrewSubmitStandsDownPolecatHolder(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	setCrewAssignee(t, h, "gastown/polecats/agate")
	rec := installStandDown(h, map[string]bool{"gt-agate": true}, nil)

	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	if len(rec.nudges) != 1 {
		t.Fatalf("nudges = %+v, want exactly one", rec.nudges)
	}
	got := rec.nudges[0]
	if got.session != "gt-agate" || got.townRoot != h.townRoot {
		t.Errorf("nudged session %q in town %q, want gt-agate in %q", got.session, got.townRoot, h.townRoot)
	}
	if got.nudge.Sender != h.r.sender || got.nudge.Priority != nudge.PriorityNormal {
		t.Errorf("nudge from %q priority %q, want %q/normal", got.nudge.Sender, got.nudge.Priority, h.r.sender)
	}
	for _, want := range []string{crewTestBranch, crewTestHead[:7], "gt done --status DEFERRED", "COMPLETED"} {
		if !strings.Contains(got.nudge.Message, want) {
			t.Errorf("nudge %q lacks %q", got.nudge.Message, want)
		}
	}
	comments, err := h.bd.Comments("bd-source")
	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments = %d, want a submission and a takeover comment", len(comments))
	}
	takeover := comments[1].Text
	for _, want := range []string{"gastown/polecats/agate", crewTestBranch, crewTestHead} {
		if !strings.Contains(takeover, want) {
			t.Errorf("takeover comment %q lacks %q", takeover, want)
		}
	}
	if !beads.HasLabel(h.source(t), land.LabelReadyToLand) {
		t.Error("stand-down left the bead unmarked")
	}
}

// TestCrewSubmitStandDownSkips: an empty, submitter, crew or sessionless
// assignee gets no nudge and no takeover comment, and the submission still
// marks the bead ready.
func TestCrewSubmitStandDownSkips(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		assignee string
		sender   string
		sessions map[string]bool
	}{
		{"empty assignee", "", "Test Operator", map[string]bool{}},
		{"submitter as assignee", "gastown/polecats/agate", "gastown/polecats/agate", map[string]bool{"gt-agate": true}},
		{"non-polecat assignee", crewTestActor, "Test Operator", map[string]bool{}},
		{"polecat with no session", "gastown/polecats/agate", "Test Operator", map[string]bool{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCrewSubmitHarness(t)
			h.r.sender = tc.sender
			if tc.assignee != "" {
				setCrewAssignee(t, h, tc.assignee)
			}
			rec := installStandDown(h, tc.sessions, nil)
			if err := submitCrewForLanding(h.r); err != nil {
				t.Fatalf("crew submit: %v", err)
			}
			if len(rec.nudges) != 0 {
				t.Errorf("nudges = %+v, want none", rec.nudges)
			}
			comments, err := h.bd.Comments("bd-source")
			if err != nil {
				t.Fatalf("comments: %v", err)
			}
			if len(comments) != 1 {
				t.Errorf("comments = %d, want only the submission comment", len(comments))
			}
			if !beads.HasLabel(h.source(t), land.LabelReadyToLand) {
				t.Error("skipped stand-down left the bead unmarked")
			}
		})
	}
}

// TestCrewSubmitStandDownNudgeFailureStillSubmits: a nudge that errors is
// logged, not fatal; the bead still lands and the takeover is still recorded.
func TestCrewSubmitStandDownNudgeFailureStillSubmits(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	setCrewAssignee(t, h, "gastown/polecats/agate")
	rec := installStandDown(h, map[string]bool{"gt-agate": true}, errors.New("queue full"))

	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit with a failing nudge: %v", err)
	}
	if len(rec.nudges) != 1 {
		t.Errorf("nudge attempts = %d, want one", len(rec.nudges))
	}
	if !beads.HasLabel(h.source(t), land.LabelReadyToLand) {
		t.Error("failing nudge left the bead unmarked")
	}
}

// TestPolecatAssigneeSeat: only an explicit <rig>/polecats/<name> assignee
// names a polecat session; the rig's prefix comes from the registry.
func TestPolecatAssigneeSeat(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	for assignee, want := range map[string]string{
		"gastown/polecats/agate": "gt-agate",
		"gastown/polecats/":      "",
		"gastown/agate":          "",
		crewTestActor:            "",
		"gastown/witness":        "",
		"":                       "",
	} {
		got, ok := polecatAssigneeSeat(assignee, reg)
		if want == "" {
			if ok {
				t.Errorf("polecatAssigneeSeat(%q) = %q, true; want no seat", assignee, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("polecatAssigneeSeat(%q) = %q, %v; want %q, true", assignee, got, ok, want)
		}
	}
}

// TestCrewBeadFromBranch: a slug that merely looks like a bead id is not
// one unless the town routes its prefix.
func TestCrewBeadFromBranch(t *testing.T) {
	t.Parallel()
	routed := func(prefix string) bool { return prefix == "gt-" }
	for branch, want := range map[string]string{
		"crew/operator/gt-3e7tk-crew-submit": "gt-3e7tk",
		"crew/operator/crew-done-submit":     "",
		"crew/operator/fix":                  "",
		"gt-abc.2":                           "gt-abc.2",
		// parseBranchName reads only a bare id; the embedded fallback finds
		// the routed id inside a crew slug, and drops an unrouted one.
		"crew/operator/gt-3e7tk.2-crew-submit": "gt-3e7tk.2",
		"feature/xx-abc-thing":                 "",
	} {
		if got := crewBeadFromBranch(branch, routed); got != want {
			t.Errorf("crewBeadFromBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}
