package drift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/land"
)

// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap is
// town-wide: it counts every Jev caller in the usage ledger, not this job alone.
const DefaultMaxUSDPerDay = 1.0

// hmTimeout bounds one hm context call. hm is optional, so a slow call costs
// the ranking, not the landing.
const hmTimeout = 30 * time.Second

// Settings is the operator's configuration for the pass, read from
// patrols.jev_drift.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it (gt-aqczg).
	Scrub bool

	// ScrubCheck runs the shadow scrub check on a sample of requests. The
	// scrub_check key sets it (gt-jb20i).
	ScrubCheck bool
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the rig's landings file, the submission
// battery's bare clone, fetched under that clone's fetch lock, and the cap
// read from the same ledger. hm ranks the doc files when it is on the PATH.
// warnings receives the ledger's own warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: s.USDPerBillionTokens}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_drift: %w", err)
	}

	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, jev.BatteryScrub{
		Scrub:      s.Scrub,
		ScrubCheck: s.ScrubCheck,
		Allows:     town.AllowsNow,
	})...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	landings, err := land.RigLandingsFile(townRoot, RigName)
	if err != nil {
		return nil, err
	}
	clone, err := submission.OpenClone(townRoot)
	if err != nil {
		return nil, err
	}

	p := &Pass{
		Asker:      client,
		Landings:   landings,
		Git:        lockedCloneRepo(clone, townRoot),
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		Allows:     town.AllowsNow,
		Warnings:   warnings,
	}
	if _, err := exec.LookPath("hm"); err == nil {
		p.Ranker = hmRanker{}
	}
	return p, nil
}

// gitClone is the part of the submission battery's bare clone the pass reads
// through. *submission.Clone satisfies it; a test can hand in any git dir.
type gitClone interface {
	Sync(ctx context.Context) error
	Run(ctx context.Context, args ...string) (string, error)
}

// lockedCloneRepo is how Open reads the rig: the shared clone, fetched under
// its fetch lock.
func lockedCloneRepo(clone submission.Cloner, townRoot string) cloneRepo {
	return cloneRepo{c: submission.FetchLocked{Cloner: clone, TownRoot: townRoot}}
}

// cloneRepo reads the rig from the submission battery's bare clone.
type cloneRepo struct {
	c gitClone
}

// Sync fetches the clone's branches.
func (r cloneRepo) Sync(ctx context.Context) error {
	return r.c.Sync(ctx)
}

// Numstat runs `git diff --numstat` for base..head in the clone.
func (r cloneRepo) Numstat(ctx context.Context, base, head string) (string, error) {
	if err := checkRefs(base, head); err != nil {
		return "", err
	}
	return r.c.Run(ctx, "diff", "--numstat", "--no-renames", base+".."+head)
}

// Patch runs a zero-context diff of base..head over the files whose changes
// can drift the docs. The pathspec leaves out Markdown and test files.
func (r cloneRepo) Patch(ctx context.Context, base, head string) (string, error) {
	if err := checkRefs(base, head); err != nil {
		return "", err
	}
	return r.c.Run(ctx, "diff", "--no-renames", "--unified=0", base+".."+head,
		"--", ".", ":(exclude)*.md", ":(exclude)*_test.go")
}

// ListDocs lists the Markdown files under docs/ at rev.
func (r cloneRepo) ListDocs(ctx context.Context, rev string) ([]string, error) {
	if err := checkRefs(rev); err != nil {
		return nil, err
	}
	// -z keeps every path verbatim and NUL-terminated. Without it git quotes a
	// path with non-ASCII or control bytes, so the quoted name would not end in
	// .md and the file would drop out of every pass without a trace.
	out, err := r.c.Run(ctx, "ls-tree", "-r", "-z", "--name-only", rev, "--", "docs")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if strings.HasSuffix(p, ".md") {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// ReadDoc returns the file at path at rev.
func (r cloneRepo) ReadDoc(ctx context.Context, rev, path string) (string, error) {
	if err := checkRefs(rev); err != nil {
		return "", err
	}
	if !strings.HasPrefix(path, "docs/") || strings.Contains(path, "..") {
		return "", fmt.Errorf("%w: doc path %q", ErrBadRef, path)
	}
	return r.c.Run(ctx, "show", rev+":"+path)
}

// ErrBadRef means a ref or path the pass would hand to git is not one it reads.
// The same landing names it every time, so the pass skips that landing rather
// than retrying it.
var ErrBadRef = errors.New("refused ref or path")

// refRe matches a branch name or commit id: letters, digits and . _ / + -, not
// starting with -, so git never reads it as an option.
var refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)

// checkRefs refuses a ref outside refRe, and one with "..", which git reads as
// a range.
func checkRefs(refs ...string) error {
	for _, ref := range refs {
		if !refRe.MatchString(ref) || strings.Contains(ref, "..") {
			return fmt.Errorf("%w: %q", ErrBadRef, ref)
		}
	}
	return nil
}

// hmRanker asks `hm context --diff -` which docs the diff's history sits
// nearest to.
type hmRanker struct{}

// docPathRe matches a doc path inside an hm hit's text.
var docPathRe = regexp.MustCompile(`docs/[A-Za-z0-9_./-]+\.md`)

// Closest runs hm context on the patch and returns the doc paths its hits name,
// in the order hm ranked them.
func (hmRanker) Closest(ctx context.Context, patch string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, hmTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "hm", "context", "--diff", "-", "--json")
	cmd.Stdin = strings.NewReader(patch)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("hm context: %w", err)
	}
	return parseDocPaths(out)
}

// parseDocPaths decodes hm context --json and collects the doc paths named in
// each hit's title and body, first mention first.
func parseDocPaths(out []byte) ([]string, error) {
	var raw struct {
		Hits []struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decoding hm context: %w", err)
	}
	seen := map[string]bool{}
	var paths []string
	for _, h := range raw.Hits {
		for _, m := range docPathRe.FindAllString(h.Title+"\n"+h.Body, -1) {
			if !seen[m] {
				seen[m] = true
				paths = append(paths, m)
			}
		}
	}
	return paths, nil
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker    = (*jev.Client)(nil)
	_ Landings = (*land.LandingsFile)(nil)
	_ Repo     = cloneRepo{}
	_ Ranker   = hmRanker{}
)
