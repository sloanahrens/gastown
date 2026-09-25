package cmd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/reaper"
	"github.com/steveyegge/gastown/internal/style"
)

var (
	reaperDB        string
	reaperHost      string
	reaperPort      int
	reaperMaxAge    string
	reaperPurgeAge  string
	reaperMailAge   string
	reaperStaleAge  string
	reaperDBDelay   string
	reaperDryRun    bool
	reaperJSON      bool
	reaperMaxCloses int
	reaperForce     bool
	reaperPreview   string
)

// parseReaperAge parses an age flag, accepting the day suffix the mol-dog-reaper
// formula emits for its 30d default. time.ParseDuration alone rejects "30d" as
// "unknown unit d" (gt-2qzr).
func parseReaperAge(flag, value string) (time.Duration, error) {
	d, err := daemon.ParseAgeDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", flag, err)
	}
	return d, nil
}

// reaperAutoCloseDisarmed reports whether daemon.json disarms the wisp_reaper
// auto-close step, and where that config lives for the message. Checked here as
// well as in the daemon so a Dog that ignores the formula instruction still
// cannot sweep: the disarm has to hold on every path that reaches the write.
func reaperAutoCloseDisarmed() (bool, string) {
	townRoot, err := findTownRoot()
	if err != nil {
		return false, ""
	}
	return daemon.WispReaperAutoCloseDisarmed(daemon.LoadPatrolConfig(townRoot)),
		daemon.PatrolConfigFile(townRoot)
}

// printAutoCloseCandidates lists the issues a sweep selected. Printed on a
// refusal as well as a dry run: the refusal is the designed stop, and it is
// only actionable if it shows what tripped it. toStderr keeps --json output
// parseable.
func printAutoCloseCandidates(result *reaper.AutoCloseResult, toStderr bool) {
	out := os.Stdout
	if toStderr {
		out = os.Stderr
	}
	for _, entry := range result.ClosedEntries {
		fmt.Fprintf(out, "  %s %s (%dd stale, db:%s)\n",
			entry.ID, entry.Title, entry.AgeDays, entry.Database)
	}
}

func reaperDatabaseNames() []string {
	if reaperDB == "" {
		return reaper.DiscoverDatabases(reaperHost, reaperPort)
	}
	parts := strings.Split(reaperDB, ",")
	databases := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name != "" {
			databases = append(databases, name)
		}
	}
	return databases
}

func defaultReaperEndpoint() (string, int) {
	host := agentconfig.ResolveDoltHost("")
	port := 0
	if p := os.Getenv("GT_DOLT_PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			port = v
		}
	}
	if townRoot, err := findTownRoot(); err == nil {
		if host == "" {
			host = agentconfig.ResolveDoltHost(townRoot)
		}
		if port == 0 {
			port = agentconfig.ResolveDoltPort(townRoot)
		}
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 3307
	}
	return host, port
}

