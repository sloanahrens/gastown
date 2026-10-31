package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beadsql"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/reaper"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
	"github.com/steveyegge/gastown/tracker/client"
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

// reaperAutoCloseDisarmed reports whether the daemon patrol config disarms the
// wisp_reaper auto-close step, and why, for the message. Checked here as well
// as in the daemon so a hand-run sweep cannot auto-close either: the disarm has
// to hold on every path that reaches the write.
func reaperAutoCloseDisarmed() (bool, string) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return false, ""
	}
	return reaperAutoCloseDisarmedAt(townRoot)
}

// reaperAutoCloseDisarmedAt is reaperAutoCloseDisarmed for the town at
// townRoot, which the tests drive directly instead of chdir-ing into a town.
// ReadPatrolConfig is the one reader: it refuses a settings/config.json that
// cannot be read or does not parse, in place of the caller-side check this
// function used to run on the operator's file (gt-dcxbw).
func reaperAutoCloseDisarmedAt(townRoot string) (bool, string) {
	cfg, readErr := daemon.ReadPatrolConfig(townRoot)
	return reaperDisarmFromConfig(cfg, readErr, daemon.PatrolConfigSource(townRoot))
}

// reaperDisarmFromConfig resolves the hand-run reaper's auto-close refusal from
// what daemon.ReadPatrolConfig returned: the config (nil when there is none),
// its error (nil when the file is absent or parsed), and source, the file the
// config was read from, which a refusal names so the operator knows which file
// to fix.
//
// A config that could not be read disarms the sweep (fail closed): the file may
// have said auto_close=false, and running the write that setting exists to stop
// is worse than skipping a sweep. An absent file is not disarmed — there is no
// setting to honor, and --force lifts either refusal.
//
// A *config.ParseError names the file it read, and that path wins over source:
// a broken settings/config.json is the operator's whole file, not the "daemon"
// section the patrol config would live in, so the refusal must not point the
// operator at a section of the file they cannot be told to fix (gt-dcxbw).
func reaperDisarmFromConfig(cfg *daemon.DaemonPatrolConfig, readErr error, source string) (bool, string) {
	if readErr != nil {
		msg := readErr.Error()
		var pe *agentconfig.ParseError
		if errors.As(readErr, &pe) && pe.Path != "" {
			source = pe.Path
		}
		if !strings.Contains(msg, source) {
			msg = fmt.Sprintf("%s: %s", source, msg)
		}
		label := "a patrol config that could not be read"
		if errors.Is(readErr, agentconfig.ErrUnparseable) {
			label = "a patrol config that does not parse"
		}
		return true, fmt.Sprintf("%s: %s", label, msg)
	}
	if daemon.WispReaperAutoCloseDisarmed(cfg) {
		return true, fmt.Sprintf("%s (patrols.wisp_reaper.auto_close=false)", source)
	}
	return false, ""
}

// reaperWriter returns the bd writer a live reaper run against dbName writes
// through, and nil for a dry run. The reaper selects candidates with SQL but
// every write is a bd verb against the beads dir whose metadata names dbName
// (gt-fcxe9.12), so a database no beads dir claims is skipped, not written.
func reaperWriter(dbName string) (reaper.Writer, error) {
	if reaperDryRun {
		return nil, nil
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return nil, fmt.Errorf("live reaper run needs the town root to reach bd: %w", err)
	}
	return reaper.WriterForDatabase(townRoot, dbName)
}

// reaperRunTargets resolves the tracker rigs a scan or reap covers, and the bd
// databases its sweep keeps, from the town find locates. A town-less run ("" with
// no error) has no route table, so it has no tracker rigs. A town lookup that
// fails is an error, not a town-less run: a run that cannot find the town cannot
// tell the tracker rigs apart, so it refuses rather than sweep them.
func reaperRunTargets(find func() (string, error), wholeTown bool, databases []string) ([]reaper.TrackerRig, []string, error) {
	townRoot, err := find()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot find the tracker rigs: %w", err)
	}
	rigs, dbs, err := reaperTrackerTargets(townRoot, wholeTown, databases)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot find the tracker rigs: %w", err)
	}
	return rigs, dbs, nil
}

// reaperTrackerTargets splits a hand run's targets into the tracker rigs it
// covers and the Dolt databases the bd sweep still runs. A whole-town run covers
// every tracker rig; a --db list covers the rigs it names, by rig or by
// database. The bd sweep never gets a database a tracker rig owns.
func reaperTrackerTargets(townRoot string, wholeTown bool, databases []string) ([]reaper.TrackerRig, []string, error) {
	if townRoot == "" {
		return nil, databases, nil
	}
	rigs, err := reaper.TrackerRigs(townRoot)
	if err != nil {
		return nil, nil, err
	}
	covered := rigs
	if !wholeTown {
		named := make(map[string]bool, len(databases))
		for _, db := range databases {
			named[db] = true
		}
		covered = nil
		for _, r := range rigs {
			if named[r.Name] || (r.Database != "" && named[r.Database]) {
				covered = append(covered, r)
			}
		}
	}
	return covered, reaper.WithoutTrackerDatabases(databases, rigs), nil
}

