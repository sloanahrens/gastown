package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/workspace"
)

// checkRigNotParkedOrDocked checks if a rig is parked or docked and returns
// an error if so. This prevents starting agents on rigs that have been
// intentionally taken offline.
func checkRigNotParkedOrDocked(rigName string) error {
	townRoot, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	if IsRigParked(townRoot, rigName) {
		return fmt.Errorf("rig '%s' is parked - use 'gt rig unpark %s' first", rigName, rigName)
	}

	prefix := "gt"
	if r.Config != nil && r.Config.Prefix != "" {
		prefix = r.Config.Prefix
	}

	docked, err := IsRigDocked(townRoot, rigName, prefix)
	if err != nil {
		return fmt.Errorf("checking whether rig '%s' is docked: %w", rigName, err)
	}
	if docked {
		return fmt.Errorf("rig '%s' is docked - use 'gt rig undock %s' first", rigName, rigName)
	}

	return nil
}

// getRig finds the town root and retrieves the specified rig.
// This is the common boilerplate extracted from get*Manager functions.
// Returns the town root path and rig instance.
func getRig(rigName string) (string, *rig.Rig, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return getRigIn(townRoot, rigName)
}

// getRigIn is getRig in a known town.
func getRigIn(townRoot, rigName string) (string, *rig.Rig, error) {
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfigOrEmpty(rigsConfigPath)
	if err != nil {
		return "", nil, fmt.Errorf("loading rigs config: %w", err)
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		return "", nil, fmt.Errorf("rig '%s' not found", rigName)
	}

	return townRoot, r, nil
}

// rigRegistered reports whether rigName is in the town's rig registry. A
// registry that cannot be read is an error, not "unregistered": the caller
// must not read a failed read as a rig the town does not know.
func rigRegistered(townRoot, rigName string) (bool, error) {
	rigsConfig, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return false, err
	}
	if rigsConfig == nil {
		return false, nil
	}
	_, ok := rigsConfig.Rigs[rigName]
	return ok, nil
}

// findCurrentRig resolves the rig the caller is working in: the first path
// component of cwd under the town root when that component is a registered rig
// (town-level dirs like mayor/ are not), else GT_RIG (gt run from the town
// root through a shell alias). An unregistered component with no GT_RIG set is
// still looked up, so the error names it.
func findCurrentRig(townRoot string) (string, *rig.Rig, error) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		cwd = ""
	}
	return findCurrentRigWith(os.Getenv, cwd, townRoot)
}

// findCurrentRigWith is findCurrentRig reading the environment through getenv
// and taking the working directory as cwd ("" when unknown).
func findCurrentRigWith(getenv func(string) string, cwd, townRoot string) (string, *rig.Rig, error) {
	rigName := ""
	if cwd != "" {
		if inferred, err := inferRigFromDir(townRoot, cwd); err == nil {
			rigName = inferred
		}
	}
	if envRig := getenv("GT_RIG"); envRig != "" {
		// GT_RIG replaces a cwd-derived rig only when the registry says that
		// rig is unknown. A registry that cannot be read keeps the cwd rig, so
		// getRigIn below reports the read failure rather than GT_RIG quietly
		// sending the command to another rig.
		if rigName == "" {
			rigName = envRig
		} else if registered, err := rigRegistered(townRoot, rigName); err == nil && !registered {
			rigName = envRig
		}
	}
	if rigName == "" {
		return "", nil, fmt.Errorf("not inside a rig directory (and GT_RIG not set)")
	}
	_, r, err := getRigIn(townRoot, rigName)
	if err != nil {
		return "", nil, err
	}
	return rigName, r, nil
}

// ringHoldNow is the hold on a dispatch at the wall clock: the daemon's boot
// hold file first, then the Ring at the wall clock, its key read under the
// daemon environment. The boot hold comes first because the daemon publishes it
// from another process while infrastructure is not answering (gt-ib2nt7). Every
// dispatch path passes it to slingBlocked or the sling engine.
func ringHoldNow(townRoot string) string {
	if reason := dispatch.BootHold(townRoot); reason != "" {
		return reason
	}
	return ring.Hold(townRoot, ring.TownKey(os.Getenv), time.Now())
}

