package submission

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap is
// town-wide: it counts every Jev caller in the usage ledger, not this job
// alone.
const DefaultMaxUSDPerDay = 1.0

const (
	// gitTimeout bounds one commit or diff read in the clone.
	gitTimeout = time.Minute
	// fetchTimeout bounds the clone's fetch, which carries a whole rig's
	// branches.
	fetchTimeout = 5 * time.Minute
)

// Settings is the operator's configuration for the pass, read from
// patrols.jev_submission.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it (gt-aqczg).
	Scrub bool
}

// beadLister is the part of *beads.Beads the pass's lister uses. It is an
// interface so a test can see the query the production lister sends without a
// Dolt server.
type beadLister interface {
	List(beads.ListOptions) ([]*beads.Issue, error)
	Show(id string) (*beads.Issue, error)
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the rig's beads, the bare clone and the
// cap read from the same ledger. warnings receives the ledger's own warnings
// and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: s.USDPerBillionTokens}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_submission: %w", err)
	}

	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, s.Scrub)...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	beadsDir := doltserver.FindRigBeadsDir(townRoot, RigName)
	if beadsDir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", RigName)
	}
	origin, err := rigOriginURL(townRoot)
	if err != nil {
		return nil, err
	}

	return &Pass{
		Asker:       client,
		Submissions: rigBeads{b: beads.New(beadsDir), records: land.SubmissionsSeenPath(townRoot), warnings: warnings},
		Git:         lockedGit{Git: newBareGit(ClonePath(townRoot), origin), townRoot: townRoot},
		LogPath:     LogPath(townRoot),
		CursorPath:  CursorPath(townRoot),
		Allows:      func() (bool, error) { return town.Allows(time.Now) },
		Warnings:    warnings,
	}, nil
}

// lockedGit fetches the shared clone under the clone's fetch lock, waiting for
// a fetch in flight. The pass holds its run lock for its whole run, which a
// fetch from another battery would wait out, so the fetch has a lock of its own.
type lockedGit struct {
	Git
	townRoot string
}

// Sync takes the clone's fetch lock, then fetches.
func (g lockedGit) Sync(ctx context.Context) error {
	return syncUnderFetchLock(ctx, g.townRoot, g.Git.Sync)
}

// rigOriginURL is the URL the pass clones from: the rig registry's git_url,
// the same place every other part of the town fetches the rig from.
func rigOriginURL(townRoot string) (string, error) {
	cfg, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return "", fmt.Errorf("reading the rig registry: %w", err)
	}
	entry, ok := cfg.Rigs[RigName]
	if !ok || strings.TrimSpace(entry.GitURL) == "" {
		return "", fmt.Errorf("rig %s has no git_url in the rig registry", RigName)
	}
	return entry.GitURL, nil
}

// ClonePath is the bare clone the pass owns: created on its first run and
// fetched by every pass after that.
func ClonePath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "repos", RigName+".git")
}

// recordWindow is how many of the newest first-sight records a pass reads. A
// pass reads more only when a backlog still fills the window after the trim, so
// its read stays flat as submissions accumulate.
const recordWindow = 500

// backlogLimit is how many submissions a pass takes from a backlog that fills
// the window after the trim. The rest wait for the next pass, which trims what
// this one asked about and takes the next backlogLimit.
const backlogLimit = recordWindow

// rigBeads reads a rig's submissions: the first-sight records the landing
// worker wrote, and the waiting work beads that have none. Its only write is
// trimming the first-sight log, and only when the log is longer than the
// read window.
type rigBeads struct {
	b beadLister
	// records is the first-sight log, land.SubmissionsSeenPath in production.
	records string
	// warnings receives a line for each record whose bead cannot be read. Nil
	// drops them.
	warnings io.Writer
}

