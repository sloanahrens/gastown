package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/notify"
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
		targets := notify.MailTargets(actions)
		fmt.Printf("Would create escalation:\n")
		fmt.Printf("  Severity: %s\n", severity)
		fmt.Printf("  Description: %s\n", description)
		fmt.Printf("  Reason: %s\n", reasonDisplay(escalateReason))
		if escalateSource != "" {
			fmt.Printf("  Source: %s\n", escalateSource)
		}
		if escalateFingerprint != "" {
			fmt.Printf("  Fingerprint: %s\n", notify.FingerprintLabel(escalateFingerprint))
		}
		fmt.Printf("  Actions: %s\n", strings.Join(actions, ", "))
		fmt.Printf("  Mail targets: %s\n", strings.Join(targets, ", "))
		return nil
	}

	res, err := notify.Raise(notify.EscalationRequest{
		TownRoot:    townRoot,
		Severity:    severity,
		Description: description,
		Reason:      escalateReason,
		Source:      escalateSource,
		Fingerprint: escalateFingerprint,
		RelatedBead: escalateRelatedBead,
		EscalatedBy: agentID,
	}, escalationConfig)
	if err != nil {
		return err
	}

	if res.Duplicate {
		printRepeatEscalation(res)
		return nil
	}
	printCreatedEscalation(res, severity)
	return nil
}

// printRepeatEscalation reports a firing that was recorded on the open
// escalation with the same alert key.
func printRepeatEscalation(res *notify.RaiseResult) {
	if escalateJSON {
		result := map[string]interface{}{
			"id":          res.ID,
			"status":      "duplicate_recorded",
			"fingerprint": res.FingerprintLabel,
			"occurrences": res.Occurrences,
			"renotified":  res.Renotified,
		}
		if res.Renotified {
			result["delivery"] = res.Delivery
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Printf("%s Repeat escalation recorded on %s (occurrence %d)\n", style.Bold.Render("✓"), res.ID, res.Occurrences)
	fmt.Printf("  Fingerprint: %s\n", res.FingerprintLabel)
	if res.Renotified {
		fmt.Printf("  Re-notified (renotify window elapsed)\n")
		for _, status := range res.Delivery {
			if status.Error != "" {
				fmt.Printf("  Delivery issue [%s:%s]: %s\n", status.Channel, status.Target, status.Error)
			}
		}
	} else {
		fmt.Printf("  Not re-notified (within renotify window)\n")
	}
	fmt.Printf("  Clear when resolved with: gt escalate clear --fingerprint %q\n", res.AlertKey)
}

// printCreatedEscalation reports a newly created escalation.
func printCreatedEscalation(res *notify.RaiseResult, severity string) {
	if escalateJSON {
		hasFailure := false
		for _, status := range res.Delivery {
			if status.Error != "" {
				hasFailure = true
				break
			}
		}
		result := map[string]interface{}{
			"id":       res.ID,
			"severity": severity,
			"reason":   escalateReason,
			"actions":  res.Actions,
			"targets":  res.Targets,
			"delivery": res.Delivery,
			"status":   map[bool]string{true: "partial_failure", false: "ok"}[hasFailure],
		}
		if escalateSource != "" {
			result["source"] = escalateSource
		}
		if res.FingerprintLabel != "" {
			result["fingerprint"] = res.FingerprintLabel
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
		return
	}
	emoji := severityEmoji(severity)
	fmt.Printf("%s Escalation created: %s\n", emoji, res.ID)
	fmt.Printf("  Severity: %s\n", severity)
	fmt.Printf("  Reason: %s\n", reasonDisplay(escalateReason))
	if escalateSource != "" {
		fmt.Printf("  Source: %s\n", escalateSource)
	}
	if res.FingerprintLabel != "" {
		fmt.Printf("  Fingerprint: %s\n", res.FingerprintLabel)
	}
	fmt.Printf("  Routed to: %s\n", strings.Join(res.Targets, ", "))
	for _, status := range res.Delivery {
		if status.Error != "" {
			fmt.Printf("  Delivery issue [%s:%s]: %s\n", status.Channel, status.Target, status.Error)
		}
	}
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
	if derived := notify.AlertKey("", escalateSource, strings.Join(args, " ")); strings.TrimSpace(derived) != "" {
		keys = append(keys, derived)
	}
	hasLabel := false
	for _, key := range keys {
		if notify.FingerprintLabel(key) != "" {
			hasLabel = true
			break
		}
	}
	if !hasLabel {
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

	closed, err := notify.Clear(townRoot, keys, closedBy, escalateClearReason)
	if err != nil {
		return err
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
	return closeDeliveryBeads(bd, ids, escalationID, closedBy)
}

// closeDeliveryBeads closes an escalation's delivery beads ids and returns
// how many closed, forcing past the recipients' assignee fence. The count is
// only the beads that closed.
func closeDeliveryBeads(bd beads.Client, ids []string, escalationID, closedBy string) (int, error) {
	reason := fmt.Sprintf("escalation %s closed by %s", escalationID, closedBy)
	// Forced: each delivery bead is assigned to its recipient, and bd
	// refuses an unforced close of another actor's issue. These beads are
	// gastown's delivery bookkeeping, not work.
	err := bd.ForceCloseWithReason(reason, ids...)
	closed := len(beads.ClosedIDs(ids, err))
	if err != nil {
		return closed, fmt.Errorf("closing %d delivery bead(s): %w", len(ids), err)
	}
	return closed, nil
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
			targets := notify.MailTargets(actions)

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