// slingBlocked is why a sling into rigName must not run, or nil when it may.
// The guards themselves are sling.Blocked's: the dispatch engine runs them for
// the automatic dispatchers as well as for this command, and a second copy
// here is a rule the two could drift apart on.
func slingBlocked(townRoot, rigName string, ringHold func(townRoot string) string, estopOn func(townRoot, rigName string) (bool, error), parked func(townRoot, rigName string) (bool, string)) error {
	_, err := sling.Blocked(townRoot, rigName, ringHold, estopOn, parked)
	return err
}

// IsRigParkedOrDocked checks if a rig is parked or docked. Returns
// (blocked, reason) with reason "parked", "docked" or sling.RigStatusUnverified.
// This is the single entry point for all dispatch paths (sling, polecat spawn,
// the daemon, gt spec) to check rig availability.
//
// Parked is the rig's record in mayor/town.json (registry), read through the
// config kernel, and fails closed: a rig whose park state cannot be read is
// parked (gt-y3pgh.4). Docked is the rig identity bead's status:docked label,
// and that read fails closed too: when bd cannot answer, the rig may be docked,
// so it is blocked as unverified. A rig with no identity bead at all
// (ErrNotFound) is not blocked.
func IsRigParkedOrDocked(townRoot, rigName string) (bool, string) {
	return rigParkedOrDockedWith(townRoot, rigName, nil)
}

// rigDockedRead is one rig's docked-label verdict, as the memo remembered it.
type rigDockedRead struct {
	blocked bool
	reason  string
}

// rigDockedEntry is one rig's memoized docked-label read: the verdict, and the
// dock stamp observed just before the read that produced it.
type rigDockedEntry struct {
	// mu is held across this rig's identity-bead read and its verdict file, so
	// concurrent checks of one rig share its one read. Another rig takes its
	// own entry, so one rig's slow bd call does not hold up the others
	// (gt-ze7z5).
	mu sync.Mutex
	// known is set once a read answered; without it there is no verdict to
	// serve and the next check reads again.
	known bool
	read  rigDockedRead
	// stamp is the dock stamp observed before the read that answered: a newer
	// one means gt docked or undocked the rig since (rig.DockStamp), so the
	// verdict is dropped and read again.
	stamp time.Time
}

// rigDockedMemo remembers docked-label reads per rig: the identity bead is
// read at most once per rig per tick (gt-ze7z5), though the tick made the same
// check for the board scan and again behind every candidate it slung.
//
// A later tick is served from a per-rig verdict file under the town runtime
// directory (rigDockedFileTTL), so a quiet tick reads no identity beads at all
// (gt-ejxjh). The file holds the docked-label verdict, the dock stamp observed
// before the read, and the read time; it answers only while that stamp is not
// newer than the check's and the read is younger than five minutes. The park
// state is never in the file: every check reads it from the registry first, so
// a park or unpark after a file was written is seen by the next check.
//
// The memo is not frozen for the tick. `gt rig dock` and `gt rig undock` stamp
// the rig (internal/rig.DockStamp), the tick compares the stamp with the one
// its entry recorded, and a newer stamp drops the entry: a dock that lands
// mid-tick stops the tick's later slings. A hand-edited label writes no stamp
// and is seen within five minutes instead, when the verdict file has aged out,
// the same bound the daemon's memo keeps.
//
// The tick makes it and passes it down, never a package variable
// (internal/testpolicy's no-global-swap), so an operator's gt sling, a polecat
// spawn and the daemon keep asking bd every time. A read that failed is not
// remembered and leaves no file, so the failure stays fail-closed and the next
// check asks bd again.
type rigDockedMemo struct {
	store beads.Client
	// stamp reads a rig's dock stamp; rig.DockStamp unless a test replaces it.
	stamp func(townRoot, rigName string) (time.Time, error)
	// files is the filesystem the verdict files are read and written through;
	// osRigDockedFiles unless a test replaces it.
	files rigDockedFiles
	// now is the clock the verdict files' age is measured against.
	now   func() time.Time
	mu    sync.Mutex
	reads map[string]*rigDockedEntry
}

