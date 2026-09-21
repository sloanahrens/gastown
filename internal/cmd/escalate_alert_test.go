package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/workspace"
)

// TestEscalationAlertKey covers the identity every alert is filed under. The
// derived key is what makes a repeat firing record onto the existing open
// escalation rather than mint a second one (gt-vwry), so the derivation rules
// matter as much as the storage.
func TestEscalationAlertKey(t *testing.T) {
	tests := []struct {
		name        string
		fingerprint string
		source      string
		description string
		want        string
	}{
		{
			name:        "explicit fingerprint wins over everything",
			fingerprint: "main_branch_test:failures",
			source:      "main_branch_test",
			description: "main branch test failures:",
			want:        "main_branch_test:failures",
		},
		{
			name:        "explicit fingerprint survives an unrelated description",
			fingerprint: "deacon:await-signal:hq-deacon",
			description: "timeout in cycle 41",
			want:        "deacon:await-signal:hq-deacon",
		},
		{
			name:        "source prefixes the description",
			source:      "jsonl_git_backup",
			description: "spike detected",
			want:        "jsonl_git_backup: spike detected",
		},
		{
			name:        "description alone when no source",
			description: "main branch test failures:",
			want:        "main branch test failures:",
		},
		{
			name:        "internal whitespace collapses so reflowed alerts match",
			description: "main   branch\ttest   failures:",
			want:        "main branch test failures:",
		},
		{
			name:        "source-only whitespace is not a prefix",
			source:      "   ",
			description: "spike detected",
			want:        "spike detected",
		},
		{
			name: "nothing to key on",
			want: "",
		},
	}

	origFP := escalateFingerprint
	defer func() { escalateFingerprint = origFP }()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			escalateFingerprint = tt.fingerprint
			if got := escalationAlertKey(tt.source, tt.description); got != tt.want {
				t.Errorf("escalationAlertKey(%q, %q) = %q, want %q", tt.source, tt.description, got, tt.want)
			}
		})
	}
}

// TestEscalationAlertKeyIsStableAcrossFirings pins the property the dedupe
// depends on: the same condition described the same way yields the same key,
// and therefore the same fingerprint label, on every firing.
func TestEscalationAlertKeyIsStableAcrossFirings(t *testing.T) {
	origFP := escalateFingerprint
	defer func() { escalateFingerprint = origFP }()
	escalateFingerprint = ""

	first := escalationFingerprintLabel(escalationAlertKey("main_branch_test", "main branch test failures:"))
	second := escalationFingerprintLabel(escalationAlertKey("main_branch_test", "main branch test failures:"))
	if first != second {
		t.Errorf("fingerprint changed between firings: %q then %q", first, second)
	}
	if first == "" {
		t.Fatal("fingerprint is empty; repeated alerts would not dedupe")
	}
	if other := escalationFingerprintLabel(escalationAlertKey("jsonl_git_backup", "spike detected")); other == first {
		t.Error("distinct alerts collided onto one fingerprint")
	}
}

// escalateStub is a shell stub for bd recording every invocation, so a test can
// assert which bead operations `gt escalate` actually issued. Show and list
// fixtures are files so the JSON stays valid instead of being shell-quoted.
func escalateStub(t *testing.T, listJSON, showJSON string) string {
	t.Helper()
	stubDir := t.TempDir()
	fixtureDir := t.TempDir()
	logPath := filepath.Join(stubDir, "calls.log")
	stdinPath := filepath.Join(stubDir, "stdin.log")
	listPath := filepath.Join(fixtureDir, "list.json")
	showPath := filepath.Join(fixtureDir, "show.json")

	for path, content := range map[string]string{listPath: listJSON, showPath: showJSON} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
	}

	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$1" in
  --allow-stale)
    exit 1
    ;;
  list)
    cat "` + listPath + `"
    exit 0
    ;;
  show)
    cat "` + showPath + `"
    exit 0
    ;;
  update)
    cat >> "` + stdinPath + `"
    exit 0
    ;;
  create)
    cat > /dev/null
    echo '{"id":"hq-created","title":"new escalation","status":"open","priority":1,"type":"task","labels":["gt:escalation"]}'
    exit 0
    ;;
esac
echo '{}'
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
	return logPath
}

