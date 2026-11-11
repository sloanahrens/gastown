package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/version"
)

// DirectiveDriftCheck reports where the deployed central directives
// (<town>/directives) differ from the gastown repo's directives/ at the commit
// this binary was installed from, so a hand edit or a missed rollout shows up
// as a finding.
//
// Fix redeploys through config.DeployCentralDirectives, the same write the
// install makes. The deploy also removes a deployed file the commit no longer
// has.
type DirectiveDriftCheck struct {
	BaseCheck

	// buildCommit names the commit the running binary was installed from.
	buildCommit func() string
	// readRepo returns the repo's directives/*.md at commit, name to bytes.
	readRepo func(townRoot, commit string) (map[string][]byte, error)
	// readFile reads one deployed file. A unit test injects a failing one,
	// since a permission bit cannot fail a read for root.
	readFile func(name string) ([]byte, error)
}

// NewDirectiveDriftCheck creates a new check for drift between the deployed
// central directives and the repo at the installed commit.
func NewDirectiveDriftCheck() *DirectiveDriftCheck {
	return &DirectiveDriftCheck{
		BaseCheck: BaseCheck{
			CheckName:        "directive-drift",
			CheckDescription: "Detect deployed central directives that differ from the repo at the installed commit",
			CheckCategory:    CategoryConfig,
		},
		buildCommit: version.BuildCommit,
		readRepo: func(townRoot, commit string) (map[string][]byte, error) {
			return config.ReadCentralDirectives(townRoot, commit, version.BuildCommit)
		},
		readFile: os.ReadFile,
	}
}

// CanFix is true: Fix redeploys the directives from the repo.
func (c *DirectiveDriftCheck) CanFix() bool {
	return true
}

// Fix redeploys the central directives from the installed commit.
func (c *DirectiveDriftCheck) Fix(ctx *CheckContext) error {
	_, err := config.DeployCentralDirectives(ctx.TownRoot, "", c.buildCommit)
	return err
}

// Run compares the deployed copy with the repo at the installed commit.
func (c *DirectiveDriftCheck) Run(ctx *CheckContext) *CheckResult {
	commit := c.buildCommit()
	if commit == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: this build records no commit, so the repo's directives cannot be named",
		}
	}

	repo, err := c.readRepo(ctx.TownRoot, commit)
	if errors.Is(err, config.ErrDirectiveCommitMissing) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Installed commit %s is not in the gastown bare repo; directive drift not checked", commit),
			Details: []string{err.Error()},
			FixHint: "Reinstall from a commit the gastown bare repo holds",
		}
	}
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not read the repo's central directives",
			Details: []string{err.Error()},
		}
	}

	deployed, irregular, err := readDeployedDirectives(ctx.TownRoot, c.readFile)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not read the deployed central directives",
			Details: []string{err.Error()},
		}
	}
	if len(irregular) > 0 {
		details := make([]string, 0, len(irregular))
		for _, name := range irregular {
			details = append(details, fmt.Sprintf("Not a regular file: directives/%s", name))
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d deployed central directive(s) are not regular files", len(irregular)),
			Details: details,
			FixHint: "Run gt doctor fix directive-drift to redeploy them",
		}
	}

	details := compareDirectives(deployed, repo)
	if len(details) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("Deployed central directives match the repo at %s", version.ShortCommit(commit)),
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d deployed central directive(s) differ from the repo at %s", len(details), version.ShortCommit(commit)),
		Details: details,
		FixHint: "Run gt doctor fix directive-drift to redeploy them from the repo",
	}
}

// compareDirectives returns one finding per file whose deployed copy differs
// from the repo's: missing from the deployed copy, extra in it, or holding
// different bytes. Findings are sorted by file name; nil means the two sets
// match.
func compareDirectives(deployed, repo map[string][]byte) []string {
	names := make([]string, 0, len(deployed)+len(repo))
	seen := make(map[string]bool, len(deployed)+len(repo))
	for _, set := range []map[string][]byte{deployed, repo} {
		for name := range set {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)

	var findings []string
	for _, name := range names {
		got, inDeployed := deployed[name]
		want, inRepo := repo[name]
		switch {
		case inRepo && !inDeployed:
			findings = append(findings, fmt.Sprintf("Missing: directives/%s is in the repo but not deployed", name))
		case inDeployed && !inRepo:
			findings = append(findings, fmt.Sprintf("Extra: directives/%s is deployed but the repo does not have it", name))
		case !bytes.Equal(got, want):
			findings = append(findings, fmt.Sprintf("Different: directives/%s differs from the repo's copy", name))
		}
	}
	return findings
}

// readDeployedDirectives reads the deployed <town>/directives/*.md files, name
// to bytes, through readFile. A missing directory holds none. An entry that is
// not a regular file (a symlink, say) is returned by name in irregular, not
// read: its target is not the deployed copy.
func readDeployedDirectives(townRoot string, readFile func(name string) ([]byte, error)) (deployed map[string][]byte, irregular []string, err error) {
	dir := config.CentralDirectiveDir(townRoot)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string][]byte{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	deployed = make(map[string][]byte)
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if !de.Type().IsRegular() {
			irregular = append(irregular, name)
			continue
		}
		data, err := readFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		deployed[name] = data
	}
	return deployed, irregular, nil
}
