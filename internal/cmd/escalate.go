package cmd

import (
	"github.com/spf13/cobra"
)

// Escalate command flags
var (
	escalateSeverity    string
	escalateReason      string
	escalateSource      string
	escalateRelatedBead string
	escalateFingerprint string
	escalateJSON        bool
	escalateListJSON    bool
	escalateListAll     bool
	escalateStaleJSON   bool
	escalateDryRun      bool
	escalateCloseReason string
	escalateClearReason string
	escalateClearKeys   []string
	escalateStdin       bool // Read reason from stdin
)

var escalateCmd = &cobra.Command{
	Use:     "escalate [description]",
	GroupID: GroupComm,
	Short:   "Escalation system for critical issues",
	RunE:    runEscalate,
	Long: `Create and manage escalations for critical issues.

The escalation system provides severity-based routing for issues that need
human or mayor attention. Escalations are tracked as beads with gt:escalation label.

SEVERITY LEVELS:
  critical  (P0) Immediate attention required
  high      (P1) Urgent, needs attention soon
  medium    (P2) Standard escalation (default)
  low       (P3) Informational, can wait

WORKFLOW:
  1. Agent encounters blocking issue
  2. Runs: gt escalate "Description" --severity high --reason "details"
  3. Escalation is routed based on settings/escalation.json
  4. Recipient acknowledges with: gt escalate ack <id>
  5. After resolution: gt escalate close <id> --reason "fixed"

RECURRING ALERTS:
  Every escalation carries an alert key — an explicit --fingerprint, or one
  derived from --source and the description. Firing the same key again does
  not mint a second bead: the existing open escalation records the repeat and
  bumps its occurrence count. This is what keeps a condition that persists for
  a night from leaving a dozen identical P0/P1 records behind, each topping
  bd ready with no owner (gt-vwry).

  A producer whose condition clears should close the key rather than leave it
  for a human: gt escalate clear --fingerprint <key>. Producers that raise
  alerts on a patrol cadence are expected to do this on the first healthy
  cycle after the condition goes away.

CONFIGURATION:
  Routing is configured in ~/gt/settings/escalation.json:
  - routes: Map severity to action lists (bead, mail:mayor, email:human, sms:human)
  - contacts: Human email/SMS for external notifications
  - stale_threshold: When unacked escalations are re-escalated (default: 4h)
  - max_reescalations: How many times to bump severity (default: 2)

Examples:
  gt escalate "Build failing" --severity critical --reason "CI blocked"
  gt escalate "Need API credentials" --severity high --source "plugin:rebuild-gt"
  gt escalate "Deacon await-signal timeout" --severity medium --source deacon --fingerprint deacon:await-signal:hq-deacon
  gt escalate "Code review requested" --reason "PR #123 ready"
  gt escalate list                          # Show open escalations
  gt escalate ack hq-abc123                 # Acknowledge
  gt escalate close hq-abc123 --reason "Fixed in commit abc"
  gt escalate stale                         # Re-escalate stale escalations`,
}

var escalateListCmd = &cobra.Command{
	Use:   "list",
	Short: "List open escalations",
	Long: `List all open escalations.

Shows escalations that haven't been closed yet. Use --all to include
closed escalations.

Examples:
  gt escalate list              # Open escalations only
  gt escalate list --all        # Include closed
  gt escalate list --json       # JSON output`,
	RunE: runEscalateList,
}

