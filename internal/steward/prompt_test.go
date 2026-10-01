package steward

import (
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

const testHead = "0123456789abcdef0123456789abcdef01234567"

func rejectionEvent(detail string) Event {
	return Event{
		Kind: KindRejection, Rig: "gastown", Bead: "gt-x", Branch: "polecat/emerald/gt-x", Head: testHead,
		Target: "main", Worker: "emerald", Attempt: 2, RejectionDetail: detail,
	}
}

func mustPrompt(t *testing.T, ev Event, final bool) string {
	t.Helper()
	p, err := PromptFor(ev, final)
	if err != nil {
		t.Fatalf("PromptFor(%s, final=%v): %v", ev.Kind, final, err)
	}
	return p
}

func requireAll(t *testing.T, name, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s lacks %q:\n%s", name, want, text)
		}
	}
}

// TestReviewPromptCarriesTheOverseersProcedure: each check of the runbook's
// "Reviewing submissions" is in the instruction, with the verdict words the
// bead's readers search for.
func TestReviewPromptCarriesTheOverseersProcedure(t *testing.T) {
	t.Parallel()
	p := mustPrompt(t, reviewEvent("gt-x", testHead), false)
	requireAll(t, "review prompt", p,
		ResultFile, "gt-x", testHead, "polecat/emerald/gt-x",
		"git merge --no-edit origin/main",
		"go build ./...", "make gate",
		"co-authored|anthropic|generated with",
		"func Test", "baseline",
		"STEWARD REVIEW PASS", "STEWARD REVIEW FAIL",
		"gt bead comment gt-x",
	)
	if strings.Contains(p, "git push") {
		t.Errorf("the review job is told to push:\n%s", p)
	}
}

// TestRejectionPromptCarriesTheRecoveryProcedure: the runbook's "When a
// landing is rejected" paths are all in the instruction.
func TestRejectionPromptCarriesTheRecoveryProcedure(t *testing.T) {
	t.Parallel()
	p := mustPrompt(t, rejectionEvent("kind=gate reason=gofmt"), false)
	requireAll(t, "rejection prompt", p,
		"attempt 2", "kind=gate reason=gofmt", ResultFile, "gt-x", testHead,
		"gt sling gt-x gastown --force --stdin",
		"git merge --no-edit origin/main",
		"make lint", "make gate",
		"git push origin HEAD:refs/heads/polecat/emerald/gt-x",
		"STEWARD FIX",
		land.LabelReadyToLand, "--remove-label rework",
	)
	for _, kind := range []land.RejectionKind{land.RejectReview, land.RejectConflict, land.RejectGate, land.RejectTimeout, land.RejectEmpty, land.RejectNotPushed} {
		if !strings.Contains(p, "**"+string(kind)+"**") {
			t.Errorf("rejection prompt names no handling for kind %q", kind)
		}
	}
}

// TestRequeueBlockIsOneTheLandingWorkerReads: the READY TO LAND block the
// rejection job is told to write parses back to the branch, head and target
// it was written for. A drifted field name would leave the bead labelled
// ready with nothing to land.
func TestRequeueBlockIsOneTheLandingWorkerReads(t *testing.T) {
	t.Parallel()
	p := mustPrompt(t, rejectionEvent("kind=conflict reason=main moved"), false)
	start := strings.Index(p, `"`+land.ReadyNoteMarker)
	if start < 0 {
		t.Fatalf("no READY TO LAND block in the prompt:\n%s", p)
	}
	block := p[start+1:]
	block = block[:strings.Index(block, `"`)]
	block = strings.ReplaceAll(block, "$(git rev-parse HEAD)", testHead)
	block = regexp.MustCompile(`\$\(date[^)]*\)`).ReplaceAllString(block, "2026-10-01T12:00:00Z")
	w, ok := land.ParseReadyNote(block)
	if !ok {
		t.Fatalf("the landing worker cannot read the block:\n%s", block)
	}
	if w.Branch != "polecat/emerald/gt-x" || w.Head != testHead || w.Target != "main" || w.Worker != "emerald" || w.Submitted.IsZero() {
		t.Errorf("block reads back as %+v", w)
	}
}

// TestPromptEscalationContract: a routine run hands a doubt to the stronger
// model and files nothing; the final run files the escalation, so exactly one
// escalation exists per stuck event (gt-9bioi.2).
func TestPromptEscalationContract(t *testing.T) {
	t.Parallel()
	for _, ev := range []Event{reviewEvent("gt-x", testHead), rejectionEvent("kind=gate reason=x")} {
		routine := mustPrompt(t, ev, false)
		requireAll(t, string(ev.Kind)+" routine prompt", routine, "needs-opus:")
		if strings.Contains(routine, "gt escalate") {
			t.Errorf("%s routine prompt files an escalation:\n%s", ev.Kind, routine)
		}
		final := mustPrompt(t, ev, true)
		requireAll(t, string(ev.Kind)+" final prompt", final, "gt escalate", "--related gt-x", "outcome `escalated`")
		if strings.Contains(final, "needs-opus") {
			t.Errorf("%s final prompt hands off to a model that never runs:\n%s", ev.Kind, final)
		}
	}
}

// TestPromptAuthority: no variant tells a job to push main, grant a revert,
// or close the bead; each names the one branch it may push.
func TestPromptAuthority(t *testing.T) {
	t.Parallel()
	for _, ev := range []Event{reviewEvent("gt-x", testHead), rejectionEvent("kind=conflict reason=x")} {
		for _, final := range []bool{false, true} {
			p := mustPrompt(t, ev, final)
			for _, banned := range []string{"push origin main", "refs/heads/main", "--allow-reverts", "bd close", "gt bead close", "--force-with-lease", "git push -f"} {
				if strings.Contains(p, banned) {
					t.Errorf("%s prompt (final=%v) contains %q", ev.Kind, final, banned)
				}
			}
			if ev.Kind == KindRejection {
				requireAll(t, "rejection prompt", p, "Co-Authored-By trailer")
			}
		}
	}
}

// TestPromptOutcomesAreRecordable: every outcome a prompt offers is one the
// runner accepts, so a job following its instructions never writes a verdict
// that records as an error.
func TestPromptOutcomesAreRecordable(t *testing.T) {
	t.Parallel()
	line := regexp.MustCompile("Outcomes for this job: (.*)\n?")
	word := regexp.MustCompile("`(\\w+)`")
	for _, ev := range []Event{reviewEvent("gt-x", testHead), rejectionEvent("kind=gate reason=x")} {
		p := mustPrompt(t, ev, false)
		m := line.FindStringSubmatch(p)
		if m == nil {
			t.Fatalf("%s prompt lists no outcomes:\n%s", ev.Kind, p)
		}
		words := word.FindAllStringSubmatch(m[1], -1)
		if len(words) == 0 {
			t.Fatalf("%s prompt's outcome line names none: %q", ev.Kind, m[0])
		}
		for _, w := range words {
			if !Outcome(w[1]).Valid() {
				t.Errorf("%s prompt offers outcome %q, which the runner rejects", ev.Kind, w[1])
			}
		}
	}
}

// TestPromptRejectsAnUnknownKind: a kind with no template is an error the
// daemon logs, not an empty prompt a job runs on.
func TestPromptRejectsAnUnknownKind(t *testing.T) {
	t.Parallel()
	if p, err := PromptFor(Event{Kind: "other", Bead: "gt-x"}, false); err == nil {
		t.Errorf("PromptFor accepted kind %q: %q", "other", p)
	}
}