// silenceEscalationRouting writes a town escalation config that routes nowhere,
// so these tests exercise the bead lifecycle without shelling out to mail.
//
// The hermetic town is process-wide, so the file this writes would otherwise
// outlive the test and change what every later test in the package sees; the
// cleanup restores the previous contents (or removes the file if there were
// none).
func silenceEscalationRouting(t *testing.T) {
	t.Helper()
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		t.Fatalf("workspace.FindFromCwdOrError: %v", err)
	}
	if err := os.MkdirAll(beads.ResolveBeadsDir(townRoot), 0o755); err != nil {
		t.Fatalf("creating .beads dir: %v", err)
	}
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("creating settings dir: %v", err)
	}

	cfgPath := filepath.Join(settingsDir, "escalation.json")
	prior, readErr := os.ReadFile(cfgPath)
	t.Cleanup(func() {
		if readErr == nil {
			_ = os.WriteFile(cfgPath, prior, 0o644)
			return
		}
		_ = os.Remove(cfgPath)
	})

	cfg := []byte(`{"routes":{"critical":[],"high":[],"medium":[],"low":[]}}`)
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatalf("writing escalation config: %v", err)
	}
}

// TestRunEscalate_FirstFiringCreatesOneKeyedBead covers the create half of the
// gt-vwry acceptance criterion. The alert must land under its stable key, so a
// later firing of the same condition can find it.
func TestRunEscalate_FirstFiringCreatesOneKeyedBead(t *testing.T) {
	silenceEscalationRouting(t)
	logPath := escalateStub(t, "[]", "[]")

	origFP := escalateFingerprint
	defer func() { escalateFingerprint = origFP }()
	escalateFingerprint = ""

	if err := runEscalate(escalateCmd, []string{"main branch test failures:"}); err != nil {
		t.Fatalf("runEscalate: %v", err)
	}

	calls := readCallLog(t, logPath)
	if !strings.Contains(calls, "create") {
		t.Fatalf("expected the first firing to create the escalation, got calls:\n%s", calls)
	}
	wantLabel := "--labels=" + escalationFingerprintLabel("main branch test failures:")
	if !strings.Contains(calls, wantLabel) {
		t.Errorf("expected the alert key label %q on the created bead, got calls:\n%s", wantLabel, calls)
	}
}

// TestRunEscalate_RepeatFiringBumpsInsteadOfCreating is the gt-vwry acceptance
// criterion "firing the same alert twice yields one open bead with count 2": the
// second firing must find the open escalation by key and record the repeat on
// it, never minting a second bead.
func TestRunEscalate_RepeatFiringBumpsInsteadOfCreating(t *testing.T) {
	silenceEscalationRouting(t)

	origFP := escalateFingerprint
	defer func() { escalateFingerprint = origFP }()
	escalateFingerprint = ""

	key := "main branch test failures:"
	label := escalationFingerprintLabel(key)

	existing := &beads.Issue{
		ID:       "hq-e1",
		Title:    key,
		Status:   string(beads.StatusOpen),
		Priority: 1,
		Type:     "task",
		Labels:   []string{"gt:escalation", label},
		Description: beads.FormatEscalationDescription(key, &beads.EscalationFields{
			Severity:    "high",
			Reason:      "first firing",
			EscalatedBy: "daemon",
			EscalatedAt: "2026-09-21T00:00:00Z",
			Fingerprint: label,
			Occurrences: 1,
		}),
	}
	listJSON, err := json.Marshal([]*beads.Issue{existing})
	if err != nil {
		t.Fatalf("marshal list fixture: %v", err)
	}
	showJSON, err := json.Marshal([]*beads.Issue{existing})
	if err != nil {
		t.Fatalf("marshal show fixture: %v", err)
	}

	logPath := escalateStub(t, string(listJSON), string(showJSON))

	if err := runEscalate(escalateCmd, []string{key}); err != nil {
		t.Fatalf("runEscalate: %v", err)
	}

	calls := readCallLog(t, logPath)
	if strings.Contains(calls, "create") {
		t.Errorf("a repeat firing must not create a second bead, got calls:\n%s", calls)
	}
	if !strings.Contains(calls, "update hq-e1") {
		t.Errorf("expected the repeat to be recorded on hq-e1, got calls:\n%s", calls)
	}

	stdin, err := os.ReadFile(filepath.Join(filepath.Dir(logPath), "stdin.log"))
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	fields := beads.ParseEscalationFields(string(stdin))
	if fields.Occurrences != 2 {
		t.Errorf("persisted occurrences = %d, want 2 after the second firing", fields.Occurrences)
	}
}

func readCallLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	return string(data)
}

