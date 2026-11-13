package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// RigConfigCoverageCheck is the gt doctor port of tools/rig-config-audit.py.
// Unparking a rig clears its park record and nothing else, so a rig a daemon
// list leaves out stays half-working. For each unparked rig the check reports:
//
//   - required: a rig missing from a daemon rig list the patrols read:
//     daemon.patrols.landing_worker.rigs, daemon.patrols.patrol_scan.rigs and
//     daemon.patrols.patrol_scan.worktree_cleanup.rigs. A list is checked only
//     when its block is present. An empty list covers what its patrol covers
//     by default: every rig, or only the default rig for worktree cleanup.
//   - settings: a rig whose settings/config.json lacks merge_queue.forgejo or
//     merge_queue.post_land_command.
//   - info: a rig missing from another non-empty list of rig names. Often
//     deliberate, so it never fails the check.
//
// Required and settings problems make the check an error. A registry, daemon
// config, park record or settings file that cannot be read is not a clean
// rig: it is a warning, or a settings problem for the settings file. The
// settings file holds credentials, so findings name a rig and a key path
// only and never print a value from a file.
type RigConfigCoverageCheck struct {
	BaseCheck

	// reader holds the reads the check makes. A nil field is the real read,
	// so a test replaces only the read it drives.
	reader rigConfigReader
}

// NewRigConfigCoverageCheck creates the rig config coverage check.
func NewRigConfigCoverageCheck() *RigConfigCoverageCheck {
	return &RigConfigCoverageCheck{
		BaseCheck: BaseCheck{
			CheckName:        "rig-config-coverage",
			CheckDescription: "Check every unparked rig is in the daemon's rig lists and has its merge-queue settings",
			CheckCategory:    CategoryConfig,
		},
	}
}

// rigConfigReader is the set of reads RigConfigCoverageCheck makes. Each one
// reads from the source gt rig list and the daemon use.
type rigConfigReader struct {
	// rigs returns the registered rig names, sorted.
	rigs func(townRoot string) ([]string, error)
	// park returns a rig's park record, nil when the rig is unparked.
	park func(townRoot, rig string) (*config.RigParked, error)
	// daemon returns the daemon patrol config; config.ErrNotFound when none.
	daemon func(townRoot string) (*config.DaemonPatrolConfig, error)
	// settings returns a rig's settings/config.json; config.ErrNotFound when absent.
	settings func(rigPath string) (*config.RigSettings, error)
}

// withDefaults fills each nil read with the real one.
func (r rigConfigReader) withDefaults() rigConfigReader {
	if r.rigs == nil {
		r.rigs = registeredRigNames
	}
	if r.park == nil {
		r.park = townconfig.ParkState
	}
	if r.daemon == nil {
		r.daemon = func(townRoot string) (*config.DaemonPatrolConfig, error) {
			return config.LoadDaemonPatrolConfig(config.DaemonPatrolConfigPath(townRoot))
		}
	}
	if r.settings == nil {
		r.settings = func(rigPath string) (*config.RigSettings, error) {
			return config.LoadRigSettings(config.RigSettingsPath(rigPath))
		}
	}
	return r
}

// registeredRigNames reads mayor/rigs.json as gt rig list does: a missing
// registry is an empty town, and an unreadable one is an error.
func registeredRigNames(townRoot string) ([]string, error) {
	cfg, err := config.LoadRigsConfigOrEmpty(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfg.Rigs))
	for name := range cfg.Rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// rigDirName reports whether a registry name can name a directory under the
