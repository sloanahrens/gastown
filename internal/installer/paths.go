package installer

import (
	"errors"
	"os"
	"path/filepath"
)

// Paths are the directories one run reads and writes.
type Paths struct {
	// BinDir is where the installed binary lives.
	BinDir string
	// TownRoot is the town the daemon and the signing material sit under,
	// empty when the town could not be found (a run that names its daemon and
	// checkout itself needs none).
	TownRoot string
	// DaemonDir holds the daemon's state.json, its restart marker and the
	// install receipts.
	DaemonDir string
	// RigDir is the checkout a run fast-forwards to the target and builds in.
	RigDir string
	// Home is the installing user's home (INSTALL_GT_GATEWAY_HOME, else the
	// real one): the gateway renders __HOME__ with it and keeps its config, log
	// and default LaunchAgents directories under it.
	Home string
	// LaunchdDir is INSTALL_GT_LAUNCHD_DIR when set; empty means Home's
	// Library/LaunchAgents.
	LaunchdDir string
	// SelfDir is the running program's own directory, the script's $SCRIPT_DIR
	// and the gateway template's fallback. Empty means the program cannot name
	// it, and then there is no fallback (gt-tpg0p.9).
	SelfDir string
}

// GT is the installed binary a run replaces.
func (p Paths) GT() string { return filepath.Join(p.BinDir, "gt") }

// Prev is where KeepPrevious puts the binary a rollback restores.
func (p Paths) Prev() string { return filepath.Join(p.BinDir, "gt.prev") }

// Marker is the restart marker the daemon reads on its way down.
func (p Paths) Marker() string { return filepath.Join(p.DaemonDir, "restart-pending.json") }

// Receipts is the file a run appends its receipt to.
func (p Paths) Receipts() string { return filepath.Join(p.DaemonDir, "install-receipts.jsonl") }

var errNoTown = errors.New("install-gt: no town found (set GT_TOWN_ROOT); on a machine with no town yet use make install-local")

// ResolvePaths reads the directories the script reads from the environment:
// INSTALL_GT_BIN_DIR, else INSTALL_DIR (the Makefile's name for it), else
// home/.local/bin; the daemon and build checkout from their own variables, else
// from the town root; and the gateway's home and LaunchAgents directory from
// their own, else the installing user's. start is the directory the town root
// is searched above when GT_TOWN_ROOT is empty.
func ResolvePaths(getenv func(string) string, home, start string) (Paths, error) {
	return resolvePaths(getenv, home, start, IsTownRoot)
}

// resolvePaths is ResolvePaths with the town marker check injected, so a test
// reads a tree it built rather than whatever sits above its temporary
// directory.
func resolvePaths(getenv func(string) string, home, start string, isTown func(string) bool) (Paths, error) {
	binDir := getenv("INSTALL_GT_BIN_DIR")
	if binDir == "" {
		binDir = getenv("INSTALL_DIR")
	}
	if binDir == "" {
		binDir = filepath.Join(home, ".local", "bin")
	}
	townRoot := getenv("GT_TOWN_ROOT")
	if townRoot == "" {
		townRoot = TownRootAbove(start, isTown)
	}
	p := Paths{
		BinDir:     binDir,
		TownRoot:   townRoot,
		DaemonDir:  getenv("INSTALL_GT_DAEMON_DIR"),
		RigDir:     getenv("INSTALL_GT_RIG_DIR"),
		Home:       getenv("INSTALL_GT_GATEWAY_HOME"),
		LaunchdDir: getenv("INSTALL_GT_LAUNCHD_DIR"),
	}
	if p.Home == "" {
		p.Home = home
	}
	if p.DaemonDir == "" {
		if townRoot == "" {
			return Paths{}, errNoTown
		}
		p.DaemonDir = filepath.Join(townRoot, "daemon")
	}
	if p.RigDir == "" {
		if townRoot == "" {
			return Paths{}, errNoTown
		}
		p.RigDir = filepath.Join(townRoot, "gastown", "mayor", "rig")
	}
	return p, nil
}

// TownRootAbove returns the outermost ancestor of dir holding the town marker,
// or "" when none does. A rig can carry a mayor/ of its own, so the outermost
// marker is the town (lib/install-gt-lib.sh's igt_town_root_above).
func TownRootAbove(dir string, isTown func(string) bool) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	found := ""
	for {
		if isTown(d) {
			found = d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return found
		}
		d = parent
	}
}

// IsTownRoot reports whether dir holds the town marker: mayor/town.json.
func IsTownRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "mayor", "town.json"))
	return err == nil
}