// newRigDockedMemo returns the memo of one tick's docked-label reads, made
// against store (nil: bd in each rig's own database).
func newRigDockedMemo(store beads.Client) *rigDockedMemo {
	return &rigDockedMemo{
		store: store,
		stamp: rig.DockStamp,
		files: osRigDockedFiles(),
		now:   time.Now,
		reads: map[string]*rigDockedEntry{},
	}
}

// parkedOrDocked is IsRigParkedOrDocked answered from the memo when this tick
// already read the rig's docked label and no dock or undock has landed since.
func (m *rigDockedMemo) parkedOrDocked(townRoot, rigName string) (bool, string) {
	if m == nil {
		return IsRigParkedOrDocked(townRoot, rigName)
	}
	// The park state is the registry's, read through the config kernel: no bd
	// call, so it is re-read on every check and a park inside the tick holds.
	// It is never part of the verdict file: the file answers only the docked
	// label, after this read.
	if IsRigParked(townRoot, rigName) {
		return true, "parked"
	}

	// The stamp is observed before the read, never after: a dock that lands
	// while the read runs leaves a newer stamp, so the next check drops the
	// entry rather than serving a verdict from before the dock.
	stamp, err := m.stamp(townRoot, rigName)
	if err != nil {
		// A stamp that cannot be read vouches for no cached verdict, and
		// nothing may be cached without it. This check reads the label fresh,
		// as every check did before the memo: slower on a host that cannot
		// stat the stamp, never stale.
		return rigDockedWith(townRoot, rigName, m.store)
	}

	entry := m.entry(rigName)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.known && !stamp.After(entry.stamp) {
		return entry.read.blocked, entry.read.reason
	}

	now := m.now()
	if rec, ok := m.files.load(townRoot, rigName); ok && rec.fresh(rigName, stamp, now) {
		// A previous tick read this rig inside the window and no gt dock or
		// undock has landed since, so its verdict answers this check. The
		// file keeps its original read time: serving it does not extend its
		// window.
		entry.read = rigDockedRead{blocked: rec.Blocked, reason: rec.Reason}
		entry.stamp = stamp
		entry.known = true
		return entry.read.blocked, entry.read.reason
	}

	// Only the docked-label read reaches the file: the park state this check
	// just read from the registry is never stored, so a later check's park
	// read decides a park on its own and the file cannot serve a stale one.
	blocked, reason := rigDockedWith(townRoot, rigName, m.store)
	if reason == sling.RigStatusUnverified {
		// A failed read is not remembered, so it stays fail-closed and the
		// next check asks bd again. It writes no file either: an unverified
		// verdict is not an answer for a later tick.
		entry.known = false
		return blocked, reason
	}
	entry.read = rigDockedRead{blocked: blocked, reason: reason}
	entry.stamp = stamp
	entry.known = true
	// The verdict is left for the next tick. A failed write is logged, never
	// fatal: it costs the next tick one read.
	rec := rigDockedRecord{
		Rig:     rigName,
		Blocked: blocked,
		Reason:  reason,
		Stamp:   stamp,
		ReadAt:  now,
	}
	if err := m.files.save(townRoot, rigName, rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: docked verdict for %s not saved: %v\n", rigName, err)
	}
	return blocked, reason
}

// entry returns rigName's memo slot, created empty. The lock guards only the
// map, never a bd read.
func (m *rigDockedMemo) entry(rigName string) *rigDockedEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.reads[rigName]
	if !ok {
		entry = &rigDockedEntry{}
		m.reads[rigName] = entry
	}
	return entry
}

// rigParkedOrDockedWith is IsRigParkedOrDocked with the identity bead read
// from store (nil: bd in the rig's own database).
func rigParkedOrDockedWith(townRoot, rigName string, store beads.Client) (bool, string) {
	if IsRigParked(townRoot, rigName) {
		return true, "parked"
	}
	return rigDockedWith(townRoot, rigName, store)
}

