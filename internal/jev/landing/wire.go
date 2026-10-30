package landing

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

// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap is
// town-wide: it counts every Jev caller in the usage ledger, not this job alone.
const DefaultMaxUSDPerDay = 1.0

// gitTimeout bounds one git diff.
const gitTimeout = time.Minute

// Settings is the operator's configuration for the pass, read from
// patrols.jev_landing.
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
// usage ledger under the Caller name, the rig's landings file, its beads and
// its git repo, and the cap read from the same ledger. warnings receives the
// ledger's own warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: s.USDPerBillionTokens}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_landing: %w", err)
	}

	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, s.Scrub)...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	beadsDir := doltserver.FindRigBeadsDir(townRoot, RigName)
	if beadsDir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", RigName)
	}
	landings, err := land.RigLandingsFile(townRoot, RigName)
	if err != nil {
		return nil, err
	}
	repo := rigRepo(filepath.Join(townRoot, RigName))
	if repo == "" {
		return nil, fmt.Errorf("rig %s has no git repo (neither .repo.git nor mayor/rig)", RigName)
	}

	return &Pass{
		Asker:      client,
		Landings:   landings,
		Beads:      beads.New(beadsDir),
		Git:        gitDiffer{repo: repo},
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		Allows:     func() (bool, error) { return town.Allows(time.Now) },
	}, nil
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

// Numstat runs `git -C repo diff --numstat --no-renames base..head`. A ref that
// starts with "-" is refused before git runs, so a landing record cannot pass
// git an option.
func (g gitDiffer) Numstat(ctx context.Context, base, head string) (string, error) {
	if strings.HasPrefix(base, "-") || strings.HasPrefix(head, "-") {
		return "", fmt.Errorf("refusing diff of refs %q..%q: a ref may not start with -", base, head)
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", g.repo,
		"diff", "--numstat", "--no-renames", base+".."+head).Output()
	if err != nil {
		return "", fmt.Errorf("git diff %s..%s: %w", base, head, err)
	}
	return string(out), nil
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker    = (*jev.Client)(nil)
	_ Landings = (*land.LandingsFile)(nil)
	_ Beads    = (*beads.Beads)(nil)
	_ Differ   = gitDiffer{}
)
