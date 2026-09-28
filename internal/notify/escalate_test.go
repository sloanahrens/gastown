package notify

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// failingSenders fails the test if any outside channel is contacted.
func failingSenders(t *testing.T) externalSenders {
	fail := func(_ *config.EscalationConfig, beadID, _, _ string) error {
		t.Errorf("outside channel contacted for %s", beadID)
		return nil
	}
	return externalSenders{email: fail, slack: fail, sms: fail}
}

// recordingSenders records which outside channels were contacted, answering
// each with err.
type recordingSenders struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (r *recordingSenders) senders() externalSenders {
	rec := func(channel string) externalSender {
		return func(_ *config.EscalationConfig, beadID, severity, description string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.sent = append(r.sent, channel+":"+beadID+":"+severity+":"+description)
			return r.err
		}
	}
	return externalSenders{email: rec("email"), slack: rec("slack"), sms: rec("sms")}
}

// TestExecuteExternalActionsNeedsContactsConfigured: a route naming a channel
// whose contact details are missing is reported as a warning and contacts
// nothing.
func TestExecuteExternalActionsNeedsContactsConfigured(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		action  string
		cfg     config.EscalationContacts
		warning string
	}{
		{"email without address", "email:human", config.EscalationContacts{}, "contacts.human_email not configured"},
		{"email without smtp host", "email:human", config.EscalationContacts{HumanEmail: "h@example.com"}, "contacts.smtp_host not configured"},
		{"sms without number", "sms:human", config.EscalationContacts{}, "contacts.human_sms not configured"},
		{"sms without webhook", "sms:human", config.EscalationContacts{HumanSMS: "+15551234567"}, "contacts.sms_webhook not configured"},
		{"slack without webhook", "slack", config.EscalationContacts{}, "contacts.slack_webhook not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.EscalationConfig{Contacts: tc.cfg}
			statuses := executeExternalActions([]string{tc.action}, cfg, "hq-test", "high", "desc", t.TempDir(), failingSenders(t))
			if len(statuses) != 1 || statuses[0].Warning != tc.warning || statuses[0].RuntimeNotified {
				t.Fatalf("statuses = %+v, want one un-notified status warning %q", statuses, tc.warning)
			}
		})
	}
}

// TestExecuteExternalActionsContactsConfiguredChannels replaces an old test
// that only checked for a panic and, with a slack webhook configured, posted
// to hooks.slack.com from the unit tier.
func TestExecuteExternalActionsContactsConfiguredChannels(t *testing.T) {
	t.Parallel()
	cfg := &config.EscalationConfig{Contacts: config.EscalationContacts{
		HumanEmail: "h@example.com", SMTPHost: "smtp.example.com",
		HumanSMS: "+15551234567", SMSWebhook: "https://sms.example.com/hook",
		SlackWebhook: "https://hooks.example.com/slack",
	}}
	rec := &recordingSenders{}
	statuses := executeExternalActions([]string{"bead", "mail:mayor", "email:human", "sms:human", "slack"}, cfg, "hq-e1", "critical", "db down", t.TempDir(), rec.senders())
	want := []string{"email:hq-e1:critical:db down", "sms:hq-e1:critical:db down", "slack:hq-e1:critical:db down"}
	if strings.Join(rec.sent, "|") != strings.Join(want, "|") {
		t.Fatalf("sent = %q, want %q", rec.sent, want)
	}
	if len(statuses) != 3 {
		t.Fatalf("statuses = %+v, want one per outside channel (bead and mail are not outside)", statuses)
	}
	for _, s := range statuses {
		if !s.RuntimeNotified || s.Error != "" {
			t.Errorf("status %+v, want notified without error", s)
		}
	}
}

func TestExecuteExternalActionsReportsSendFailure(t *testing.T) {
	t.Parallel()
	cfg := &config.EscalationConfig{Contacts: config.EscalationContacts{SlackWebhook: "https://hooks.example.com/slack"}}
	rec := &recordingSenders{err: errors.New("slack webhook returned 500: boom")}
	statuses := executeExternalActions([]string{"slack"}, cfg, "hq-e1", "high", "x", t.TempDir(), rec.senders())
	if len(statuses) != 1 || statuses[0].Error != "slack webhook returned 500: boom" || statuses[0].RuntimeNotified {
		t.Fatalf("statuses = %+v, want the send failure recorded", statuses)
	}
}