// Since returns the submissions from t on: each first-sight record, then each
// waiting bead that no record names at its head. A record whose bead cannot be
// read is reported and left out, so one bad read does not stop the pass for
// the submissions behind it.
//
// When the log is longer than recordWindow, Since first trims it of the records
// no pass can still ask about: those seen before t whose bead is not waiting at
// the head they name. A waiting bead keeps its record, because the dedupe below
// still needs it. If the trimmed log is still longer than the window, a backlog
// is waiting: reading only the newest records would drop a waiting bead's
// record and ask that bead a second time, so Since streams the whole log but
// returns only the oldest backlogLimit submissions. Run drains those and moves
// the cursor, and the next pass takes the following ones.
func (r rigBeads) Since(t time.Time) ([]Entry, error) {
	waiting, err := r.b.List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1, Limit: 0})
	if err != nil {
		return nil, err
	}
	waitingKeys := make(map[string]bool, len(waiting))
	var listed []Entry
	for _, issue := range waiting {
		if issue == nil || !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() {
			continue
		}
		w, err := land.WorkFromBead(issue, RigName)
		if err != nil {
			continue
		}
		waitingKeys[land.SubmissionKey(issue.ID, w.Head)] = true
		listed = append(listed, Entry{Issue: issue, Work: w, Seen: w.Submitted})
	}

	// One record past the window tells a log that fits from one that does not:
	// only a log longer than the window is trimmed, so a log of exactly
	// recordWindow records is not rewritten on every pass.
	recs, err := land.ReadRecentSubmissionRecords(r.records, recordWindow+1)
	if err != nil {
		return nil, err
	}
	// deferred holds the waiting beads whose record a backlog left for a later
	// pass, and cut is the seen time the backlog read stopped at, zero when it
	// did not stop.
	var deferred map[string]bool
	var cut time.Time
	if len(recs) > recordWindow {
		keep := func(rec land.SubmissionRecord) bool {
			return !rec.SeenAt.Before(t) || waitingKeys[land.SubmissionKey(rec.Bead, rec.Head)]
		}
		if _, err := land.TrimSubmissionRecords(r.records, keep); err != nil {
			return nil, fmt.Errorf("jev submission: trimming %s: %w", r.records, err)
		}
		if recs, err = land.ReadRecentSubmissionRecords(r.records, recordWindow+1); err != nil {
			return nil, err
		}
		// A window still full after the trim is a backlog. The records past
		// the window are the ones the dedupe needs, so the whole log is
		// streamed, and only the oldest backlogLimit submissions are kept.
		if len(recs) > recordWindow {
			if recs, deferred, cut, err = readBacklog(r.records, t, waitingKeys, backlogLimit); err != nil {
				return nil, err
			}
		}
	}

	recorded := make(map[string]bool, len(recs))
	var out []Entry
	for _, rec := range recs {
		if recorded[land.SubmissionKey(rec.Bead, rec.Head)] {
			continue
		}
		recorded[land.SubmissionKey(rec.Bead, rec.Head)] = true
		if rec.SeenAt.Before(t) {
			continue
		}
		issue, err := r.b.Show(rec.Bead)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			r.warnf("jev submission: skipping %s at %s: reading the bead: %v\n", rec.Bead, rec.Head, err)
			continue
		}
		out = append(out, Entry{
			Issue: issue,
			Work: land.Work{
				BeadID: rec.Bead, Rig: rec.Rig, Branch: rec.Branch, Head: rec.Head,
				Target: rec.Target, Submitted: rec.Submitted,
			},
			Seen:     rec.SeenAt,
			Subjects: rec.Subjects,
			Numstat:  rec.Numstat,
		})
	}

	for _, e := range listed {
		key := land.SubmissionKey(e.Issue.ID, e.Work.Head)
		// Past the cut, an entry waits for a later pass: the cursor moving to
		// it now would skip the records the backlog read left out.
		if recorded[key] || deferred[key] || (!cut.IsZero() && e.Seen.After(cut)) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// readBacklog streams the whole first-sight log and keeps the oldest limit
// distinct submissions seen at or after t, plus every record of a waiting bead,
// so the log is never held whole. Records seen at the same time as the last one
// kept are kept too, so the cursor's tie order on the bead id never skips one.
// When it stops short of the log's end, cut is the seen time it stopped at, and
// deferred names the waiting beads whose record lies past it: their record is
// asked on a later pass, so listing the bead now would ask it twice.
//
// The log is read without its lock, as ReadRecentSubmissionRecords reads it: a
// partly written last line does not parse and is skipped. Lines have no length
// cap, so a long record cannot stop the read.
func readBacklog(path string, t time.Time, waitingKeys map[string]bool, limit int) (recs []land.SubmissionRecord, deferred map[string]bool, cut time.Time, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, time.Time{}, nil
	}
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	deferred = map[string]bool{}
	taken := map[string]bool{}
	var last time.Time
	r := bufio.NewReader(f)
	for {
		line, rerr := r.ReadBytes('\n')
		var rec land.SubmissionRecord
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &rec) == nil {
			key := land.SubmissionKey(rec.Bead, rec.Head)
			switch {
			case rec.SeenAt.Before(t):
				// Only a waiting bead's old record matters, to the dedupe.
				if waitingKeys[key] {
					recs = append(recs, rec)
				}
			case taken[key]:
			case len(taken) < limit || rec.SeenAt.Equal(last):
				taken[key] = true
				last = rec.SeenAt
				recs = append(recs, rec)
			default:
				cut = last
				if waitingKeys[key] {
					deferred[key] = true
				}
			}
		}
		if errors.Is(rerr, io.EOF) {
			return recs, deferred, cut, nil
		}
		if rerr != nil {
			return nil, nil, time.Time{}, fmt.Errorf("reading %s: %w", path, rerr)
		}
	}
}

