package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/mail"
)

type fakeUnitCycle struct {
	sent        []*mail.Message
	sendErr     error
	inbox       []*mail.Message
	archived    []string
	comments    []string
	tempDeleted []string
	patrolSum   []string
	patrolErr   error
	respawned   int
	respawnErr  error
	escalations []string
	recorded    int
	paneSession string
	handoffAge  time.Duration
	hasHandoff  bool
	env         map[string]string
}

func (f *fakeUnitCycle) deps(out *bytes.Buffer) unitCycleDeps {
	return unitCycleDeps{
		Send:        func(m *mail.Message) error { f.sent = append(f.sent, m); return f.sendErr },
		ListInbox:   func() ([]*mail.Message, error) { return f.inbox, nil },
		Archive:     func(id string) error { f.archived = append(f.archived, id); return nil },
		AddComment:  func(id, text string) error { f.comments = append(f.comments, id+": "+text); return nil },
		DeleteTemp:  func(dir string) error { f.tempDeleted = append(f.tempDeleted, dir); return nil },
		ClosePatrol: func(summary string) error { f.patrolSum = append(f.patrolSum, summary); return f.patrolErr },
		Respawn:     func() error { f.respawned++; return f.respawnErr },
		Escalate:    func(fp, sev, msg string) { f.escalations = append(f.escalations, fp+"|"+sev) },
		RecordCycle: func() { f.recorded++ },
		PaneSession: func() (string, error) { return f.paneSession, nil },
		HandoffAge:  func() (time.Duration, bool) { return f.handoffAge, f.hasHandoff },
		Getenv:      func(k string) string { return f.env[k] },
		Out:         out,
	}
}

func refineryFake() *fakeUnitCycle {
	return &fakeUnitCycle{
		paneSession: "gt-refinery",
		env:         map[string]string{"GT_ROLE": "gastown/refinery", "TMUX_PANE": "%7"},
		inbox: []*mail.Message{
			{ID: "hq-m1", Subject: "MERGE_READY nux", Body: "Branch: polecat/nux/gt-1+ab\nIssue: gt-1\n"},
			{ID: "hq-m2", Subject: "MERGE_READY opal", Body: "Branch: polecat/opal/gt-2+cd\nIssue: gt-2\n"},
		},
	}
}

func singleParams() unitCycleParams {
	return unitCycleParams{
		Rig: "gastown", Mode: unitSingle,
		RefinerySession: "gt-refinery",
		WorkDir:         "/town/gastown/refinery/rig",
		MRs:             []unitMR{{ID: "gt-mr1", Branch: "polecat/nux/gt-1+ab", Worker: "polecats/nux", SourceIssue: "gt-1", Target: "main"}},
		MergeCommit:     "abc123",
		CycleEnabled:    true,
	}
}

func TestCompleteUnitAndCycle_SingleDoesChoresPatrolAndRespawn(t *testing.T) {
	f := refineryFake()
	var out bytes.Buffer
	p := singleParams()
	p.LandedCommitAttested = true
	rep := completeUnitAndCycle(p, f.deps(&out))

	if len(f.sent) != 1 || f.sent[0].Subject != "MERGED nux" || f.sent[0].To != "gastown/witness" {
		t.Fatalf("MERGED sends = %+v, want one MERGED nux to gastown/witness", f.sent)
	}
	if !strings.Contains(f.sent[0].Body, "Merge-Commit: abc123") {
		t.Fatalf("MERGED body missing merge commit: %q", f.sent[0].Body)
	}
	if len(f.archived) != 1 || f.archived[0] != "hq-m1" {
		t.Fatalf("archived = %v, want only hq-m1 (the MR's own MERGE_READY)", f.archived)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "gt-mr1: post-merge: attested --landed-commit abc123") {
		t.Fatalf("attestation comments = %v", f.comments)
	}
	if len(f.tempDeleted) != 1 || f.tempDeleted[0] != "/town/gastown/refinery/rig" {
		t.Fatalf("temp deleted in %v, want the refinery worktree", f.tempDeleted)
	}
	if len(f.patrolSum) != 1 || f.patrolSum[0] != "unit landed: gt-mr1 @ abc123" {
		t.Fatalf("patrol summaries = %v", f.patrolSum)
	}
	if f.respawned != 1 || f.recorded != 1 || !rep.Respawned || rep.SkipCause != "" {
		t.Fatalf("respawned=%d recorded=%d report=%+v, want one respawn", f.respawned, f.recorded, rep)
	}
	for _, line := range []string{"✓ MERGED sent to gastown/witness", "✓ MERGE_READY archived: hq-m1", "✓ temp branch deleted"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("output missing %q:\n%s", line, out.String())
		}
	}
}

