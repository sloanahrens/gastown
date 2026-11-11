package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	gtgit "github.com/steveyegge/gastown/internal/git"
)

// The town_state_backup patrol copies the town's configuration into the JSONL
// backup repository under town-state/. Configuration that exists nowhere else
// (the pod's claim is reclaimed with policy Delete) is lost with the claim
// unless a copy sits off the pod.

const (
	defaultTownStateBackupInterval = time.Hour
	townStateBackupDir             = "town-state"
	alertKeyTownStateBackup        = "town_state_backup:sync"

	// townStateIdentityFile must be present for a snapshot to be taken: a town
	// root without it is not a town to copy, and committing it would record an
	// empty town as the latest state.
	townStateIdentityFile = "mayor/town.json"
)

// townStateExport is one row of the export table. Exactly one field is set.
type townStateExport struct {
	// Path is one file, relative to the town root.
	Path string
	// Dir is every file directly inside one directory, relative to the town root.
	Dir string
	// RigPath is one file relative to each rig's directory.
	RigPath string
}

// townStateExports is the table of what the backup copies. A row is the whole
// contract: a file outside it is not copied.
var townStateExports = []townStateExport{
	{Path: townStateIdentityFile},
	{Path: "settings/config.json"},
	{Path: "settings/models.json"},
	{Path: "settings/placement.json"},
	{Path: "settings/regime-events.json"},
	{Dir: "settings/themes"},
	{RigPath: "settings/config.json"},
	{RigPath: ".beads/config.yaml"},
}

// The secret shapes, matched against a file's base name, lowercased, in every
// directory the walk reads: a name ending in a suffix (*.env, *.key) or holding
// a part (*token*). A file that matches is refused even when an export row names
// it, so a secret added to a copied directory later is refused by its shape, not
// by a name someone remembered to list.
var (
	townStateSecretSuffixes = []string{".env", ".key"}
	townStateSecretParts    = []string{"token"}
)

// townStateSnapshot is what one tick reads from the town.
type townStateSnapshot struct {
	// Files maps each exported file, relative to the town root with forward
	// slashes, to its content.
	Files map[string][]byte
	// Refused lists the secret-looking files the walk found, by the same paths.
	Refused []string
}

// townStateBackupInterval returns the configured interval, or the hourly default.
func townStateBackupInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.TownStateBackup != nil {
		if d, err := time.ParseDuration(config.Patrols.TownStateBackup.IntervalStr); err == nil && d > 0 {
			return d
		}
	}
	return defaultTownStateBackupInterval
}

