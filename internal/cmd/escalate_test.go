package cmd

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
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

func TestRunEscalateValidation(t *testing.T) {
	t.Parallel()

	t.Run("stdin and reason conflict", func(t *testing.T) {
		fx := newEscalateFixture(t)
		fx.r.stdin, fx.r.reason, fx.r.severity = true, "some reason", "medium"
		err := fx.r.escalate([]string{"test"})
		if err == nil || !strings.Contains(err.Error(), "cannot use --stdin with --reason/-r") {
			t.Fatalf("err = %v, want the --stdin/--reason conflict", err)
		}
	})

	t.Run("invalid severity", func(t *testing.T) {
		fx := newEscalateFixture(t)
		fx.r.severity = "emergency"
		err := fx.r.escalate([]string{"test escalation"})
		if err == nil || !strings.Contains(err.Error(), "invalid severity") {
			t.Fatalf("err = %v, want invalid severity", err)
		}
		if len(fx.raised) != 0 {
			t.Errorf("an invalid severity reached notify: %v", fx.raised)
		}
	})
}

func TestCloseEscalationDeliveryBeads(t *testing.T) {
	t.Parallel()
	bd := &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		if cmd == "list" {
			return bdOut(`[{"id":"hq-885m","title":"[HIGH] test","status":"open","labels":["gt:message","gt:escalation","thread:hq-kl7"]}]`)
		}
		return bdOut("")
	}}
	n, err := closeEscalationDeliveryBeads(beads.NewWithBeadsDirAndRunner(t.TempDir(), "", bd.run), "hq-kl7", "gastown/witness")
	if err != nil {
		t.Fatalf("closeEscalationDeliveryBeads: %v", err)
	}
	if n != 1 {
		t.Fatalf("closed count = %d, want 1", n)
	}

	callLog := bd.log()
	// --limit=0: bd list returns 50 rows by default, and an escalation
	// broadcast to more recipients than that left the rest open.
	for _, want := range []string{"list", "--label=gt:message", "--label=thread:hq-kl7", "--status=open", "--include-infra", "--json", "--limit=0"} {
		if !strings.Contains(callLog, want) {
			t.Errorf("expected list query to contain %q, got log:\n%s", want, callLog)
		}
	}
	if !strings.Contains(callLog, "close hq-885m") {
		t.Errorf("expected close call for hq-885m, got log:\n%s", callLog)
	}
}

// TestCloseEscalationDeliveryBeadsNoneOpen verifies the no-op path when no
// delivery beads are open on the escalation's thread.
func TestCloseEscalationDeliveryBeadsNoneOpen(t *testing.T) {
	t.Parallel()
	bd := listingBD("[]")
	n, err := closeEscalationDeliveryBeads(beads.NewWithBeadsDirAndRunner(t.TempDir(), "", bd.run), "hq-kl7", "gastown/witness")
	if err != nil {
		t.Fatalf("closeEscalationDeliveryBeads: %v", err)
	}
	if n != 0 {
		t.Fatalf("closed count = %d, want 0", n)
	}
}

func TestRunEscalateListAllPassesIncludeInfra(t *testing.T) {
	t.Parallel()
	row := `[{"id":"hq-wisp1","title":"Dolt: server unreachable","status":"open","priority":0,"labels":["gt:escalation"],"ephemeral":true,"wisp_type":"escalation"}]`
	bd := &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		// show is the live cross-check that filters phantom escalations.
		if cmd == "list" || cmd == "show" {
			return bdOut(row)
		}
		return bdOut("{}")
	}}
	var out strings.Builder
	if err := listEscalations(&out, io.Discard, beads.NewWithBeadsDirAndRunner(t.TempDir(), "", bd.run), true, true); err != nil {
		t.Fatalf("listEscalations: %v", err)
	}
	for _, want := range []string{"--label=gt:escalation", "--status=all", "--include-infra", "--flat"} {
		if !strings.Contains(bd.log(), want) {
			t.Errorf("expected list query to contain %q, got log:\n%s", want, bd.log())
		}
	}
	if !strings.Contains(out.String(), "hq-wisp1") {
		t.Errorf("output = %q, want hq-wisp1", out.String())
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