// TestEscalationAlertKey covers the identity every alert is filed under. The
// derived key is what makes a repeat firing record onto the existing open
// escalation rather than mint a second one (gt-vwry), so the derivation rules
// matter as much as the storage.
func TestAlertKey(t *testing.T) {
	t.Parallel()
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := AlertKey(tt.fingerprint, tt.source, tt.description); got != tt.want {
				t.Errorf("AlertKey(%q, %q, %q) = %q, want %q", tt.fingerprint, tt.source, tt.description, got, tt.want)
			}
		})
	}
}

// TestEscalationAlertKeyIsStableAcrossFirings pins the property the dedupe
// depends on: the same condition described the same way yields the same key,
// and therefore the same fingerprint label, on every firing.
func TestAlertKeyIsStableAcrossFirings(t *testing.T) {
	t.Parallel()

	first := FingerprintLabel(AlertKey("", "main_branch_test", "main branch test failures:"))
	second := FingerprintLabel(AlertKey("", "main_branch_test", "main branch test failures:"))
	if first != second {
		t.Errorf("fingerprint changed between firings: %q then %q", first, second)
	}
	if first == "" {
		t.Fatal("fingerprint is empty; repeated alerts would not dedupe")
	}
	if other := FingerprintLabel(AlertKey("", "jsonl_git_backup", "spike detected")); other == first {
		t.Error("distinct alerts collided onto one fingerprint")
	}
}

func TestMailTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		actions []string
		want    []string
	}{
		{
			name:    "empty actions",
			actions: []string{},
			want:    nil,
		},
		{
			name:    "nil actions",
			actions: nil,
			want:    nil,
		},
		{
			name:    "no mail actions",
			actions: []string{"bead", "log", "email:human"},
			want:    nil,
		},
		{
			name:    "single mail target",
			actions: []string{"bead", "mail:mayor"},
			want:    []string{"mayor"},
		},
		{
			name:    "multiple mail targets",
			actions: []string{"bead", "mail:mayor", "mail:gastown/witness", "email:human"},
			want:    []string{"mayor", "gastown/witness"},
		},
		{
			name:    "mail prefix with empty target ignored",
			actions: []string{"mail:"},
			want:    nil,
		},
		{
			name:    "mixed actions",
			actions: []string{"bead", "mail:mayor", "sms:human", "slack", "mail:deacon", "log"},
			want:    []string{"mayor", "deacon"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MailTargets(tt.actions)
			if len(got) != len(tt.want) {
				t.Fatalf("MailTargets(%v) returned %d targets, want %d: got %v",
					tt.actions, len(got), len(tt.want), got)
			}
			for i, target := range got {
				if target != tt.want[i] {
					t.Errorf("target[%d] = %q, want %q", i, target, tt.want[i])
				}
			}
		})
	}
}

func TestExecuteExternalActionsReportsWarningsAndFailures(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	statuses := executeExternalActions([]string{"email:human", "log"}, &config.EscalationConfig{}, "hq-esc1", "high", "desc", townRoot, failingSenders(t))
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if statuses[0].Channel != "email" || statuses[0].Warning == "" {
		t.Fatalf("expected email warning status, got %#v", statuses[0])
	}
	if statuses[1].Channel != "log" || !statuses[1].RuntimeNotified {
		t.Fatalf("expected successful log delivery status, got %#v", statuses[1])
	}
}

func TestDeliveryStatusJSONContainsPartialFailure(t *testing.T) {
	t.Parallel()
	statuses := []DeliveryStatus{{Channel: "bead", Created: true}, {Channel: "mail", Target: "mayor", Error: "notify failed"}}
	hasFailure := false
	for _, status := range statuses {
		if status.Error != "" {
			hasFailure = true
			break
		}
	}
	result := map[string]interface{}{
		"id":       "hq-esc1",
		"severity": "critical",
		"actions":  []string{"bead", "mail:mayor"},
		"targets":  []string{"mayor"},
		"delivery": statuses,
		"status":   map[bool]string{true: "partial_failure", false: "ok"}[hasFailure],
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	text := string(data)
	for _, want := range []string{"\"status\":\"partial_failure\"", "\"delivery\"", "\"channel\":\"mail\"", "\"error\":\"notify failed\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("json output missing %q: %s", want, text)
		}
	}
}

