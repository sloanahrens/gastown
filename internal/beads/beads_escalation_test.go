package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatEscalationDescription(t *testing.T) {
	tests := []struct {
		name   string
		title  string
		fields *EscalationFields
		want   []string
		notIn  []string
	}{
		{
			name:   "nil fields returns title only",
			title:  "Test Escalation",
			fields: nil,
			want:   []string{"Test Escalation"},
			notIn:  []string{"severity:"},
		},
		{
			name:  "basic escalation",
			title: "Build failure",
			fields: &EscalationFields{
				Severity:    "high",
				Reason:      "Build failed 3 times",
				Source:      "patrol:deacon",
				EscalatedBy: "gastown/deacon",
				EscalatedAt: "2024-01-15T10:00:00Z",
			},
			want: []string{
				"Build failure",
				"severity: high",
				"reason: Build failed 3 times",
				"source: patrol:deacon",
				"escalated_by: gastown/deacon",
				"escalated_at: 2024-01-15T10:00:00Z",
			},
		},
		{
			name:  "acknowledged escalation",
			title: "Agent stuck",
			fields: &EscalationFields{
				Severity:    "medium",
				Reason:      "Agent not responding",
				EscalatedBy: "gastown/witness",
				EscalatedAt: "2024-01-15T10:00:00Z",
				AckedBy:     "gastown/crew/joe",
				AckedAt:     "2024-01-15T10:05:00Z",
			},
			want: []string{
				"severity: medium",
				"acked_by: gastown/crew/joe",
				"acked_at: 2024-01-15T10:05:00Z",
			},
		},
		{
			name:  "closed escalation",
			title: "Disk full",
			fields: &EscalationFields{
				Severity:     "critical",
				Reason:       "Disk >95%",
				EscalatedBy:  "gastown/deacon",
				EscalatedAt:  "2024-01-15T10:00:00Z",
				ClosedBy:     "human",
				ClosedReason: "Cleaned up temp files",
			},
			want: []string{
				"closed_by: human",
				"closed_reason: Cleaned up temp files",
			},
		},
		{
			name:  "null fields formatted explicitly",
			title: "New escalation",
			fields: &EscalationFields{
				Severity:    "low",
				Reason:      "Minor issue",
				EscalatedBy: "test",
				EscalatedAt: "2024-01-01T00:00:00Z",
			},
			want: []string{
				"acked_by: null",
				"acked_at: null",
				"closed_by: null",
				"closed_reason: null",
				"related_bead: null",
				"original_severity: null",
			},
		},
		{
			name:  "reescalation fields",
			title: "Bumped escalation",
			fields: &EscalationFields{
				Severity:          "high",
				Reason:            "Stale for 2h",
				EscalatedBy:       "patrol",
				EscalatedAt:       "2024-01-15T08:00:00Z",
				OriginalSeverity:  "low",
				ReescalationCount: 2,
				LastReescalatedAt: "2024-01-15T10:00:00Z",
				LastReescalatedBy: "deacon",
			},
			want: []string{
				"original_severity: low",
				"reescalation_count: 2",
				"last_reescalated_at: 2024-01-15T10:00:00Z",
				"last_reescalated_by: deacon",
			},
		},
		{
			name:  "fingerprint field",
			title: "Repeated alert",
			fields: &EscalationFields{
				Severity:    "medium",
				Reason:      "control-plane timeout",
				EscalatedBy: "deacon",
				EscalatedAt: "2024-01-15T10:00:00Z",
				Fingerprint: "escalation-fp:abc123def456",
			},
			want: []string{
				"fingerprint: escalation-fp:abc123def456",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatEscalationDescription(tt.title, tt.fields)
			for _, line := range tt.want {
				if !strings.Contains(got, line) {
					t.Errorf("missing line %q in output:\n%s", line, got)
				}
			}
			for _, line := range tt.notIn {
				if strings.Contains(got, line) {
					t.Errorf("unexpected %q in output:\n%s", line, got)
				}
			}
		})
	}
}