// TestRunEscalateClear_ClosesTheKeyAndNothingElse is the gt-vwry acceptance
// criterion "the next patrol after the condition clears closes it": a producer
// whose condition is gone closes the alert it raised, without reaching a
// different condition's alert.
func TestRunEscalateClear_ClosesTheKeyAndNothingElse(t *testing.T) {
	silenceEscalationRouting(t)

	key := "main_branch_test:failures"
	label := escalationFingerprintLabel(key)
	mine := "hq-mine"
	other := "hq-other"

	issueJSON := func(id, fp string) string {
		return `{"id":"` + id + `","title":"alert","status":"open","priority":1,"type":"task",` +
			`"labels":["gt:escalation","` + fp + `"],` +
			`"description":"alert\n\nseverity: high\nreason: r\nescalated_by: daemon\nescalated_at: 2026-09-21T00:00:00Z\nfingerprint: ` + fp + `\n"}`
	}
	listJSON := "[" + issueJSON(mine, label) + "," + issueJSON(other, "escalation-fp:other1") + "]"
	// CloseEscalation re-reads the bead before mutating it; only the targeted
	// bead is ever named, so answering with it stands in for the lookup.
	showJSON := "[" + issueJSON(mine, label) + "]"

	logPath := escalateStub(t, listJSON, showJSON)

	origKeys, origSource := escalateClearKeys, escalateSource
	defer func() { escalateClearKeys, escalateSource = origKeys, origSource }()
	escalateClearKeys = []string{key}
	escalateSource = ""

	if err := runEscalateClear(escalateClearCmd, nil); err != nil {
		t.Fatalf("runEscalateClear: %v", err)
	}

	calls := readCallLog(t, logPath)
	if !strings.Contains(calls, "close "+mine) {
		t.Errorf("expected %s to be closed, got calls:\n%s", mine, calls)
	}
	if strings.Contains(calls, "close "+other) {
		t.Errorf("%s's condition did not clear and its alert must stay open, got calls:\n%s", other, calls)
	}
}

// TestRunEscalateClear_NothingToClearIsSuccess covers the healthy cycle. A
// producer clears its key on every pass, so a key that matches nothing is the
// ordinary case — if it were an error, a clean patrol would report itself as
// failed and the daemon would log noise on every tick.
func TestRunEscalateClear_NothingToClearIsSuccess(t *testing.T) {
	silenceEscalationRouting(t)
	logPath := escalateStub(t, "[]", "[]")

	origKeys, origSource := escalateClearKeys, escalateSource
	defer func() { escalateClearKeys, escalateSource = origKeys, origSource }()
	escalateClearKeys = []string{"jsonl_git_backup:spike"}
	escalateSource = ""

	if err := runEscalateClear(escalateClearCmd, nil); err != nil {
		t.Fatalf("clearing an absent key must succeed: %v", err)
	}
	if calls := readCallLog(t, logPath); strings.Contains(calls, "close") {
		t.Errorf("nothing matched, so nothing should have been closed, got calls:\n%s", calls)
	}
}

// TestRunEscalateClear_DerivesKeyFromSourceAndDescription keeps the two halves
// of a keyed alert symmetrical: a producer that raised an alert with a source
// and a description must be able to clear it the same way, with no bookkeeping
// of the hashed label on its side.
func TestRunEscalateClear_DerivesKeyFromSourceAndDescription(t *testing.T) {
	silenceEscalationRouting(t)

	key := "main_branch_test: main branch test failures:"
	label := escalationFingerprintLabel(key)
	issueJSON := `{"id":"hq-e1","title":"alert","status":"open","priority":1,"type":"task",` +
		`"labels":["gt:escalation","` + label + `"],` +
		`"description":"alert\n\nseverity: high\nreason: r\nescalated_by: daemon\nescalated_at: 2026-09-21T00:00:00Z\nfingerprint: ` + label + `\n"}`
	logPath := escalateStub(t, "["+issueJSON+"]", "["+issueJSON+"]")

	origKeys, origSource := escalateClearKeys, escalateSource
	defer func() { escalateClearKeys, escalateSource = origKeys, origSource }()
	escalateClearKeys = nil
	escalateSource = "main_branch_test"

	if err := runEscalateClear(escalateClearCmd, []string{"main", "branch", "test", "failures:"}); err != nil {
		t.Fatalf("runEscalateClear: %v", err)
	}
	if calls := readCallLog(t, logPath); !strings.Contains(calls, "close hq-e1") {
		t.Errorf("expected the derived key to find and close hq-e1, got calls:\n%s", calls)
	}
}

// TestRunEscalateClear_RequiresAKey guards against a caller clearing "the"
// escalation by accident: with neither a key nor a description there is nothing
// to match, and clearing everything open would be far worse than an error.
func TestRunEscalateClear_RequiresAKey(t *testing.T) {
	origKeys, origSource := escalateClearKeys, escalateSource
	defer func() { escalateClearKeys, escalateSource = origKeys, origSource }()
	escalateClearKeys = nil
	escalateSource = ""

	if err := runEscalateClear(escalateClearCmd, nil); err == nil {
		t.Fatal("expected an error when no key or description is given")
	}
}
