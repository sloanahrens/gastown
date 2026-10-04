package notify

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
)

// EscalationRequest is one firing of an alert as `gt escalate` receives it.
type EscalationRequest struct {
	TownRoot    string
	Severity    string // already validated and lower-cased
	Description string
	Reason      string
	Source      string
	Fingerprint string // explicit alert key; empty derives it (AlertKey)
	RelatedBead string
	EscalatedBy string // sender address
	// Prefixes names the mail recipients' sessions for their notifications;
	// nil gives every rig session.DefaultPrefix.
	Prefixes *session.PrefixRegistry
}

// DeliveryStatus is the outcome of one delivery channel for one firing.
type DeliveryStatus struct {
	Target            string `json:"target,omitempty"`
	Channel           string `json:"channel"`
	Created           bool   `json:"created,omitempty"`
	Persisted         bool   `json:"persisted,omitempty"`
	RuntimeNotified   bool   `json:"runtime_notified,omitempty"`
	Annotated         bool   `json:"annotated,omitempty"`
	Severity          string `json:"severity,omitempty"`
	Error             string `json:"error,omitempty"`
	Warning           string `json:"warning,omitempty"`
	NotificationRoute string `json:"notification_route,omitempty"`
}

// RaiseResult says what Raise did.
type RaiseResult struct {
	// ID is the escalation bead: the new one, or the open one a repeat
	// firing was recorded on.
	ID string
	// Duplicate is true when the firing was recorded on an existing
	// escalation instead of creating one.
	Duplicate bool
	// Occurrences and Renotified describe a duplicate firing.
	Occurrences int
	Renotified  bool
	// AlertKey is the key the firing was filed under; FingerprintLabel is
	// its bead label ("" when the key is empty).
	AlertKey         string
	FingerprintLabel string
	// Actions and Targets are the configured route for the severity.
	Actions []string
	Targets []string
	// Delivery is one entry per channel tried. It is empty for a duplicate
	// firing that was not re-notified.
	Delivery []DeliveryStatus
}

// Raise files one firing of an escalation: it records a repeat on the open
// escalation with the same alert key when there is one (re-notifying only
// once the renotify window has elapsed, gt-9qg1), and otherwise creates the
// escalation bead and routes it to every channel cfg names for the severity.
//
// Delivery problems are reported in the result, not as an error: the
// escalation exists once the bead does.
func Raise(req EscalationRequest, cfg *config.EscalationConfig) (*RaiseResult, error) {
	bd := beads.New(beads.ResolveBeadsDir(req.TownRoot))
	actions := cfg.GetRouteForSeverity(req.Severity)
	res := &RaiseResult{Actions: actions, Targets: MailTargets(actions)}

	// Every alert carries a stable key so that a recurring condition upserts
	// onto the one bead that represents it rather than minting a fresh P0/P1
	// record per firing (gt-vwry). An explicit fingerprint wins; otherwise
	// the key is derived from the source and description, which is enough for
	// producers that pass a stable class string (the daemon's patrol readers
	// pass "jsonl_git_backup: ..." / "main_branch_test: ..." titles).
	res.AlertKey = AlertKey(req.Fingerprint, req.Source, req.Description)
	res.FingerprintLabel = FingerprintLabel(res.AlertKey)
	if res.FingerprintLabel != "" {
		matches, err := beads.ListEscalationsByFingerprint(bd, res.FingerprintLabel)
		if err != nil {
			return nil, fmt.Errorf("checking escalation fingerprint: %w", err)
		}
		if len(matches) > 0 {
			existing := matches[0]
			// BumpEscalation decides whether this firing should re-notify, based
			// on how long it's been since the alert's last actual notification —
			// a repeat firing that arrives inside the renotify window bumps the
			// occurrence count and stops there, so a fast-recurring condition
			// doesn't spam every channel on every cycle. One that arrives after
			// the window elapsed must still reach a human, or a persisting
			// condition would go silent forever after its first alert (gt-9qg1).
			occurrences, renotify, err := beads.BumpEscalation(bd, existing.ID, req.Severity, req.Reason, req.Source, cfg.GetRenotifyWindow())
			if err != nil {
				return nil, fmt.Errorf("recording repeat of escalation %s: %w", existing.ID, err)
			}
			res.ID = existing.ID
			res.Duplicate = true
			res.Occurrences = occurrences
			res.Renotified = renotify
			if renotify {
				res.Delivery = sendNotifications(req, existing.ID, cfg, defaultSenders())
			}
			return res, nil
		}
	}

	notifiedAt := time.Now().Format(time.RFC3339)
	fields := &beads.EscalationFields{
		Severity:    req.Severity,
		Reason:      req.Reason,
		Source:      req.Source,
		EscalatedBy: req.EscalatedBy,
		EscalatedAt: notifiedAt,
		RelatedBead: req.RelatedBead,
		Fingerprint: res.FingerprintLabel,
		// The create path always notifies immediately, so LastNotifiedAt
		// starts equal to EscalatedAt rather than empty — a repeat firing's
		// renotify-window check (gt-9qg1) then measures from this bead's real
		// first notification instead of falling back to it implicitly.
		LastNotifiedAt: notifiedAt,
	}
	issue, err := beads.CreateEscalationBead(bd, req.Description, fields)
	if err != nil {
		return nil, fmt.Errorf("creating escalation bead: %w", err)
	}
	res.ID = issue.ID
	res.Delivery = sendNotifications(req, issue.ID, cfg, defaultSenders())
	return res, nil
}

