package rework

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

// gitTimeout bounds one numstat diff.
const gitTimeout = time.Minute

// Settings is the operator's configuration for the pass, read from
// patrols.jev_rework.
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

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the town's om review log, the beads and
// repositories of the rigs it names, the log and cursor, and the cap read from
// the same ledger. warnings receives the ledger's own warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: s.USDPerBillionTokens}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_rework: %w", err)
	}

	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, s.Scrub)...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	return &Pass{
		Asker:      client,
		Reviews:    land.TownReviewsFile(townRoot),
		Rigs:       &townRigs{townRoot: townRoot},
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		Allows:     func() (bool, error) { return town.Allows(time.Now) },
		Warnf:      warnTo(warnings),
	}, nil
}

// warnTo writes the pass's warnings to w, one line each, and drops them when
// w is nil.
func warnTo(w io.Writer) func(format string, args ...any) {
	if w == nil {
		return nil
	}
	return func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, "jev_rework: "+format+"\n", args...)
	}
}

// townRigs reads the rig a review log line names: its beads and its git
// repository, one client of each per rig, built on first use.
type townRigs struct {
	townRoot string

	beadsClients map[string]*beads.Beads
	differs      map[string]gitDiffer
}

// Notes reads bead's notes from rig's beads database.
func (t *townRigs) Notes(rig, bead string) (string, error) {
	client, err := t.client(rig)
	if err != nil {
		return "", err
	}
	issue, err := client.Show(bead)
	if err != nil {
		return "", err
	}
	if issue == nil {
		return "", beads.ErrNotFound
	}
	return issue.Notes, nil
}

// Numstat runs the refused submission's diff in rig's repository.
func (t *townRigs) Numstat(ctx context.Context, rig, target, head string) (string, error) {
	diff, err := t.differ(rig)
	if err != nil {
		return "", err
	}
	return diff.Numstat(ctx, target, head)
}

// checkRig refuses a rig name that is not one path element. The name comes from
// the review log and is joined into the town's paths.
func checkRig(rig string) error {
	if rig == "" || strings.ContainsAny(rig, `/\`) || rig == "." || rig == ".." {
		return fmt.Errorf("invalid rig name %q", rig)
	}
	return nil
}

func (t *townRigs) client(rig string) (*beads.Beads, error) {
	if err := checkRig(rig); err != nil {
		return nil, err
	}
	if b, ok := t.beadsClients[rig]; ok {
		return b, nil
	}
	dir := doltserver.FindRigBeadsDir(t.townRoot, rig)
	if dir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", rig)
	}
	if t.beadsClients == nil {
		t.beadsClients = map[string]*beads.Beads{}
	}
	b := beads.New(dir)
	t.beadsClients[rig] = b
	return b, nil
}

func (t *townRigs) differ(rig string) (gitDiffer, error) {
	if err := checkRig(rig); err != nil {
		return gitDiffer{}, err
	}
	if d, ok := t.differs[rig]; ok {
		return d, nil
	}
	repo := rigRepo(filepath.Join(t.townRoot, rig))
	if repo == "" {
		return gitDiffer{}, fmt.Errorf("rig %s has no git repo (neither .repo.git nor mayor/rig)", rig)
	}
	if t.differs == nil {
		t.differs = map[string]gitDiffer{}
	}
	d := gitDiffer{repo: repo}
	t.differs[rig] = d
	return d, nil
}

// rigRepo is the rig's git repo: the shared bare .repo.git, else mayor/rig.
// It is empty when the rig has neither.
func rigRepo(rigRoot string) string {
	for _, rel := range []string{".repo.git", filepath.Join("mayor", "rig")} {
		dir := filepath.Join(rigRoot, rel)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

// gitDiffer runs git diff in the rig's repo.
type gitDiffer struct {
	repo string
}

// Numstat runs `git -C repo diff --numstat --no-renames target...head`: the
// files the refused head changed against its merge base with target, and no
// hunk text. A ref that starts with "-" is refused before git runs, so a
// rejection note cannot pass git an option.
func (g gitDiffer) Numstat(ctx context.Context, target, head string) (string, error) {
	if strings.HasPrefix(target, "-") || strings.HasPrefix(head, "-") {
		return "", fmt.Errorf("refusing diff of refs %q...%q: a ref may not start with -", target, head)
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", g.repo,
		"diff", "--numstat", "--no-renames", target+"..."+head).Output()
	if err != nil {
		return "", fmt.Errorf("git diff %s...%s: %w", target, head, err)
	}
	return string(out), nil
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker   = (*jev.Client)(nil)
	_ Reviews = (*land.ReviewsFile)(nil)
	_ Rigs    = (*townRigs)(nil)
)