// town root. A name that cannot is never joined into a path.
func rigDirName(rig string) bool {
	return rig != "" && !strings.ContainsAny(rig, `/\`) && rig != "." && rig != ".."
}

// rigList is one daemon list of rig names and the rule that says whether it
// covers a rig. path is the key path a finding names.
type rigList struct {
	path   string
	covers func(rig string) bool
}

// Run reports each unparked rig the daemon lists or its settings leave out.
func (c *RigConfigCoverageCheck) Run(ctx *CheckContext) *CheckResult {
	rd := c.reader.withDefaults()

	rigs, err := rd.rigs(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "cannot read the rig registry; rig lists not checked",
			Details: []string{err.Error()},
			FixHint: "Fix mayor/rigs.json, the file gt rig list reads",
		}
	}

	daemon, err := rd.daemon(ctx.TownRoot)
	if errors.Is(err, config.ErrNotFound) {
		daemon, err = nil, nil
	}
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "cannot read the daemon patrol config; rig lists not checked",
			Details: []string{err.Error()},
			FixHint: "Fix the daemon patrol config (mayor/daemon.json, or the daemon section of settings/config.json)",
		}
	}
	required, info := daemonRigLists(daemon)

	var problems, notes, unknown []string
	unparked := 0
	for _, rig := range rigs {
		if !rigDirName(rig) {
			unknown = append(unknown, fmt.Sprintf("registry names %q, which is not a rig directory name; not checked", rig))
			continue
		}
		park, err := rd.park(ctx.TownRoot, rig)
		if err != nil {
			unknown = append(unknown, fmt.Sprintf("rig %s: park state unreadable, not checked: %v", rig, err))
			continue
		}
		if park != nil {
			continue
		}
		unparked++
		for _, l := range required {
			if !l.covers(rig) {
				problems = append(problems, fmt.Sprintf("rig %s: not in %s", rig, l.path))
			}
		}
		problems = append(problems, settingsProblems(rd, rig, filepath.Join(ctx.TownRoot, rig))...)
		for _, l := range info {
			if !l.covers(rig) {
				notes = append(notes, fmt.Sprintf("info: rig %s: not in %s", rig, l.path))
			}
		}
	}

	if len(problems) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d problem(s) for unparked rigs: daemon rig lists or merge-queue settings", len(problems)),
			Details: append(append(problems, unknown...), notes...),
			FixHint: "Add the rig to the named daemon list, or set the key in the rig's settings/config.json. This check only reads.",
		}
	}
	if len(unknown) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d rig(s) have an unreadable park state and were not checked", len(unknown)),
			Details: append(unknown, notes...),
			FixHint: "Fix the park record named in the detail; gt rig park --migrate moves a legacy one",
		}
	}
	msg := fmt.Sprintf("All %d unparked rig(s) are in the daemon's rig lists and have their settings", unparked)
	if unparked == 0 {
		msg = "No unparked rigs to check"
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: msg,
		Details: notes,
	}
}

// settingsProblems names the merge_queue keys a rig's settings/config.json
// lacks. A file that cannot be read is one problem naming the file, since
// the keys cannot be shown present.
func settingsProblems(rd rigConfigReader, rig, rigPath string) []string {
	s, err := rd.settings(rigPath)
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		return []string{fmt.Sprintf("rig %s: settings/config.json cannot be read, merge_queue keys not checked: %v", rig, err)}
	}
	var mq *config.MergeQueueConfig
	if s != nil {
		mq = s.MergeQueue
	}
	var out []string
	if mq == nil || mq.Forgejo == nil {
		out = append(out, fmt.Sprintf("rig %s: settings/config.json lacks merge_queue.forgejo", rig))
	}
	if mq == nil || mq.PostLandCommand == "" {
		out = append(out, fmt.Sprintf("rig %s: settings/config.json lacks merge_queue.post_land_command", rig))
	}
	return out
}

// daemonRigLists returns the required lists (their blocks present) and the
// other non-empty lists of rig names in the daemon patrol config.
func daemonRigLists(cfg *config.DaemonPatrolConfig) (required, info []rigList) {
	if cfg == nil || cfg.Patrols == nil {
		return nil, nil
	}
	p := cfg.Patrols
	if lw := p.LandingWorker; lw != nil {
		required = append(required, rigList{path: "daemon.patrols.landing_worker.rigs", covers: coversEmptyIsAll(lw.Rigs)})
	}
	if ps := p.PatrolScan; ps != nil {
		required = append(required, rigList{path: "daemon.patrols.patrol_scan.rigs", covers: coversEmptyIsAll(ps.Rigs)})
		if wc := ps.WorktreeCleanup; wc != nil {
			required = append(required, rigList{path: "daemon.patrols.patrol_scan.worktree_cleanup.rigs", covers: wc.CoversRig})
		}
	}

	info = patrolConfigRigLists(p)
	if ts := p.TierSweep; ts != nil {
		info = appendNamedRigList(info, "daemon.patrols.tier_sweep.rigs", ts.Rigs)
		info = appendNamedRigList(info, "daemon.patrols.tier_sweep.image_rigs", ts.ImageRigs)
	}
	if ss := p.ScheduledSlings; ss != nil {
		var entryRigs []string
		for _, e := range ss.Entries {
			entryRigs = append(entryRigs, e.Rig)
		}
		info = appendNamedRigList(info, "daemon.patrols.scheduled_slings.entries[].rig", entryRigs)
	}
	// main_branch_test is a retired patrol the daemon accepts and ignores, so
	// a block that does not decode has no effect to report.
	if len(p.MainBranchTest) > 0 {
		var legacy struct {
			Rigs []string `json:"rigs"`
		}
		if json.Unmarshal(p.MainBranchTest, &legacy) == nil {
			info = appendNamedRigList(info, "daemon.patrols.main_branch_test.rigs", legacy.Rigs)
		}
	}
	return required, info
}

// patrolConfigRigLists names the rig lists of every patrol that takes a
// PatrolConfig block. They are found by field type, so a new patrol is covered
// without a new line here.
func patrolConfigRigLists(p *config.PatrolsConfig) []rigList {
	var out []rigList
	v := reflect.ValueOf(p).Elem()
	t := v.Type()
	want := reflect.TypeOf((*config.PatrolConfig)(nil))
	for i := range t.NumField() {
		if t.Field(i).Type != want || v.Field(i).IsNil() {
			continue
		}
		pc := v.Field(i).Interface().(*config.PatrolConfig)
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		out = appendNamedRigList(out, "daemon.patrols."+name+".rigs", pc.Rigs)
	}
	return out
}

// appendNamedRigList adds a list only when it names rigs: an empty list
// names no rig to miss.
func appendNamedRigList(lists []rigList, path string, names []string) []rigList {
	if len(names) == 0 {
		return lists
	}
	return append(lists, rigList{path: path, covers: func(rig string) bool { return slices.Contains(names, rig) }})
}

// coversEmptyIsAll covers every rig when names is empty, as the patrols that
// read it do.
func coversEmptyIsAll(names []string) func(rig string) bool {
	return func(rig string) bool { return len(names) == 0 || slices.Contains(names, rig) }
}