func (r rigBeads) warnf(format string, args ...any) {
	if r.warnings != nil {
		fmt.Fprintf(r.warnings, format, args...)
	}
}

// gitRunner runs one git command in dir and returns its standard output. The
// pass only ever hands it its own clone, never a town worktree; tests record
// the dir to prove it.
type gitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// repoEnvVars are the variables that point git at a repository other than the
// one -C names. A caller running inside a hook or a worktree session can carry
// them, and they would send the pass's commands into that repository.
var repoEnvVars = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_COMMON_DIR":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_NAMESPACE":                    true,
	"GIT_PREFIX":                       true,
}

// gitEnv is environ without the variables that pick a repository. The rest,
// such as the ssh command or credential helpers a fetch needs, pass through.
func gitEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if repoEnvVars[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// runGit is the production gitRunner. It runs git without the caller's
// repository-selecting environment.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitEnv(ctx, os.Environ(), dir, args...)
}

// runGitEnv runs git in dir with environ as the caller's environment.
func runGitEnv(ctx context.Context, environ []string, dir string, args ...string) (string, error) {
	return runGitInput(ctx, environ, dir, "", args...)
}

// runGitInput is runGitEnv with input on git's standard input.
func runGitInput(ctx context.Context, environ []string, dir, input string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(environ)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), dir, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// bareGit is the pass's own bare clone. Every command runs -C dir, so the
// pass never touches the rig's repository or any worktree cut from it.
type bareGit struct {
	dir     string
	origin  string
	timeout time.Duration
	run     gitRunner
}

// newBareGit is the production clone: dir is the clone path and origin is the
// URL it is created from.
func newBareGit(dir, origin string) *bareGit {
	return &bareGit{dir: dir, origin: origin, timeout: gitTimeout, run: runGit}
}

// Sync creates the clone when it is missing and fetches refs/heads/* into it,
// force-updating every branch so a rebased or force-pushed submission reads as
// it is now.
func (g *bareGit) Sync(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return fmt.Errorf("creating the clone directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "HEAD")); errors.Is(err, fs.ErrNotExist) {
		if _, err := g.run(ctx, g.dir, "init", "--bare", "--quiet"); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("reading the clone: %w", err)
	}

	cur, err := g.run(ctx, g.dir, "config", "--get", "remote.origin.url")
	cur = strings.TrimSpace(cur)
	switch {
	case err != nil || cur == "":
		if _, err := g.run(ctx, g.dir, "remote", "add", "origin", g.origin); err != nil {
			return err
		}
	case cur != g.origin:
		if _, err := g.run(ctx, g.dir, "remote", "set-url", "origin", g.origin); err != nil {
			return err
		}
	}
	if _, err := g.run(ctx, g.dir, "fetch", "--prune", "--quiet", "origin", "+refs/heads/*:refs/heads/*"); err != nil {
		return err
	}
	return nil
}

// CommitSubjects returns the subjects on target..head, newest first, at most
// limit. A ref that starts with "-" is refused before git runs, so a READY TO
// LAND block cannot pass git an option.
func (g *bareGit) CommitSubjects(ctx context.Context, target, head string, limit int) ([]string, error) {
	if err := checkRefs(target, head); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	args := []string{"log", "--format=%s"}
	if limit > 0 {
		args = append(args, fmt.Sprintf("-n%d", limit))
	}
	args = append(args, target+".."+head)
	out, err := g.run(ctx, g.dir, args...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n"), nil
}

// Numstat returns `git diff --numstat target..head`, refusing a ref that
// starts with "-" for the same reason as CommitSubjects.
func (g *bareGit) Numstat(ctx context.Context, target, head string) (string, error) {
	if err := checkRefs(target, head); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	out, err := g.run(ctx, g.dir, "diff", "--numstat", "--no-renames", target+".."+head)
	if err != nil {
		return "", err
	}
	return out, nil
}

// checkRefs refuses a ref git would read as an option.
func checkRefs(refs ...string) error {
	for _, ref := range refs {
		if strings.HasPrefix(ref, "-") {
			return fmt.Errorf("refusing %q: a ref may not start with -", ref)
		}
	}
	return nil
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker       = (*jev.Client)(nil)
	_ Submissions = rigBeads{}
	_ Git         = (*bareGit)(nil)
	_ Git         = lockedGit{}
)