func TestCompleteUnitAndCycle_NoAttestationWithoutLandedCommit(t *testing.T) {
	f := refineryFake()
	completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if len(f.comments) != 0 {
		t.Fatalf("comments = %v, want none when --landed-commit was not used", f.comments)
	}
}

func TestCompleteUnitAndCycle_NonPolecatBranchGetsNoMERGED(t *testing.T) {
	f := refineryFake()
	p := singleParams()
	p.MRs[0].Branch = "crew/sloan/fix"
	completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
	if len(f.sent) != 0 {
		t.Fatalf("sent %d MERGED for a non-polecat branch; the witness has no worktree to reap", len(f.sent))
	}
}

func TestCompleteUnitAndCycle_NoMergeReadyMailIsReported(t *testing.T) {
	f := refineryFake()
	f.inbox = nil
	var out bytes.Buffer
	completeUnitAndCycle(singleParams(), f.deps(&out))
	if len(f.archived) != 0 {
		t.Fatalf("archived %v from an empty inbox", f.archived)
	}
	if !strings.Contains(out.String(), "○ no MERGE_READY mail for gt-mr1") {
		t.Fatalf("output does not say MERGE_READY was not found:\n%s", out.String())
	}
}

func TestCompleteUnitAndCycle_BatchSkipsMERGEDAndTempButArchivesEach(t *testing.T) {
	f := refineryFake()
	p := singleParams()
	p.Mode = unitBatch
	p.MRs = []unitMR{
		{ID: "gt-mr1", Branch: "polecat/nux/gt-1+ab", Worker: "polecats/nux"},
		{ID: "gt-mr2", Branch: "polecat/opal/gt-2+cd", Worker: "polecats/opal"},
	}
	completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
	if len(f.sent) != 0 {
		t.Fatalf("batch sent %d MERGED; HandleMRInfoSuccess already sent them (gt-9gjl)", len(f.sent))
	}
	if len(f.tempDeleted) != 0 {
		t.Fatalf("batch deleted temp in %v; batch never creates temp", f.tempDeleted)
	}
	if strings.Join(f.archived, ",") != "hq-m1,hq-m2" {
		t.Fatalf("archived = %v, want both members' MERGE_READY", f.archived)
	}
	if len(f.patrolSum) != 1 || f.patrolSum[0] != "unit landed: gt-mr1,gt-mr2 @ abc123" || f.respawned != 1 {
		t.Fatalf("batch patrol=%v respawned=%d, want one patrol close and one respawn per batch", f.patrolSum, f.respawned)
	}
}

func TestCompleteUnitAndCycle_GuardsSkipPatrolAndRespawnButKeepChores(t *testing.T) {
	cases := map[string]func(p *unitCycleParams, f *fakeUnitCycle){
		"human caller":       func(p *unitCycleParams, f *fakeUnitCycle) { f.env["GT_ROLE"] = "gastown/crew/sloan" },
		"other rig refinery": func(p *unitCycleParams, f *fakeUnitCycle) { f.env["GT_ROLE"] = "hm/refinery" },
		"not in tmux":        func(p *unitCycleParams, f *fakeUnitCycle) { delete(f.env, "TMUX_PANE") },
		"wrong pane session": func(p *unitCycleParams, f *fakeUnitCycle) { f.paneSession = "gt-crew-sloan" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := refineryFake()
			p := singleParams()
			mutate(&p, f)
			rep := completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
			if len(f.sent) != 1 || len(f.archived) != 1 {
				t.Fatalf("chores skipped: sent=%d archived=%d; chores always run", len(f.sent), len(f.archived))
			}
			if len(f.patrolSum) != 0 {
				t.Fatalf("closed the refinery patrol from a non-refinery caller: %v", f.patrolSum)
			}
			if f.respawned != 0 || rep.Respawned {
				t.Fatal("respawned from a non-refinery caller")
			}
			if rep.SkipCause == "" {
				t.Fatal("guard skip left SkipCause empty; a skip must never be silent")
			}
		})
	}
}