func TestParseEscalationFields(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want *EscalationFields
	}{
		{
			name: "empty description",
			desc: "",
			want: &EscalationFields{},
		},
		{
			name: "full escalation",
			desc: `Escalation: Build failure

severity: high
reason: Build failed 3 times
source: patrol:deacon
escalated_by: gastown/deacon
escalated_at: 2024-01-15T10:00:00Z
acked_by: gastown/crew/joe
acked_at: 2024-01-15T10:05:00Z
closed_by: null
closed_reason: null
related_bead: gt-abc123
original_severity: medium
reescalation_count: 1
last_reescalated_at: 2024-01-15T09:30:00Z
last_reescalated_by: deacon
fingerprint: escalation-fp:abc123def456`,
			want: &EscalationFields{
				Severity:          "high",
				Reason:            "Build failed 3 times",
				Source:            "patrol:deacon",
				EscalatedBy:       "gastown/deacon",
				EscalatedAt:       "2024-01-15T10:00:00Z",
				AckedBy:           "gastown/crew/joe",
				AckedAt:           "2024-01-15T10:05:00Z",
				ClosedBy:          "",
				ClosedReason:      "",
				RelatedBead:       "gt-abc123",
				OriginalSeverity:  "medium",
				ReescalationCount: 1,
				LastReescalatedAt: "2024-01-15T09:30:00Z",
				LastReescalatedBy: "deacon",
				Fingerprint:       "escalation-fp:abc123def456",
			},
		},
		{
			name: "null values become empty strings",
			desc: "severity: critical\nsource: null\nacked_by: null",
			want: &EscalationFields{
				Severity: "critical",
				Source:   "",
				AckedBy:  "",
			},
		},
		{
			name: "invalid reescalation_count ignored",
			desc: "severity: low\nreescalation_count: not-a-number",
			want: &EscalationFields{
				Severity:          "low",
				ReescalationCount: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseEscalationFields(tt.desc)
			if got.Severity != tt.want.Severity {
				t.Errorf("Severity = %q, want %q", got.Severity, tt.want.Severity)
			}
			if got.Reason != tt.want.Reason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.want.Reason)
			}
			if got.Source != tt.want.Source {
				t.Errorf("Source = %q, want %q", got.Source, tt.want.Source)
			}
			if got.EscalatedBy != tt.want.EscalatedBy {
				t.Errorf("EscalatedBy = %q, want %q", got.EscalatedBy, tt.want.EscalatedBy)
			}
			if got.EscalatedAt != tt.want.EscalatedAt {
				t.Errorf("EscalatedAt = %q, want %q", got.EscalatedAt, tt.want.EscalatedAt)
			}
			if got.AckedBy != tt.want.AckedBy {
				t.Errorf("AckedBy = %q, want %q", got.AckedBy, tt.want.AckedBy)
			}
			if got.AckedAt != tt.want.AckedAt {
				t.Errorf("AckedAt = %q, want %q", got.AckedAt, tt.want.AckedAt)
			}
			if got.ClosedBy != tt.want.ClosedBy {
				t.Errorf("ClosedBy = %q, want %q", got.ClosedBy, tt.want.ClosedBy)
			}
			if got.ClosedReason != tt.want.ClosedReason {
				t.Errorf("ClosedReason = %q, want %q", got.ClosedReason, tt.want.ClosedReason)
			}
			if got.RelatedBead != tt.want.RelatedBead {
				t.Errorf("RelatedBead = %q, want %q", got.RelatedBead, tt.want.RelatedBead)
			}
			if got.OriginalSeverity != tt.want.OriginalSeverity {
				t.Errorf("OriginalSeverity = %q, want %q", got.OriginalSeverity, tt.want.OriginalSeverity)
			}
			if got.ReescalationCount != tt.want.ReescalationCount {
				t.Errorf("ReescalationCount = %d, want %d", got.ReescalationCount, tt.want.ReescalationCount)
			}
			if got.LastReescalatedAt != tt.want.LastReescalatedAt {
				t.Errorf("LastReescalatedAt = %q, want %q", got.LastReescalatedAt, tt.want.LastReescalatedAt)
			}
			if got.LastReescalatedBy != tt.want.LastReescalatedBy {
				t.Errorf("LastReescalatedBy = %q, want %q", got.LastReescalatedBy, tt.want.LastReescalatedBy)
			}
			if got.Fingerprint != tt.want.Fingerprint {
				t.Errorf("Fingerprint = %q, want %q", got.Fingerprint, tt.want.Fingerprint)
			}
		})
	}
}

