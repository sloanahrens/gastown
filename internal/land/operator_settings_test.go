package land

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// testOperator is the operator identity the internal/land tests configure and
// write their comment fixtures under. It is a fixture name, not a person.
const testOperator = "Test Operator"

// testOperatorConfig is the operator config a test hands the entitlement, read
// from a temporary town settings file through the loader the lander reads.
func testOperatorConfig(t *testing.T) *config.OperatorConfig {
	t.Helper()
	return operatorConfigFor(t, testOperator)
}

// operatorConfigFor writes a town settings file naming names and reads it back
// through config.LoadOperatorConfig, so what a test entitles is the setting the
// production path reads rather than a literal in the test (gt-ywuip).
func operatorConfigFor(t *testing.T, names ...string) *config.OperatorConfig {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	doc, err := json.Marshal(map[string]any{"operator": map[string]any{"names": names}})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := os.WriteFile(config.TownSettingsPath(townRoot), doc, 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	op, err := config.LoadOperatorConfig(townRoot)
	if err != nil {
		t.Fatalf("LoadOperatorConfig: %v", err)
	}
	if op == nil {
		t.Fatal("LoadOperatorConfig returned no operator section for a settings file that has one")
	}
	return op
}

// The wrappers below are the production entitlement calls a test drives, with
// the operator identities the test configures.

func beadBrief(t *testing.T, description string, comments []beads.Comment) string {
	t.Helper()
	return BeadBrief(description, comments, testOperatorConfig(t))
}

func writeBeadBrief(t *testing.T, dir, description string, comments []beads.Comment) (string, error) {
	t.Helper()
	return WriteBeadBrief(dir, description, comments, testOperatorConfig(t))
}

func acceptedRisksText(t *testing.T, comments []beads.Comment) string {
	t.Helper()
	return AcceptedRisksText(comments, testOperatorConfig(t))
}

func acceptedRisksUsed(t *testing.T, comments []beads.Comment) []AcceptedRisk {
	t.Helper()
	return AcceptedRisksUsed(comments, testOperatorConfig(t))
}

func findingOutcome(t *testing.T, prev Outcome, class Classification, comments []beads.Comment, key FindingKey) Outcome {
	t.Helper()
	return FindingOutcome(prev, class, comments, key, testOperatorConfig(t))
}

// TestDecisionAuthorsFromSettings: the openers select a comment only for an
// author the town settings name as the operator's, the canonical OPERATOR
// DECISION and the legacy SLOAN DECISION alike, and the same text by an
// unlisted name is not a decision (gt-ywuip).
func TestDecisionAuthorsFromSettings(t *testing.T) {
	t.Parallel()
	const at = "2026-10-26T20:00:00Z"
	// The literals pin what existing bead comments carry: the canonical opener
	// and the legacy spelling the accepted-legacy list still honours.
	const (
		canonicalOpener = "OPERATOR DECISION"
		legacyOpener    = "SLOAN DECISION"
	)
	if operatorDecisionOpener != canonicalOpener {
		t.Fatalf("operatorDecisionOpener = %q, want %q", operatorDecisionOpener, canonicalOpener)
	}
	if !slices.Contains(legacyDecisionOpeners, legacyOpener) {
		t.Fatalf("legacyDecisionOpeners = %q, want the legacy opener %q", legacyDecisionOpeners, legacyOpener)
	}
	op := testOperatorConfig(t)
	for _, tc := range []struct {
		name   string
		author string
		opener string
		want   bool
	}{
		{name: "a listed name, canonical opener", author: testOperator, opener: canonicalOpener, want: true},
		{name: "a listed name, legacy opener", author: testOperator, opener: legacyOpener, want: true},
		{name: "an unlisted name, canonical opener", author: "gastown/polecats/coma", opener: canonicalOpener, want: false},
		{name: "an unlisted name, legacy opener", author: "gastown/polecats/coma", opener: legacyOpener, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text := tc.opener + ": keep the cap"
			got := BeadBrief("", []beads.Comment{{Author: tc.author, Text: text, CreatedAt: at}}, op)
			if carried := strings.Contains(got, "> "+text); carried != tc.want {
				t.Errorf("BeadBrief carried a %s comment by %q = %v, want %v:\n%s", tc.opener, tc.author, carried, tc.want, got)
			}
		})
	}
}

