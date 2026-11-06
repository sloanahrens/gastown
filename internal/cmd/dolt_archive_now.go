package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/workspace"
)

// The daemon's archive outcomes the command acts on, as the result contract
// spells them. The partial answer's word comes from the update package's
// OutcomePartial, so the command never carries a second copy of it.
const (
	archiveOutcomeCompleted = "completed"
	archiveOutcomeDeferred  = "deferred"
	archiveOutcomeFailed    = "failed"
	archiveOutcomePartial   = string(update.OutcomePartial)
)

// doltArchiveNowPoll is how often the wait re-reads the request file. The
// daemon's scheduled_maintenance tick is the other side of the wait, so a
// shorter poll only re-reads a file.
const doltArchiveNowPoll = 15 * time.Second

// doltArchiveNowReads is where archive-now reads the town and the archive: the
// town root, whether the hold and request files exist, the database list, the
// archive-behind readings, the request protocol, and the wait's own pieces.
// Production passes the real calls; a test passes fakes, so no command in a
// test touches the live town, a Dolt server, a real sleep or the live archive.
type doltArchiveNowReads struct {
	townRoot    func() (string, error)
	stat        func(path string) error
	databases   func(townRoot string) ([]string, error)
	behind      func(ctx context.Context, townRoot string, databases []string) (map[string]archiveNowReading, error)
	request     func(townRoot string) archiveNowRequester
	requestPath func(townRoot string) string
	runID       func() string
	sleep       func(context.Context, time.Duration) error
	timeout     time.Duration
}

// archiveNowRequester is the daemon request protocol archive-now drives: the
// archive file pair's exclusive create and bounded wait.
type archiveNowRequester interface {
	Request(runID string) error
	Wait(ctx context.Context, sleep func(context.Context, time.Duration) error, runID string, timeout, poll time.Duration) (update.MaintenanceResult, error)
}

// archiveNowReading is one database's archive lag at one moment: how many
// commits and events rows the archive has not written out.
type archiveNowReading struct {
	commits int64
	events  int64
}

// liveDoltArchiveNowReads is the command's production reads.
func liveDoltArchiveNowReads(timeout time.Duration) doltArchiveNowReads {
	return doltArchiveNowReads{
		townRoot: workspace.FindFromCwdOrError,
		stat:     updateFiles{}.Stat,
		databases: func(townRoot string) ([]string, error) {
			return doltserver.ListDatabases(townRoot)
		},
		behind: liveArchiveNowBehind,
		request: func(townRoot string) archiveNowRequester {
			return &update.ArchiveRequest{
				Dir:   update.Dir(townRoot),
				Files: updateMaintFiles{},
				Clock: updateClock{},
			}
		},
		requestPath: func(townRoot string) string {
			return filepath.Join(update.Dir(townRoot), update.ArchiveRequestFileName)
		},
		runID:   newUpdateRunID,
		sleep:   updateSleep,
		timeout: timeout,
	}
}

// liveArchiveNowBehind counts every database's archive lag the way `gt dolt
// status` does: the daemon's watermarks and a status archive reader.
func liveArchiveNowBehind(ctx context.Context, townRoot string, databases []string) (map[string]archiveNowReading, error) {
	return archiveNowBehind(ctx, databases,
		func() (daemon.ArchiveWatermarks, error) { return daemon.LoadArchiveWatermarks(townRoot) },
		func(shown []string) archiveBehindReader {
			return newStatusArchiveReader(townRoot, doltserver.DefaultConfig(townRoot), shown)
		})
}

// archiveNowBehind is liveArchiveNowBehind's body over an injected watermark
// loader and reader. A new reader per reading, so the reading after the pass
// sees the archive it wrote; the production loader and reader read the town's
// state file and count over SQL, so a test drives their failure paths with
// fakes.
func archiveNowBehind(ctx context.Context, databases []string, loadWatermarks func() (daemon.ArchiveWatermarks, error), newReader func([]string) archiveBehindReader) (map[string]archiveNowReading, error) {
	wms, err := loadWatermarks()
	if err != nil {
		return nil, fmt.Errorf("reading the archive watermarks: %w", err)
	}
	reader := newReader(databases)
	out := make(map[string]archiveNowReading, len(databases))
	for _, db := range databases {
		commits, err := reader.commitsBehind(ctx, db, wms.Commits[db])
		if err != nil {
			return nil, fmt.Errorf("database %s: reading how far its commits are behind the archive: %w", db, err)
		}
		events, err := reader.eventsBehind(ctx, db, wms.Events[db])
		if err != nil {
			return nil, fmt.Errorf("database %s: reading how far its events are behind the archive: %w", db, err)
		}
		out[db] = archiveNowReading{commits: commits, events: events}
	}
	return out, nil
}