func TestEscalationFieldsRoundTrip(t *testing.T) {
	original := &EscalationFields{
		Severity:          "high",
		Reason:            "Agent stuck for 1h",
		Source:            "patrol:witness",
		EscalatedBy:       "gastown/witness",
		EscalatedAt:       "2024-06-15T12:00:00Z",
		AckedBy:           "gastown/crew/joe",
		AckedAt:           "2024-06-15T12:05:00Z",
		RelatedBead:       "gt-stuck123",
		OriginalSeverity:  "medium",
		ReescalationCount: 1,
		LastReescalatedAt: "2024-06-15T11:30:00Z",
		LastReescalatedBy: "deacon",
		Fingerprint:       "escalation-fp:feedface1234",
	}

	formatted := FormatEscalationDescription("Escalation: Agent stuck", original)
	parsed := ParseEscalationFields(formatted)

	if parsed.Severity != original.Severity {
		t.Errorf("Severity: got %q, want %q", parsed.Severity, original.Severity)
	}
	if parsed.Reason != original.Reason {
		t.Errorf("Reason: got %q, want %q", parsed.Reason, original.Reason)
	}
	if parsed.Source != original.Source {
		t.Errorf("Source: got %q, want %q", parsed.Source, original.Source)
	}
	if parsed.EscalatedBy != original.EscalatedBy {
		t.Errorf("EscalatedBy: got %q, want %q", parsed.EscalatedBy, original.EscalatedBy)
	}
	if parsed.EscalatedAt != original.EscalatedAt {
		t.Errorf("EscalatedAt: got %q, want %q", parsed.EscalatedAt, original.EscalatedAt)
	}
	if parsed.AckedBy != original.AckedBy {
		t.Errorf("AckedBy: got %q, want %q", parsed.AckedBy, original.AckedBy)
	}
	if parsed.AckedAt != original.AckedAt {
		t.Errorf("AckedAt: got %q, want %q", parsed.AckedAt, original.AckedAt)
	}
	if parsed.RelatedBead != original.RelatedBead {
		t.Errorf("RelatedBead: got %q, want %q", parsed.RelatedBead, original.RelatedBead)
	}
	if parsed.OriginalSeverity != original.OriginalSeverity {
		t.Errorf("OriginalSeverity: got %q, want %q", parsed.OriginalSeverity, original.OriginalSeverity)
	}
	if parsed.ReescalationCount != original.ReescalationCount {
		t.Errorf("ReescalationCount: got %d, want %d", parsed.ReescalationCount, original.ReescalationCount)
	}
	if parsed.LastReescalatedAt != original.LastReescalatedAt {
		t.Errorf("LastReescalatedAt: got %q, want %q", parsed.LastReescalatedAt, original.LastReescalatedAt)
	}
	if parsed.LastReescalatedBy != original.LastReescalatedBy {
		t.Errorf("LastReescalatedBy: got %q, want %q", parsed.LastReescalatedBy, original.LastReescalatedBy)
	}
	if parsed.Fingerprint != original.Fingerprint {
		t.Errorf("Fingerprint: got %q, want %q", parsed.Fingerprint, original.Fingerprint)
	}
}

func TestFilterEscalationRecordsSkipsMailMessages(t *testing.T) {
	issues := []*Issue{
		{ID: "hq-root", Labels: []string{"gt:escalation"}},
		{ID: "hq-mail", Labels: []string{"gt:escalation", "gt:message"}},
	}

	got := filterEscalationRecords(issues)
	if len(got) != 1 || got[0].ID != "hq-root" {
		t.Fatalf("filterEscalationRecords() = %#v, want only root escalation", got)
	}
}