func waitBeforeReaperDatabase(index int) error {
	if index == 0 {
		return nil
	}
	delay, err := time.ParseDuration(reaperDBDelay)
	if err != nil {
		return fmt.Errorf("invalid --db-delay: %w", err)
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return nil
}

var reaperCmd = &cobra.Command{
	Use:     "reaper",
	GroupID: GroupServices,
	Short:   "Wisp and issue cleanup operations (Dog-callable helpers)",
	Long: `Execute wisp reaper operations against Dolt databases.

These subcommands are the callable helper functions for the mol-dog-reaper
formula. They execute SQL operations but leave eligibility decisions to the
Dog agent or daemon orchestrator.

When run by a Dog:
  gt reaper scan --db=gastown                          # Discover candidates
  gt reaper reap --db=gastown                          # Close stale wisps
  gt reaper purge --db=gastown                         # Delete old closed wisps + mail
  gt reaper auto-close --db=gastown --dry-run          # Preview stale issues (prints a preview hash)
  gt reaper auto-close --db=gastown --preview=<hash>   # Close exactly that previewed set`,
	RunE: requireSubcommand,
}

var reaperDatabasesCmd = &cobra.Command{
	Use:   "databases",
	Short: "List databases available for reaping",
	RunE: func(cmd *cobra.Command, args []string) error {
		dbs := reaper.DiscoverDatabases(reaperHost, reaperPort)
		if reaperJSON {
			fmt.Println(reaper.FormatJSON(dbs))
		} else {
			for _, db := range dbs {
				fmt.Println(db)
			}
		}
		return nil
	},
}

var reaperScanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Scan databases for reaper candidates",
	Long: `Count reap, purge, auto-close, and mail candidates in databases.

When --db is provided, scans a single database. When omitted, auto-discovers
all databases on the Dolt server and scans each one, printing a summary.

Returns counts and anomaly detection results without modifying any data.
The Dog uses this to understand the state before deciding what to reap.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		maxAge, err := parseReaperAge("--max-age", reaperMaxAge)
		if err != nil {
			return err
		}
		purgeAge, err := parseReaperAge("--purge-age", reaperPurgeAge)
		if err != nil {
			return err
		}
		mailAge, err := parseReaperAge("--mail-age", reaperMailAge)
		if err != nil {
			return err
		}
		staleAge, err := parseReaperAge("--stale-age", reaperStaleAge)
		if err != nil {
			return err
		}

		databases := reaperDatabaseNames()

		var results []*reaper.ScanResult
		for i, dbName := range databases {
			if err := waitBeforeReaperDatabase(i); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Fprintf(os.Stderr, "skip invalid db: %s\n", dbName)
				continue
			}

			db, err := reaper.OpenDB(reaperHost, reaperPort, dbName, 10*time.Second, 10*time.Second)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: connect error: %v\n", dbName, err)
				continue
			}

			if ok, err := reaper.HasReaperSchema(db); err != nil {
				fmt.Fprintf(os.Stderr, "%s: schema check error: %v\n", dbName, err)
				db.Close()
				continue
			} else if !ok {
				db.Close()
				continue
			}

			result, err := reaper.Scan(db, dbName, maxAge, purgeAge, mailAge, staleAge)
			db.Close()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: scan error: %v\n", dbName, err)
				continue
			}
			results = append(results, result)
		}

		if reaperJSON {
			fmt.Println(reaper.FormatJSON(results))
		} else {
			var totalReap, totalMoleculeSteps, totalPurge, totalMail, totalStale, totalOpen int
			for _, r := range results {
				fmt.Printf("Database: %s\n", r.Database)
				fmt.Printf("  Reap candidates:  %d\n", r.ReapCandidates)
				if r.MoleculeStepCandidates > 0 {
					fmt.Printf("  Molecule steps:   %d\n", r.MoleculeStepCandidates)
				}
				fmt.Printf("  Purge candidates: %d\n", r.PurgeCandidates)
				fmt.Printf("  Mail candidates:  %d\n", r.MailCandidates)
				fmt.Printf("  Stale candidates: %d\n", r.StaleCandidates)
				fmt.Printf("  Open wisps:       %d\n", r.OpenWisps)
				for _, a := range r.Anomalies {
					fmt.Printf("  %s %s\n", style.Warning.Render("ANOMALY:"), a.Message)
				}
				totalReap += r.ReapCandidates
				totalMoleculeSteps += r.MoleculeStepCandidates
				totalPurge += r.PurgeCandidates
				totalMail += r.MailCandidates
				totalStale += r.StaleCandidates
				totalOpen += r.OpenWisps
			}
			if len(results) > 1 {
				fmt.Printf("\nScan summary (%d databases):\n", len(results))
				fmt.Printf("  Reap candidates:  %d\n", totalReap)
				if totalMoleculeSteps > 0 {
					fmt.Printf("  Molecule steps:   %d\n", totalMoleculeSteps)
				}
				fmt.Printf("  Purge candidates: %d\n", totalPurge)
				fmt.Printf("  Mail candidates:  %d\n", totalMail)
				fmt.Printf("  Stale candidates: %d\n", totalStale)
				fmt.Printf("  Open wisps:       %d\n", totalOpen)
			}
		}
		return nil
	},
}

var reaperReapCmd = &cobra.Command{
	Use:   "reap",
	Short: "Close stale wisps past max-age",
	Long: `Close wisps that are past the max-age threshold and whose parent
molecule is already closed (or missing/orphaned).

When --db is provided, reaps a single database. When omitted, auto-discovers
all databases on the Dolt server and reaps each one.

Returns the count of reaped wisps. Use --dry-run to preview.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		maxAge, err := parseReaperAge("--max-age", reaperMaxAge)
		if err != nil {
			return err
		}

		databases := reaperDatabaseNames()

		var results []*reaper.ReapResult
		for i, dbName := range databases {
			if err := waitBeforeReaperDatabase(i); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Fprintf(os.Stderr, "skip invalid db: %s\n", dbName)
				continue
			}

			db, err := reaper.OpenDB(reaperHost, reaperPort, dbName, 10*time.Second, 10*time.Second)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: connect error: %v\n", dbName, err)
				continue
			}

			if ok, err := reaper.HasReaperSchema(db); err != nil {
				fmt.Fprintf(os.Stderr, "%s: schema check error: %v\n", dbName, err)
				db.Close()
				continue
			} else if !ok {
				db.Close()
				continue
			}

			result, err := reaper.Reap(db, dbName, maxAge, reaperDryRun)
			db.Close()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: reap error: %v\n", dbName, err)
				continue
			}
			results = append(results, result)
		}

		if reaperJSON {
			fmt.Println(reaper.FormatJSON(results))
		} else {
			var totalReaped, totalMoleculeSteps, totalOpen int
			for _, r := range results {
				prefix := ""
				if r.DryRun {
					prefix = "[DRY RUN] would "
				}
				extra := ""
				if r.MoleculeStepsClosed > 0 {
					extra = fmt.Sprintf(" (+%d closed-molecule steps)", r.MoleculeStepsClosed)
				}
				fmt.Printf("%s: %sreaped %d wisps%s, %d open remain\n",
					r.Database, prefix, r.Reaped, extra, r.OpenRemain)
				totalReaped += r.Reaped
				totalMoleculeSteps += r.MoleculeStepsClosed
				totalOpen += r.OpenRemain
			}
			if len(results) > 1 {
				prefix := ""
				if reaperDryRun {
					prefix = "[DRY RUN] "
				}
				extra := ""
				if totalMoleculeSteps > 0 {
					extra = fmt.Sprintf(" (+%d closed-molecule steps)", totalMoleculeSteps)
				}
				fmt.Printf("\n%sReap summary (%d databases): reaped %d wisps%s, %d open remain\n",
					prefix, len(results), totalReaped, extra, totalOpen)
				if totalOpen > reaper.DefaultAlertThreshold {
					fmt.Fprintf(os.Stderr, "WARNING: %d open wisps exceed alert threshold (%d)\n",
						totalOpen, reaper.DefaultAlertThreshold)
				}
			}
		}
		return nil
	},
}