func TestDeliveryStatusJSONContainsSuccessfulMailPathDetails(t *testing.T) {
	t.Parallel()
	statuses := []DeliveryStatus{{Channel: "bead", Created: true, Severity: "critical"}, {Channel: "mail", Target: "mayor", Persisted: true, RuntimeNotified: true, Annotated: true, Severity: "critical", NotificationRoute: "mail+nudge"}}
	result := map[string]interface{}{
		"id":       "hq-esc2",
		"severity": "critical",
		"actions":  []string{"bead", "mail:mayor"},
		"targets":  []string{"mayor"},
		"delivery": statuses,
		"status":   "ok",
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	text := string(data)
	for _, want := range []string{"\"status\":\"ok\"", "\"runtime_notified\":true", "\"annotated\":true", "\"notification_route\":\"mail+nudge\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("json output missing %q: %s", want, text)
		}
	}
}

func TestFingerprintLabel(t *testing.T) {
	t.Parallel()
	got := FingerprintLabel(" deacon:await-signal:hq-deacon ")
	trimmed := FingerprintLabel("deacon:await-signal:hq-deacon")
	if got != trimmed {
		t.Fatalf("fingerprint should trim whitespace: %q != %q", got, trimmed)
	}
	if !strings.HasPrefix(got, "escalation-fp:") {
		t.Fatalf("fingerprint label %q missing prefix", got)
	}
	if len(got) != len("escalation-fp:")+12 {
		t.Fatalf("fingerprint label %q has length %d, want %d", got, len(got), len("escalation-fp:")+12)
	}
	if got == FingerprintLabel("deacon:convoy-check:hq-cv-123") {
		t.Fatal("different raw fingerprints should produce different labels")
	}
	if FingerprintLabel(" ") != "" {
		t.Fatal("blank fingerprint should produce empty label")
	}
}

func TestFormatMailBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		beadID   string
		severity string
		reason   string
		from     string
		related  string
		wantIn   []string
		notIn    []string
	}{
		{
			name:     "basic escalation",
			beadID:   "hq-abc123",
			severity: "high",
			reason:   "Build failing",
			from:     "gastown/witness",
			related:  "",
			wantIn: []string{
				"Escalation ID: hq-abc123",
				"Severity: high",
				"From: gastown/witness",
				"Reason:",
				"Build failing",
				"gt escalate ack hq-abc123",
				"gt escalate close hq-abc123",
			},
			notIn: []string{"Related:"},
		},
		{
			name:     "with related bead",
			beadID:   "hq-xyz789",
			severity: "critical",
			reason:   "Agent stuck",
			from:     "gastown/deacon",
			related:  "gt-stuck42",
			wantIn: []string{
				"Escalation ID: hq-xyz789",
				"Severity: critical",
				"Related: gt-stuck42",
			},
		},
		{
			name:     "no reason",
			beadID:   "hq-nnn",
			severity: "low",
			reason:   "",
			from:     "system",
			related:  "",
			wantIn: []string{
				"Escalation ID: hq-nnn",
				"Severity: low",
				"From: system",
			},
			notIn: []string{"Reason:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatMailBody(tt.beadID, tt.severity, tt.reason, tt.from, tt.related)
			for _, s := range tt.wantIn {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in output:\n%s", s, got)
				}
			}
			for _, s := range tt.notIn {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q in output:\n%s", s, got)
				}
			}
		})
	}
}

func TestWriteEscalationLog(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	err := writeEscalationLog(tmpDir, "hq-abc", "critical", "Test failure")
	if err != nil {
		t.Fatalf("writeEscalationLog returned error: %v", err)
	}

	logPath := tmpDir + "/logs/escalations.log"
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "[CRITICAL]") {
		t.Errorf("log entry missing severity, got: %s", content)
	}
	if !strings.Contains(content, "hq-abc") {
		t.Errorf("log entry missing bead ID, got: %s", content)
	}
	if !strings.Contains(content, "Test failure") {
		t.Errorf("log entry missing description, got: %s", content)
	}
}

func TestFormatMailBodyNeutralSubjectStillCarriesStructuredBody(t *testing.T) {
	t.Parallel()
	body := FormatMailBody("hq-abc123", "high", "Database drift", "deacon/", "gt-xyz")
	for _, want := range []string{
		"Escalation ID: hq-abc123",
		"Severity: high",
		"From: deacon/",
		"Related: gt-xyz",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}