// runDoltArchiveNow is the cobra entry point: it takes an interruptible
// context, because the bounded wait is long enough that Ctrl-C is how an
// operator gives up on it.
func runDoltArchiveNow(cmd *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDoltArchiveNowCmd(cmd, ctx, liveDoltArchiveNowReads(doltArchiveNowTimeout))
}

// runDoltArchiveNowCmd runs the command with its context and reads, and
// silences cobra when the exit code is the answer.
func runDoltArchiveNowCmd(cmd *cobra.Command, ctx context.Context, reads doltArchiveNowReads) error {
	err := runDoltArchiveNowWith(ctx, cmd.OutOrStdout(), reads)
	if _, ok := IsSilentExit(err); ok {
		// The exit code is the answer; cobra must not print it as an error.
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
	}
	return err
}

// runDoltArchiveNowWith asks the daemon for the history archive pass and
// reports, per database, how far behind the archive it was before and after.
//
// It refuses without the overseer's dispatch hold and while another archive
// request is pending (exit 3), reports the daemon's failed answer (exit 1) and
// reports anything not done and verified caught up (exit 2). Zero is only for
// a completed answer with every database reading 0 behind afterwards, because
// the pass is run before a flatten, where a partial answer reads as history
// that is safe to discard.
func runDoltArchiveNowWith(ctx context.Context, w io.Writer, reads doltArchiveNowReads) error {
	townRoot, err := reads.townRoot()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	holdPath := dispatch.HoldFilePath(townRoot)
	switch err := reads.stat(holdPath); {
	case err == nil:
		// The hold exists: the overseer has authorized the pass.
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(w, "refusing to run archive-now: no dispatch hold at %s\n", holdPath)
		fmt.Fprintln(w, "The overseer creates and lifts that hold, never the operator or a bird. Ask the overseer for the hold, then run this again.")
		return NewSilentExit(3)
	default:
		// An unreadable hold is not an absent one: refuse rather than run a
		// pass whose authorization could not be checked.
		return fmt.Errorf("checking the dispatch hold %s: %w", holdPath, err)
	}

	databases, err := reads.databases(townRoot)
	if err != nil {
		return fmt.Errorf("listing the databases to archive: %w", err)
	}
	if len(databases) == 0 {
		// An empty list would pass the "every database reads 0 behind" check
		// without the pass having been asked for anything.
		return fmt.Errorf("no databases to archive in %s", townRoot)
	}

	// The before reading must precede the request: once the request is written
	// the daemon may start the pass at any time.
	before, err := archiveNowCounts(ctx, reads, townRoot, databases, "before")
	if err != nil {
		return err
	}

	requestPath := reads.requestPath(townRoot)
	runID := reads.runID()
	req := reads.request(townRoot)
	if err := req.Request(runID); err != nil {
		if statErr := reads.stat(requestPath); statErr == nil {
			// The file exists, so a request is already pending: the daemon
			// answers the run that wrote it, and a second request must not
			// replace it.
			fmt.Fprintf(w, "refusing to run archive-now: an archive request is already pending at %s; another job is waiting for it. Wait for that run to finish, then run this again.\n", requestPath)
			return NewSilentExit(3)
		}
		return fmt.Errorf("writing the archive request %s: %w", requestPath, err)
	}

	res, err := req.Wait(ctx, reads.sleep, runID, reads.timeout, doltArchiveNowPoll)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// An interrupted wait leaves the request in place, so the daemon may
			// still run the pass: that is still pending, not a failure, and the
			// exit code must not tell the operator to start a second run.
			fmt.Fprintf(w, "archive-now: still pending: the wait was interrupted, and the request is still at %s for the daemon. Run archive-now again once it has answered.\n", requestPath)
			return NewSilentExit(2)
		}
		return fmt.Errorf("waiting for the archive request %s: %w", requestPath, err)
	}

	after, err := archiveNowCounts(ctx, reads, townRoot, databases, "after")
	if err != nil {
		return err
	}

	for _, db := range databases {
		fmt.Fprintln(w, archiveNowLine(db, before[db], after[db]))
	}
	return archiveNowReport(w, databases, after, res)
}