// TestListEscalationsPassesIncludeInfra verifies that ListEscalations queries
// bd with --include-infra.
//
// Regression test for gt-fcsf: escalations are created as ephemeral wisps
// (--ephemeral --wisp-type=escalation, see CreateEscalationBead), which `bd
// list` hides by default. Without --include-infra, `gt escalate list`
// reported "No escalations found" while 11 escalations sat open and
// unacknowledged for hours — the same bug class as gt-4mnd (ephemeral wisps
// invisible to bd list).
// TestListEscalationsReturnsOpenEphemeralEscalations proves ListEscalations
// actually surfaces an open escalation, not merely that it invokes bd with
// the right flag. Escalations are created as ephemeral wisps (gt-fcsf),
// which `bd list` hides unless --include-infra is passed; the stub below
// only returns the escalation when that flag is present, so a regression
// that drops the flag fails this test the same way it fails in production
// (an empty result), rather than only being caught by an argv grep.
func TestListEscalationsReturnsOpenEphemeralEscalations(t *testing.T) {
	stubDir := t.TempDir()
	argsPath := filepath.Join(stubDir, "args.txt")

	stubScript := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a" >> "` + argsPath + `"
done
case "$*" in
  *--include-infra*)
    echo '[{"id":"hq-wisp1","title":"Dolt: server unreachable","status":"open","priority":0,"labels":["gt:escalation","severity:critical"],"ephemeral":true,"wisp_type":"escalation"}]'
    ;;
  *)
    echo '[]'
    ;;
esac
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ResetBdAllowStaleCacheForTest()

	b := New(t.TempDir())
	escalations, err := b.ListEscalations()
	if err != nil {
		t.Fatalf("ListEscalations: %v", err)
	}
	if len(escalations) != 1 {
		t.Fatalf("ListEscalations returned %d escalations, want 1 (the open ephemeral wisp): %#v", len(escalations), escalations)
	}
	if escalations[0].ID != "hq-wisp1" {
		t.Fatalf("ListEscalations returned %q, want hq-wisp1", escalations[0].ID)
	}

	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	args := string(argsData)
	if !strings.Contains(args, "--include-infra") {
		t.Fatalf("ListEscalations did not pass --include-infra, got args:\n%s", args)
	}
}

// escalationStubPayload is what the stub bd prints for a successful list --json.
// It carries one plain escalation and one gt:message carrier so the tests can
// tell the open-only view (which filters messages) from the --all view (which
// does not).
const escalationStubPayload = `[{"id":"hq-wisp1","title":"Dolt: server unreachable","status":"open","priority":0,"labels":["gt:escalation","severity:critical"],"ephemeral":true,"wisp_type":"escalation"},{"id":"hq-msg1","title":"escalation mail","status":"open","priority":3,"labels":["gt:escalation","gt:message"],"ephemeral":true,"wisp_type":"message"}]`

// escalationBDStub puts a fake bd on PATH that models the bd behaviours these
// tests depend on instead of just recording argv, and logs each invocation's
// argv plus BEADS_DIR. It returns the log path.
//
// rejectFlat selects which bd is modelled:
//   - false (bd v0.59+): `list --json` emits human-readable tree text unless
//     --flat is passed, so a caller that drops the injection cannot parse.
//   - true (bd < v0.59): --flat is an unknown flag, so a caller that injects it
//     without the remove-and-retry fallback fails outright.
//
// Either way the stub only returns JSON to a caller that got the flag handling
// right, which is what makes these tests fail on the regressions they cover.
func escalationBDStub(t *testing.T, rejectFlat bool) string {
	t.Helper()

	withFlat := "echo '" + escalationStubPayload + "'"
	withoutFlat := "echo 'hq-wisp1  [open] Dolt: server unreachable'  # tree text, not JSON"
	if rejectFlat {
		withFlat = "echo 'unknown flag: --flat' >&2; exit 1"
		withoutFlat = "echo '" + escalationStubPayload + "'"
	}

	logPath := filepath.Join(t.TempDir(), "bd.log")
	stubScript := `#!/bin/sh
{
  printf 'env BEADS_DIR=%s\n' "${BEADS_DIR-<unset>}"
  for a in "$@"; do printf 'arg %s\n' "$a"; done
} >> "` + logPath + `"

# Probe for --allow-stale support; report unsupported so args[0] stays "list".
if [ "$1" = "--allow-stale" ]; then
  exit 1
fi

has_flat=0
for a in "$@"; do
  if [ "$a" = "--flat" ]; then has_flat=1; fi
done

if [ "$has_flat" = 1 ]; then
  ` + withFlat + `
else
  ` + withoutFlat + `
fi
exit 0
`

	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "bd"), []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ResetBdAllowStaleCacheForTest()
	return logPath
}

func readEscalationStubLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd stub log: %v", err)
	}
	return string(data)
}

// TestListEscalationsAcrossRigsInjectsFlatAndRoutes covers the two independent
// ways the routed display path could fail while looking correct.
//
//  1. bd v0.59+ ignores --json on `list` without --flat and emits tree text, so
//     json.Unmarshal fails. The stub models that, meaning a regression that
//     drops the injection fails here the way it would against a real bd.
//  2. Cross-rig routing only engages when BEADS_DIR is left unset. The stub
//     records the variable, and the test requires it absent — otherwise the
//     query is silently pinned to one database again, which is the original
//     gt-wbxb "No escalations found" symptom.
//
// Regression test for the review of MR gt-wisp-nysy, where runWithRouting had
// neither the --flat injection nor the retry that run/runWithStdin perform.
func TestListEscalationsAcrossRigsInjectsFlatAndRoutes(t *testing.T) {
	logPath := escalationBDStub(t, false)
	b := New(t.TempDir())

	escalations, err := b.ListEscalationsAcrossRigs()
	if err != nil {
		t.Fatalf("ListEscalationsAcrossRigs: %v", err)
	}
	if len(escalations) != 1 || escalations[0].ID != "hq-wisp1" {
		t.Fatalf("ListEscalationsAcrossRigs() = %#v, want only the hq-wisp1 escalation", escalations)
	}

	log := readEscalationStubLog(t, logPath)
	if !strings.Contains(log, "arg --flat") {
		t.Errorf("routed list query did not pass --flat, so bd would emit tree text:\n%s", log)
	}
	if !strings.Contains(log, "env BEADS_DIR=<unset>") {
		t.Errorf("routed list query pinned BEADS_DIR, so it cannot see other rigs' escalations:\n%s", log)
	}
}

// TestListEscalationsAcrossRigsRetriesWithoutFlatOnOldBd covers the other half
// of the --flat handling: bd < v0.59 rejects the flag as unknown, so the routed
// path needs the same remove-and-retry that run/runWithStdin have. Without it,
// adding the injection above would break older bd instead of fixing newer bd.
func TestListEscalationsAcrossRigsRetriesWithoutFlatOnOldBd(t *testing.T) {
	logPath := escalationBDStub(t, true)
	b := New(t.TempDir())

	escalations, err := b.ListEscalationsAcrossRigs()
	if err != nil {
		t.Fatalf("ListEscalationsAcrossRigs against a pre---flat bd: %v", err)
	}
	if len(escalations) != 1 || escalations[0].ID != "hq-wisp1" {
		t.Fatalf("ListEscalationsAcrossRigs() = %#v, want only the hq-wisp1 escalation", escalations)
	}

	// The first attempt must have carried --flat (so it exercised the retry),
	// and a later attempt must have run without it.
	log := readEscalationStubLog(t, logPath)
	if !strings.Contains(log, "arg --flat") {
		t.Errorf("expected an initial attempt carrying --flat before the retry:\n%s", log)
	}
}

// TestListEscalationsStaysPinnedToCurrentDatabase pins the scope decision made
// for gt-wbxb: only the display path routes across rigs. ListEscalations feeds
// ListStaleEscalations (which reescalates and sends mail) and shares its query
// shape with the fingerprint dedup in runEscalate, so it must keep querying the
// one database it always has.
func TestListEscalationsStaysPinnedToCurrentDatabase(t *testing.T) {
	logPath := escalationBDStub(t, false)
	b := New(t.TempDir())

	if _, err := b.ListEscalations(); err != nil {
		t.Fatalf("ListEscalations: %v", err)
	}

	log := readEscalationStubLog(t, logPath)
	if strings.Contains(log, "env BEADS_DIR=<unset>") {
		t.Errorf("ListEscalations must stay pinned to a single database, but it ran without BEADS_DIR — "+
			"that widens the mutating flows built on it (stale re-escalation, fingerprint dedup):\n%s", log)
	}
}