// rigDockedWith is the docked-label half of IsRigParkedOrDocked: the rig
// registry's park state is the caller's to read, so a memo's verdict file
// holds only this answer and never a park.
func rigDockedWith(townRoot, rigName string, store beads.Client) (bool, string) {
	// Look up the beads prefix from the rig registry, with fallback
	// to the rig's own config.json for isolated/test scenarios.
	rigPath := filepath.Join(townRoot, rigName)
	prefix := rigBeadsPrefix(townRoot, rigPath, rigName)
	if prefix == "" {
		return false, ""
	}

	if store == nil {
		beadsPath := filepath.Join(rigPath, "mayor", "rig")
		if _, err := os.Stat(beadsPath); err != nil {
			beadsPath = rigPath
		}
		store = beads.New(beadsPath)
	}

	rigBeadID := beads.RigBeadIDWithPrefix(prefix, rigName)
	rigBead, err := store.Show(rigBeadID)
	if errors.Is(err, beads.ErrNotFound) {
		return false, ""
	}
	if err != nil {
		return true, sling.RigStatusUnverified
	}

	for _, l := range rigBead.Labels {
		if l == RigDockedLabel {
			return true, "docked"
		}
	}

	return false, ""
}

func rigBeadsPrefix(townRoot, rigPath, rigName string) string {
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	if rigsConfig, err := config.LoadRigsConfig(rigsConfigPath); err == nil {
		if entry, ok := rigsConfig.Rigs[rigName]; ok && entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
			return entry.BeadsConfig.Prefix
		}
	}

	rigCfg, cfgErr := rig.LoadRigConfigIfPresent(rigPath)
	if cfgErr != nil {
		rig.WarnRigConfigOnce(rigPath, cfgErr)
	}
	if rigCfg != nil && rigCfg.Beads != nil && rigCfg.Beads.Prefix != "" {
		return rigCfg.Beads.Prefix
	}

	return ""
}

// discoverRigsForTownRoot loads the rigs config for the given town root and
// returns all registered rigs. Callers that don't yet have a town root
// should use getAllRigs, which resolves it from the cwd first.
func discoverRigsForTownRoot(townRoot string) ([]*rig.Rig, error) {
	rigsConfig, err := config.LoadRigsConfigOrEmpty(constants.MayorRigsPath(townRoot))
	if err != nil {
		return nil, fmt.Errorf("loading rigs config: %w", err)
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	return rigMgr.DiscoverRigs()
}

// autoInferRig returns the sole registered rig for a given townRoot, or an
// actionable error when the result is ambiguous. Callers use this when no
// --rig flag was provided and cwd-based detection found nothing (e.g. Deacon
// at HQ level on a non-default install where "gastown" rig does not exist).
func autoInferRig(townRoot string) (name, path string, err error) {
	rigs, err := discoverRigsForTownRoot(townRoot)
	if err != nil {
		return "", "", fmt.Errorf("cannot determine target rig: %w; use --rig=NAME", err)
	}

	switch len(rigs) {
	case 1:
		return rigs[0].Name, rigs[0].Path, nil
	case 0:
		return "", "", fmt.Errorf("cannot determine target rig: no rigs registered in this workspace; use --rig=NAME")
	default:
		names := make([]string, len(rigs))
		for i, r := range rigs {
			names[i] = r.Name
		}
		return "", "", fmt.Errorf("cannot determine target rig (available: %s); use --rig=NAME", strings.Join(names, ", "))
	}
}

// getAllRigs discovers all rigs in the current Gas Town workspace.
// Returns the list of rigs and any error.
func getAllRigs() ([]*rig.Rig, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return discoverRigsForTownRoot(townRoot)
}

// rigCmdEnv is where a rig command finds its rig and writes its report. The
// cobra entry points use the cwd's town and stdout; tests give a town and a
// buffer, so they need neither a chdir nor a stdout swap.
type rigCmdEnv struct {
	findRig func(rigName string) (string, *rig.Rig, error)
	out     io.Writer
	errOut  io.Writer
}

func cwdRigCmdEnv() rigCmdEnv { return rigCmdEnv{findRig: getRig, out: os.Stdout, errOut: os.Stderr} }

func townRigCmdEnv(townRoot string, out, errOut io.Writer) rigCmdEnv {
	return rigCmdEnv{
		findRig: func(rigName string) (string, *rig.Rig, error) { return getRigIn(townRoot, rigName) },
		out:     out,
		errOut:  errOut,
	}
}