// archiveNowCounts takes one reading of every database's archive lag and
// refuses a reading that omits a database: a missing entry would print and be
// checked as zero.
func archiveNowCounts(ctx context.Context, reads doltArchiveNowReads, townRoot string, databases []string, when string) (map[string]archiveNowReading, error) {
	counts, err := reads.behind(ctx, townRoot, databases)
	if err != nil {
		return nil, fmt.Errorf("reading the archive lag %s the pass: %w", when, err)
	}
	for _, db := range databases {
		if _, ok := counts[db]; !ok {
			return nil, fmt.Errorf("reading the archive lag %s the pass: no reading for database %s", when, db)
		}
	}
	return counts, nil
}

// archiveNowLine is one database's before/after line: the two readings of the
// same count, so the operator sees what the pass moved.
func archiveNowLine(db string, before, after archiveNowReading) string {
	return fmt.Sprintf("%s: before %s, %s; after %s, %s",
		db, commitsBehindText(before.commits), archiveNowEventsText(before.events),
		commitsBehindText(after.commits), archiveNowEventsText(after.events))
}

// archiveNowEventsText renders an events count. A negative count is the
// reader's no-watermark existence check, which means at least one row is
// behind, not a count of -1.
func archiveNowEventsText(n int64) string {
	if n < 0 {
		return "at least one event behind"
	}
	return eventsBehindText(n)
}

// archiveNowReport writes the daemon's answer and returns the command's exit
// code: 0 only for a completed pass that every database's after reading
// confirms caught up, 1 for a failed answer, 2 for anything still pending.
func archiveNowReport(w io.Writer, databases []string, after map[string]archiveNowReading, res update.MaintenanceResult) error {
	switch res.Outcome {
	case archiveOutcomeCompleted:
		if behind := archiveNowStillBehind(databases, after); len(behind) > 0 {
			fmt.Fprintf(w, "archive-now: the daemon answered completed, but %s still reads history behind the archive. Run archive-now again.\n", strings.Join(behind, ", "))
			return NewSilentExit(2)
		}
		fmt.Fprintln(w, "archive-now: done: every database reads 0 behind its archive.")
		return nil
	case archiveOutcomePartial:
		if detail := strings.TrimSpace(res.Detail); detail != "" {
			fmt.Fprintln(w, detail)
		}
		fmt.Fprintln(w, "archive-now: the archive pass stopped with history still behind. Run archive-now again.")
		return NewSilentExit(2)
	case archiveOutcomeDeferred:
		fmt.Fprintf(w, "archive-now: still pending: %s. Run archive-now again.\n", archiveNowDetail(res))
		return NewSilentExit(2)
	case archiveOutcomeFailed:
		fmt.Fprintf(w, "archive-now: the daemon answered failed: %s\n", archiveNowDetail(res))
		return NewSilentExit(1)
	default:
		// An outcome this command does not know is not a completed pass.
		fmt.Fprintf(w, "archive-now: the daemon answered %q, which this command does not understand; the pass did not report done. Run archive-now again.\n", res.Outcome)
		return NewSilentExit(1)
	}
}

// archiveNowStillBehind names the databases whose after reading is not zero on
// both exports. A negative events count is the existence check, not caught up.
func archiveNowStillBehind(databases []string, after map[string]archiveNowReading) []string {
	var out []string
	for _, db := range databases {
		r := after[db]
		if r.commits != 0 || r.events != 0 {
			out = append(out, db)
		}
	}
	return out
}

// archiveNowDetail is the daemon's detail text, or a placeholder when the
// result carries none.
func archiveNowDetail(res update.MaintenanceResult) string {
	if detail := strings.TrimSpace(res.Detail); detail != "" {
		return detail
	}
	return "the daemon gave no detail"
}
