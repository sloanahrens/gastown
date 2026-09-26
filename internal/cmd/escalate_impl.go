package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

func runEscalate(cmd *cobra.Command, args []string) error {
	// Handle --stdin: read reason from stdin (avoids shell quoting issues)
	if escalateStdin {
		if escalateReason != "" {
			return fmt.Errorf("cannot use --stdin with --reason/-r")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		escalateReason = strings.TrimRight(string(data), "\n")
	}

	// Require at least a description when creating an escalation
	if len(args) == 0 {
		return cmd.Help()
	}

	description := strings.Join(args, " ")

	// Validate severity
	severity := strings.ToLower(escalateSeverity)
	if !config.IsValidSeverity(severity) {
		return fmt.Errorf("invalid severity '%s': must be critical, high, medium, or low", escalateSeverity)
	}

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load escalation config
	escalationConfig, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading escalation config: %w", err)
	}

	// Detect agent identity
	agentID := detectSender()
	if agentID == "" {
		agentID = "unknown"
	}

	// Dry run mode
	if escalateDryRun {
		actions := escalationConfig.GetRouteForSeverity(severity)
		targets := extractMailTargetsFromActions(actions)
		fmt.Printf("Would create escalation:\n")
		fmt.Printf("  Severity: %s\n", severity)
		fmt.Printf("  Description: %s\n", description)
		fmt.Printf("  Reason: %s\n", reasonDisplay(escalateReason))
		if escalateSource != "" {
			fmt.Printf("  Source: %s\n", escalateSource)
		}
		if escalateFingerprint != "" {
			fmt.Printf("  Fingerprint: %s\n", escalationFingerprintLabel(escalateFingerprint))
		}
		fmt.Printf("  Actions: %s\n", strings.Join(actions, ", "))
		fmt.Printf("  Mail targets: %s\n", strings.Join(targets, ", "))
		return nil
	}

	// Create escalation bead
	bd := beads.New(beads.ResolveBeadsDir(townRoot))

	// Every alert carries a stable key so that a recurring condition upserts
	// onto the one bead that represents it rather than minting a fresh P0/P1
	// record per firing (gt-vwry). An explicit --fingerprint wins; otherwise
	// the key is derived from the source and description, which is enough for
	// producers that pass a stable class string (the daemon's patrol readers
	// pass "jsonl_git_backup: ..." / "main_branch_test: ..." titles).
	alertKey := escalationAlertKey(escalateSource, description)
	fingerprintLabel := escalationFingerprintLabel(alertKey)
	if fingerprintLabel != "" {
		matches, err := bd.ListEscalationsByFingerprint(fingerprintLabel)
		if err != nil {
			return fmt.Errorf("checking escalation fingerprint: %w", err)
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
			occurrences, renotify, err := bd.BumpEscalation(existing.ID, severity, escalateReason, escalateSource, escalationConfig.GetRenotifyWindow())
			if err != nil {
				return fmt.Errorf("recording repeat of escalation %s: %w", existing.ID, err)
			}

			var statuses []deliveryStatus
			if renotify {
				statuses = sendEscalationNotifications(townRoot, existing.ID, severity, description, escalateReason, escalateSource, agentID, escalateRelatedBead, escalationConfig)
			}

			if escalateJSON {
				result := map[string]interface{}{
					"id":          existing.ID,
					"status":      "duplicate_recorded",
					"fingerprint": fingerprintLabel,
					"occurrences": occurrences,
					"renotified":  renotify,
				}
				if renotify {
					result["delivery"] = statuses
				}
				out, _ := json.MarshalIndent(result, "", "  ")
				fmt.Println(string(out))
			} else {
				fmt.Printf("%s Repeat escalation recorded on %s (occurrence %d)\n", style.Bold.Render("✓"), existing.ID, occurrences)
				fmt.Printf("  Fingerprint: %s\n", fingerprintLabel)
				if renotify {
					fmt.Printf("  Re-notified (renotify window elapsed)\n")
					for _, status := range statuses {
						if status.Error != "" {
							fmt.Printf("  Delivery issue [%s:%s]: %s\n", status.Channel, status.Target, status.Error)
						}
					}
				} else {
					fmt.Printf("  Not re-notified (within renotify window)\n")
				}
				fmt.Printf("  Clear when resolved with: gt escalate clear --fingerprint %q\n", alertKey)
			}
			return nil
		}
	}
	notifiedAt := time.Now().Format(time.RFC3339)
	fields := &beads.EscalationFields{
		Severity:    severity,
		Reason:      escalateReason,
		Source:      escalateSource,
		EscalatedBy: agentID,
		EscalatedAt: notifiedAt,
		RelatedBead: escalateRelatedBead,
		Fingerprint: fingerprintLabel,
		// The create path always notifies immediately, so LastNotifiedAt
		// starts equal to EscalatedAt rather than empty — a repeat firing's
		// renotify-window check (gt-9qg1) then measures from this bead's real
		// first notification instead of falling back to it implicitly.
		LastNotifiedAt: notifiedAt,
	}

	issue, err := bd.CreateEscalationBead(description, fields)
	if err != nil {
		return fmt.Errorf("creating escalation bead: %w", err)
	}

	statuses := sendEscalationNotifications(townRoot, issue.ID, severity, description, escalateReason, escalateSource, agentID, escalateRelatedBead, escalationConfig)
	actions := escalationConfig.GetRouteForSeverity(severity)
	targets := extractMailTargetsFromActions(actions)

	// Output
	if escalateJSON {
		hasFailure := false
		for _, status := range statuses {
			if status.Error != "" {
				hasFailure = true
				break
			}
		}
		result := map[string]interface{}{
			"id":       issue.ID,
			"severity": severity,
			"reason":   escalateReason,
			"actions":  actions,
			"targets":  targets,
			"delivery": statuses,
			"status":   map[bool]string{true: "partial_failure", false: "ok"}[hasFailure],
		}
		if escalateSource != "" {
			result["source"] = escalateSource
		}
		if fingerprintLabel != "" {
			result["fingerprint"] = fingerprintLabel
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
	} else {
		emoji := severityEmoji(severity)
		fmt.Printf("%s Escalation created: %s\n", emoji, issue.ID)
		fmt.Printf("  Severity: %s\n", severity)
		fmt.Printf("  Reason: %s\n", reasonDisplay(escalateReason))
		if escalateSource != "" {
			fmt.Printf("  Source: %s\n", escalateSource)
		}
		if fingerprintLabel != "" {
			fmt.Printf("  Fingerprint: %s\n", fingerprintLabel)
		}
		fmt.Printf("  Routed to: %s\n", strings.Join(targets, ", "))
		for _, status := range statuses {
			if status.Error != "" {
				fmt.Printf("  Delivery issue [%s:%s]: %s\n", status.Channel, status.Target, status.Error)
			}
		}
	}

	return nil
}

// sendEscalationNotifications routes one firing of an escalation to its
// configured channels (mail, email, sms, slack, log) and records the send to
// the activity feed. Shared by the create path (which always notifies) and
// the repeat-firing path (which notifies only once BumpEscalation says the
// renotify window has elapsed, gt-9qg1) so the two can never drift into
// sending through different channels for the same alert.
func sendEscalationNotifications(townRoot, issueID, severity, description, reason, source, agentID, relatedBead string, escalationConfig *config.EscalationConfig) []deliveryStatus {
	actions := escalationConfig.GetRouteForSeverity(severity)
	targets := extractMailTargetsFromActions(actions)

	router := mail.NewRouter(townRoot)
	defer router.WaitPendingNotifications()
	statuses := []deliveryStatus{{Channel: "bead", Created: true, Severity: severity}}
	for _, target := range targets {
		status := deliveryStatus{Target: target, Channel: "mail", Severity: severity, NotificationRoute: "mail+nudge"}
		msg := &mail.Message{
			From:     agentID,
			To:       target,
			Subject:  fmt.Sprintf("[%s] %s", strings.ToUpper(severity), description),
			Body:     formatEscalationMailBody(issueID, severity, reason, agentID, relatedBead),
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
		mailIssue, err := mailBeads.FindLatestIssueByTitleAndAssignee(msg.Subject, mail.AddressToIdentity(target))
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
	statuses = append(statuses, executeExternalActions(actions, escalationConfig, issueID, severity, description, townRoot)...)

	// Log to activity feed
	payload := events.EscalationPayload(issueID, agentID, strings.Join(targets, ","), description)
	payload["severity"] = severity
	payload["actions"] = strings.Join(actions, ",")
	if source != "" {
		payload["source"] = source
	}
	_ = events.LogFeed(events.TypeEscalationSent, agentID, payload)

	return statuses
}

// runEscalateClear closes open escalations that share an alert key, because the
// condition that raised them no longer holds (gt-vwry).
//
// Keys are taken from repeated --fingerprint flags verbatim, and/or derived
// from --source plus the positional description the same way runEscalate
// derives them — so a producer that raised an alert with
// `gt escalate -s HIGH --source X "Y"` can clear it with
// `gt escalate clear --source X "Y"` and no bookkeeping. A producer that owns
// several keys (the daemon's patrol readers) clears them all in one call, which
// costs one listing rather than one per key.
//
// Clearing a key that matches nothing is the healthy-path case, not a failure:
// a producer re-checks every cycle, and on the cycle where the condition is
// gone the first clear wins while every later one is a no-op that must not
// turn a patrol into an error. Exit status is therefore 0 either way.
func runEscalateClear(cmd *cobra.Command, args []string) error {
	// Keys come from repeated --fingerprint flags and/or one derived from
	// --source plus the positional description.
	keys := make([]string, 0, len(escalateClearKeys)+1)
	keys = append(keys, escalateClearKeys...)
	if derived := escalationAlertKey(escalateSource, strings.Join(args, " ")); strings.TrimSpace(derived) != "" {
		keys = append(keys, derived)
	}

	labels := make([]string, 0, len(keys))
	for _, key := range keys {
		if label := escalationFingerprintLabel(key); label != "" {
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return fmt.Errorf("clear requires --fingerprint or a description (see gt escalate clear --help)")
	}

	closedBy := detectSender()
	if closedBy == "" {
		closedBy = "unknown"
	}

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	reason := escalateClearReason
	if reason == "" {
		reason = fmt.Sprintf("condition cleared: %s", strings.Join(keys, ", "))
	}

	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	closed, err := bd.CloseEscalationsByFingerprints(labels, closedBy, reason)
	if err != nil {
		return fmt.Errorf("clearing escalations for %s: %w", strings.Join(keys, ", "), err)
	}

	for _, id := range closed {
		_ = events.LogFeed(events.TypeEscalationClosed, closedBy, map[string]interface{}{
			"id":     id,
			"keys":   strings.Join(keys, ","),
			"reason": reason,
			"source": "auto-clear",
		})
	}

	if escalateJSON {
		result := map[string]interface{}{
			"keys":   keys,
			"closed": closed,
			"count":  len(closed),
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	if len(closed) == 0 {
		fmt.Printf("%s Nothing to clear for %s\n", style.Bold.Render("✓"), strings.Join(keys, ", "))
		return nil
	}
	fmt.Printf("%s Cleared %d escalation(s) for %s: %s\n", style.Bold.Render("✓"), len(closed), strings.Join(keys, ", "), strings.Join(closed, ", "))
	return nil
}

// reasonDisplay renders an escalation's reason field for human-readable
// output, making an absent reason explicit rather than printing nothing.
// gt-umx6: a silently omitted reason line was mistaken for lost escalation
// data by two operators independently, since an escalation created with
// only a description (no --reason) genuinely stores an empty reason.
func reasonDisplay(reason string) string {
	if reason == "" {
		return "(none provided)"
	}
	return reason
}

func escalationFingerprintLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("escalation-fp:%x", sum[:6])
}

// escalationAlertKey is the stable identity of an alert: its class (source,
// when the producer names one) plus its subject. Two firings of the same
// condition produce the same key, which is what lets runEscalate record the
// repeat on the existing bead instead of minting a new one (gt-vwry), and what
// `gt escalate clear` later matches on to auto-close it once the condition
// clears.
//
// An explicit --fingerprint is returned verbatim: a producer that knows its
// alert's identity (a bead id, a branch, a polecat) should say so rather than
// have one inferred from prose that may embed varying detail.
func escalationAlertKey(source, description string) string {
	if key := strings.TrimSpace(escalateFingerprint); key != "" {
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

type deliveryStatus struct {
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

func runEscalateList(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	bd := beads.New(beads.ResolveBeadsDir(townRoot))

	// Both branches query across rigs: escalations live in the database of the
	// rig that filed them, so a single-database list reports "No escalations
	// found" while other rigs' escalations sit open (gt-wbxb).
	var issues []*beads.Issue
	if escalateListAll {
		// List all (open and closed)
		issues, err = bd.ListAllEscalationsAcrossRigs()
		if err != nil {
			return fmt.Errorf("listing escalations: %w", err)
		}
	} else {
		issues, err = bd.ListEscalationsAcrossRigs()
		if err != nil {
			return fmt.Errorf("listing escalations: %w", err)
		}
	}

	// Cross-check each entry against live Dolt to filter out phantom escalations.
	// When a rig's Dolt server dies and is restarted fresh, the label-based list
	// query may still return stale IDs (e.g. from a cached or cross-rig query)
	// that no longer exist in the live database. We skip any entries that cannot
	// be fetched individually, since they cannot be acked or closed anyway.
	var live []*beads.Issue
	var phantomCount int
	for _, issue := range issues {
		if _, err := bd.Show(issue.ID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				phantomCount++
				fmt.Fprintf(os.Stderr, "warning: skipping unresolvable escalation %s (not found in live Dolt)\n", issue.ID)
				continue
			}
			// For other errors (e.g. Dolt temporarily unreachable), include
			// the entry so the user can see it — just warn.
			fmt.Fprintf(os.Stderr, "warning: could not verify escalation %s: %v\n", issue.ID, err)
		}
		live = append(live, issue)
	}
	issues = live

	if escalateListJSON {
		out, _ := json.MarshalIndent(issues, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	if len(issues) == 0 {
		if phantomCount > 0 {
			fmt.Printf("No escalations found (%d phantom entr%s skipped — bead IDs no longer exist in live Dolt)\n",
				phantomCount, map[bool]string{true: "y", false: "ies"}[phantomCount == 1])
		} else {
			fmt.Println("No escalations found")
		}
		return nil
	}

	fmt.Printf("Escalations (%d):\n\n", len(issues))
	for _, issue := range issues {
		fields := beads.ParseEscalationFields(issue.Description)
		emoji := severityEmoji(fields.Severity)

		status := issue.Status
		if beads.HasLabel(issue, "acked") {
			status = "acked"
		}

		fmt.Printf("  %s %s [%s] %s\n", emoji, issue.ID, status, issue.Title)
		fmt.Printf("     Severity: %s | From: %s | %s\n",
			fields.Severity, fields.EscalatedBy, formatRelativeTime(issue.CreatedAt))
		if fields.AckedBy != "" {
			fmt.Printf("     Acked by: %s\n", fields.AckedBy)
		}
		fmt.Println()
	}

	return nil
}

func runEscalateAck(cmd *cobra.Command, args []string) error {
	escalationID := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Detect who is acknowledging
	ackedBy := detectSender()
	if ackedBy == "" {
		ackedBy = "unknown"
	}

	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	if err := bd.AckEscalation(escalationID, ackedBy); err != nil {
		return fmt.Errorf("acknowledging escalation: %w", err)
	}

	// Log to activity feed
	_ = events.LogFeed(events.TypeEscalationAcked, ackedBy, map[string]interface{}{
		"escalation_id": escalationID,
		"acked_by":      ackedBy,
	})

	fmt.Printf("%s Escalation acknowledged: %s\n", style.Bold.Render("✓"), escalationID)
	return nil
}

func runEscalateClose(cmd *cobra.Command, args []string) error {
	escalationID := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Detect who is closing
	closedBy := detectSender()
	if closedBy == "" {
		closedBy = "unknown"
	}

	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	if err := bd.CloseEscalation(escalationID, closedBy, escalateCloseReason); err != nil {
		return fmt.Errorf("closing escalation: %w", err)
	}

	// The escalation's routed mail notification(s) share its thread but are
	// separate beads (see mail.Router.buildLabels) — closing the escalation
	// wisp doesn't close them. Left open, they accumulate as phantom P1/P2s
	// on the dashboard and pollute `bd ready`. Close them too, best-effort.
	closedDeliveries, err := closeEscalationDeliveryBeads(bd, escalationID, closedBy)
	if err != nil {
		style.PrintWarning("failed to close escalation delivery bead(s): %v", err)
	}

	// Log to activity feed
	_ = events.LogFeed(events.TypeEscalationClosed, closedBy, map[string]interface{}{
		"escalation_id": escalationID,
		"closed_by":     closedBy,
		"reason":        escalateCloseReason,
	})

	fmt.Printf("%s Escalation closed: %s\n", style.Bold.Render("✓"), escalationID)
	fmt.Printf("  Reason: %s\n", escalateCloseReason)
	if closedDeliveries > 0 {
		fmt.Printf("  Delivery beads closed: %d\n", closedDeliveries)
	}
	return nil
}

// closeEscalationDeliveryBeads closes the open mail-delivery beads routed for
// an escalation (identified by thread:<escalationID>), so they don't linger
// as phantom open escalations on the dashboard or in `bd ready`. Returns the
// number closed.
func closeEscalationDeliveryBeads(bd *beads.Beads, escalationID, closedBy string) (int, error) {
	out, err := bd.Run("list", "--label=gt:message", "--label=thread:"+escalationID, "--status=open", "--include-infra", "--json")
	if err != nil {
		return 0, fmt.Errorf("listing delivery beads: %w", err)
	}

	var issues []*beads.Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return 0, fmt.Errorf("parsing delivery beads: %w", err)
	}
	if len(issues) == 0 {
		return 0, nil
	}

	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	reason := fmt.Sprintf("escalation %s closed by %s", escalationID, closedBy)
	if err := bd.CloseWithReason(reason, ids...); err != nil {
		return 0, fmt.Errorf("closing %d delivery bead(s): %w", len(ids), err)
	}
	return len(ids), nil
}

func runEscalateStale(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load escalation config for threshold and max reescalations
	escalationConfig, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading escalation config: %w", err)
	}

	threshold := escalationConfig.GetStaleThreshold()
	maxReescalations := escalationConfig.GetMaxReescalations()

	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	stale, err := bd.ListStaleEscalations(threshold)
	if err != nil {
		return fmt.Errorf("listing stale escalations: %w", err)
	}

	if len(stale) == 0 {
		if !escalateStaleJSON {
			fmt.Printf("No stale escalations (threshold: %s)\n", threshold)
		} else {
			fmt.Println("[]")
		}
		return nil
	}

	// Detect who is reescalating
	reescalatedBy := detectSender()
	if reescalatedBy == "" {
		reescalatedBy = "system"
	}

	// Dry run mode - just show what would happen
	if escalateDryRun {
		fmt.Printf("Would re-escalate %d stale escalations (threshold: %s):\n\n", len(stale), threshold)
		for _, issue := range stale {
			fields := beads.ParseEscalationFields(issue.Description)
			newSeverity := getNextSeverity(fields.Severity)
			willSkip := maxReescalations > 0 && fields.ReescalationCount >= maxReescalations
			if fields.Severity == "critical" {
				willSkip = true
			}

			emoji := severityEmoji(fields.Severity)
			if willSkip {
				fmt.Printf("  %s %s [SKIP] %s\n", emoji, issue.ID, issue.Title)
				if fields.Severity == "critical" {
					fmt.Printf("     Already at critical severity\n")
				} else {
					fmt.Printf("     Already at max reescalations (%d)\n", maxReescalations)
				}
			} else {
				fmt.Printf("  %s %s %s\n", emoji, issue.ID, issue.Title)
				fmt.Printf("     %s → %s (reescalation %d/%d)\n",
					fields.Severity, newSeverity, fields.ReescalationCount+1, maxReescalations)
			}
			fmt.Println()
		}
		return nil
	}

	// Perform re-escalation
	var results []*beads.ReescalationResult
	router := mail.NewRouter(townRoot)
	defer router.WaitPendingNotifications()

	for _, issue := range stale {
		result, err := bd.ReescalateEscalation(issue.ID, reescalatedBy, maxReescalations)
		if err != nil {
			style.PrintWarning("failed to reescalate %s: %v", issue.ID, err)
			continue
		}
		results = append(results, result)

		// If not skipped, re-route to new severity targets
		if !result.Skipped {
			actions := escalationConfig.GetRouteForSeverity(result.NewSeverity)
			targets := extractMailTargetsFromActions(actions)

			// Send mail to each target about the reescalation
			for _, target := range targets {
				msg := &mail.Message{
					From:    reescalatedBy,
					To:      target,
					Subject: fmt.Sprintf("[%s→%s] Re-escalated: %s", strings.ToUpper(result.OldSeverity), strings.ToUpper(result.NewSeverity), result.Title),
					Body:    formatReescalationMailBody(result, reescalatedBy),
					Type:    mail.TypeTask,
				}

				// Set priority based on new severity
				switch result.NewSeverity {
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
					style.PrintWarning("failed to send reescalation to %s: %v", target, err)
				}
			}

			// Log to activity feed
			_ = events.LogFeed(events.TypeEscalationSent, reescalatedBy, map[string]interface{}{
				"escalation_id":    result.ID,
				"reescalated":      true,
				"old_severity":     result.OldSeverity,
				"new_severity":     result.NewSeverity,
				"reescalation_num": result.ReescalationNum,
				"targets":          strings.Join(targets, ","),
			})
		}
	}

	// Output results
	if escalateStaleJSON {
		out, _ := json.MarshalIndent(results, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	reescalated := 0
	skipped := 0
	for _, r := range results {
		if r.Skipped {
			skipped++
		} else {
			reescalated++
		}
	}

	if reescalated == 0 && skipped > 0 {
		fmt.Printf("No escalations re-escalated (%d at max level)\n", skipped)
		return nil
	}

	fmt.Printf("🔄 Re-escalated %d stale escalations:\n\n", reescalated)
	for _, result := range results {
		if result.Skipped {
			continue
		}
		emoji := severityEmoji(result.NewSeverity)
		fmt.Printf("  %s %s: %s → %s (reescalation %d)\n",
			emoji, result.ID, result.OldSeverity, result.NewSeverity, result.ReescalationNum)
	}

	if skipped > 0 {
		fmt.Printf("\n  (%d skipped - at max level)\n", skipped)
	}

	return nil
}

func getNextSeverity(severity string) string {
	switch severity {
	case "low":
		return "medium"
	case "medium":
		return "high"
	case "high":
		return "critical"
	default:
		return "critical"
	}
}

func formatReescalationMailBody(result *beads.ReescalationResult, reescalatedBy string) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Escalation ID: %s", result.ID))
	lines = append(lines, fmt.Sprintf("Severity bumped: %s → %s", result.OldSeverity, result.NewSeverity))
	lines = append(lines, fmt.Sprintf("Reescalation #%d", result.ReescalationNum))
	lines = append(lines, fmt.Sprintf("Reescalated by: %s", reescalatedBy))
	lines = append(lines, "")
	lines = append(lines, "This escalation was not acknowledged within the stale threshold and has been automatically re-escalated to a higher severity.")
	lines = append(lines, "")
	lines = append(lines, "---")
	lines = append(lines, "To acknowledge: gt escalate ack "+result.ID)
	lines = append(lines, "To close: gt escalate close "+result.ID+" --reason \"resolution\"")
	return strings.Join(lines, "\n")
}

func runEscalateShow(cmd *cobra.Command, args []string) error {
	escalationID := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	issue, fields, err := bd.GetEscalationBead(escalationID)
	if err != nil {
		return fmt.Errorf("getting escalation: %w", err)
	}
	if issue == nil {
		return fmt.Errorf("escalation not found: %s", escalationID)
	}

	if escalateJSON {
		data := map[string]interface{}{
			"id":           issue.ID,
			"title":        issue.Title,
			"status":       issue.Status,
			"created_at":   issue.CreatedAt,
			"severity":     fields.Severity,
			"reason":       fields.Reason,
			"escalatedBy":  fields.EscalatedBy,
			"escalatedAt":  fields.EscalatedAt,
			"ackedBy":      fields.AckedBy,
			"ackedAt":      fields.AckedAt,
			"closedBy":     fields.ClosedBy,
			"closedReason": fields.ClosedReason,
			"relatedBead":  fields.RelatedBead,
		}
		out, _ := json.MarshalIndent(data, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	emoji := severityEmoji(fields.Severity)
	fmt.Printf("%s Escalation: %s\n", emoji, issue.ID)
	fmt.Printf("  Title: %s\n", issue.Title)
	fmt.Printf("  Status: %s\n", issue.Status)
	fmt.Printf("  Severity: %s\n", fields.Severity)
	fmt.Printf("  Created: %s\n", formatRelativeTime(issue.CreatedAt))
	fmt.Printf("  Escalated by: %s\n", fields.EscalatedBy)
	fmt.Printf("  Reason: %s\n", reasonDisplay(fields.Reason))
	if fields.AckedBy != "" {
		fmt.Printf("  Acknowledged by: %s at %s\n", fields.AckedBy, fields.AckedAt)
	}
	if fields.ClosedBy != "" {
		fmt.Printf("  Closed by: %s\n", fields.ClosedBy)
		fmt.Printf("  Resolution: %s\n", fields.ClosedReason)
	}
	if fields.RelatedBead != "" {
		fmt.Printf("  Related: %s\n", fields.RelatedBead)
	}

	return nil
}

// Helper functions

// extractMailTargetsFromActions extracts mail targets from action strings.
// Action format: "mail:target" returns "target"
// E.g., ["bead", "mail:mayor", "email:human"] returns ["mayor"]
func extractMailTargetsFromActions(actions []string) []string {
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

// executeExternalActions processes external notification actions (email:, sms:, slack, log).
func executeExternalActions(actions []string, cfg *config.EscalationConfig, beadID, severity, description, townRoot string) []deliveryStatus {
	statuses := []deliveryStatus{}
	for _, action := range actions {
		switch {
		case strings.HasPrefix(action, "email:"):
			status := deliveryStatus{Channel: "email", Target: strings.TrimPrefix(action, "email:"), Severity: severity}
			if cfg.Contacts.HumanEmail == "" {
				status.Warning = "contacts.human_email not configured"
				style.PrintWarning("email action '%s' skipped: contacts.human_email not configured in settings/escalation.json", action)
			} else if cfg.Contacts.SMTPHost == "" {
				status.Warning = "contacts.smtp_host not configured"
				style.PrintWarning("email action '%s' skipped: contacts.smtp_host not configured in settings/escalation.json", action)
			} else {
				if err := sendEscalationEmail(cfg, beadID, severity, description); err != nil {
					status.Error = err.Error()
					style.PrintWarning("email send failed: %v", err)
				} else {
					status.RuntimeNotified = true
					fmt.Printf("  📧 Email sent to %s\n", cfg.Contacts.HumanEmail)
				}
			}
			statuses = append(statuses, status)

		case strings.HasPrefix(action, "sms:"):
			status := deliveryStatus{Channel: "sms", Target: strings.TrimPrefix(action, "sms:"), Severity: severity}
			if cfg.Contacts.HumanSMS == "" {
				status.Warning = "contacts.human_sms not configured"
				style.PrintWarning("sms action '%s' skipped: contacts.human_sms not configured in settings/escalation.json", action)
			} else if cfg.Contacts.SMSWebhook == "" {
				status.Warning = "contacts.sms_webhook not configured"
				style.PrintWarning("sms action '%s' skipped: contacts.sms_webhook not configured in settings/escalation.json", action)
			} else {
				if err := sendEscalationSMS(cfg, beadID, severity, description); err != nil {
					status.Error = err.Error()
					style.PrintWarning("sms send failed: %v", err)
				} else {
					status.RuntimeNotified = true
					fmt.Printf("  📱 SMS sent to %s\n", cfg.Contacts.HumanSMS)
				}
			}
			statuses = append(statuses, status)

		case action == "slack":
			status := deliveryStatus{Channel: "slack", Target: "slack", Severity: severity}
			if cfg.Contacts.SlackWebhook == "" {
				status.Warning = "contacts.slack_webhook not configured"
				style.PrintWarning("slack action skipped: contacts.slack_webhook not configured in settings/escalation.json")
			} else {
				if err := sendEscalationSlack(cfg, beadID, severity, description); err != nil {
					status.Error = err.Error()
					style.PrintWarning("slack post failed: %v", err)
				} else {
					status.RuntimeNotified = true
					fmt.Printf("  💬 Posted to Slack\n")
				}
			}
			statuses = append(statuses, status)

		case action == "log":
			status := deliveryStatus{Channel: "log", Target: "log", Severity: severity}
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

func formatEscalationMailBody(beadID, severity, reason, from, related string) string {
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

func severityEmoji(severity string) string {
	switch severity {
	case config.SeverityCritical:
		return "🚨"
	case config.SeverityHigh:
		return "⚠️"
	case config.SeverityMedium:
		return "📢"
	case config.SeverityLow:
		return "ℹ️"
	default:
		return "📋"
	}
}

func formatRelativeTime(timestamp string) string {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return timestamp
	}

	duration := time.Since(t)
	if duration < time.Minute {
		return "just now"
	}
	if duration < time.Hour {
		mins := int(duration.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	}
	if duration < 24*time.Hour {
		hours := int(duration.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	}
	days := int(duration.Hours() / 24)
	if days == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", days)
}

// detectSender is defined in mail_send.go - we reuse it here
// If it's not accessible, we fall back to environment variables
func detectSenderFallback() string {
	// Try BD_ACTOR first (most common in agent context)
	if actor := os.Getenv("BD_ACTOR"); actor != "" {
		return actor
	}
	// Try GT_ROLE
	if role := os.Getenv("GT_ROLE"); role != "" {
		return role
	}
	return ""
}