// reaperTrackerWisps reaches the tracker the town's tracker rigs are on, from
// the environment the tracker client reads. The token never comes from a flag.
func reaperTrackerWisps() (reaper.TrackerWisps, error) {
	return reaperTrackerWispsFrom(os.Getenv)
}

// reaperTrackerWispsFrom is reaperTrackerWisps reading its settings from getenv.
func reaperTrackerWispsFrom(getenv func(string) string) (reaper.TrackerWisps, error) {
	cfg := client.FromEnv(getenv)
	if cfg.BaseURL == "" {
		return nil, errors.New("tracker rigs need TRACKER_URL set")
	}
	if cfg.Token == "" && cfg.TokenFile == "" {
		return nil, errors.New("tracker rigs need TRACKER_TOKEN or TRACKER_TOKEN_FILE set")
	}
	tc, err := client.New(cfg)
	if err != nil {
		// client.New never quotes the token; its message names the problem.
		return nil, err
	}
	return tc, nil
}

// scanTrackerRigs scans each tracker rig through the tracker's wisp reads. A rig
// that fails is named on stderr and left out, as a Dolt database that fails is.
func scanTrackerRigs(ctx context.Context, t reaper.TrackerWisps, rigs []reaper.TrackerRig, maxAge, purgeAge time.Duration, now time.Time) []*reaper.ScanResult {
	var results []*reaper.ScanResult
	for _, rig := range rigs {
		result, err := reaper.ScanTracker(ctx, t, rig, maxAge, purgeAge, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: scan error: %v\n", rig.Name, err)
			continue
		}
		results = append(results, result)
	}
	return results
}

// reapTrackerRigs reaps each tracker rig through the tracker's purge. It returns
// the results and how many rigs did not reap cleanly, which the open-wisp alert
// needs to know. A rig whose purge failed part way keeps the count of the wisps
// it did purge: its partial result is returned and the rig counts as failed. A
// purge the tracker kept wisps from is named on stderr.
func reapTrackerRigs(ctx context.Context, t reaper.TrackerWisps, rigs []reaper.TrackerRig, purgeAge time.Duration, dryRun bool, now time.Time) ([]*reaper.ReapResult, int) {
	var results []*reaper.ReapResult
	failed := 0
	for _, rig := range rigs {
		result, err := reaper.ReapTracker(ctx, t, rig, purgeAge, dryRun, now)
		if result != nil {
			for _, a := range result.Anomalies {
				fmt.Fprintf(os.Stderr, "%s: WARNING: %s\n", rig.Name, a.Message)
			}
			results = append(results, result)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: reap error: %v\n", rig.Name, err)
			failed++
		}
	}
	return results, failed
}

// reapTrackerPurgeAge is the purge age a reap applies to its tracker rigs. A reap
// with no tracker rig among its targets never reads --purge-age, so a value that
// does not parse is no error for a run over bd databases alone.
func reapTrackerPurgeAge(rigs []reaper.TrackerRig, value string) (time.Duration, error) {
	if len(rigs) == 0 {
		return 0, nil
	}
	return parseReaperAge("--purge-age", value)
}

// scanTrackerTargets scans the tracker rigs a run covers. It reaches the tracker
// only when a rig is covered, and a tracker it cannot be reached through is named
// on stderr with its rigs left out, as a Dolt database that cannot be reached is.
func scanTrackerTargets(ctx context.Context, rigs []reaper.TrackerRig, newTracker func() (reaper.TrackerWisps, error), maxAge, purgeAge time.Duration, now time.Time) []*reaper.ScanResult {
	if len(rigs) == 0 {
		return nil
	}
	t, err := newTracker()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracker rigs: %v\n", err)
		return nil
	}
	return scanTrackerRigs(ctx, t, rigs, maxAge, purgeAge, now)
}

// reapTrackerTargets reaps the tracker rigs a run covers. A tracker it cannot be
// reached through counts every covered rig as a failure, as a Dolt database that
// cannot be reached is counted, so the open-wisp alert skips the reading.
func reapTrackerTargets(ctx context.Context, rigs []reaper.TrackerRig, newTracker func() (reaper.TrackerWisps, error), purgeAge time.Duration, dryRun bool, now time.Time) ([]*reaper.ReapResult, int) {
	if len(rigs) == 0 {
		return nil, 0
	}
	t, err := newTracker()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracker rigs: %v\n", err)
		return nil, len(rigs)
	}
	return reapTrackerRigs(ctx, t, rigs, purgeAge, dryRun, now)
}

