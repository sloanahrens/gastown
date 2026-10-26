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
	// DaemonDir holds the daemon's state.json, its restart marker and the
	// install receipts.
	DaemonDir string
	// RigDir is the checkout a run fast-forwards to the target and builds in.
	RigDir string
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
// home/.local/bin; and the daemon and build checkout from their own variables,
// else from the town root. start is the directory the town root is searched
// above when GT_TOWN_ROOT is empty.
func ResolvePaths(getenv func(string) string, home, start string) (Paths, error) {
	binDir := getenv("INSTALL_GT_BIN_DIR")
	if binDir == "" {
		binDir = getenv("INSTALL_DIR")
	}
	if binDir == "" {
		binDir = filepath.Join(home, ".local", "bin")
	}
	townRoot := getenv("GT_TOWN_ROOT")
	if townRoot == "" {
		townRoot = townRootAbove(start)
	}
	p := Paths{BinDir: binDir, DaemonDir: getenv("INSTALL_GT_DAEMON_DIR"), RigDir: getenv("INSTALL_GT_RIG_DIR")}
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

// townRootAbove is the outermost ancestor of dir holding the town marker: a rig
// can carry a mayor/ of its own, so the outermost is the town
// (lib/install-gt-lib.sh's igt_town_root_above).
func townRootAbove(dir string) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	found := ""
	for {
		if _, err := os.Stat(filepath.Join(d, "mayor", "town.json")); err == nil {
			found = d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return found
		}
		d = parent
	}
}
