package notes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/submission"
)

// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap is
// town-wide: it counts every Jev caller in the usage ledger, not this job alone.
const DefaultMaxUSDPerDay = 1.0

// helpTimeout bounds one --help run.
const helpTimeout = 20 * time.Second

// mainRef is the ref the label reads the repo tree at.
const mainRef = "refs/heads/main"

// Settings is the operator's configuration for the pass, read from
// patrols.jev_notes.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the submission battery's bare clone, the
// installed binaries' --help and the cap read from the same ledger. The files
// are the memory directory's markdown and the operator handbook, both under the
// operator's home. warnings receives the ledger's own warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: s.USDPerBillionTokens}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_notes: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("finding the home directory: %w", err)
	}

	client, err := jev.NewClient(
		jev.WithLedger(jev.UsageLedgerPath(townRoot)),
		jev.WithCaller(Caller),
		jev.WithWarnings(warnings),
	)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}
	clone, err := submission.OpenClone(townRoot)
	if err != nil {
		return nil, err
	}

	return &Pass{
		Asker:   client,
		Repo:    cloneRepo{c: clone},
		Help:    execHelp{},
		Files:   func() ([]File, error) { return ListFiles(home, townRoot) },
		LogPath: LogPath(townRoot),
		Allows:  func() (bool, error) { return town.Allows(time.Now) },
	}, nil
}

// nonAlnumRe matches the characters Claude Code replaces with "-" in a project
// directory's name.
var nonAlnumRe = regexp.MustCompile(`[^A-Za-z0-9]`)

// MemoryDir is the memory directory of the sessions run from townRoot, under
// the home directory home.
func MemoryDir(home, townRoot string) string {
	return filepath.Join(home, ".claude", "projects", nonAlnumRe.ReplaceAllString(townRoot, "-"), "memory")
}

// HandbookPath is the operator handbook under the home directory home.
func HandbookPath(home string) string {
	return filepath.Join(home, ".claude", "commands", "gastown.md")
}

// ListFiles lists the markdown files in the memory directory and the handbook.
// A missing directory or handbook is no files, not an error.
func ListFiles(home, townRoot string) ([]File, error) {
	var files []File
	dir := MemoryDir(home, townRoot)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".md") {
			files = append(files, File{Source: SourceMemory, Name: e.Name(), Path: filepath.Join(dir, e.Name())})
		}
	}
	handbook := HandbookPath(home)
	if info, err := os.Stat(handbook); err == nil && info.Mode().IsRegular() {
		files = append(files, File{Source: SourceHandbook, Name: filepath.Base(handbook), Path: handbook})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].key() < files[j].key() })
	return files, nil
}

// gitClone is the part of the submission battery's bare clone the pass reads
// through. *submission.Clone satisfies it.
type gitClone interface {
	Sync(ctx context.Context) error
	// Run returns the command's error wrapped so errors.As reaches the exit
	// status it carries; submission.Clone wraps it with %w. HasKey reads git
	// grep's "no match" exit from it.
	Run(ctx context.Context, args ...string) (string, error)
}

// exitCoder is the exit status of a failed command. An error that reports one,
// such as the *exec.ExitError Clone.Run wraps, satisfies it.
type exitCoder interface {
	ExitCode() int
}

// exitCode returns a failed command's exit status, read from the error chain,
// and whether the error carries one.
func exitCode(err error) (int, bool) {
	var exit exitCoder
	if errors.As(err, &exit) {
		return exit.ExitCode(), true
	}
	return 0, false
}

// cloneRepo reads the rig's main tree from the submission battery's bare clone.
type cloneRepo struct {
	c gitClone
}

// Sync fetches the clone's branches.
func (r cloneRepo) Sync(ctx context.Context) error {
	return r.c.Sync(ctx)
}

// Tree lists every file at the head of main. -z keeps each path verbatim.
func (r cloneRepo) Tree(ctx context.Context) ([]string, error) {
	out, err := r.c.Run(ctx, "ls-tree", "-r", "-z", "--name-only", mainRef)
	if err != nil {
		return nil, err
	}
	return strings.Split(out, "\x00"), nil
}

// HasKey runs git grep for the key as a quoted name, with or without a JSON tag
// option after it, in the Go sources at main. key is a segment of a config key,
// so it holds only lowercase letters, digits and underscores.
func (r cloneRepo) HasKey(ctx context.Context, key string) (bool, error) {
	if !segmentRe.MatchString(key) {
		return false, fmt.Errorf("refused config key %q", key)
	}
	_, err := r.c.Run(ctx, "grep", "-q", "-E", "-e", `"`+key+`(,[A-Za-z0-9]+)*"`, mainRef, "--", "*.go")
	if code, ok := exitCode(err); ok && code == 1 {
		return false, nil
	}
	return err == nil, err
}

// execHelp runs the installed binary with --help.
type execHelp struct{}

// Help runs bin on the PATH with path and --help, and returns what it printed.
// A binary prints its usage to stderr and exits non-zero when it does not know
// --help, so any output counts; no output with a failure is an error.
func (execHelp) Help(ctx context.Context, bin string, path ...string) (string, error) {
	exe, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoBinary, bin)
	}
	ctx, cancel := context.WithTimeout(ctx, helpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, append(append([]string{}, path...), "--help")...)
	cmd.Dir = os.TempDir()
	out, err := cmd.CombinedOutput()
	if len(strings.TrimSpace(string(out))) == 0 && err != nil {
		return "", fmt.Errorf("%s --help: %w", bin, err)
	}
	return string(out), nil
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker  = (*jev.Client)(nil)
	_ Repo   = cloneRepo{}
	_ Helper = execHelp{}
)