// reapResultLine is the one-line summary a reap prints for a database, whichever
// store reaped it.
func reapResultLine(r *reaper.ReapResult) string {
	prefix := ""
	if r.DryRun {
		prefix = "[DRY RUN] would "
	}
	extra := ""
	if r.MoleculeStepsClosed > 0 {
		extra = fmt.Sprintf(" (+%d closed-molecule steps)", r.MoleculeStepsClosed)
	}
	return fmt.Sprintf("%s: %sreaped %d wisps%s, %d open remain", r.Database, prefix, r.Reaped, extra, r.OpenRemain)
}

// printAutoCloseCandidates lists the issues a sweep selected. Printed on every
// refusal as well as a dry run: a refusal is only actionable if it shows what
// tripped it, and for the below-floor one (gt-ecpj) the set is the whole report
// — nothing was closed, so the count and the list are what the operator has to
// read before deciding whether to pass --force.
func printAutoCloseCandidates(out io.Writer, result *reaper.AutoCloseResult) {
	for _, entry := range result.ClosedEntries {
		fmt.Fprintf(out, "  %s %s (%dd stale, db:%s)\n",
			entry.ID, entry.Title, entry.AgeDays, entry.Database)
	}
}

// writeAutoCloseReport prints one database's auto-close outcome — the floor
// notice when the sweep was refused for a mis-set threshold (gt-ecpj), the
// candidate list, and the summary line — and returns the number closed so the
// caller can total across databases.
//
// It reads the result, never the error: a below-floor refusal arrives as both,
// so a report that keyed off the error would drop the refused set — the only
// part of a refusal there is to act on — and print nothing at all.
func writeAutoCloseReport(stdout, stderr io.Writer, r *reaper.AutoCloseResult) int {
	if r.Floored {
		// The sweep refused, so it reports the set the threshold would take
		// and counts as nothing closed. A close count here would claim a write
		// the live run refuses, and on a dry run the --preview hint beside it
		// would hand over a hash no live run will accept.
		fmt.Fprintf(stderr, "%s: %s\n", r.Database,
			reaper.FloorNotice(r.FlooredAt, len(r.ClosedEntries)))
		printAutoCloseCandidates(stdout, r)
		return 0
	}

	prefix := ""
	if r.DryRun {
		prefix = "[DRY RUN] would "
	}
	printAutoCloseCandidates(stdout, r)
	line := fmt.Sprintf("%s: %sauto-closed %d stale issues", r.Database, prefix, r.Closed)
	if r.DryRun {
		// A dry run's job is to hand the live run its authorization, so print
		// the flag to paste verbatim — the pair reads as one instruction
		// instead of two commands a caller might reorder.
		line += fmt.Sprintf(" — live run: --preview=%s", r.PreviewHash)
	}
	fmt.Fprintln(stdout, line)
	return r.Closed
}

// autoCloseCandidatesWriter routes a hard refusal's candidate list: stderr
// under --json, where the list is the one thing on it that is not the JSON
// document, and stdout otherwise, where it reads with the rest of the report.
func autoCloseCandidatesWriter() io.Writer {
	if reaperJSON {
		return os.Stderr
	}
	return os.Stdout
}

func reaperDatabaseNames() []string {
	if reaperDB == "" {
		return reaper.DiscoverDatabases(reaperUserInfo(), reaperHost, reaperPort)
	}
	return parseReaperDatabaseList(reaperDB)
}

// parseReaperPreviewHashes splits a --preview list on commas, dropping blanks.
// A whole-town dry run prints one hash per database — PreviewHash binds the
// database name — so the live run takes them together rather than one hash
// that can only ever authorize the database it was made for.
func parseReaperPreviewHashes(list string) []string {
	var hashes []string
	for _, part := range strings.Split(list, ",") {
		if h := strings.TrimSpace(part); h != "" {
			hashes = append(hashes, h)
		}
	}
	return hashes
}

