package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
)

// externalSender delivers one escalation to an outside channel.
type externalSender func(cfg *config.EscalationConfig, beadID, severity, description string) error

// externalSenders are the outside channels an escalation route can name.
// Tests replace them so no unit test reaches an SMTP server or a webhook.
type externalSenders struct {
	email externalSender
	slack externalSender
	sms   externalSender
}

func defaultSenders() externalSenders {
	return externalSenders{email: sendEscalationEmail, slack: sendEscalationSlack, sms: sendEscalationSMS}
}

// executeExternalActions processes external notification actions (email:, sms:, slack, log).
func executeExternalActions(actions []string, cfg *config.EscalationConfig, beadID, severity, description, townRoot string, senders externalSenders) []DeliveryStatus {
	statuses := []DeliveryStatus{}
	for _, action := range actions {
		switch {
		case strings.HasPrefix(action, "email:"):
			status := DeliveryStatus{Channel: "email", Target: strings.TrimPrefix(action, "email:"), Severity: severity}
			if cfg.Contacts.HumanEmail == "" {
				status.Warning = "contacts.human_email not configured"
				style.PrintWarning("email action '%s' skipped: contacts.human_email not configured in settings/escalation.json", action)
			} else if cfg.Contacts.SMTPHost == "" {
				status.Warning = "contacts.smtp_host not configured"
				style.PrintWarning("email action '%s' skipped: contacts.smtp_host not configured in settings/escalation.json", action)
			} else {
				if err := senders.email(cfg, beadID, severity, description); err != nil {
					status.Error = err.Error()
					style.PrintWarning("email send failed: %v", err)
				} else {
					status.RuntimeNotified = true
					fmt.Printf("  📧 Email sent to %s\n", cfg.Contacts.HumanEmail)
				}
			}
			statuses = append(statuses, status)

		case strings.HasPrefix(action, "sms:"):
			status := DeliveryStatus{Channel: "sms", Target: strings.TrimPrefix(action, "sms:"), Severity: severity}
			if cfg.Contacts.HumanSMS == "" {
				status.Warning = "contacts.human_sms not configured"
				style.PrintWarning("sms action '%s' skipped: contacts.human_sms not configured in settings/escalation.json", action)
			} else if cfg.Contacts.SMSWebhook == "" {
				status.Warning = "contacts.sms_webhook not configured"
				style.PrintWarning("sms action '%s' skipped: contacts.sms_webhook not configured in settings/escalation.json", action)
			} else {
				if err := senders.sms(cfg, beadID, severity, description); err != nil {
					status.Error = err.Error()
					style.PrintWarning("sms send failed: %v", err)
				} else {
					status.RuntimeNotified = true
					fmt.Printf("  📱 SMS sent to %s\n", cfg.Contacts.HumanSMS)
				}
			}
			statuses = append(statuses, status)

		case action == "slack":
			status := DeliveryStatus{Channel: "slack", Target: "slack", Severity: severity}
			if cfg.Contacts.SlackWebhook == "" {
				status.Warning = "contacts.slack_webhook not configured"
				style.PrintWarning("slack action skipped: contacts.slack_webhook not configured in settings/escalation.json")
			} else {
				if err := senders.slack(cfg, beadID, severity, description); err != nil {
					status.Error = err.Error()
					style.PrintWarning("slack post failed: %v", err)
				} else {
					status.RuntimeNotified = true
					fmt.Printf("  💬 Posted to Slack\n")
				}
			}
			statuses = append(statuses, status)

		case action == "log":
			status := DeliveryStatus{Channel: "log", Target: "log", Severity: severity}
			if err := writeEscalationLog(townRoot, beadID, severity, description); err != nil {
				status.Error = err.Error()
				style.PrintWarning("log write failed: %v", err)
			} else {
				status.RuntimeNotified = true
				fmt.Printf("  📝 Logged to escalation log\n")
			}
			statuses = append(statuses, status)
		}
	}
	return statuses
}

// sendEscalationEmail sends an escalation notification via SMTP.
func sendEscalationEmail(cfg *config.EscalationConfig, beadID, severity, description string) error {
	host := cfg.Contacts.SMTPHost
	port := cfg.Contacts.SMTPPort
	if port == "" {
		port = "587"
	}
	from := cfg.Contacts.SMTPFrom
	if from == "" {
		from = "gastown@localhost"
	}
	to := cfg.Contacts.HumanEmail
	subject := fmt.Sprintf("[Gas Town %s] %s", strings.ToUpper(severity), description)

	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n"+
		"Gas Town Escalation\r\n"+
		"====================\r\n"+
		"Bead: %s\r\n"+
		"Severity: %s\r\n"+
		"Description: %s\r\n\r\n"+
		"Acknowledge: gt escalate ack %s\r\n",
		from, to, subject, beadID, strings.ToUpper(severity), description, beadID)

	addr := fmt.Sprintf("%s:%s", host, port)

	var auth smtp.Auth
	if cfg.Contacts.SMTPUser != "" {
		auth = smtp.PlainAuth("", cfg.Contacts.SMTPUser, cfg.Contacts.SMTPPass, host)
	}

	return smtp.SendMail(addr, auth, from, []string{to}, []byte(body))
}

// sendEscalationSlack posts an escalation notification to a Slack webhook.
func sendEscalationSlack(cfg *config.EscalationConfig, beadID, severity, description string) error {
	severityEmoji := map[string]string{
		"critical": "🔴",
		"high":     "🟠",
		"medium":   "🟡",
	}
	emoji := severityEmoji[severity]
	if emoji == "" {
		emoji = "⚪"
	}

	payload := map[string]string{
		"text": fmt.Sprintf("%s *[%s] Escalation %s*\n%s\n_Acknowledge: `gt escalate ack %s`_",
			emoji, strings.ToUpper(severity), beadID, description, beadID),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling slack payload: %w", err)
	}

	resp, err := http.Post(cfg.Contacts.SlackWebhook, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("posting to slack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("slack webhook returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// sendEscalationSMS posts an escalation notification via SMS webhook (e.g. Twilio).
func sendEscalationSMS(cfg *config.EscalationConfig, beadID, severity, description string) error {
	payload := map[string]string{
		"to":   cfg.Contacts.HumanSMS,
		"body": fmt.Sprintf("[Gas Town %s] %s (bead: %s)", strings.ToUpper(severity), description, beadID),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling sms payload: %w", err)
	}

	resp, err := http.Post(cfg.Contacts.SMSWebhook, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("posting to sms webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sms webhook returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// writeEscalationLog appends an escalation entry to the log file.
func writeEscalationLog(townRoot, beadID, severity, description string) error {
	logDir := fmt.Sprintf("%s/logs", townRoot)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	logPath := fmt.Sprintf("%s/escalations.log", logDir)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	defer f.Close()

	entry := fmt.Sprintf("%s [%s] %s: %s\n", time.Now().Format(time.RFC3339), strings.ToUpper(severity), beadID, description)
	_, err = f.WriteString(entry)
	return err
}
