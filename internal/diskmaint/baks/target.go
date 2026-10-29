package baks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// Compile-time check that the baks target satisfies the diskmaint contract.
var _ diskmaint.Target = (*Target)(nil)

// settingsDir is the directory under a rig that holds the backups this target
// judges, so a town subdirectory that has one is a rig.
const settingsDir = "settings"

// lsofUnavailable is the OpenBy text a candidate carries when lsof could not
// say whether a process holds it. BuildPlan keeps an entry with a non-empty
// OpenBy and counts it toward its group's keep count, so a check that failed
// never reads as "not open" (fail closed).
const lsofUnavailable = "lsof unavailable"

var (
	// errNoRunner reports a target with no lsof runner: whether a file is open
	// cannot be checked, so a plan refuses rather than remove a file in use.
	errNoRunner = errors.New("baks: no lsof runner configured")
	// errNoDirs reports a target with no directory to read at all, which is a
	// configuration mistake rather than a town with no backups.
	errNoDirs = errors.New("baks: no config or binary directory configured")
	// errNotWired reports an apply to a target that only reads: it lists
	// directories and asks lsof, and it removes nothing.
	errNotWired = errors.New("baks apply: deletion is not wired yet; the plan is a dry run")
	// errReadDir reports a directory that exists but cannot be listed. A plan
	// refuses on it, because an unlisted directory could hide a backup that
	// must stay.
	errReadDir = errors.New("cannot be read")
	// errStat reports a file whose state cannot be read, so whether it is a
	// backup, how old it is and how big it is are all unknown.
	errStat = errors.New("the file state cannot be read")
)

// Config is what the baks target needs to discover backup candidates and read
// whether they are open.
type Config struct {
	// ConfigDirs are the directories holding config backups: a town's settings,
	// each rig's settings, mayor and the operator's om config.
	ConfigDirs []string
	// BinaryDirs are the directories holding installed and stand-alone binaries.
	BinaryDirs []string
	// Installed names the binaries whose installed copy is read for
	// InstalledMod, e.g. bd, gt, om. A name not in BinaryDirs is left out, and
	// BuildPlan then keeps a stand-alone bd.<sha> rather than judge it.
	Installed []string
	// Run runs lsof. A test passes a fake that records calls and returns canned
	// output, so no test runs real lsof.
	Run diskmaint.Runner
	// Now is the clock a policy without a time is read against; nil uses the
	// system clock.
	Now func() time.Time
	// Ledger records an apply, which this target does not do yet.
	Ledger *diskmaint.Ledger
}

// Target discovers config and binary backups and plans their removal.
type Target struct {
	cfg Config
}

// NewTarget returns a Target that reads the directories named by cfg.
func NewTarget(cfg Config) *Target {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Target{cfg: cfg}
}

// Name is the target's name in plans and ledger entries.
func (t *Target) Name() string { return planTarget }

// DefaultDirs returns the directories this target reads for a town: its own
// settings, each rig's settings, mayor and the operator's om config; and the
// two directories installed and stand-alone binaries live in. A blank town or
// home is skipped rather than joined into a relative path, which would list
// the working directory.
func DefaultDirs(town, home string) (config, binary []string) {
	if town != "" {
		config = append(config, filepath.Join(town, settingsDir))
		for _, rig := range rigDirs(town) {
			config = append(config, filepath.Join(town, rig, settingsDir))
		}
		config = append(config, filepath.Join(town, "mayor"))
	}
	if home != "" {
		config = append(config, filepath.Join(home, ".config", "om"))
		binary = append(binary, filepath.Join(home, ".local", "bin"), filepath.Join(home, "go", "bin"))
	}
	return config, binary
}

// rigDirs returns the names of the town's subdirectories that hold a settings
// directory, which is what makes one a rig here: a rig with no settings
// directory has no settings backups to read. The names come in the order
// os.ReadDir gives them, so the same town always yields the same paths. A town
// that cannot be listed contributes no rigs; a rig this misses costs nothing,
// because the directory it would name holds no backups the plan could act on.
func rigDirs(town string) []string {
	entries, err := os.ReadDir(town)
	if err != nil {
		return nil
	}
	var rigs []string
	for _, e := range entries {
		dir, err := os.Stat(filepath.Join(town, e.Name()))
		if err != nil || !dir.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(town, e.Name(), settingsDir))
		if err != nil || !info.IsDir() {
			continue
		}
		rigs = append(rigs, e.Name())
	}
	return rigs
}