// autoClosePreviewHash returns the preview hash to give one database's sweep.
//
// A single-database run hands the operator's hash through untouched: the sweep
// compares it and reports the hash to re-run for, exactly as before.
//
// A whole-town run cannot, because PreviewHash includes the database name and
// one hash therefore authorizes one database. The database's current set is
// read here, and its own hash authorizes the sweep only when the operator's
// dry run printed it. A database whose set the dry run did not show gets no
// hash, so the sweep refuses with the one to re-run for instead of tripping
// over a hash that belongs to a sibling database (gt-fokkb).
//
// opts is the live sweep's own options: the preview is that sweep with only
// the write defeated, so it cannot hash a different set because an option
// (StaleAge, MaxCloses, Force) reached one call and not the other. The
// operator's dry run goes through the same options, which makes this call
// identical to the one that printed the hash.
//
// sweep is reaper.AutoClose, passed in so the mapping is testable without a
// Dolt server.
func autoClosePreviewHash(sweep func(*beadsql.DB, reaper.Writer, string, reaper.AutoCloseOptions) (*reaper.AutoCloseResult, error), db *beadsql.DB, dbName string, opts reaper.AutoCloseOptions, hashes []string, wholeTown bool) (string, error) {
	if len(hashes) == 0 {
		return "", nil
	}
	if !wholeTown {
		return hashes[0], nil
	}
	previewOpts := opts
	previewOpts.DryRun = true
	previewOpts.PreviewHash = ""
	preview, err := sweep(db, nil, dbName, previewOpts)
	if err != nil {
		return "", err
	}
	for _, h := range hashes {
		if h == preview.PreviewHash {
			return h, nil
		}
	}
	return "", nil
}

// parseReaperDatabaseList splits a --db list on commas, dropping blanks.
func parseReaperDatabaseList(list string) []string {
	parts := strings.Split(list, ",")
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
	townRoot, _ := workspace.FindFromCwd()
	return reaperEndpoint(townRoot)
}

// reaperUserInfo is the user[:password] the reaper connects to Dolt with, from
// the town's Dolt config.
func reaperUserInfo() string {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		townRoot = ""
	}
	return reaperUserInfoFor(townRoot)
}

// reaperUserInfoFor is the user[:password] for the town at townRoot. An empty
// townRoot (the cwd is outside any town) connects as the default user with no
// password, where DefaultConfig("") would read the config of no town.
func reaperUserInfoFor(townRoot string) string {
	if townRoot == "" {
		return doltserver.DefaultUser
	}
	return doltserver.DefaultConfig(townRoot).UserDSN()
}

// reaperEndpoint is the town's Dolt host and port (gt-y3pgh.3), with host
// 127.0.0.1 when the town names none. The port is 0 when there is no town
// endpoint: the reaper then needs --port and never guesses one.
func reaperEndpoint(townRoot string) (string, int) {
	ep, _ := agentconfig.ResolveDoltEndpoint(townRoot)
	if ep.Host == "" {
		ep.Host = "127.0.0.1"
	}
	return ep.Host, ep.Port
}

func waitBeforeReaperDatabase(clk clockwork.Clock, index int, dbDelay string) error {
	if index == 0 {
		return nil
	}
	delay, err := time.ParseDuration(dbDelay)
	if err != nil {
		return fmt.Errorf("invalid --db-delay: %w", err)
	}
	if delay > 0 {
		clk.Sleep(delay)
	}
	return nil
}

