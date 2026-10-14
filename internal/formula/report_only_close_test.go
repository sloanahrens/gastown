package formula

import (
	"strings"
	"testing"
)

// TestReportOnlyFormulasCloseTheirBeadBeforeDeferred guards gt-hqbkr: gt done
// --status DEFERRED keeps a non-workflow bead hooked, so a finished report-only
// run that does not close its own bead leaves it hooked and the patrol reads
// the retired session as a crash. Each report-only submit step closes the bead
// with gt bead close (the only bd verb agent prose may run to close) as its
// last action before gt done.
func TestReportOnlyFormulasCloseTheirBeadBeforeDeferred(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"mol-deep-review", "mol-doc-audit"} {
		t.Run(name, func(t *testing.T) {
			raw, err := GetEmbeddedFormulaContent(name)
			if err != nil {
				t.Fatalf("GetEmbeddedFormulaContent(%q): %v", name, err)
			}
			text := string(raw)
			const closeCmd = `gt bead close {{issue}} --reason="no-changes:`
			closeAt := strings.Index(text, closeCmd)
			if closeAt < 0 {
				t.Fatalf("%s lacks %q", name, closeCmd)
			}
			deferredAt := strings.Index(text[closeAt:], "gt done --status DEFERRED")
			if deferredAt < 0 {
				t.Fatalf("%s runs no gt done --status DEFERRED after its bead close", name)
			}
			if between := text[closeAt : closeAt+deferredAt]; strings.Contains(between, "gt done") {
				t.Errorf("%s runs a gt done between the bead close and the DEFERRED exit", name)
			}
		})
	}
}

// TestDeepReviewQuietDayReachesTheClosingSubmitStep checks that the quiet-day
// shortcut in the work step sends the run to the submit step, which holds the
// close, rather than straight to gt done.
func TestDeepReviewQuietDayReachesTheClosingSubmitStep(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "skip to the submit step, which closes {{issue}}") {
		t.Error("deep-review-run's quiet-day path does not route through the closing submit step")
	}
	if strings.Contains(text, "Then skip to the submit step (`gt done --status DEFERRED`)") {
		t.Error("deep-review-run's quiet-day path still names a bare gt done --status DEFERRED")
	}
}

// TestDeepReviewFilesEveryConfirmedFinding guards gt-j6phl: the run files each
// confirmed finding itself, so no severity waits on a human approval, and the
// queued ledger line carries every severity the filing step does.
func TestDeepReviewFilesEveryConfirmedFinding(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	for _, retired := range []string{
		"P3: digest only",     // the tier that left a confirmed finding unfiled
		"P3 for approval",     // its digest section
		"file 1,3,4",          // the comment a human approved it with
		"filed from approval", // and how the next run picked it up
	} {
		if strings.Contains(text, retired) {
			t.Errorf("deep-review-run still carries the retired approval path: %q", retired)
		}
	}
	// gt-oedvw: no path keeps a finding off the dispatcher; a risk path is
	// named in the bead body instead.
	if strings.Contains(strings.ToLower(text), "crew-only") {
		t.Error("deep-review-run still routes findings with the crew-only label")
	}
	for _, want := range []string{
		"File every CONFIRMED finding",
		"-p <1|2|3>",
		"-l review-deep",
		"names that risk path in `## Constraints`",
		"queued: <fingerprint> <P1|P2|P3> <file:line> <claim>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deep-review-run lacks %q", want)
		}
	}
}

// TestDeepReviewLedgerLoadRunsInAPolecatSession guards gt-balyj: step 1 runs as
// written under BD_MACHINE=1 (bd --json returns a {schema_version, data}
// envelope) and past the polecat-paths guard (it refuses a mkdir or a redirect
// whose target is built from a variable).
func TestDeepReviewLedgerLoadRunsInAPolecatSession(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	start := strings.Index(text, "**1. Load the ledger and plan.")
	end := strings.Index(text, "**2. A quiet day ends here.")
	if start < 0 || end < start {
		t.Fatal("deep-review-run: step 1 not found")
	}
	step1 := text[start:end]
	if !strings.Contains(step1, `(if type == "object" then .data else . end)[]`) {
		t.Error("step 1's jq does not accept both the bare array and the BD_MACHINE=1 envelope")
	}
	for _, bad := range []string{"mkdir -p $D", "> $D/", "--out $D "} {
		if strings.Contains(step1, bad) {
			t.Errorf("step 1 builds a mkdir or redirect target from $D (%q); the polecat-paths guard refuses it", bad)
		}
	}
	for _, want := range []string{
		"mkdir -p /tmp/deep-review-{{issue}}\n",
		"> /tmp/deep-review-{{issue}}/prev-notes.txt",
	} {
		if !strings.Contains(step1, want) {
			t.Errorf("step 1 lacks the literal-path form %q", want)
		}
	}
}
