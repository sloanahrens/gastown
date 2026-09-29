package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

func TestGetNextSeverity(t *testing.T) {
	t.Parallel()
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
			got := getNextSeverity(tt.input)
			if got != tt.want {
				t.Errorf("getNextSeverity(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSeverityEmoji(t *testing.T) {
	t.Parallel()
	tests := []struct {
		severity string
		want     string
	}{
		{config.SeverityCritical, "🚨"},
		{config.SeverityHigh, "⚠️"},
		{config.SeverityMedium, "📢"},
		{config.SeverityLow, "ℹ️"},
		{"unknown", "📋"},
		{"", "📋"},
	}

	for _, tt := range tests {
		t.Run(tt.severity, func(t *testing.T) {
			got := severityEmoji(tt.severity)
			if got != tt.want {
				t.Errorf("severityEmoji(%q) = %q, want %q", tt.severity, got, tt.want)
			}
		})
	}
}

func TestFormatRelativeTime(t *testing.T) {
	t.Parallel()
	now := time.Now()

	tests := []struct {
		name      string
		timestamp string
		want      string
	}{
		{
			name:      "just now",
			timestamp: now.Add(-10 * time.Second).Format(time.RFC3339),
			want:      "just now",
		},
		{
			name:      "1 minute ago",
			timestamp: now.Add(-1 * time.Minute).Format(time.RFC3339),
			want:      "1 minute ago",
		},
		{
			name:      "multiple minutes ago",
			timestamp: now.Add(-15 * time.Minute).Format(time.RFC3339),
			want:      "15 minutes ago",
		},
		{
			name:      "1 hour ago",
			timestamp: now.Add(-1 * time.Hour).Format(time.RFC3339),
			want:      "1 hour ago",
		},
		{
			name:      "multiple hours ago",
			timestamp: now.Add(-5 * time.Hour).Format(time.RFC3339),
			want:      "5 hours ago",
		},
		{
			name:      "1 day ago",
			timestamp: now.Add(-25 * time.Hour).Format(time.RFC3339),
			want:      "1 day ago",
		},
		{
			name:      "multiple days ago",
			timestamp: now.Add(-72 * time.Hour).Format(time.RFC3339),
			want:      "3 days ago",
		},
		{
			name:      "invalid timestamp returns raw",
			timestamp: "not-a-timestamp",
			want:      "not-a-timestamp",
		},
		{
			name:      "empty timestamp returns raw",
			timestamp: "",
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatRelativeTime(tt.timestamp)
			if got != tt.want {
				t.Errorf("formatRelativeTime(%q) = %q, want %q", tt.timestamp, got, tt.want)
			}
		})
	}
}

func TestReasonDisplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{name: "empty reason is explicit", reason: "", want: "(none provided)"},
		{name: "non-empty reason passes through", reason: "CI blocked", want: "CI blocked"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reasonDisplay(tt.reason); got != tt.want {
				t.Errorf("reasonDisplay(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

func TestFormatReescalationMailBody(t *testing.T) {
	t.Parallel()
	result := &beads.ReescalationResult{
		ID:              "hq-esc123",
		Title:           "Build blocked",
		OldSeverity:     "medium",
		NewSeverity:     "high",
		ReescalationNum: 2,
	}

	got := formatReescalationMailBody(result, "gastown/patrol")

	wantIn := []string{
		"Escalation ID: hq-esc123",
		"Severity bumped: medium → high",
		"Reescalation #2",
		"Reescalated by: gastown/patrol",
		"stale threshold",
		"gt escalate ack hq-esc123",
		"gt escalate close hq-esc123",
	}

	for _, s := range wantIn {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in output:\n%s", s, got)
		}
	}
}

func TestDetectSenderFallback(t *testing.T) {
	// Save original env vars
	origActor := os.Getenv("BD_ACTOR")
	origRole := os.Getenv("GT_ROLE")
	defer func() {
		os.Setenv("BD_ACTOR", origActor)
		os.Setenv("GT_ROLE", origRole)
	}()

	tests := []struct {
		name  string
		actor string
		role  string
		want  string
	}{
		{
			name:  "BD_ACTOR takes priority",
			actor: "gastown/polecats/alpha",
			role:  "gastown/witness",
			want:  "gastown/polecats/alpha",
		},
		{
			name:  "GT_ROLE used when BD_ACTOR empty",
			actor: "",
			role:  "gastown/witness",
			want:  "gastown/witness",
		},
		{
			name:  "empty when both unset",
			actor: "",
			role:  "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("BD_ACTOR", tt.actor)
			os.Setenv("GT_ROLE", tt.role)

			got := detectSenderFallback()
			if got != tt.want {
				t.Errorf("detectSenderFallback() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunEscalateValidation(t *testing.T) {
	// Save and restore package-level flags
	origSeverity := escalateSeverity
	origReason := escalateReason
	origStdin := escalateStdin
	origDryRun := escalateDryRun
	defer func() {
		escalateSeverity = origSeverity
		escalateReason = origReason
		escalateStdin = origStdin
		escalateDryRun = origDryRun
	}()

	t.Run("stdin and reason conflict", func(t *testing.T) {
		escalateStdin = true
		escalateReason = "some reason"
		escalateSeverity = "medium"

		err := runEscalate(escalateCmd, []string{"test"})
		if err == nil {
			t.Fatal("expected error when --stdin and --reason are both set")
		}
		if !strings.Contains(err.Error(), "cannot use --stdin with --reason/-r") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no args shows help", func(t *testing.T) {
		escalateStdin = false
		escalateReason = ""
		escalateSeverity = "medium"

		// No args should return nil (shows help)
		err := runEscalate(escalateCmd, []string{})
		if err != nil {
			t.Errorf("expected nil error for no args (help case), got: %v", err)
		}
	})

	t.Run("invalid severity", func(t *testing.T) {
		escalateStdin = false
		escalateReason = ""
		escalateSeverity = "emergency"

		err := runEscalate(escalateCmd, []string{"test escalation"})
		if err == nil {
			t.Fatal("expected error for invalid severity")
		}
		if !strings.Contains(err.Error(), "invalid severity") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// TestCloseEscalationDeliveryBeads verifies that closing an escalation's
// mail-delivery beads queries for open beads on the escalation's thread and
// closes exactly those.
//
// Regression test for gt-kl7: `gt escalate close` closed the escalation wisp
// but left its routed mail-delivery bead(s) open, so resolved incidents kept
// showing up as unacked P1/P2s on the dashboard and polluted `bd ready`.
func TestCloseEscalationDeliveryBeads(t *testing.T) {
	stubDir := t.TempDir()
	logPath := filepath.Join(stubDir, "calls.log")

	stubScript := `#!/bin/sh
{
  for a in "$@"; do printf '%s\t' "$a"; done
  printf '\n'
} >> "` + logPath + `"

case "$1" in
  --allow-stale)
    exit 1
    ;;
  list)
    echo '[{"id":"hq-885m","title":"[HIGH] test","status":"open","labels":["gt:message","gt:escalation","thread:hq-kl7"]}]'
    exit 0
    ;;
  close)
    exit 0
    ;;
  *)
    echo '{}'
    exit 0
    ;;
esac
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()

	bd := beads.New(t.TempDir())
	n, err := closeEscalationDeliveryBeads(bd, "hq-kl7", "gastown/witness")
	if err != nil {
		t.Fatalf("closeEscalationDeliveryBeads: %v", err)
	}
	if n != 1 {
		t.Fatalf("closed count = %d, want 1", n)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	callLog := string(logData)

	for _, want := range []string{"list", "--label=gt:message", "--label=thread:hq-kl7", "--status=open", "--include-infra", "--json"} {
		if !strings.Contains(callLog, want) {
			t.Errorf("expected list query to contain %q, got log:\n%s", want, callLog)
		}
	}
	if !strings.Contains(callLog, "close\thq-885m") {
		t.Errorf("expected close call for hq-885m, got log:\n%s", callLog)
	}
}

// TestCloseEscalationDeliveryBeadsNoneOpen verifies the no-op path when no
// delivery beads are open on the escalation's thread.
func TestCloseEscalationDeliveryBeadsNoneOpen(t *testing.T) {
	stubDir := t.TempDir()

	stubScript := `#!/bin/sh
case "$1" in
  --allow-stale)
    exit 1
    ;;
  list)
    echo '[]'
    exit 0
    ;;
  *)
    echo '{}'
    exit 0
    ;;
esac
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()

	bd := beads.New(t.TempDir())
	n, err := closeEscalationDeliveryBeads(bd, "hq-kl7", "gastown/witness")
	if err != nil {
		t.Fatalf("closeEscalationDeliveryBeads: %v", err)
	}
	if n != 0 {
		t.Fatalf("closed count = %d, want 0", n)
	}
}

// TestRunEscalateListAllPassesIncludeInfra verifies `gt escalate list --all`
// queries bd with --include-infra, that the query is cross-rig, and that it
// survives bd's tree-vs-JSON behaviour.
//
// Regression test for gt-fcsf: escalations are ephemeral wisps, invisible to
// `bd list` without --include-infra — the same bug class as gt-4mnd.
//
// The stub models bd rather than echoing JSON unconditionally, so a green run
// shows the query actually worked, not merely that argv looked right:
//   - it answers with human-readable tree text unless --flat is present
//     (bd v0.59+ ignores --json on list without --flat), so dropping the flag
//     makes runEscalateList fail its json.Unmarshal;
//   - it records BEADS_DIR, which must be absent for prefix routing to reach
//     other rigs' databases. Pinning it is the gt-wbxb "No escalations found"
//     symptom.
func TestRunEscalateListAllPassesIncludeInfra(t *testing.T) {
	stubDir := t.TempDir()
	argsPath := filepath.Join(stubDir, "args.txt")

	stubScript := `#!/bin/sh
{
  printf 'BEADS_DIR=%s\n' "${BEADS_DIR-<unset>}"
  for a in "$@"; do printf '%s\t' "$a"; done
  printf '\n'
} >> "` + argsPath + `"
case "$1" in
  --allow-stale)
    exit 1
    ;;
  list)
    has_flat=0
    for a in "$@"; do
      if [ "$a" = "--flat" ]; then has_flat=1; fi
    done
    if [ "$has_flat" = 1 ]; then
      echo '[{"id":"hq-wisp1","title":"Dolt: server unreachable","status":"open","priority":0,"labels":["gt:escalation"],"ephemeral":true,"wisp_type":"escalation"}]'
    else
      echo 'hq-wisp1  [open] Dolt: server unreachable'
    fi
    exit 0
    ;;
  show)
    # The live'd cross-check that filters phantom escalations; answer for the
    # one ID the list query above returned.
    echo '[{"id":"hq-wisp1","title":"Dolt: server unreachable","status":"open","priority":0,"labels":["gt:escalation"],"ephemeral":true,"wisp_type":"escalation"}]'
    exit 0
    ;;
  *)
    echo '{}'
    exit 0
    ;;
esac
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()

	// runEscalateList resolves the workspace via workspace.FindFromCwdOrError()
	// and shells out with cmd.Dir set to <townRoot>/.beads. The hermetic test
	// sandbox town (see testutil.HermeticMain) only creates mayor/town.json,
	// not .beads, so that directory must exist before the subprocess runs or
	// the exec fails with an unrelated "no such file or directory" from the
	// chdir, not from bd itself.
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		t.Fatalf("workspace.FindFromCwdOrError: %v", err)
	}
	if err := os.MkdirAll(beads.ResolveBeadsDir(townRoot), 0o755); err != nil {
		t.Fatalf("creating .beads dir: %v", err)
	}

	origAll, origJSON := escalateListAll, escalateListJSON
	defer func() { escalateListAll, escalateListJSON = origAll, origJSON }()
	escalateListAll = true
	escalateListJSON = true

	if err := runEscalateList(escalateListCmd, nil); err != nil {
		t.Fatalf("runEscalateList: %v", err)
	}

	logData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	callLog := string(logData)
	for _, want := range []string{"--label=gt:escalation", "--status=all", "--include-infra", "--flat"} {
		if !strings.Contains(callLog, want) {
			t.Errorf("expected list query to contain %q, got log:\n%s", want, callLog)
		}
	}
	if !strings.Contains(callLog, "BEADS_DIR=<unset>") {
		t.Errorf("expected the list query to run without BEADS_DIR so bd routes across rigs, got log:\n%s", callLog)
	}
}

func TestGetNextSeverityMatchesConfig(t *testing.T) {
	t.Parallel()
	// Verify getNextSeverity in escalate_impl.go matches config.NextSeverity
	// to catch if they ever diverge.
	severities := []string{"low", "medium", "high", "critical"}
	for _, s := range severities {
		cmdResult := getNextSeverity(s)
		configResult := config.NextSeverity(s)
		if cmdResult != configResult {
			t.Errorf("getNextSeverity(%q) = %q but config.NextSeverity(%q) = %q — they diverge!",
				s, cmdResult, s, configResult)
		}
	}
}