var reaperCmd = &cobra.Command{
	Use:     "reaper",
	GroupID: GroupServices,
	Short:   "Wisp and issue cleanup operations",
	Long: `Execute wisp reaper operations against Dolt databases.

The daemon's wisp_reaper patrol runs the same cycle in-process; these
subcommands run its steps by hand. They execute SQL operations but leave
eligibility decisions to the caller.

Examples:
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
		dbs := reaper.DiscoverDatabases(reaperUserInfo(), reaperHost, reaperPort)
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
Use this to understand the state before deciding what to reap.`,
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
		trackerRigs, databases, err := reaperRunTargets(workspace.FindFromCwd, reaperDB == "", databases)
		if err != nil {
			return err
		}

		var results []*reaper.ScanResult
		for i, dbName := range databases {
			if err := waitBeforeReaperDatabase(clockwork.NewRealClock(), i, reaperDBDelay); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Fprintf(os.Stderr, "skip invalid db: %s\n", dbName)
				continue
			}

			db, err := reaper.OpenDB(reaperUserInfo(), reaperHost, reaperPort, dbName, 10*time.Second, 10*time.Second)
			if err != nil {
				if errors.Is(err, beadsql.ErrNotBeads) {
					continue
				}
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
		results = append(results, scanTrackerTargets(context.Background(), trackerRigs, reaperTrackerWisps, maxAge, purgeAge, time.Now())...)

		if reaperJSON {
			fmt.Println(reaper.FormatJSON(results))
		} else {
			var totalReap, totalMoleculeSteps, totalPurge, totalMail, totalStale, totalOpen int
			for _, r := range results {
				fmt.Printf("Database: %s\n", r.Database)
				if r.Floored {
					// Soft floor (gt-ecpj): the stale count is at the threshold
					// that was asked for — the same set auto-close's refusal
					// reports — so name the threshold the count belongs to.
					// Counting at the floor would show a smaller set than the
					// refusal does, and the two could not be compared.
					fmt.Printf("  NOTE: --stale-age %s is below the %s floor; stale count is at the threshold asked for, which auto-close refuses (pass --force to override)\n",
						r.FlooredAt, reaper.MinStaleIssueAge)
				}
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
		trackerRigs, databases, err := reaperRunTargets(workspace.FindFromCwd, reaperDB == "", databases)
		if err != nil {
			return err
		}
		purgeAge, err := reapTrackerPurgeAge(trackerRigs, reaperPurgeAge)
		if err != nil {
			return err
		}

		var results []*reaper.ReapResult
		// reapFailures counts the databases that contributed no reading, so the
		// open-wisp alert can tell a reading of the town from a reading of part
		// of it (gt-11kyy).
		var reapFailures int
		for i, dbName := range databases {
			if err := waitBeforeReaperDatabase(clockwork.NewRealClock(), i, reaperDBDelay); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Fprintf(os.Stderr, "skip invalid db: %s\n", dbName)
				reapFailures++
				continue
			}

			db, err := reaper.OpenDB(reaperUserInfo(), reaperHost, reaperPort, dbName, 10*time.Second, 10*time.Second)
			if err != nil {
				if errors.Is(err, beadsql.ErrNotBeads) {
					continue
				}
				fmt.Fprintf(os.Stderr, "%s: connect error: %v\n", dbName, err)
				reapFailures++
				continue
			}

			if ok, err := reaper.HasReaperSchema(db); err != nil {
				fmt.Fprintf(os.Stderr, "%s: schema check error: %v\n", dbName, err)
				db.Close()
				reapFailures++
				continue
			} else if !ok {
				db.Close()
				continue
			}

			w, err := reaperWriter(dbName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", dbName, err)
				db.Close()
				reapFailures++
				continue
			}
			result, err := reaper.Reap(db, w, dbName, maxAge, reaperDryRun)
			db.Close()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: reap error: %v\n", dbName, err)
				reapFailures++
				continue
			}
			results = append(results, result)
		}
		tracked, failed := reapTrackerTargets(context.Background(), trackerRigs, reaperTrackerWisps, purgeAge, reaperDryRun, time.Now())
		results = append(results, tracked...)
		reapFailures += failed

		var totalReaped, totalMoleculeSteps, totalOpen int
		for _, r := range results {
			totalReaped += r.Reaped
			totalMoleculeSteps += r.MoleculeStepsClosed
			totalOpen += r.OpenRemain
		}

		if reaperJSON {
			fmt.Println(reaper.FormatJSON(results))
		} else {
			for _, r := range results {
				fmt.Println(reapResultLine(r))
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
			}
		}
		townRoot, _ := workspace.FindFromCwd()
		reportReaperRunOpenWisps(os.Stderr, townRoot, reaperRunAlert{
			Sample:          reaper.OpenWispSample{OpenWisps: totalOpen, Databases: len(results), DryRun: reaperDryRun},
			WholeTown:       reaperDB == "" && !reaperJSON,
			FailedDatabases: reapFailures,
		})
		return nil
	},
}

// reaperRunAlert is one hand-run cycle's open-wisp reading together with what
// the run covered, which is what decides whether the reading belongs in the
// alert's series at all.
type reaperRunAlert struct {
	Sample          reaper.OpenWispSample
	WholeTown       bool // false for a --db subset or a --json run
	FailedDatabases int  // databases that contributed no reading
}

// reportReaperRunOpenWisps records a hand-run cycle's reading in the series
// that belongs to hand runs, and warns on w when it has grown enough to mean
// accumulation.
//
// Only a whole-town, non-JSON run that lost no database is a reading of the
// town (gt-11kyy): a --db run measures a subset, a --json run usually feeds a
// monitor sampling at its own rate, and a run missing a database counts low —
// each would post a reading the next real cycle reads as growth or as a fall.
func reportReaperRunOpenWisps(w io.Writer, townRoot string, run reaperRunAlert) {
	switch {
	case townRoot == "":
		return
	case !run.WholeTown:
		return
	case run.FailedDatabases > 0:
		fmt.Fprintf(w, "WARNING: open-wisp alert skipped: %d database(s) failed, so this run's count is incomplete\n", run.FailedDatabases)
		return
	}
	reportReaperOpenWispAlert(w, townRoot, run.Sample)
}

