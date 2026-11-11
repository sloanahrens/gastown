package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// JsonlBackupOriginCheck reports an offsite JSONL backup repository that exists
// but has no origin remote. The jsonl_git_backup patrol pushes its exports to
// origin, so without one every cycle exports and then fails to push. The repo is
// resolved by daemon.JsonlBackupRepo, the function the patrol itself uses.
type JsonlBackupOriginCheck struct {
	BaseCheck
}

// NewJsonlBackupOriginCheck creates the backup-origin check.
func NewJsonlBackupOriginCheck() *JsonlBackupOriginCheck {
	return &JsonlBackupOriginCheck{
		BaseCheck: BaseCheck{
			CheckName:        "jsonl-backup-origin",
			CheckDescription: "Check that the offsite JSONL backup repo has an origin remote",
			CheckCategory:    CategoryCore,
		},
	}
}

// Run reports a backup repo that exists without an origin. An absent repo is not
// a finding: the patrol initializes it on its first cycle.
func (c *JsonlBackupOriginCheck) Run(ctx *CheckContext) *CheckResult {
	cfg, err := daemon.ReadPatrolConfig(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not read the daemon patrol config",
			Details: []string{err.Error()},
		}
	}
	repo, _ := daemon.JsonlBackupRepo(ctx.TownRoot, cfg)

	// The same existence test the patrol makes before it initializes the repo.
	if _, err := os.Stat(filepath.Join(repo, ".git")); errors.Is(err, fs.ErrNotExist) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("No offsite backup repo at %s yet (the jsonl_git_backup patrol creates it)", repo),
		}
	} else if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: fmt.Sprintf("unknown: could not check offsite backup repo %s", repo),
			Details: []string{err.Error()},
		}
	}

	// Remotes, not RemoteURL: a failed listing says nothing about origin, and must
	// not be read as a missing one.
	remotes, err := ctx.git(repo).Remotes()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: fmt.Sprintf("unknown: could not list remotes of offsite backup repo %s", repo),
			Details: []string{err.Error()},
		}
	}
	if slices.Contains(remotes, "origin") {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("Offsite backup repo %s has an origin", repo),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Offsite backup repo %s has no 'origin' remote", repo),
		Details: []string{
			"The jsonl_git_backup patrol pushes its exports to origin.",
			"Without one, every cycle exports and then fails to push offsite.",
		},
		FixHint: fmt.Sprintf("Add an origin to %s: git -C %s remote add origin <url>", repo, config.ShellQuote(repo)),
	}
}