func TestCompleteUnitAndCycle_FlagOffOrCooldownClosesPatrolButNoRespawn(t *testing.T) {
	for name, mutate := range map[string]func(p *unitCycleParams, f *fakeUnitCycle){
		"flag off": func(p *unitCycleParams, f *fakeUnitCycle) { p.CycleEnabled = false },
		"cooldown": func(p *unitCycleParams, f *fakeUnitCycle) { f.hasHandoff = true; f.handoffAge = 30 * time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			f := refineryFake()
			p := singleParams()
			mutate(&p, f)
			rep := completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
			if len(f.patrolSum) != 1 {
				t.Fatalf("patrol not closed: %v", f.patrolSum)
			}
			if f.respawned != 0 || rep.Respawned || f.recorded != 0 {
				t.Fatal("respawned despite flag off / cooldown")
			}
			if rep.SkipCause == "" {
				t.Fatal("skip left SkipCause empty")
			}
		})
	}
}

func TestCompleteUnitAndCycle_OldHandoffIsNotCooldown(t *testing.T) {
	f := refineryFake()
	f.hasHandoff, f.handoffAge = true, 10*time.Minute
	completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if f.respawned != 1 {
		t.Fatalf("respawned=%d, want 1 when the last handoff is past MinHandoffCooldown", f.respawned)
	}
}

func TestCompleteUnitAndCycle_FailedMERGEDEscalatesAndOtherChoresRun(t *testing.T) {
	f := refineryFake()
	f.sendErr = errors.New("dolt down")
	completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if len(f.escalations) != 1 || f.escalations[0] != "refinery-unit-chore:gastown|medium" {
		t.Fatalf("escalations = %v, want one refinery-unit-chore:gastown", f.escalations)
	}
	if len(f.archived) != 1 || len(f.tempDeleted) != 1 || f.respawned != 1 {
		t.Fatal("a failed MERGED send stopped the other chores or the cycle")
	}
}

func TestCompleteUnitAndCycle_PatrolFailureStillRespawns(t *testing.T) {
	f := refineryFake()
	f.patrolErr = errors.New("bd timeout")
	completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if f.respawned != 1 {
		t.Fatal("patrol close failure blocked the respawn; the successor finds the old wisp as today")
	}
	if len(f.escalations) != 0 {
		t.Fatalf("patrol failure escalated %v; the patrol watchdog owns stuck wisps", f.escalations)
	}
}

func TestCompleteUnitAndCycle_RespawnFailureEscalates(t *testing.T) {
	f := refineryFake()
	f.respawnErr = errors.New("no server")
	rep := completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if rep.Respawned {
		t.Fatal("report claims a respawn that failed")
	}
	if len(f.escalations) != 1 || f.escalations[0] != "refinery-respawn-failed:gastown|medium" {
		t.Fatalf("escalations = %v", f.escalations)
	}
}

func TestCompleteUnitAndCycle_BatchIsOneUnit(t *testing.T) {
	f := refineryFake()
	p := singleParams()
	p.Mode = unitBatch
	p.MRs = []unitMR{{ID: "a", Branch: "polecat/x/1"}, {ID: "b", Branch: "polecat/y/2"}, {ID: "c", Branch: "polecat/z/3"}}
	completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
	if len(f.patrolSum) != 1 || f.respawned != 1 {
		t.Fatalf("3-member batch closed %d patrols and respawned %d times, want 1 and 1", len(f.patrolSum), f.respawned)
	}
}
