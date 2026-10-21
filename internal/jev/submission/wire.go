package submission

import (
	"bytes"
	"context"
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

	client, err := jev.NewClient(
		jev.WithLedger(jev.UsageLedgerPath(townRoot)),
		jev.WithCaller(Caller),
		jev.WithWarnings(warnings),
	)
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
		Git:         newBareGit(ClonePath(townRoot), origin),
		LogPath:     LogPath(townRoot),
		CursorPath:  CursorPath(townRoot),
		Allows:      func() (bool, error) { return town.Allows(time.Now) },
		Warnings:    warnings,
	}, nil
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

// rigBeads reads a rig's submissions: the first-sight records the landing
// worker wrote, and the waiting work beads that have none. It never writes.
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
func (r rigBeads) Since(t time.Time) ([]Entry, error) {
	recs, err := land.ReadSubmissionRecords(r.records)
	if err != nil {
		return nil, err
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
			if r.warnings != nil {
				fmt.Fprintf(r.warnings, "jev submission: skipping %s at %s: reading the bead: %v\n", rec.Bead, rec.Head, err)
			}
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

	waiting, err := r.b.List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1, Limit: 0})
	if err != nil {
		return nil, err
	}
	for _, issue := range waiting {
		if issue == nil || !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() {
			continue
		}
		w, err := land.WorkFromBead(issue, RigName)
		if err != nil || recorded[land.SubmissionKey(issue.ID, w.Head)] {
			continue
		}
		out = append(out, Entry{Issue: issue, Work: w, Seen: w.Submitted})
	}
	return out, nil
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
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(environ)
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
)