// sendNotifications routes one firing of an escalation to its configured
// channels (mail, email, sms, slack, log) and records the send to the
// activity feed. Shared by the create path (which always notifies) and the
// repeat-firing path (which notifies only once BumpEscalation says the
// renotify window has elapsed, gt-9qg1) so the two can never drift into
// sending through different channels for the same alert.
func sendNotifications(req EscalationRequest, issueID string, cfg *config.EscalationConfig, senders externalSenders) []DeliveryStatus {
	severity, description, townRoot, agentID := req.Severity, req.Description, req.TownRoot, req.EscalatedBy
	actions := cfg.GetRouteForSeverity(severity)
	targets := MailTargets(actions)

	router := mail.NewRouter(townRoot, req.Prefixes)
	defer router.WaitPendingNotifications()
	statuses := []DeliveryStatus{{Channel: "bead", Created: true, Severity: severity}}
	for _, target := range targets {
		status := DeliveryStatus{Target: target, Channel: "mail", Severity: severity, NotificationRoute: "mail+nudge"}
		msg := &mail.Message{
			From:     agentID,
			To:       target,
			Subject:  fmt.Sprintf("[%s] %s", strings.ToUpper(severity), description),
			Body:     FormatMailBody(issueID, severity, req.Reason, agentID, req.RelatedBead),
			Type:     mail.TypeEscalation,
			ThreadID: issueID,
		}

		// Set priority based on severity
		switch severity {
		case config.SeverityCritical:
			msg.Priority = mail.PriorityUrgent
		case config.SeverityHigh:
			msg.Priority = mail.PriorityHigh
		case config.SeverityMedium:
			msg.Priority = mail.PriorityNormal
		default:
			msg.Priority = mail.PriorityLow
		}

		if err := router.Send(msg); err != nil {
			status.Error = err.Error()
			statuses = append(statuses, status)
			style.PrintWarning("failed to send to %s: %v", target, err)
			continue
		}
		status.Persisted = true
		status.RuntimeNotified = true

		mailBeads := beads.New(beads.ResolveBeadsDir(townRoot))
		mailIssue, err := beads.FindLatestIssueByTitleAndAssignee(mailBeads, msg.Subject, mail.AddressToIdentity(target))
		if err != nil {
			status.Warning = fmt.Sprintf("annotation lookup failed: %v", err)
			statuses = append(statuses, status)
			style.PrintWarning("failed to annotate escalation mail for %s: %v", target, err)
			continue
		}

		addLabels := []string{
			fmt.Sprintf("severity:%s", severity),
			fmt.Sprintf("escalation:%s", issueID),
		}
		if err := mailBeads.Update(mailIssue.ID, beads.UpdateOptions{AddLabels: addLabels}); err != nil {
			status.Warning = fmt.Sprintf("annotation update failed: %v", err)
			style.PrintWarning("failed to annotate escalation mail labels for %s: %v", target, err)
		} else {
			status.Annotated = true
		}
		statuses = append(statuses, status)
	}

	// Process external notification actions (email:, sms:, slack, log)
	statuses = append(statuses, executeExternalActions(actions, cfg, issueID, severity, description, townRoot, senders)...)

	// Log to activity feed
	payload := events.EscalationPayload(issueID, agentID, strings.Join(targets, ","), description)
	payload["severity"] = severity
	payload["actions"] = strings.Join(actions, ",")
	if req.Source != "" {
		payload["source"] = req.Source
	}
	_ = events.LogFeed(events.TypeEscalationSent, agentID, payload)

	return statuses
}

