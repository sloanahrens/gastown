package targets

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap is
// town-wide: it counts every Jev caller in the usage ledger, not this battery
// alone.
const DefaultMaxUSDPerDay = 1.0

// fileTimeout bounds one file read from the ref.
const fileTimeout = 30 * time.Second

// Settings is the operator's configuration for the run, read from
// patrols.jev_targets.
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
	// Repo is the repository the slice was ranked in. Empty means the current
	// directory.
	Repo string
	// Ref is the ref the slice was ranked at. Empty means HEAD.
	Ref string
}

// Open wires the production run: the Jev client writing to the usage ledger
// under the Caller name, the repository read at Ref, and the cap read from the
// same ledger. warnings receives the ledger's own warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: s.USDPerBillionTokens}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_targets: %w", err)
	}

	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, jev.BatteryScrub{
		Scrub:      s.Scrub,
		ScrubCheck: s.ScrubCheck,
		Allows:     func() (bool, error) { return town.Allows(time.Now) },
	})...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	repo, ref := s.Repo, s.Ref
	if repo == "" {
		repo = "."
	}
	if ref == "" {
		ref = "HEAD"
	}
	return &Pass{
		Asker:   client,
		Source:  newGitSource(repo, ref),
		LogPath: LogPath(townRoot),
		Allows:  func() (bool, error) { return town.Allows(time.Now) },
	}, nil
}

// gitRunner runs one git command in dir and returns its standard output.
type gitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// runGit is the production gitRunner.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), dir, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// gitSource reads a file's text from the ref the slice was ranked at. It runs
// git only in the repository it was given, never in a town worktree it did not
// name.
type gitSource struct {
	repo    string
	ref     string
	timeout time.Duration
	run     gitRunner
}

// newGitSource is the production source.
func newGitSource(repo, ref string) *gitSource {
	return &gitSource{repo: repo, ref: ref, timeout: fileTimeout, run: runGit}
}

// ReadFile returns the file's text at the source's ref.
func (g *gitSource) ReadFile(ctx context.Context, path string) (string, error) {
	if err := checkPath(path); err != nil {
		return "", err
	}
	if strings.HasPrefix(g.ref, "-") {
		return "", fmt.Errorf("refusing ref %q: a ref may not start with -", g.ref)
	}
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.run(ctx, g.repo, "show", g.ref+":"+path)
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker  = (*jev.Client)(nil)
	_ Source = (*gitSource)(nil)
)