var escalateAckCmd = &cobra.Command{
	Use:   "ack <escalation-id>",
	Short: "Acknowledge an escalation",
	Long: `Acknowledge an escalation to indicate you're working on it.

Adds an "acked" label and records who acknowledged and when.
This stops the stale escalation warnings.

Examples:
  gt escalate ack hq-abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runEscalateAck,
}

var escalateCloseCmd = &cobra.Command{
	Use:   "close <escalation-id>",
	Short: "Close a resolved escalation",
	Long: `Close an escalation after the issue is resolved.

Records who closed it and the resolution reason.

Examples:
  gt escalate close hq-abc123 --reason "Fixed in commit abc"
  gt escalate close hq-abc123 --reason "Not reproducible"`,
	Args: cobra.ExactArgs(1),
	RunE: runEscalateClose,
}

var escalateClearCmd = &cobra.Command{
	Use:   "clear [description]",
	Short: "Auto-close a keyed escalation whose condition has cleared",
	Long: `Close open escalations that share an alert key, because the condition
that raised them no longer holds.

This is the counterpart to the recurrence handling on the main command: a
producer that raises a keyed alert (a jsonl export spike, a main-branch test
failure, a state-collapse condition) is expected to re-check its condition on
the next cycle and clear the same key when the condition is gone. Without it,
an alert that fired once stays open forever and keeps topping bd ready (gt-vwry).

A key that matches no open escalation is not an error — that is the ordinary
case on a healthy cycle, so the command exits 0 and reports "nothing to clear".

Examples:
  gt escalate clear --fingerprint jsonl_git_backup:spike:gastown
  gt escalate clear --fingerprint main_branch_test:gastown --reason "main is green"
  gt escalate clear --source main_branch_test "main branch test failures:"`,
	RunE: runEscalateClear,
}

var escalateStaleCmd = &cobra.Command{
	Use:   "stale",
	Short: "Re-escalate stale unacknowledged escalations",
	Long: `Find and re-escalate escalations that haven't been acknowledged within the threshold.

When run without --dry-run, this command:
1. Finds escalations older than the stale threshold (default: 4h)
2. Bumps their severity: low→medium→high→critical
3. Re-routes them according to the new severity level
4. Sends mail to the new routing targets

Respects max_reescalations from config (default: 2) to prevent infinite escalation.

The threshold is configured in settings/escalation.json.

Examples:
  gt escalate stale              # Re-escalate stale escalations
  gt escalate stale --dry-run    # Show what would be done
  gt escalate stale --json       # JSON output of results`,
	RunE: runEscalateStale,
}

var escalateShowCmd = &cobra.Command{
	Use:   "show <escalation-id>",
	Short: "Show details of an escalation",
	Long: `Display detailed information about an escalation.

Examples:
  gt escalate show hq-abc123
  gt escalate show hq-abc123 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runEscalateShow,
}

func init() {
	// Main escalate command flags
	escalateCmd.Flags().StringVarP(&escalateSeverity, "severity", "s", "medium", "Severity level: critical, high, medium, low")
	escalateCmd.Flags().StringVarP(&escalateReason, "reason", "r", "", "Detailed reason for escalation")
	escalateCmd.Flags().StringVar(&escalateSource, "source", "", "Source identifier (e.g., plugin:rebuild-gt, patrol:deacon)")
	escalateCmd.Flags().StringVar(&escalateRelatedBead, "related", "", "Related bead ID (task, bug, etc.)")
	escalateCmd.Flags().StringVar(&escalateFingerprint, "fingerprint", "", "Stable alert key: a repeat firing records onto the existing open escalation instead of creating another (default: derived from --source and the description)")
	escalateCmd.Flags().BoolVar(&escalateJSON, "json", false, "Output as JSON")
	escalateCmd.Flags().BoolVarP(&escalateDryRun, "dry-run", "n", false, "Show what would be done without executing")
	escalateCmd.Flags().BoolVar(&escalateStdin, "stdin", false, "Read reason from stdin (avoids shell quoting issues)")

	// List subcommand flags
	escalateListCmd.Flags().BoolVar(&escalateListJSON, "json", false, "Output as JSON")
	escalateListCmd.Flags().BoolVar(&escalateListAll, "all", false, "Include closed escalations")

	// Close subcommand flags
	escalateCloseCmd.Flags().StringVar(&escalateCloseReason, "reason", "", "Resolution reason")
	_ = escalateCloseCmd.MarkFlagRequired("reason")

	// Clear subcommand flags
	escalateClearCmd.Flags().StringArrayVar(&escalateClearKeys, "fingerprint", nil, "Alert key to clear (repeatable; as passed when the alert was raised)")
	escalateClearCmd.Flags().StringVar(&escalateSource, "source", "", "Source identifier used to derive the alert key from the description")
	escalateClearCmd.Flags().StringVar(&escalateClearReason, "reason", "", "Why the condition cleared (default: recorded per-key)")

	// Stale subcommand flags
	escalateStaleCmd.Flags().BoolVar(&escalateStaleJSON, "json", false, "Output as JSON")
	escalateStaleCmd.Flags().BoolVarP(&escalateDryRun, "dry-run", "n", false, "Show what would be re-escalated without acting")

	// Show subcommand flags
	escalateShowCmd.Flags().BoolVar(&escalateJSON, "json", false, "Output as JSON")

	// Add subcommands
	escalateCmd.AddCommand(escalateListCmd)
	escalateCmd.AddCommand(escalateAckCmd)
	escalateCmd.AddCommand(escalateCloseCmd)
	escalateCmd.AddCommand(escalateClearCmd)
	escalateCmd.AddCommand(escalateStaleCmd)
	escalateCmd.AddCommand(escalateShowCmd)

	rootCmd.AddCommand(escalateCmd)
}