// townStateSecret reports whether a base name has a secret shape.
func townStateSecret(base string) bool {
	lower := strings.ToLower(base)
	for _, suffix := range townStateSecretSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	for _, part := range townStateSecretParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

// townStateExported reports whether a town-relative file path is a row of the
// export table, for the rigs given.
func townStateExported(name string, rigs []string) bool {
	for _, ex := range townStateExports {
		switch {
		case ex.Path != "":
			if name == ex.Path {
				return true
			}
		case ex.Dir != "":
			if path.Dir(name) == ex.Dir {
				return true
			}
		default:
			for _, rig := range rigs {
				if name == path.Join(rig, ex.RigPath) {
					return true
				}
			}
		}
	}
	return false
}

// townStateScanDirs lists the directories the walk reads for the rigs given:
// each row's directory, once.
func townStateScanDirs(rigs []string) []string {
	seen := map[string]bool{}
	for _, ex := range townStateExports {
		switch {
		case ex.Path != "":
			seen[path.Dir(ex.Path)] = true
		case ex.Dir != "":
			seen[ex.Dir] = true
		default:
			for _, rig := range rigs {
				seen[path.Join(rig, path.Dir(ex.RigPath))] = true
			}
		}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// townStateNoSymlink refuses name when any component of it is a symbolic link.
// The export reads the town's own files: a link under the town root can point
// anywhere on the pod, and reading through it would back up a file the town
// does not hold. Each component is checked with Lstat, so a link in a directory
// on the way to a file is caught as well as a link at the file itself. A
// missing component returns its fs.ErrNotExist.
func townStateNoSymlink(fsys fs.FS, name string) error {
	if name == "." {
		return nil
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		info, err := fs.Lstat(fsys, prefix)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; the backup does not follow links out of the town", prefix)
		}
	}
	return nil
}

// collectTownState reads the town through fsys: every file the export table
// names, and every secret-looking file in the directories the table reads. A
// directory that does not exist holds nothing; any other read error fails the
// whole snapshot, so a tick never reports a clean backup of a town it did not
// fully read.
func collectTownState(fsys fs.FS, rigs []string) (townStateSnapshot, error) {
	snap := townStateSnapshot{Files: map[string][]byte{}}
	for _, dir := range townStateScanDirs(rigs) {
		if err := townStateNoSymlink(fsys, dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return townStateSnapshot{}, err
		}
		entries, err := fs.ReadDir(fsys, dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return townStateSnapshot{}, fmt.Errorf("reading %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := path.Join(dir, e.Name())
			if townStateSecret(e.Name()) {
				snap.Refused = append(snap.Refused, name)
				continue
			}
			if !townStateExported(name, rigs) {
				continue
			}
			if err := townStateNoSymlink(fsys, name); err != nil {
				return townStateSnapshot{}, err
			}
			data, err := fs.ReadFile(fsys, name)
			if err != nil {
				return townStateSnapshot{}, fmt.Errorf("reading %s: %w", name, err)
			}
			snap.Files[name] = data
		}
	}
	if _, ok := snap.Files[townStateIdentityFile]; !ok {
		return townStateSnapshot{}, fmt.Errorf("%s is not on the town root; refusing to back up a partial town", townStateIdentityFile)
	}
	sort.Strings(snap.Refused)
	return snap, nil
}

// townStateFileSystem is the town's files: the test seam when one is set, else
// the town root on disk.
func (d *Daemon) townStateFileSystem() fs.FS {
	if d.townStateFS != nil {
		return d.townStateFS(d.config.TownRoot)
	}
	return os.DirFS(d.config.TownRoot)
}

// townStateRigNames is the rig registry's names. A registry that exists but
// does not load is an error, not zero rigs: zero rigs would back up no rig's
// configuration and report the tick clean. A name is a path under the backup
// repository, so one that is not a single directory name is refused too.
func (d *Daemon) townStateRigNames() ([]string, error) {
	if d.townStateRigs != nil {
		return d.townStateRigs(d.config.TownRoot), nil
	}
	parsed, err := config.LoadRigsConfigOrEmpty(constants.MayorRigsPath(d.config.TownRoot))
	if err != nil {
		return nil, fmt.Errorf("reading the rig registry: %w", err)
	}
	names := make([]string, 0, len(parsed.Rigs))
	for name := range parsed.Rigs {
		if !fs.ValidPath(name) || name == "." || strings.Contains(name, "/") {
			return nil, fmt.Errorf("rig registry names %q, which is not a single directory name under the town root", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// triggerTownStateBackup runs a town_state_backup cycle when the patrol is due,
// on its own goroutine. Due-ness comes from the persisted last run, as for
// jsonl_git_backup (gt-ima2, gt-gxpwc).
func (d *Daemon) triggerTownStateBackup() {
	if !d.isPatrolActive("town_state_backup") {
		return
	}

	dec := evaluatePatrolDue(d.config.TownRoot, "town_state_backup", d.townStateBackupRun.last(), time.Now(), townStateBackupInterval(d.patrolConfig))
	if !dec.due {
		return
	}

	if !d.townStateBackupRunning.CompareAndSwap(false, true) {
		d.logger.Printf("town_state_backup: previous cycle still running — skipping this check")
		return
	}

	if dec.warn != "" {
		d.logger.Printf("town_state_backup: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("town_state_backup: due — %s", dec.note)
	}

	go func() {
		defer d.townStateBackupRunning.Store(false)
		d.syncTownStateBackup()
	}()
}

// syncTownStateBackup copies the town's configuration into the backup
// repository, commits it when something changed, and pushes. It holds
// jsonlGitBackupRunning while it stages, so the two patrols never write the
// repository's index at once: a jsonl cycle would otherwise sweep the staged
// files into its own commit.
func (d *Daemon) syncTownStateBackup() {
	if !d.isPatrolActive("town_state_backup") {
		return
	}
	if !d.jsonlGitBackupRunning.CompareAndSwap(false, true) {
		d.logger.Printf("town_state_backup: jsonl_git_backup cycle in flight — retrying at the next check")
		return
	}
	defer d.jsonlGitBackupRunning.Store(false)

	// Record that a cycle ran, whatever it found: a failure is raised as an
	// alert, and the next check waits a full interval rather than retrying
	// each tick.
	now := time.Now()
	defer d.recordPatrolRun(&d.townStateBackupRun, "town_state_backup", now)

	gitRepo, _ := JsonlBackupRepo(d.config.TownRoot, d.patrolConfig)
	if err := d.runTownStateBackup(gitRepo); err != nil {
		d.logger.Printf("town_state_backup: %v", err)
		d.escalateAlert(alertKeyTownStateBackup, "town_state_backup", fmt.Sprintf("the town's configuration is not backed up to %s: %v", gitRepo, err))
		return
	}
	d.clearAlerts("town_state_backup ran clean", alertKeyTownStateBackup)
}

// runTownStateBackup is one cycle's work: read the town, write its snapshot
// under town-state/, commit what changed, push.
func (d *Daemon) runTownStateBackup(gitRepo string) error {
	rigs, err := d.townStateRigNames()
	if err != nil {
		return err
	}
	snap, err := collectTownState(d.townStateFileSystem(), rigs)
	if err != nil {
		return err
	}
	if len(snap.Refused) > 0 {
		d.logger.Printf("town_state_backup: refused %d secret-looking file(s) by pattern: %s", len(snap.Refused), strings.Join(snap.Refused, ", "))
	}

	if err := d.ensureGitRepoInitialized(gitRepo); err != nil {
		return err
	}
	// The snapshot is written beside the jsonl export, never over it. Files are
	// only ever written: a file removed from the town stays in the backup, since
	// a backup must not drop its only copy because the source went missing.
	for name, data := range snap.Files {
		dst := filepath.Join(gitRepo, townStateBackupDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", dst, err)
		}
	}

	localDeadline := time.Now().Add(d.jsonlLocalPhaseBudget())
	remaining := func() time.Duration { return time.Until(localDeadline) }
	if err := d.stageGit(gitRepo, "git add town-state", remaining, func(g daemonGit) error {
		return g.Add("-A", townStateBackupDir)
	}); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	var staged []gtgit.StagedChange
	if err := d.stageGit(gitRepo, "git diff --cached", remaining, func(g daemonGit) error {
		var err error
		staged, err = g.StagedChanges()
		return err
	}); err != nil {
		return fmt.Errorf("git diff --cached: %w", err)
	}

	// Another writer's staged file would ride into this commit under this
	// subject. Refuse, and let the alert name the repository.
	var changed []string
	for _, c := range staged {
		if !strings.HasPrefix(c.Path, townStateBackupDir+"/") {
			return fmt.Errorf("the backup repository's index holds %s outside %s/; refusing to commit it under the town-state subject", c.Path, townStateBackupDir)
		}
		changed = append(changed, strings.TrimPrefix(c.Path, townStateBackupDir+"/"))
	}

	committed := len(changed) > 0
	if committed {
		sort.Strings(changed)
		msg := townStateBackupMessage(changed)
		if err := d.stageGit(gitRepo, "git commit", remaining, func(g daemonGit) error {
			return g.CommitWithAuthor(msg, jsonlBackupAuthor)
		}); err != nil {
			return fmt.Errorf("git commit: %w", err)
		}
		d.logger.Printf("town_state_backup: committed %d changed file(s): %s", len(changed), strings.Join(changed, ", "))
	} else {
		d.logger.Printf("town_state_backup: no changes to commit")
	}

	return d.pushTownStateBackup(gitRepo, committed)
}

// townStateBackupMessage is the commit's subject: the changed files named in
// it, so `git log --oneline` says what a commit carried.
func townStateBackupMessage(changed []string) string {
	return fmt.Sprintf("town-state: back up %d changed file(s): %s", len(changed), strings.Join(changed, ", "))
}

// pushTownStateBackup pushes the backup repository's branch when origin does not
// carry HEAD: this tick's commit, or an earlier tick's commit whose push failed.
func (d *Daemon) pushTownStateBackup(gitRepo string, committed bool) error {
	local := d.backupGitAt(gitRepo, gitCmdTimeout)
	if _, err := local.RemoteURL("origin"); err != nil {
		return fmt.Errorf("backup repo %s has no 'origin' remote; offsite backup cannot push", gitRepo)
	}
	branch, err := local.CurrentBranch()
	if err != nil {
		return fmt.Errorf("reading the backup repo's branch: %w", err)
	}
	if !committed && !d.jsonlBackupAheadOf(gitRepo, "origin", branch) {
		return nil
	}
	if err := d.backupGitAt(gitRepo, gitCmdTimeout).PushWithTimeout("origin", branch, false, gitPushTimeout); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	return nil
}