// TestOperatorNamesUnreadableSettingsFailsClosedAndLogs: a landing whose town
// settings file does not parse still lands, but entitles no operator decision
// or accepted risk — the overseer's accepted risk still counts — and the log
// names the file it could not read once (gt-ywuip).
func TestOperatorNamesUnreadableSettingsFailsClosedAndLogs(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	if err := f.bd.AddCommentAs(f.work.BeadID, testOperator, "OPERATOR DECISION: keep the cap"); err != nil {
		t.Fatal(err)
	}
	if err := f.bd.AddCommentAs(f.work.BeadID, testOperator, "ACCEPTED-RISK: the operator approved the mount"); err != nil {
		t.Fatal(err)
	}
	if err := f.bd.AddCommentAs(f.work.BeadID, "town/crew/overseer", "ACCEPTED-RISK: the overseer approved the mount"); err != nil {
		t.Fatal(err)
	}
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	l.TownRoot = townRoot
	var out strings.Builder
	l.Out = &out
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v; unreadable settings must not cost the landing", err)
	}
	if len(rev.inputs) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(rev.inputs))
	}
	brief := rev.inputs[0].BriefText
	if strings.Contains(brief, "keep the cap") {
		t.Errorf("an unreadable setting still entitled the operator decision:\n%s", brief)
	}
	risks := rev.inputs[0].AcceptedRisksText
	if strings.Contains(risks, "the operator approved the mount") {
		t.Errorf("an unreadable setting still entitled the operator's accepted risk: %q", risks)
	}
	if !strings.Contains(risks, "the overseer approved the mount") {
		t.Errorf("the overseer's accepted risk was not carried: %q", risks)
	}
	log := out.String()
	if got := strings.Count(log, "could not read the operator identities"); got != 1 {
		t.Errorf("the failed setting read is logged %d times, want once:\n%s", got, log)
	}
	if !strings.Contains(log, "config.json") {
		t.Errorf("the landing log does not name the settings file it could not read:\n%s", log)
	}
}

// TestOperatorNamesAbsentKeyIsEmptyAndQuiet: a readable settings file with no
// operator key is an empty list, not a failed read: no author is entitled and
// the landing log carries no warning (gt-ywuip).
func TestOperatorNamesAbsentKeyIsEmptyAndQuiet(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	if err := f.bd.AddCommentAs(f.work.BeadID, testOperator, "OPERATOR DECISION: keep the cap"); err != nil {
		t.Fatal(err)
	}
	if err := f.bd.AddCommentAs(f.work.BeadID, testOperator, "ACCEPTED-RISK: the operator approved the mount"); err != nil {
		t.Fatal(err)
	}
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte(`{"type":"town-settings","version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	l.TownRoot = townRoot
	var out strings.Builder
	l.Out = &out
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(rev.inputs) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(rev.inputs))
	}
	brief := rev.inputs[0].BriefText
	if strings.Contains(brief, "keep the cap") {
		t.Errorf("an absent setting still entitled the operator decision:\n%s", brief)
	}
	if got := rev.inputs[0].AcceptedRisksText; strings.Contains(got, "the operator approved the mount") {
		t.Errorf("an absent setting still entitled the operator's accepted risk: %q", got)
	}
	if log := out.String(); strings.Contains(log, "could not read the operator identities") {
		t.Errorf("an absent key is not a failed read, but the log warns:\n%s", log)
	}
}

// TestLanderWithoutTownRootEntitlesNothingAndLogsOnce: a lander that was not
// told its town root entitles no operator identity and says why once, since
// there is no settings file to name (gt-ywuip).
func TestLanderWithoutTownRootEntitlesNothingAndLogsOnce(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	if err := f.bd.AddCommentAs(f.work.BeadID, testOperator, "OPERATOR DECISION: keep the cap"); err != nil {
		t.Fatal(err)
	}
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	l.TownRoot = "" // the lander was not told its town
	var out strings.Builder
	l.Out = &out
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(rev.inputs) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(rev.inputs))
	}
	if strings.Contains(rev.inputs[0].BriefText, "keep the cap") {
		t.Errorf("a lander with no town root still entitled the operator decision:\n%s", rev.inputs[0].BriefText)
	}
	log := out.String()
	if got := strings.Count(log, "no town root"); got != 1 {
		t.Errorf("the missing town root is logged %d times, want once:\n%s", got, log)
	}
}

// TestAcceptedRiskAuthorsFromSettings: an ACCEPTED-RISK comment counts for a
// name in operator.names and for the overseer, and with an empty list for the
// overseer only (gt-ywuip).
func TestAcceptedRiskAuthorsFromSettings(t *testing.T) {
	t.Parallel()
	const at = "2026-10-23T18:00:00Z"
	const risk = "ACCEPTED-RISK: the mount is approved"
	configured := testOperatorConfig(t)
	empty := operatorConfigFor(t)
	for _, tc := range []struct {
		name   string
		op     *config.OperatorConfig
		author string
		want   bool
	}{
		{name: "a listed name", op: configured, author: testOperator, want: true},
		{name: "the overseer", op: configured, author: "town/crew/overseer", want: true},
		{name: "an unlisted name", op: configured, author: "gastown/polecats/coma", want: false},
		{name: "the overseer with an empty list", op: empty, author: "town/crew/overseer", want: true},
		{name: "an unlisted name with an empty list", op: empty, author: "gastown/polecats/coma", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := AcceptedRisksText([]beads.Comment{{Author: tc.author, Text: risk, CreatedAt: at}}, tc.op)
			if carried := strings.Contains(got, risk); carried != tc.want {
				t.Errorf("AcceptedRisksText carried a comment by %q = %v, want %v: %q", tc.author, carried, tc.want, got)
			}
		})
	}
}