// reportReaperOpenWispAlert warns on w when this reading has grown enough over
// the last one recorded for hand runs to mean accumulation rather than a
// working set, then records it as the reading the next run is judged against
// (gt-11kyy).
func reportReaperOpenWispAlert(w io.Writer, townRoot string, sample reaper.OpenWispSample) {
	path := daemon.WispAlertCLIBaselinePath(townRoot)
	previous, err := daemon.LoadWispAlertState(path)
	if err != nil {
		fmt.Fprintf(w, "WARNING: cannot read the open-wisp baseline (%v) — this run cannot be judged against the last one\n", err)
	}
	next, alert, detail := reaper.NextOpenWispAlertState(previous, sample)
	if alert {
		fmt.Fprintf(w, "WARNING: %s — investigate wisp lifecycle\n", detail)
	}
	if err := daemon.SaveWispAlertState(path, next); err != nil {
		fmt.Fprintf(w, "WARNING: cannot record the open-wisp baseline (%v) — the next run will have nothing to compare against\n", err)
	}
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
			if err := waitBeforeReaperDatabase(clockwork.NewRealClock(), i, reaperDBDelay); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Fprintf(os.Stderr, "skip invalid db: %s\n", dbName)
				continue
			}

			db, err := reaper.OpenDB(reaperUserInfo(), reaperHost, reaperPort, dbName, 30*time.Second, 30*time.Second)
			if err != nil {
				if errors.Is(err, beadsql.ErrNotBeads) {
					continue
				}
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

			w, err := reaperWriter(dbName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", dbName, err)
				db.Close()
				continue
			}
			result, err := reaper.Purge(db, w, dbName, purgeAge, mailAge, reaperDryRun)
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
issue types, agent beads (every deacon, dog, witness, refinery, crew and
polecat bead carries gt:agent), plugin receipts, and issues with active
dependencies. Scan and auto-close share one eligibility clause and one
stale-age floor rule, so 'gt reaper scan' is a faithful preview of this
command — including below the floor, where both report the set the threshold
asked for and this command closes nothing.

Four guards apply, the first three answering the 2026-09-16 mis-close that swept
102 durable beads (gt-2qzr):
  - --stale-age below 7d refuses as a soft error (gt-ecpj): nothing is closed,
    the command reports the set that threshold would take — the blast radius,
    which the floor's smaller set would hide — and it exits 0, because the
    notice is the refusal rather than a stop. --force lifts the floor.
  - more than --max-closes candidates in one database refuses the whole run
    (nothing is closed) unless --force.
  - auto-close disarmed in the daemon patrol config
    (daemon.patrols.wisp_reaper.auto_close=false in settings/config.json)
    is refused unless --force. A config that does not parse is refused the
    same way: the setting cannot be read, so the sweep does not assume it is
    unset (gt-ptslm).
  - a live run with no --preview hash is refused (nothing is closed) unless
    --force: the sweep only closes a set a dry run already showed, whatever
    order the two commands ran in (gt-39bu — the 2026-09-20 reaper ran live
    before its dry run). --preview says where the hash comes from.

When --db is provided, auto-closes in a single database. When omitted,
auto-discovers all databases on the Dolt server and auto-closes in each one.

Returns the count of closed issues. Use --dry-run to preview.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		staleAge, err := parseReaperAge("--stale-age", reaperStaleAge)
		if err != nil {
			return err
		}

		if disarmed, reason := reaperAutoCloseDisarmed(); disarmed && !reaperForce {
			return fmt.Errorf("auto-close is disarmed by %s; pass --force to override", reason)
		}

		databases := reaperDatabaseNames()
		previewHashes := parseReaperPreviewHashes(reaperPreview)

		var results []*reaper.AutoCloseResult
		var refusals []string
		for i, dbName := range databases {
			if err := waitBeforeReaperDatabase(clockwork.NewRealClock(), i, reaperDBDelay); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Fprintf(os.Stderr, "skip invalid db: %s\n", dbName)
				continue
			}

			db, err := reaper.OpenDB(reaperUserInfo(), reaperHost, reaperPort, dbName, 10*time.Second, 10*time.Second)
			if err != nil {
				if errors.Is(err, beadsql.ErrNotBeads) {
					continue
				}
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

			w, err := reaperWriter(dbName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", dbName, err)
				db.Close()
				continue
			}
			opts := reaper.AutoCloseOptions{
				StaleAge:  staleAge,
				DryRun:    reaperDryRun,
				MaxCloses: reaperMaxCloses,
				Force:     reaperForce,
			}
			previewHash, err := autoClosePreviewHash(reaper.AutoClose, db, dbName, opts, previewHashes, len(databases) > 1 && !reaperDryRun)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: preview error: %v\n", dbName, err)
				db.Close()
				continue
			}
			opts.PreviewHash = previewHash
			result, err := reaper.AutoClose(db, w, dbName, opts)
			db.Close()
			if err != nil {
				// A below-floor stale-age is a soft error (gt-ecpj): the sweep
				// closed nothing, and its result carries the set the mis-set
				// threshold would take. The command exits 0 so a cycle passing
				// on status still completes — the notice is the refusal, not
				// the stop — which makes the result, not the error, the thing
				// to report.
				if errors.Is(err, reaper.ErrStaleAgeTooLow) {
					if result != nil {
						results = append(results, result)
					}
					continue
				}
				// Every other refusal leaves the database untouched, so it is a
				// stop to read rather than a partial sweep to retry. Keep the
				// detail so the operator sees what the sweep wanted to take and
				// what it was missing. Candidates go to stderr under --json so
				// the stream stays parseable.
				if errors.Is(err, reaper.ErrTooManyCloses) ||
					errors.Is(err, reaper.ErrPreviewRequired) ||
					errors.Is(err, reaper.ErrPreviewMismatch) {
					fmt.Fprintf(os.Stderr, "%s: %v\n", dbName, err)
					if result != nil {
						printAutoCloseCandidates(autoCloseCandidatesWriter(), result)
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
				totalClosed += writeAutoCloseReport(os.Stdout, os.Stderr, r)
			}
			if len(results) > 1 {
				prefix := ""
				if reaperDryRun {
					prefix = "[DRY RUN] "
				}
				fmt.Printf("\n%sAuto-close summary (%d databases): auto-closed %d stale issues\n",
					prefix, len(results), totalClosed)
			}
			if reaperDryRun && len(results) > 1 {
				// The live run needs one hash per database, since PreviewHash
				// binds the database name; print the whole-town list so the
				// operator hands back every set this run showed.
				var hints []string
				for _, r := range results {
					if !r.Floored && r.PreviewHash != "" {
						hints = append(hints, r.PreviewHash)
					}
				}
				if len(hints) > 0 {
					fmt.Printf("whole-town live run: --preview=%s\n", strings.Join(hints, ","))
				}
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

The daemon's wisp_reaper patrol runs this cycle in-process; this command runs
it by hand.`,
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

		autoCloseDisarmed, autoCloseReason := reaperAutoCloseDisarmed()

		var totalReaped, totalMoleculeSteps, totalPurged, totalMailPurged, totalClosed, totalOpen int
		// openDBs counts the databases totalOpen covers and openFailures the ones
		// that contributed no reading, so the open-wisp alert can tell a reading
		// of the town from a reading of part of it (gt-11kyy).
		var openDBs, openFailures int

		for i, dbName := range databases {
			if err := waitBeforeReaperDatabase(clockwork.NewRealClock(), i, reaperDBDelay); err != nil {
				return err
			}
			if err := reaper.ValidateDBName(dbName); err != nil {
				fmt.Printf("skip invalid db: %s\n", dbName)
				openFailures++
				continue
			}

			db, err := reaper.OpenDB(reaperUserInfo(), reaperHost, reaperPort, dbName, 30*time.Second, 30*time.Second)
			if err != nil {
				if errors.Is(err, beadsql.ErrNotBeads) {
					continue
				}
				fmt.Printf("%s: connect error: %v\n", dbName, err)
				openFailures++
				continue
			}

			if ok, err := reaper.HasReaperSchema(db); err != nil {
				fmt.Printf("%s: schema check error: %v\n", dbName, err)
				db.Close()
				openFailures++
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
				openFailures++
				continue
			}
			for _, a := range scanResult.Anomalies {
				fmt.Printf("%s: %s %s\n", dbName, style.Warning.Render("ANOMALY:"), a.Message)
			}

			w, err := reaperWriter(dbName)
			if err != nil {
				fmt.Printf("%s: %v\n", dbName, err)
				db.Close()
				openFailures++
				continue
			}

			// Reap
			reapResult, err := reaper.Reap(db, w, dbName, maxAge, reaperDryRun)
			if err != nil {
				fmt.Printf("%s: reap error: %v\n", dbName, err)
				openFailures++
			} else {
				totalReaped += reapResult.Reaped
				totalMoleculeSteps += reapResult.MoleculeStepsClosed
				totalOpen += reapResult.OpenRemain
				openDBs++
			}

			// Purge
			purgeResult, err := reaper.Purge(db, w, dbName, purgeAge, mailAge, reaperDryRun)
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
				fmt.Printf("%s: auto-close skipped (disarmed by %s)\n", dbName, autoCloseReason)
			} else {
				preview, err := reaper.AutoClose(db, nil, dbName, reaper.AutoCloseOptions{
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
				if preview.Floored {
					// A below-floor threshold is refused (gt-ecpj), and the
					// refusal is a notice rather than a stop: report the set
					// that threshold would take — its blast radius, which is
					// the number the refusal is for — and carry on.
					fmt.Fprintf(os.Stderr, "%s: %s\n", dbName,
						reaper.FloorNotice(preview.FlooredAt, len(preview.ClosedEntries)))
				} else {
					fmt.Printf("%s: auto-close preview: %d candidate(s)\n", dbName, preview.Closed)
				}

				closeResult, err := reaper.AutoClose(db, w, dbName, reaper.AutoCloseOptions{
					StaleAge:    staleAge,
					DryRun:      reaperDryRun,
					MaxCloses:   reaperMaxCloses,
					PreviewHash: preview.PreviewHash,
					Force:       reaperForce,
				})
				if err != nil && !errors.Is(err, reaper.ErrStaleAgeTooLow) {
					// Any refusal but the below-floor one is a stop, not a
					// partial sweep: report it and leave the database untouched.
					fmt.Printf("%s: auto-close refused: %v\n", dbName, err)
				}
				if closeResult != nil {
					// A below-floor refusal (gt-ecpj) is soft and reports the
					// set the mis-set threshold would take: it closes nothing,
					// so it counts as nothing closed, and it arrives with a
					// non-nil error, so neither the error nor its Close count
					// may decide what prints.
					printAutoCloseCandidates(os.Stdout, closeResult)
					if !closeResult.Floored {
						totalClosed += closeResult.Closed
					}
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

		townRoot, _ := workspace.FindFromCwd()
		reportReaperRunOpenWisps(os.Stderr, townRoot, reaperRunAlert{
			Sample:          reaper.OpenWispSample{OpenWisps: totalOpen, Databases: openDBs, DryRun: reaperDryRun},
			WholeTown:       reaperDB == "" && !reaperJSON,
			FailedDatabases: openFailures,
		})

		return nil
	},
}

func init() {
	// Shared flags
	// GH#2601: Default host/port from the town's Dolt endpoint for
	// non-localhost setups.
	defaultHost, defaultPort := defaultReaperEndpoint()

	for _, cmd := range []*cobra.Command{reaperScanCmd, reaperReapCmd, reaperPurgeCmd, reaperAutoCloseCmd, reaperRunCmd, reaperDatabasesCmd} {
		cmd.Flags().StringVar(&reaperDB, "db", "", "Database name (required for single-db commands)")
		cmd.Flags().StringVar(&reaperHost, "host", defaultHost, "Dolt server host (default: the town's Dolt host)")
		cmd.Flags().IntVar(&reaperPort, "port", defaultPort, "Dolt server port (default: the town's Dolt port)")
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
	// reap's purge-age is only read for a tracker rig, whose reap is the tracker's
	// purge of closed wisps; a bd rig's reap closes by max-age and ignores it.
	reaperReapCmd.Flags().StringVar(&reaperPurgeAge, "purge-age", "168h",
		"Max closed wisp age before a tracker rig's purge (7d); bd rigs ignore it")
	for _, cmd := range []*cobra.Command{reaperScanCmd, reaperAutoCloseCmd, reaperRunCmd} {
		cmd.Flags().StringVar(&reaperStaleAge, "stale-age", "720h", "Max issue staleness before auto-close (30d; below 7d is a soft refusal that reports the set and closes nothing; --force lifts it)")
	}
	// Guardrails on the auto-close write (gt-2qzr). --force is deliberately
	// absent from scan: a preview should never need overriding.
	for _, cmd := range []*cobra.Command{reaperAutoCloseCmd, reaperRunCmd} {
		cmd.Flags().IntVar(&reaperMaxCloses, "max-closes", reaper.DefaultAutoCloseMaxPerRun,
			"Refuse the run if a database has more than this many auto-close candidates")
		cmd.Flags().BoolVar(&reaperForce, "force", false,
			"Override the stale-age floor, the per-run cap, the preview hash requirement, and the daemon patrol config's auto-close disarm")
	}

	// --preview is auto-close's own: `gt reaper run` previews in-process before
	// it writes, so it has no separate preview to be handed (gt-39bu).
	reaperAutoCloseCmd.Flags().StringVar(&reaperPreview, "preview", "",
		"Preview hash from a preceding --dry-run; a live run without it is refused. A whole-town dry run prints one per database: pass them comma-separated")

	reaperCmd.AddCommand(reaperDatabasesCmd)
	reaperCmd.AddCommand(reaperScanCmd)
	reaperCmd.AddCommand(reaperReapCmd)
	reaperCmd.AddCommand(reaperPurgeCmd)
	reaperCmd.AddCommand(reaperAutoCloseCmd)
	reaperCmd.AddCommand(reaperRunCmd)

	rootCmd.AddCommand(reaperCmd)
}