// Measure reports how many backup candidates the directories hold and their
// total size. Used is that size: it is what this target accounts for, not what
// the directories hold. A directory that cannot be listed makes the measurement
// an error naming it, because a total that leaves it out would be a guess.
func (t *Target) Measure(ctx context.Context) (diskmaint.Measurement, error) {
	config, binaries, readErrs, err := t.scan(ctx)
	if err != nil {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %w", err)
	}
	if len(readErrs) > 0 {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %s", strings.Join(readErrs, "; "))
	}
	files, bytes := candidateTotals(config, binaries)
	return diskmaint.Measurement{
		Used:   bytes,
		Detail: fmt.Sprintf("%d backup candidates holding %d bytes", files, bytes),
	}, nil
}

// Plan lists the directories, asks lsof which candidates are open, reads the
// installed binaries' times and returns BuildPlan's decision for them. A
// directory or file that cannot be read refuses the whole plan, because it
// could hide a backup that must stay; a candidate lsof cannot answer for is
// kept as if it were open. Plan changes nothing.
func (t *Target) Plan(ctx context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	plan := diskmaint.Plan{Target: planTarget}
	if t.cfg.Run == nil {
		plan.Refused = errNoRunner.Error()
		return plan, nil
	}
	config, binaries, readErrs, err := t.scan(ctx)
	if err != nil {
		if errors.Is(err, errNoDirs) {
			plan.Refused = err.Error()
			return plan, nil
		}
		return plan, err
	}
	in := Input{Config: config, Binaries: binaries, Now: t.now(p), ReadErrs: readErrs}
	// A read error refuses the plan in BuildPlan whatever else is true of the
	// files, so the open check and the installed times are not read: their
	// answers could not change a plan that is already refused.
	if len(readErrs) == 0 {
		if err := t.markOpen(ctx, config); err != nil {
			return plan, err
		}
		if err := t.markOpen(ctx, binaries); err != nil {
			return plan, err
		}
		in.InstalledMod = t.installedMods()
	}
	return BuildPlan(in), nil
}

// Apply refuses the plan and deletes nothing: this target reads directory state
// and asks lsof, it does not remove a file, so a caller that ignored a dry run
// cannot lose a backup.
func (t *Target) Apply(context.Context, diskmaint.Plan) (diskmaint.Result, error) {
	return diskmaint.Result{Target: planTarget}, errNotWired
}

// scan reads the configured directories into the config and binary entries
// BuildPlan decides from. It returns the read errors it collected, which refuse
// the plan, and an error of its own only for a configuration that cannot be
// planned or a cancelled context.
func (t *Target) scan(ctx context.Context) (config, binaries []Entry, readErrs []string, err error) {
	if len(t.cfg.ConfigDirs) == 0 && len(t.cfg.BinaryDirs) == 0 {
		return nil, nil, nil, errNoDirs
	}
	for _, dir := range t.cfg.ConfigDirs {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		entries, dirErrs := readDir(dir)
		config = append(config, entries...)
		readErrs = append(readErrs, dirErrs...)
	}
	for _, dir := range t.cfg.BinaryDirs {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		entries, dirErrs := readDir(dir)
		binaries = append(binaries, entries...)
		readErrs = append(readErrs, dirErrs...)
	}
	return config, binaries, readErrs, nil
}