// TestListAllEscalationsAcrossRigsKeepsMessages checks that the --all display
// view stays cross-rig but does not pick up the open-only view's gt:message
// filtering, which would silently change `gt escalate list --all` output.
func TestListAllEscalationsAcrossRigsKeepsMessages(t *testing.T) {
	logPath := escalationBDStub(t, false)
	b := New(t.TempDir())

	escalations, err := b.ListAllEscalationsAcrossRigs()
	if err != nil {
		t.Fatalf("ListAllEscalationsAcrossRigs: %v", err)
	}
	if len(escalations) != 2 {
		t.Fatalf("ListAllEscalationsAcrossRigs() returned %d entries, want 2 (message carriers are kept): %#v",
			len(escalations), escalations)
	}

	log := readEscalationStubLog(t, logPath)
	if !strings.Contains(log, "arg --status=all") {
		t.Errorf("--all view did not query --status=all:\n%s", log)
	}
	if !strings.Contains(log, "env BEADS_DIR=<unset>") {
		t.Errorf("--all view pinned BEADS_DIR, so it cannot see other rigs' escalations:\n%s", log)
	}
}

func TestBumpSeverity(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"low", "medium"},
		{"medium", "high"},
		{"high", "critical"},
		{"critical", "critical"}, // already at max
		{"unknown", "critical"},  // default fallthrough
		{"", "critical"},         // empty defaults to critical
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := bumpSeverity(tt.input)
			if got != tt.want {
				t.Errorf("bumpSeverity(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestCreateEscalationBead_PassesDescriptionViaStdin verifies that
// CreateEscalationBead passes the multi-line description through bd's stdin
// (--body-file=-) rather than embedding newlines in --description=...
//
// Regression test for dc-1bxe: bd 1.0.3+ rejects newlines inside --description
// flag values, which broke `gt escalate` for any escalation containing the
// structured YAML metadata block (severity, reason, escalated_by, etc.).
func TestCreateEscalationBead_PassesDescriptionViaStdin(t *testing.T) {
	stubDir := t.TempDir()
	argsPath := filepath.Join(stubDir, "args.txt")
	stdinPath := filepath.Join(stubDir, "stdin.txt")

	// Stub bd: write each arg on its own line to args.txt, capture stdin to
	// stdin.txt, and emit a minimal valid issue JSON so unmarshal succeeds.
	stubScript := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a" >> "` + argsPath + `"
done
cat > "` + stdinPath + `"
echo '{"id":"dc-test1","title":"x","status":"open","priority":2,"type":"task","labels":["gt:escalation"]}'
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Reset --allow-stale capability cache so the stub gets probed fresh.
	ResetBdAllowStaleCacheForTest()

	b := New(t.TempDir())
	fields := &EscalationFields{
		Severity:    "high",
		Reason:      "multi-line\nreason\nwith embedded newlines",
		EscalatedBy: "test/agent",
		EscalatedAt: "2026-05-08T15:00:00Z",
		Fingerprint: "escalation-fp:abc123def456",
	}

	if _, err := b.CreateEscalationBead("Test escalation", fields); err != nil {
		t.Fatalf("CreateEscalationBead: %v", err)
	}

	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read stub args: %v", err)
	}
	args := string(argsData)

	// Must use --body-file=- to read description from stdin.
	if !strings.Contains(args, "--body-file=-") {
		t.Errorf("expected --body-file=- in bd args, got:\n%s", args)
	}
	if !strings.Contains(args, "--labels=escalation-fp:abc123def456") {
		t.Errorf("expected fingerprint label in bd args, got:\n%s", args)
	}
	// Must NOT pass --description=... at all (any --description value would
	// embed the newline-containing structured description and fail bd 1.0.3+).
	for _, line := range strings.Split(args, "\n") {
		if strings.HasPrefix(line, "--description=") {
			t.Errorf("--description=... must not be used (bd rejects newlines), got %q", line)
		}
	}

	stdinData, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read stub stdin: %v", err)
	}
	stdin := string(stdinData)
	// The structured description must reach bd via stdin.
	wantInStdin := []string{
		"Test escalation",
		"severity: high",
		"escalated_by: test/agent",
	}
	for _, want := range wantInStdin {
		if !strings.Contains(stdin, want) {
			t.Errorf("expected stdin to contain %q, got:\n%s", want, stdin)
		}
	}
	// Sanity: stdin must contain newlines (it's the multi-line description).
	if !strings.Contains(stdin, "\n") {
		t.Errorf("expected stdin to be multi-line, got %q", stdin)
	}
}