// Clear closes the open escalations filed under any of keys, because the
// condition that raised them no longer holds (gt-vwry), and returns the IDs
// it closed. Clearing keys that match nothing is the healthy-path case and
// returns no IDs and no error.
func Clear(townRoot string, keys []string, closedBy, reason string) ([]string, error) {
	labels := make([]string, 0, len(keys))
	for _, key := range keys {
		if label := FingerprintLabel(key); label != "" {
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("clear requires --fingerprint or a description (see gt escalate clear --help)")
	}
	if reason == "" {
		reason = fmt.Sprintf("condition cleared: %s", strings.Join(keys, ", "))
	}

	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	closed, err := beads.CloseEscalationsByFingerprints(bd, labels, closedBy, reason)
	if err != nil {
		return nil, fmt.Errorf("clearing escalations for %s: %w", strings.Join(keys, ", "), err)
	}
	for _, id := range closed {
		_ = events.LogFeed(events.TypeEscalationClosed, closedBy, map[string]interface{}{
			"id":     id,
			"keys":   strings.Join(keys, ","),
			"reason": reason,
			"source": "auto-clear",
		})
	}
	return closed, nil
}

// FingerprintLabel is the bead label an alert key is stored under, or "" for
// a blank key.
func FingerprintLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("escalation-fp:%x", sum[:6])
}

// AlertKey is the stable identity of an alert: its class (source, when the
// producer names one) plus its subject. Two firings of the same condition
// produce the same key, which is what lets Raise record the repeat on the
// existing bead instead of minting a new one (gt-vwry), and what Clear later
// matches on to auto-close it once the condition clears.
//
// An explicit fingerprint is returned verbatim: a producer that knows its
// alert's identity (a bead id, a branch, a polecat) should say so rather than
// have one inferred from prose that may embed varying detail.
func AlertKey(fingerprint, source, description string) string {
	if key := strings.TrimSpace(fingerprint); key != "" {
		return key
	}
	subject := strings.Join(strings.Fields(description), " ")
	if subject == "" {
		return ""
	}
	if src := strings.TrimSpace(source); src != "" {
		return src + ": " + subject
	}
	return subject
}

// MailTargets extracts the mail targets from route actions:
// ["bead", "mail:gastown/witness", "email:human"] returns ["gastown/witness"].
func MailTargets(actions []string) []string {
	var targets []string
	for _, action := range actions {
		if strings.HasPrefix(action, "mail:") {
			target := strings.TrimPrefix(action, "mail:")
			if target != "" {
				targets = append(targets, target)
			}
		}
	}
	return targets
}

// FormatMailBody is the body of the mail an escalation sends each target.
func FormatMailBody(beadID, severity, reason, from, related string) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Escalation ID: %s", beadID))
	lines = append(lines, fmt.Sprintf("Severity: %s", severity))
	lines = append(lines, fmt.Sprintf("From: %s", from))
	if reason != "" {
		lines = append(lines, "")
		lines = append(lines, "Reason:")
		lines = append(lines, reason)
	}
	if related != "" {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("Related: %s", related))
	}
	lines = append(lines, "")
	lines = append(lines, "---")
	lines = append(lines, "To acknowledge: gt escalate ack "+beadID)
	lines = append(lines, "To close: gt escalate close "+beadID+" --reason \"resolution\"")
	return strings.Join(lines, "\n")
}
