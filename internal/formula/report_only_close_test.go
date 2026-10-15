package formula

import (
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
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
		"File the CONFIRMED findings",
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

// TestDeepReviewVerifierOverflowIsRecorded guards gt-97dew: a finding past the
// verifiers' capacity is verified in P1, P2, P3 order and the rest is queued,
// not dropped, because the 21-day file skip keeps a later run from re-reading
// the file and finding it again.
func TestDeepReviewVerifierOverflowIsRecorded(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "left out of\nthis run and unrecorded") {
		t.Error("deep-review-run still drops a finding past the verifiers' capacity unrecorded")
	}
	for _, want := range []string{
		"all P1, then all P2, then all P3",
		"A finding left past capacity is never dropped",
		"a `queued:` line per unverified finding",
		"takes a `queued:` line with its own\nseverity, whatever that is",
		"Never leave a finding\nunrecorded because it is a P3",
		"`unverified=<n>`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deep-review-run lacks the overflow rule %q", want)
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

// TestDeepReviewSpendsFewerTokensPerFinding guards gt-3dozz: the slice is a byte
// budget, P3 findings are batched through the script instead of filed one by
// one, reviewers grep tests instead of reading them whole, and a run where no
// finding was refuted is flagged.
func TestDeepReviewSpendsFewerTokensPerFinding(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "read each whole file with its tests") {
		t.Error("deep-review-run still has reviewers read whole test files")
	}
	for _, want := range []string{
		"--slice-bytes {{slice_bytes}}",
		"grep -n <symbol>",
		"one subagent per three findings, at most {{max_verifiers}}",
		"refuted=<n> of <total>",
		"UNCHALLENGED: no finding was refuted",
		"p3: <YYYY-MM-DD> <fingerprint> <file:line> <claim>",
		"bash /tmp/deep-review-{{issue}}/deep-review.sh batches --prev-notes $D/prev-notes.txt",
		"batch beads included",
		"reviewed-lines.txt",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deep-review-run lacks %q", want)
		}
	}
	// The cursor stays the last ledger line, after the reviewed: notes.
	if strings.Index(text, "reviewed-lines.txt") > strings.Index(text, "**9. Commit the cursor, last**") {
		t.Error("deep-review-run writes the reviewed: lines after the cursor")
	}

	mol, err := GetEmbeddedFormulaContent("mol-deep-review")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	var f struct {
		Vars map[string]struct {
			Default string `toml:"default"`
		} `toml:"vars"`
	}
	if _, err := toml.Decode(string(mol), &f); err != nil {
		t.Fatalf("decode mol-deep-review: %v", err)
	}
	for name, want := range map[string]string{
		"slice_bytes":   "250000",
		"slice_files":   "40",
		"max_reviewers": "4",
		"max_verifiers": "4",
		"max_beads":     "5",
	} {
		if got := f.Vars[name].Default; got != want {
			t.Errorf("mol-deep-review var %s default = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(string(mol), "| slice_bytes |") {
		t.Error("mol-deep-review's variable table lacks slice_bytes")
	}
}

// TestDeepReviewRunsAsSliceOrLandingsJob guards gt-32nag: one formula serves
// two scheduled jobs. The mode and run_label vars reach the plan and the
// ledger read, a slice run moves no landing cursor, and the run bead's label
// comes from the sling, not from the formula.
func TestDeepReviewRunsAsSliceOrLandingsJob(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "scheduled:deep-review") {
		t.Error("deep-review-run hardcodes the run label instead of {{run_label}}")
	}
	for _, want := range []string{
		"bd list -l {{run_label}} --all",
		"--mode {{mode}}",
		"Mode `slice` also skips step 8 and step 9's `cursor:` line: no landing cursor moves.",
		"`{{dry_run}}` is `true` or the mode is `slice`",
		"**9. Commit the cursor, last** (not in mode `slice`)",
		"the `cursor:` line (none in mode\n`slice`)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deep-review-run lacks %q", want)
		}
	}

	mol, err := GetEmbeddedFormulaContent("mol-deep-review")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	var f struct {
		Vars map[string]struct {
			Default string `toml:"default"`
		} `toml:"vars"`
	}
	if _, err := toml.Decode(string(mol), &f); err != nil {
		t.Fatalf("decode mol-deep-review: %v", err)
	}
	for name, want := range map[string]string{
		"mode":      "both",
		"run_label": "scheduled:deep-review",
	} {
		if got := f.Vars[name].Default; got != want {
			t.Errorf("mol-deep-review var %s default = %q, want %q", name, got, want)
		}
		if !strings.Contains(string(mol), "| "+name+" |") {
			t.Errorf("mol-deep-review's variable table lacks %s", name)
		}
	}
}

// TestDeepReviewReviewsLivingDocsForDrift guards gt-32nag: the slice run reads
// the docs the plan chose, reports each contradiction as a P3 doc-drift
// finding that carries its replacement text, never reviews a historical doc,
// files doc-drift findings as a docs-only batch, and notes each doc it read.
func TestDeepReviewReviewsLivingDocsForDrift(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"the docs in `$D/docs.txt`",
		"every behavioral claim, command, flag and\n  path against the code on origin/{{base_branch}}",
		"kind `doc-drift`, severity P3, fingerprint `<doc>:<short claim anchor>:doc-drift`",
		"`-> <exact replacement text>`",
		"docs/plans, docs/design,\n  docs/adr and docs/research are historical: never review them for drift",
		"A `doc-drift` bead's",
		"`## Gate` says docs-only change; `## Constraints` forbid touching code",
		"the file:line that contradicts it and the\nreplacement text",
		"`reviewed: <date> <doc>`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deep-review-run lacks %q", want)
		}
	}
	// The doc reviewed: notes are written before the cursor, as the code ones are.
	if strings.Index(text, "`reviewed: <date> <doc>`") > strings.Index(text, "**9. Commit the cursor, last**") {
		t.Error("deep-review-run notes the reviewed docs after the cursor")
	}
}

// TestDeepReviewFingerprintGrammarHasNoSpaces guards gt-e39z4: the first
// doc-drift run wrote queued: lines whose fingerprint carried spaces in its
// claim anchor (docs/testing.md:gate row runs budget runner:doc-drift), and
// every later run stopped reading the ledger. The formula states the one-token
// grammar and gives a doc-drift form an agent can copy.
func TestDeepReviewFingerprintGrammarHasNoSpaces(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	grammar := regexp.MustCompile("A fingerprint is `<file>:<symbol>:<kind>`, one token, no\\s+spaces")
	if !grammar.MatchString(text) {
		t.Error("deep-review-run does not state that a fingerprint is one token with no spaces")
	}
	examples := regexp.MustCompile("`(docs/[^`]*:doc-drift)`").FindAllStringSubmatch(text, -1)
	if len(examples) == 0 {
		t.Fatal("deep-review-run gives no doc-drift fingerprint example")
	}
	for _, e := range examples {
		if strings.ContainsAny(e[1], " \t\n") {
			t.Errorf("the doc-drift fingerprint example %q has whitespace; the line an agent writes from it fails the ledger read", e[1])
		}
	}
}
