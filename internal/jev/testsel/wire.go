package testsel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

// gitTimeout bounds one git diff.
const gitTimeout = time.Minute

// listTimeout bounds one go list of the tree.
const listTimeout = 5 * time.Minute

// Settings is the operator's configuration for the battery, read from
// patrols.jev_testsel.
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

// LogPath is the test-selection log, one JSON line per landing.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "testsel.jsonl")
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the log, and the cap read from the same
// ledger. warnings receives the ledger's own warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: s.USDPerBillionTokens}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_testsel: %w", err)
	}
	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, s.Scrub)...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}
	return &Pass{
		Asker:   client,
		LogPath: LogPath(townRoot),
		Allows:  func() (bool, error) { return town.Allows(time.Now) },
	}, nil
}

// gitTree is the production tree reader.
type gitTree struct{}

// Read reads one landing's tree: the diff from base to head, the changed
// packages, and the Go package list with each package's in-module imports.
// dir is the tree the gate runs on, so the reads are local to the landing's
// work clone and never touch the rig's own checkout.
func (gitTree) Read(ctx context.Context, dir, base, head string) (treeState, error) {
	if strings.HasPrefix(base, "-") || strings.HasPrefix(head, "-") {
		return treeState{}, fmt.Errorf("refusing diff of refs %q..%q: a ref may not start with -", base, head)
	}
	nameStatus, err := gitOut(ctx, dir, land.NameStatusArgs(base, head)...)
	if err != nil {
		return treeState{}, err
	}
	numstat, err := gitOut(ctx, dir, "diff", "--numstat", "--no-renames", base, head)
	if err != nil {
		return treeState{}, err
	}
	pkgs, err := listPackages(ctx, dir)
	if err != nil {
		return treeState{}, err
	}
	changed, err := land.ChangedPackages(nameStatus, func(d string) bool { return land.PackageDirHasGo(dir, d) })
	if err != nil {
		return treeState{}, err
	}
	treeWide, err := anyTreeWide(nameStatus)
	if err != nil {
		return treeState{}, err
	}
	files, stat := parseNumstat(numstat)
	return treeState{
		pkgs:     pkgs,
		changed:  changed,
		treeWide: treeWide,
		stat:     stat,
		files:    files,
	}, nil
}

// gitOut runs git in dir and returns its stdout.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// listPackages returns the main module's packages in dir, each with the
// main-module packages it imports directly. Dirs are relative to dir, and the
// root package is ".". Dependencies outside the main module are left out.
func listPackages(ctx context.Context, dir string) ([]Package, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-json", "./...")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var listed []goListPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p goListPackage
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("go list: decoding the package list: %w", err)
		}
		if p.Module != nil && p.Module.Main {
			listed = append(listed, p)
		}
	}
	return packagesOf(listed), nil
}

// goListPackage is the part of one `go list -json` object the battery reads.
// go list reports the test imports apart from the build's, so they are read
// separately and merged in packagesOf.
type goListPackage struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Module       *goListModule
}

// goListModule is the module a listed package belongs to.
type goListModule struct {
	Path string
	Main bool
}

// packagesOf turns go list's main-module packages into Packages, with dirs
// relative to the module root and in-module imports named by dir. A package's
// test imports count as its imports: a change to a package its tests import
// can change what those tests verify.
func packagesOf(listed []goListPackage) []Package {
	if len(listed) == 0 {
		return nil
	}
	modPath := listed[0].Module.Path
	dirOf := func(importPath string) (string, bool) {
		if importPath == modPath {
			return ".", true
		}
		if rest, ok := strings.CutPrefix(importPath, modPath+"/"); ok {
			return rest, true
		}
		return "", false
	}
	pkgs := make([]Package, 0, len(listed))
	for _, p := range listed {
		dir, _ := dirOf(p.ImportPath)
		var imports []string
		seen := make(map[string]bool)
		for _, list := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
			for _, imp := range list {
				if d, ok := dirOf(imp); ok && !seen[d] {
					seen[d] = true
					imports = append(imports, d)
				}
			}
		}
		pkgs = append(pkgs, Package{Dir: dir, ImportPath: p.ImportPath, Imports: imports})
	}
	return pkgs
}

// parseNumstat reads `git diff --numstat` output into the changed files, at
// most maxFiles, and the diff's one-line stat over every file.
func parseNumstat(out string) ([]fileChange, string) {
	var files []fileChange
	var added, removed int
	count := 0
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		// A binary file reports "-" for both counts, which Atoi fails on and
		// leaves 0.
		a, _ := strconv.Atoi(parts[0])
		r, _ := strconv.Atoi(parts[1])
		added += a
		removed += r
		count++
		if len(files) < maxFiles {
			files = append(files, fileChange{Path: parts[2], Added: a, Removed: r})
		}
	}
	stat := fmt.Sprintf("%d files changed, %d insertions(+), %d deletions(-)", count, added, removed)
	return files, stat
}

// anyTreeWide reports whether any path in `git diff --name-status -z` output is
// a tree-wide input. Both sides of a rename or copy count.
func anyTreeWide(nameStatus string) (bool, error) {
	paths, err := land.ChangedPaths(nameStatus)
	if err != nil {
		return false, err
	}
	for _, p := range paths {
		if TreeWideInput(p) {
			return true, nil
		}
	}
	return false, nil
}

// Ensure the production dependencies satisfy the pass's seams.
var _ Asker = (*jev.Client)(nil)