var reaperPurgeCmd = &cobra.Command{
	Use:   "purge",
	Short: "Delete old closed wisps and mail",
	Long: `Delete closed wisps past the purge-age threshold and closed mail
past the mail-age threshold. Irreversible operation.

Wisps a live agent bead still names as active_mr or hook_bead are kept, so the
reference keeps resolving (gt-gyb6). They are purge candidates again once the
pointer clears.

When --db is provided, purges a single database. When omitted, auto-discovers
all databases on the Dolt server and purges each one.

Returns counts of purged rows. Use --dry-run to preview.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		purgeAge, err := parseReaperAge("--purge-age", reaperPurgeAge)
		if err != nil {
			return err
		}
		mailAge, err := parseReaperAge("--mail-age", reaperMailAge)
		if err != nil {
			return err
		}

		databases := reaperDatabaseNames()

		var results []*reaper.PurgeResult
		for i, dbName := range databases {
			if err := waitBeforeReaperDatabase(i); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Fprintf(os.Stderr, "skip invalid db: %s\n", dbName)
				continue
			}

			db, err := reaper.OpenDB(reaperHost, reaperPort, dbName, 30*time.Second, 30*time.Second)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: connect error: %v\n", dbName, err)
				continue
			}

			if ok, err := reaper.HasReaperSchema(db); err != nil {
				fmt.Fprintf(os.Stderr, "%s: schema check error: %v\n", dbName, err)
				db.Close()
				continue
			} else if !ok {
				db.Close()
				continue
			}

			result, err := reaper.Purge(db, dbName, purgeAge, mailAge, reaperDryRun)
			db.Close()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: purge error: %v\n", dbName, err)
				continue
			}
			results = append(results, result)
		}

		if reaperJSON {
			fmt.Println(reaper.FormatJSON(results))
		} else {
			var totalWisps, totalMail int
			for _, r := range results {
				prefix := ""
				if r.DryRun {
					prefix = "[DRY RUN] would "
				}
				fmt.Printf("%s: %spurged %d wisps, %d mail\n",
					r.Database, prefix, r.WispsPurged, r.MailPurged)
				for _, a := range r.Anomalies {
					fmt.Printf("  %s %s\n", style.Warning.Render("ANOMALY:"), a.Message)
				}
				totalWisps += r.WispsPurged
				totalMail += r.MailPurged
			}
			if len(results) > 1 {
				prefix := ""
				if reaperDryRun {
					prefix = "[DRY RUN] "
				}
				fmt.Printf("\n%sPurge summary (%d databases): purged %d wisps, %d mail\n",
					prefix, len(results), totalWisps, totalMail)
			}
		}
		return nil
	},
}

var reaperAutoCloseCmd = &cobra.Command{
	Use:   "auto-close",
	Short: "Close stale issues past stale-age",
	Long: `Close issues open with no updates past the stale-age threshold.

Eligibility excludes P0/P1, epics, convoys, molecules and other infrastructure
issue types, agent beads (every mayor, deacon, dog, witness, refinery, crew and
polecat bead carries gt:agent), plugin receipts, and issues with active
dependencies. Scan and auto-close share one eligibility clause, so 'gt reaper
scan' is a faithful preview of this command.

Four guards apply, the first three answering the 2026-09-16 mis-close that swept
102 durable beads (gt-2qzr):
  - --stale-age below 7d is refused unless --force.
  - more than --max-closes candidates in one database refuses the whole run
    (nothing is closed) unless --force.
  - auto-close disarmed in daemon.json (patrols.wisp_reaper.auto_close=false)
    is refused unless --force.
  - a live run with no --preview hash is refused (nothing is closed) unless
    --force: the sweep only closes a set a dry run already showed. The dry run
    prints the hash; hand it back verbatim. This is what keeps the counting
    ahead of the writing regardless of how the two commands were ordered
    (gt-39bu — the 2026-09-20 reaper ran live before its dry run).

When --db is provided, auto-closes in a single database. When omitted,
auto-discovers all databases on the Dolt server and auto-closes in each one.

Returns the count of closed issues. Use --dry-run to preview.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		staleAge, err := parseReaperAge("--stale-age", reaperStaleAge)
		if err != nil {
			return err
		}

		if disarmed, configPath := reaperAutoCloseDisarmed(); disarmed && !reaperForce {
			return fmt.Errorf("auto-close is disarmed by %s (patrols.wisp_reaper.auto_close=false); pass --force to override", configPath)
		}

		databases := reaperDatabaseNames()

		var results []*reaper.AutoCloseResult
		var refusals []string
		for i, dbName := range databases {
			if err := waitBeforeReaperDatabase(i); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Fprintf(os.Stderr, "skip invalid db: %s\n", dbName)
				continue
			}

			db, err := reaper.OpenDB(reaperHost, reaperPort, dbName, 10*time.Second, 10*time.Second)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: connect error: %v\n", dbName, err)
				continue
			}

			if ok, err := reaper.HasReaperSchema(db); err != nil {
				fmt.Fprintf(os.Stderr, "%s: schema check error: %v\n", dbName, err)
				db.Close()
				continue
			} else if !ok {
				db.Close()
				continue
			}

			result, err := reaper.AutoClose(db, dbName, reaper.AutoCloseOptions{
				StaleAge:    staleAge,
				DryRun:      reaperDryRun,
				MaxCloses:   reaperMaxCloses,
				PreviewHash: reaperPreview,
				Force:       reaperForce,
			})
			db.Close()
			if err != nil {
				// Every refusal leaves the database untouched, so it is a stop
				// to read rather than a partial sweep to retry. Keep the detail
				// so the operator sees what the sweep wanted to take and what it
				// was missing. Candidates go to stderr under --json so the
				// stream stays parseable.
				if errors.Is(err, reaper.ErrTooManyCloses) ||
					errors.Is(err, reaper.ErrPreviewRequired) ||
					errors.Is(err, reaper.ErrPreviewMismatch) {
					fmt.Fprintf(os.Stderr, "%s: %v\n", dbName, err)
					if result != nil {
						printAutoCloseCandidates(result, reaperJSON)
					}
					refusals = append(refusals, fmt.Sprintf("%s: %v", dbName, err))
					continue
				}
				fmt.Fprintf(os.Stderr, "%s: auto-close error: %v\n", dbName, err)
				continue
			}
			results = append(results, result)
		}

		if reaperJSON {
			fmt.Println(reaper.FormatJSON(results))
		} else {
			var totalClosed int
			for _, r := range results {
				prefix := ""
				if r.DryRun {
					prefix = "[DRY RUN] would "
				}
				printAutoCloseCandidates(r, false)
				line := fmt.Sprintf("%s: %sauto-closed %d stale issues",
					r.Database, prefix, r.Closed)
				if r.DryRun {
					// A dry run's job is to hand the live run its authorization,
					// so print the flag to paste verbatim — the pair reads as one
					// instruction instead of two commands a caller might reorder.
					line += fmt.Sprintf(" — live run: --preview=%s", r.PreviewHash)
				}
				fmt.Println(line)
				totalClosed += r.Closed
			}
			if len(results) > 1 {
				prefix := ""
				if reaperDryRun {
					prefix = "[DRY RUN] "
				}
				fmt.Printf("\n%sAuto-close summary (%d databases): auto-closed %d stale issues\n",
					prefix, len(results), totalClosed)
			}
		}

		if len(refusals) > 0 {
			return fmt.Errorf("auto-close refused in %d database(s): %s",
				len(refusals), strings.Join(refusals, "; "))
		}
		return nil
	},
}

var reaperRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run full reaper cycle across all databases",
	Long: `Execute a full reaper cycle: scan → reap → purge → auto-close → report.

This is the inline fallback for when Dog dispatch is unavailable.
Normally the daemon dispatches a Dog to execute the mol-dog-reaper formula.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		databases := reaperDatabaseNames()

		maxAge, err := parseReaperAge("--max-age", reaperMaxAge)
		if err != nil {
			return err
		}
		purgeAge, err := parseReaperAge("--purge-age", reaperPurgeAge)
		if err != nil {
			return err
		}
		mailAge, err := parseReaperAge("--mail-age", reaperMailAge)
		if err != nil {
			return err
		}
		staleAge, err := parseReaperAge("--stale-age", reaperStaleAge)
		if err != nil {
			return err
		}

		autoCloseDisarmed, autoCloseConfigPath := reaperAutoCloseDisarmed()

		var totalReaped, totalMoleculeSteps, totalPurged, totalMailPurged, totalClosed, totalOpen int

		for i, dbName := range databases {
			if err := waitBeforeReaperDatabase(i); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Printf("skip invalid db: %s\n", dbName)
				continue
			}

			db, err := reaper.OpenDB(reaperHost, reaperPort, dbName, 30*time.Second, 30*time.Second)
			if err != nil {
				fmt.Printf("%s: connect error: %v\n", dbName, err)
				continue
			}

			if ok, err := reaper.HasReaperSchema(db); err != nil {
				fmt.Printf("%s: schema check error: %v\n", dbName, err)
				db.Close()
				continue
			} else if !ok {
				fmt.Printf("%s: skipped (no reaper schema)\n", dbName)
				db.Close()
				continue
			}

			// Scan
			scanResult, err := reaper.Scan(db, dbName, maxAge, purgeAge, mailAge, staleAge)
			if err != nil {
				fmt.Printf("%s: scan error: %v\n", dbName, err)
				db.Close()
				continue
			}
			for _, a := range scanResult.Anomalies {
				fmt.Printf("%s: %s %s\n", dbName, style.Warning.Render("ANOMALY:"), a.Message)
			}

			// Reap
			reapResult, err := reaper.Reap(db, dbName, maxAge, reaperDryRun)
			if err != nil {
				fmt.Printf("%s: reap error: %v\n", dbName, err)
			} else {
				totalReaped += reapResult.Reaped
				totalMoleculeSteps += reapResult.MoleculeStepsClosed
				totalOpen += reapResult.OpenRemain
			}

			// Purge
			purgeResult, err := reaper.Purge(db, dbName, purgeAge, mailAge, reaperDryRun)
			if err != nil {
				fmt.Printf("%s: purge error: %v\n", dbName, err)
			} else {
				totalPurged += purgeResult.WispsPurged
				totalMailPurged += purgeResult.MailPurged
			}

			// Auto-close composes its own preview: this path has no caller to
			// order the two phases, and a live sweep closes only what a preview
			// showed (gt-39bu).
			if autoCloseDisarmed && !reaperForce {
				fmt.Printf("%s: auto-close skipped (disarmed by %s)\n", dbName, autoCloseConfigPath)
			} else {
				preview, err := reaper.AutoClose(db, dbName, reaper.AutoCloseOptions{
					StaleAge:  staleAge,
					DryRun:    true,
					MaxCloses: reaperMaxCloses,
					Force:     reaperForce,
				})
				if err != nil {
					fmt.Printf("%s: auto-close preview error: %v\n", dbName, err)
					db.Close()
					continue
				}
				fmt.Printf("%s: auto-close preview: %d candidate(s)\n", dbName, preview.Closed)

				closeResult, err := reaper.AutoClose(db, dbName, reaper.AutoCloseOptions{
					StaleAge:    staleAge,
					DryRun:      reaperDryRun,
					MaxCloses:   reaperMaxCloses,
					PreviewHash: preview.PreviewHash,
					Force:       reaperForce,
				})
				if err != nil {
					// A refusal is a stop, not a partial sweep: report it and
					// leave the database untouched.
					fmt.Printf("%s: auto-close refused: %v\n", dbName, err)
					if closeResult != nil {
						printAutoCloseCandidates(closeResult, false)
					}
				} else {
					printAutoCloseCandidates(closeResult, false)
					totalClosed += closeResult.Closed
				}
			}

			db.Close()
		}

		// Report
		prefix := ""
		if reaperDryRun {
			prefix = "[DRY RUN] "
		}
		fmt.Printf("\n%sReaper cycle complete:\n", prefix)
		fmt.Printf("  Databases: %d\n", len(databases))
		fmt.Printf("  Reaped:    %d", totalReaped)
		if totalMoleculeSteps > 0 {
			fmt.Printf(" (+%d closed-molecule steps)", totalMoleculeSteps)
		}
		fmt.Println()
		fmt.Printf("  Purged:    %d wisps, %d mail\n", totalPurged, totalMailPurged)
		fmt.Printf("  Closed:    %d stale issues\n", totalClosed)
		fmt.Printf("  Open:      %d wisps remain\n", totalOpen)

		return nil
	},
}

func init() {
	// Shared flags
	// GH#2601: Default host/port from GT/town config for non-localhost setups.
	// BEADS_DOLT_* aliases are intentionally ignored because they are derived bd
	// client outputs, not endpoint authority.
	defaultHost, defaultPort := defaultReaperEndpoint()

	for _, cmd := range []*cobra.Command{reaperScanCmd, reaperReapCmd, reaperPurgeCmd, reaperAutoCloseCmd, reaperRunCmd, reaperDatabasesCmd} {
		cmd.Flags().StringVar(&reaperDB, "db", "", "Database name (required for single-db commands)")
		cmd.Flags().StringVar(&reaperHost, "host", defaultHost, "Dolt server host (env: GT_DOLT_HOST)")
		cmd.Flags().IntVar(&reaperPort, "port", defaultPort, "Dolt server port (env: GT_DOLT_PORT)")
		cmd.Flags().BoolVar(&reaperDryRun, "dry-run", false, "Report what would happen without acting")
	}
	for _, cmd := range []*cobra.Command{reaperScanCmd, reaperReapCmd, reaperPurgeCmd, reaperAutoCloseCmd, reaperRunCmd} {
		cmd.Flags().StringVar(&reaperDBDelay, "db-delay", "250ms", "Delay between databases to reduce Dolt load")
	}

	// JSON output flag for single-db commands
	for _, cmd := range []*cobra.Command{reaperScanCmd, reaperReapCmd, reaperPurgeCmd, reaperAutoCloseCmd, reaperDatabasesCmd} {
		cmd.Flags().BoolVar(&reaperJSON, "json", false, "Output as JSON")
	}

	// Threshold flags
	for _, cmd := range []*cobra.Command{reaperScanCmd, reaperReapCmd, reaperRunCmd} {
		cmd.Flags().StringVar(&reaperMaxAge, "max-age", "24h", "Max wisp age before reaping")
	}
	for _, cmd := range []*cobra.Command{reaperScanCmd, reaperPurgeCmd, reaperRunCmd} {
		cmd.Flags().StringVar(&reaperPurgeAge, "purge-age", "168h", "Max closed wisp age before purging (7d)")
		cmd.Flags().StringVar(&reaperMailAge, "mail-age", "168h", "Max closed mail age before purging (7d)")
	}
	for _, cmd := range []*cobra.Command{reaperScanCmd, reaperAutoCloseCmd, reaperRunCmd} {
		cmd.Flags().StringVar(&reaperStaleAge, "stale-age", "720h", "Max issue staleness before auto-close (30d; below 7d requires --force)")
	}
	// Guardrails on the auto-close write (gt-2qzr). --force is deliberately
	// absent from scan: a preview should never need overriding.
	for _, cmd := range []*cobra.Command{reaperAutoCloseCmd, reaperRunCmd} {
		cmd.Flags().IntVar(&reaperMaxCloses, "max-closes", reaper.DefaultAutoCloseMaxPerRun,
			"Refuse the run if a database has more than this many auto-close candidates")
		cmd.Flags().BoolVar(&reaperForce, "force", false,
			"Override the stale-age floor, the per-run cap, the preview hash requirement, and the daemon.json auto-close disarm")
	}

	// --preview is auto-close's own: `gt reaper run` previews in-process before
	// it writes, so it has no separate preview to be handed (gt-39bu).
	reaperAutoCloseCmd.Flags().StringVar(&reaperPreview, "preview", "",
		"Preview hash from a preceding --dry-run; a live run without it is refused")

	reaperCmd.AddCommand(reaperDatabasesCmd)
	reaperCmd.AddCommand(reaperScanCmd)
	reaperCmd.AddCommand(reaperReapCmd)
	reaperCmd.AddCommand(reaperPurgeCmd)
	reaperCmd.AddCommand(reaperAutoCloseCmd)
	reaperCmd.AddCommand(reaperRunCmd)

	rootCmd.AddCommand(reaperCmd)
}
