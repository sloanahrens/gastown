package done

import (
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
	crewTestBranch = "crew/sloan/crew-done-submit"
	crewTestHead   = "c0ffee1234abcdef"
)

// newCrewSubmitHarness is the submit harness as a crew run: a crew branch
// already pushed at HEAD, no polecat, no seat.
func newCrewSubmitHarness(t *testing.T) *submitHarness {
	t.Helper()
	h := newSubmitHarness(t)
	h.r.branch = crewTestBranch
	h.r.polecatName = ""
	h.r.sender = "Sloan Ahrens"
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
		{"BEADS_ACTOR leads BD_ACTOR", map[string]string{"BD_ACTOR": "gastown/crew/sloan", "BEADS_ACTOR": "town/crew/overseer"}, "town/crew/overseer"},
		{"BD_ACTOR is the identity when BEADS_ACTOR is unset", map[string]string{"BD_ACTOR": "gastown/crew/sloan"}, "gastown/crew/sloan"},
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
	r := crewRunFromEnv(t, h, map[string]string{"BEADS_ACTOR": actor}, "Sloan Ahrens")
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
	const committer = "Sloan Ahrens"
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
		{"session actor wins over the committer", map[string]string{"BEADS_ACTOR": "town/crew/overseer"}, "Sloan Ahrens", "town/crew/overseer"},
		{"no session actor falls back to the committer", map[string]string{}, "Sloan Ahrens", "Sloan Ahrens"},
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
		crewDir    = "/town/gastown/crew/sloan"
		polecatDir = "/town/gastown/polecats/refuge/gastown"
		townDir    = "/town"
	)
	cases := []struct {
		env  map[string]string
		cwd  string
		want bool
	}{
		{map[string]string{}, crewDir, true},
		{map[string]string{}, "/town/gastown/crew/sloan-cmd-sling/internal/cmd", true},
		{map[string]string{"BD_ACTOR": "gastown/crew/sloan"}, townDir, true},
		{map[string]string{"GT_ROLE": "crew"}, townDir, true},
		{map[string]string{"BD_ACTOR": "gastown/crew/sloan", "GT_ROLE": "gastown/crew/sloan"}, crewDir, true},
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
		{"empty assignee", "", "Sloan Ahrens", map[string]bool{}},
		{"submitter as assignee", "gastown/polecats/agate", "gastown/polecats/agate", map[string]bool{"gt-agate": true}},
		{"non-polecat assignee", "gastown/crew/sloan", "Sloan Ahrens", map[string]bool{}},
		{"polecat with no session", "gastown/polecats/agate", "Sloan Ahrens", map[string]bool{}},
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
		"gastown/crew/sloan":     "",
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
		"crew/sloan/gt-3e7tk-crew-submit": "gt-3e7tk",
		"crew/sloan/crew-done-submit":     "",
		"crew/sloan/fix":                  "",
		"gt-abc.2":                        "gt-abc.2",
		// parseBranchName reads only a bare id; the embedded fallback finds
		// the routed id inside a crew slug, and drops an unrouted one.
		"crew/sloan/gt-3e7tk.2-crew-submit": "gt-3e7tk.2",
		"feature/xx-abc-thing":              "",
	} {
		if got := crewBeadFromBranch(branch, routed); got != want {
			t.Errorf("crewBeadFromBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}
