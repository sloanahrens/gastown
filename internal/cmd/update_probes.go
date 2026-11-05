package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/version"
)

const (
	// updateProbeGCMaxAge is how old a database's last patrol gc may be before
	// GCDue calls a gc due. The bead asks for the daemon's weekly rule without
	// its four-hour slack: older than seven days is due.
	updateProbeGCMaxAge = 7 * 24 * time.Hour

	// updateProbeBackupDateLayout is the dated directory the daemon writes a
	// night's backups into, <root>/<date>/<database>.
	updateProbeBackupDateLayout = "2006-01-02"

	// updateProbeImageRepo is the sandbox image repository the installer
	// publishes to, tagged with the short commit.
	updateProbeImageRepo = "gastown-polecat"

	// updateProbeImageSHALen is how many characters of the commit the image
	// tag carries.
	updateProbeImageSHALen = 9

	// updateProbeGCStateFile is the daemon's per-database maintenance state,
	// relative to the town root.
	updateProbeGCStateFile = "daemon/maintenance_state.json"
)

// updateProbeEnv is the real state the plan probes read and the seams that let
// a test replace it. A nil field takes its production value in
// newUpdateProbes.
type updateProbeEnv struct {
	TownRoot, RepoRoot string
	Run                update.RunFunc // runs git and docker
	Now                func() time.Time
	Stat               func(path string) (os.FileInfo, error)
	ReadFile           func(path string) ([]byte, error)
	Commit             func() string                                                // the running gt's commit
	Databases          func(ctx context.Context) ([]string, error)                  // the Dolt databases
	BackupRoot         func() (string, error)                                       // doltbackup.DefaultRoot
	Archive            func(ctx context.Context) (commits, events int64, err error) // supplied by B10b2
}

// newUpdateProbes returns the plan's probes over env, filling each nil seam
// with the real one. It never fetches and never writes.
func newUpdateProbes(env updateProbeEnv) update.Probes {
	if env.Run == nil {
		env.Run = update.ExecRun
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Stat == nil {
		env.Stat = os.Stat
	}
	if env.ReadFile == nil {
		env.ReadFile = os.ReadFile
	}
	if env.Commit == nil {
		env.Commit = version.BuildCommit
	}
	if env.Databases == nil {
		townRoot := env.TownRoot
		env.Databases = func(context.Context) ([]string, error) {
			return doltserver.ListDatabases(townRoot)
		}
	}
	if env.BackupRoot == nil {
		env.BackupRoot = doltbackup.DefaultRoot
	}
	return &updateProbes{env: env}
}

// updateProbes answers the plan's questions about the host through env.
type updateProbes struct {
	env updateProbeEnv
}

// InstalledSHA is the commit the running gt was built from. An empty answer is
// an error: the plan compares it with main, and a blank would name no build.
func (p *updateProbes) InstalledSHA(context.Context) (string, error) {
	sha := p.env.Commit()
	if strings.TrimSpace(sha) == "" {
		return "", errors.New("the installed build named no commit")
	}
	return sha, nil
}

// MainSHA is the tip of origin's main, read with ls-remote. It never fetches:
// nothing in the town's checkouts fetches except the installer.
func (p *updateProbes) MainSHA(ctx context.Context) (string, error) {
	return update.RemoteMainSHA(ctx, p.env.Run, p.env.RepoRoot)
}

// ArchiveBehind is the archive gap B10b2 supplies: how many commits and events
// the history archive is behind. An unwired archive is an error, never zero,
// because a zero reads as caught up.
func (p *updateProbes) ArchiveBehind(ctx context.Context) (int, int, error) {
	if p.env.Archive == nil {
		return 0, 0, errors.New("the archive gap probe is not wired")
	}
	commits, events, err := p.env.Archive(ctx)
	if err != nil {
		return 0, 0, err
	}
	return int(commits), int(events), nil
}

// BackupToday reports whether today's dated backup directory exists and holds
// one subdirectory for every database. A missing directory or database is
// false, not an error; a stat that fails for another reason is an error, so an
// unreadable backup is never read as present.
func (p *updateProbes) BackupToday(ctx context.Context) (bool, error) {
	root, err := p.env.BackupRoot()
	if err != nil {
		return false, fmt.Errorf("reading the Dolt backup root: %w", err)
	}
	databases, err := p.env.Databases(ctx)
	if err != nil {
		return false, fmt.Errorf("listing the Dolt databases: %w", err)
	}
	dir := filepath.Join(root, p.env.Now().Format(updateProbeBackupDateLayout))
	present, err := p.isDirectory(dir)
	if err != nil {
		return false, fmt.Errorf("reading today's backup directory %s: %w", dir, err)
	}
	if !present {
		return false, nil
	}
	for _, db := range databases {
		present, err := p.isDirectory(filepath.Join(dir, db))
		if err != nil {
			return false, fmt.Errorf("reading database %q in today's backup: %w", db, err)
		}
		if !present {
			return false, nil
		}
	}
	return true, nil
}

// isDirectory reports whether path is a directory. A path that does not exist
// (or is not a directory) is false with no error.
func (p *updateProbes) isDirectory(path string) (bool, error) {
	info, err := p.env.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}

// updateGCState is the part of daemon/maintenance_state.json the gc probe
// reads: per database, when its last patrol gc ran.
type updateGCState struct {
	LastGC map[string]time.Time `json:"last_gc"`
}

// GCDue reports whether any database has never been gc'd or was last gc'd more
// than seven days ago, naming the first such database and its age. A missing or
// unreadable state file is an error: it says nothing about gc, and reading it as
// "not due" would skip the maintenance work the plan exists to schedule.
func (p *updateProbes) GCDue(ctx context.Context) (bool, string, error) {
	databases, err := p.env.Databases(ctx)
	if err != nil {
		return false, "", fmt.Errorf("listing the Dolt databases: %w", err)
	}
	path := filepath.Join(p.env.TownRoot, updateProbeGCStateFile)
	data, err := p.env.ReadFile(path)
	if err != nil {
		return false, "", fmt.Errorf("reading the gc state %s: %w", path, err)
	}
	var st updateGCState
	if err := json.Unmarshal(data, &st); err != nil {
		return false, "", fmt.Errorf("parsing the gc state %s: %w", path, err)
	}
	now := p.env.Now()
	for _, db := range databases {
		last := st.LastGC[db]
		if last.IsZero() {
			return true, fmt.Sprintf("database %s has no recorded gc", db), nil
		}
		age := now.Sub(last)
		if age > updateProbeGCMaxAge {
			return true, fmt.Sprintf("database %s was last gc %v ago", db, age.Round(time.Hour)), nil
		}
	}
	return false, "", nil
}

// ImagePublished reports whether the sandbox image for sha exists in the local
// Docker engine. Exit 0 is present, exit 1 is absent, anything else is an
// error. It says nothing about the registry: publishing is the installer's job.
func (p *updateProbes) ImagePublished(ctx context.Context, sha string) (bool, error) {
	image := updateProbeImageRepo + ":" + shortUpdateSHA(sha)
	_, exit, err := p.env.Run(ctx, "", nil, "docker", "image", "inspect", image)
	if err != nil {
		return false, fmt.Errorf("running docker image inspect %s: %w", image, err)
	}
	switch exit {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("docker image inspect %s exited %d", image, exit)
	}
}

// shortUpdateSHA is the first updateProbeImageSHALen characters of sha, or the
// whole sha when it is shorter. The image tag carries the short commit.
func shortUpdateSHA(sha string) string {
	if len(sha) <= updateProbeImageSHALen {
		return sha
	}
	return sha[:updateProbeImageSHALen]
}