// readDir lists the regular files directly under dir. A directory that is not
// there holds nothing, which is not an error: DefaultDirs names settings and
// binary directories a town or an operator may not have. A directory that
// cannot be listed, or a file whose state cannot be read, becomes a read error,
// which refuses the plan.
func readDir(dir string) ([]Entry, []string) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", dir, errReadDir)}
	}
	var entries []Entry
	var readErrs []string
	for _, de := range des {
		path := filepath.Join(dir, de.Name())
		// Lstat, not Stat: a symlink's own mode is what decides, so a link to a
		// backup is skipped instead of being judged and removed through a name
		// that does not hold the file.
		info, err := os.Lstat(path)
		if err != nil {
			readErrs = append(readErrs, fmt.Sprintf("%s: %v", path, errStat))
			continue
		}
		if !info.Mode().IsRegular() {
			continue // a symlink, a directory, a socket: never a backup file
		}
		entries = append(entries, Entry{
			Path:    path,
			Name:    de.Name(),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}
	return entries, readErrs
}

// markOpen sets OpenBy on every candidate in entries from lsof's answer. A
// file that is not a candidate is not asked: it is kept whatever its handle,
// so the answer would tell the plan nothing.
func (t *Target) markOpen(ctx context.Context, entries []Entry) error {
	for i := range entries {
		e := &entries[i]
		if !candidate(e.Name) {
			continue
		}
		openBy, err := t.openBy(ctx, e.Path)
		if err != nil {
			return err
		}
		e.OpenBy = openBy
	}
	return nil
}

// openBy asks lsof which processes hold path open. lsof -t prints one pid per
// line and exits 1 with no output when nothing holds the file, so that pair is
// the only answer that reads as "not open". A pid it listed is the holder.
// Every other failure — lsof not installed, an exit status other than 1, a
// success that lists nothing — leaves the handle unknown and keeps the file.
func (t *Target) openBy(ctx context.Context, path string) (string, error) {
	out, err := t.cfg.Run(ctx, nil, "lsof", "-t", "--", path)
	if err != nil {
		// A cancelled run is the caller's, not lsof's: report it instead of
		// planning on a check that was cut short.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if exitCode(err) == 1 && pidList(out) == "" {
			return "", nil
		}
		return lsofUnavailable, nil
	}
	if pids := pidList(out); pids != "" {
		return pids, nil
	}
	return lsofUnavailable, nil
}

// installedMods reads the mtime of each installed binary cfg names, from the
// first binary directory that holds it. A binary that is not there, or cannot
// be read, is left out, and BuildPlan then keeps every stand-alone bd.<sha> as
// unreadable rather than judge it against a build it could not compare with.
func (t *Target) installedMods() map[string]time.Time {
	if len(t.cfg.Installed) == 0 {
		return nil
	}
	mods := make(map[string]time.Time, len(t.cfg.Installed))
	for _, name := range t.cfg.Installed {
		for _, dir := range t.cfg.BinaryDirs {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			mods[name] = info.ModTime()
			break
		}
	}
	return mods
}

// now is the clock a plan's age rules are read against: the policy's time when
// it has one, else the target's clock.
func (t *Target) now(p diskmaint.Policy) time.Time {
	if !p.Now.IsZero() {
		return p.Now
	}
	return t.cfg.Now()
}

// candidate reports whether a file name is one this target decides about: a
// config or binary backup, or a stand-alone bd build named by its sha.
func candidate(name string) bool {
	return strings.Contains(name, configMarker) || shaName.MatchString(name)
}

// candidateTotals counts the candidates in the two entry lists and sums their
// sizes.
func candidateTotals(lists ...[]Entry) (files int, bytes int64) {
	for _, entries := range lists {
		for _, e := range entries {
			if !candidate(e.Name) {
				continue
			}
			files++
			bytes += e.Size
		}
	}
	return files, bytes
}

// pidList is the pids `lsof -t` printed, comma-separated, or "" when it printed
// none.
func pidList(out []byte) string {
	var pids []string
	for _, line := range strings.Split(string(out), "\n") {
		if pid := strings.TrimSpace(line); pid != "" {
			pids = append(pids, pid)
		}
	}
	return strings.Join(pids, ",")
}

// exitCode is the process exit status err reports, or -1 when err is not an
// exit status at all (lsof missing, say, which exec reports differently).
func exitCode(err error) int {
	var exited interface{ ExitCode() int }
	if errors.As(err, &exited) {
		return exited.ExitCode()
	}
	return -1
}
