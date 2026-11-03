package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// The container daemon cannot run everything the launchd daemon runs: some
// plugins and patrols need the Mac's toolchain, its Docker Desktop engine, its
// launchd or its files. daemonSteps classifies every one the town has, so L3
// does not find them one at a time (gt-1oz0t). The host-side and needs-a-seam
// names are daemon.container_off in the container's settings; the host agent
// runs the host-side ones on the host (gt-w9nux).

// stepClass is where a daemon step can run.
type stepClass string

const (
	// stepRuns: the step runs in the container once the parity beads have
	// landed; needs lists the programs it execs, which the image must carry.
	stepRuns stepClass = "runs-in-container"
	// stepHost: the step needs the host and runs there.
	stepHost stepClass = "host-side"
	// stepSeam: the step could run in the container with a seam that does not
	// exist yet; until it does it is off in the container.
	stepSeam stepClass = "needs-a-seam"
)

// stepKind says which namespace a name is in: a plugin is a directory with a
// plugin.md, a patrol is a key of daemon.patrols.
type stepKind string

const (
	kindPlugin stepKind = "plugin"
	kindPatrol stepKind = "patrol"
)

// daemonStep is one row of the audit: a plugin (a directory with plugin.md) or
// a patrol (a key of daemon.patrols), what it needs, and why it is classed so.
type daemonStep struct {
	name  string
	kind  stepKind
	class stepClass
	needs []string // programs it execs, for stepRuns
	why   string
}

// daemonSteps is the audit, read from the code and the daemon log on 2026-11-01.
// A plugin or patrol the town has that this table does not name fails the
// rehearsal until it is classified. The mechanism is generic; the rows and their
// reasons describe this town (its plugins, its Mac and its Docker Desktop), so a
// fork that contributes upstream keeps them out of the shared diff.
var daemonSteps = []daemonStep{
	{"polecat-image", kindPlugin, stepHost, nil, "go, make, crane and the Docker Desktop engine build and publish the polecat image; a stale image refuses every sandbox sling"},
	{"hm-sync", kindPlugin, stepHost, nil, "hm, make and go against hm-postgres on Docker Desktop"},
	{"tool-updater", kindPlugin, stepHost, nil, "manual plugin that runs brew"},
	{"cloud-check", kindPlugin, stepHost, nil, "launchctl, the Docker Desktop engine and /Users/Shared/gt-cloud"},
	{"app-promote", kindPlugin, stepSeam, nil, "needs the Forgejo viewer token, the deploy key, outbound ssh to GitHub and the Forgejo API address"},
	{"devops-reporter", kindPlugin, stepSeam, nil, "bd and gt only, but reads /Users/Shared/gt-cloud/reports"},
	{"release-reporter", kindPlugin, stepSeam, nil, "bd and gt only, but reads /Users/Shared/gt-cloud/releases and two files under HOME"},
	{"town-metrics", kindPlugin, stepSeam, nil, "its lsof, ps and desktop-linux docker probes would measure the container, not the host"},

	{"landing_worker", kindPatrol, stepHost, nil, "gates with make gate: go, make and golangci-lint"},
	{"rebuild_gt", kindPatrol, stepHost, nil, "make install and launchctl"},
	{"update", kindPatrol, stepHost, nil, "make install and launchctl install the binary on the host and reload the host's daemon"},
	{"rebuild_om", kindPatrol, stepHost, nil, "go build"},
	{"rebuild_hm", kindPatrol, stepHost, nil, "go build"},
	{"disk_maintenance", kindPatrol, stepHost, nil, "every target is the Mac, Colima or Docker Desktop"},
	{"main_branch_test", kindPatrol, stepHost, nil, "go test"},
	{"doctor_dog", kindPatrol, stepSeam, nil, "gt doctor does not finish in the image; Dolt and the backup directory need the same addresses and mounts"},
	{"health_notify", kindPatrol, stepSeam, nil, "the macOS notify scripts (deferred by the operator)"},

	{"dolt_server", kindPatrol, stepHost, nil, "starts and watches the Dolt server, which runs on the host; the container reaches it over host.docker.internal"},
	{"hm_sync", kindPatrol, stepHost, nil, "hm on the host (the plugin of the same name does the same)"},
	{"landing_infra", kindPatrol, stepHost, nil, "the landing worker's companion: watches Forgejo and its run queue for a landing worker that runs on the host"},
	{"resource_sampler", kindPatrol, stepSeam, nil, "ps rows for the host's components would describe the container"},

	{"handler", kindPatrol, stepRuns, []string{"bash"}, "dispatches the plugins that run here"},
	{"tier_sweep", kindPatrol, stepRuns, []string{"docker"}, "drives the sweep engine over DOCKER_HOST"},
	{"spec_dispatch", kindPatrol, stepRuns, []string{"openshell", "kubectl"}, "gt sling through the gateway and the cluster"},
	{"jsonl_git_backup", kindPatrol, stepRuns, []string{"dolt", "git"}, "dolt and git"},
	{"patrol_scan", kindPatrol, stepRuns, []string{"git"}, "git and gt"},
	{"checkpoint_dog", kindPatrol, stepRuns, []string{"git"}, "git"},
	{"overrun_guard", kindPatrol, stepRuns, []string{"ps"}, "ps"},
	{"overseer_report", kindPatrol, stepRuns, nil, "in process"},
	{"refinery", kindPatrol, stepRuns, nil, "retired and ignored; the key stays in the schema so old files load"},
	{"bird_reports", kindPatrol, stepRuns, nil, "in process"},
	{"findings_filer", kindPatrol, stepRuns, nil, "in process"},
	{"events_prune", kindPatrol, stepRuns, nil, "in process"},
	{"git_hygiene", kindPatrol, stepRuns, []string{"git"}, "git and git gc in the rigs' repositories under the town"},
	{"compactor_dog", kindPatrol, stepRuns, nil, "in process"},
	{"scheduled_maintenance", kindPatrol, stepRuns, nil, "in process"},
	{"scheduled_slings", kindPatrol, stepRuns, nil, "gt sling"},
	{"wisp_reaper", kindPatrol, stepRuns, nil, "in process"},
}

// jev_ patrols are in-process readers of the town's own files.
const jevPatrolPrefix = "jev_"

// stepNamed reports whether the audit table, or the jev_ prefix rule for
// patrols, classifies name as a step of the given kind.
func stepNamed(name string, kind stepKind) bool {
	if kind == kindPatrol && strings.HasPrefix(name, jevPatrolPrefix) {
		return true
	}
	return slices.ContainsFunc(daemonSteps, func(s daemonStep) bool { return s.name == name && s.kind == kind })
}

// stepKnown reports whether name is a classified step of either kind, which is
// what a container_off entry may name.
func stepKnown(name string) bool { return stepNamed(name, kindPlugin) || stepNamed(name, kindPatrol) }

// containerOffNames are the names daemon.container_off must list in the
// container's settings: every step that is not stepRuns, sorted.
func containerOffNames() []string {
	var names []string
	for _, s := range daemonSteps {
		if s.class != stepRuns {
			names = append(names, s.name)
		}
	}
	sort.Strings(names)
	return names
}

// stagedDaemonSettings is the part of the staged config.json the plugins check
// reads.
type stagedDaemonSettings struct {
	Daemon struct {
		ContainerOff []string `json:"container_off"`
		Patrols      map[string]struct {
			Enabled *bool `json:"enabled"`
		} `json:"patrols"`
	} `json:"daemon"`
}

func checkDaemonSteps(ctx context.Context, r *rehearsal) []string {
	var st stagedDaemonSettings
	if err := json.Unmarshal(r.config, &st); err != nil {
		return []string{"the staged config.json does not give daemon.container_off and daemon.patrols the shapes the daemon reads: " + err.Error()}
	}
	var fails []string
	off := st.Daemon.ContainerOff
	for _, s := range daemonSteps {
		if s.class == stepRuns || slices.Contains(off, s.name) {
			continue
		}
		if p, ok := st.Daemon.Patrols[s.name]; ok && p.Enabled != nil && !*p.Enabled {
			continue // already off for every supervisor
		}
		fails = append(fails, fmt.Sprintf("%s (%s) is not in daemon.container_off: %s", s.name, s.class, s.why))
	}
	for _, name := range off {
		if !stepKnown(name) {
			fails = append(fails, name+": in daemon.container_off but unknown to the audit table (a typo, or a step to classify)")
		}
	}
	found, problems := r.discoverSteps()
	fails = append(fails, problems...)
	for _, name := range found {
		if !stepNamed(name, kindPlugin) {
			fails = append(fails, name+": a plugin the audit table does not classify; add it to daemonSteps")
		}
	}
	keys := knownPatrolKeys()
	if r.d.patrolKeys != nil {
		keys = r.d.patrolKeys()
	}
	for _, name := range unclassifiedNames(keys) {
		fails = append(fails, name+": a patrol the daemon knows (config.PatrolsConfig) that the audit table does not classify, enabled by its default or by the settings; add it to daemonSteps")
	}
	settingsPatrols := make([]string, 0, len(st.Daemon.Patrols))
	for name := range st.Daemon.Patrols {
		settingsPatrols = append(settingsPatrols, name)
	}
	sort.Strings(settingsPatrols)
	for _, name := range settingsPatrols {
		if !stepNamed(name, kindPatrol) {
			fails = append(fails, name+": a patrol the audit table does not classify; add it to daemonSteps")
		}
	}
	return append(fails, r.missingStepPrograms(ctx)...)
}

// discoverSteps lists the plugins the daemon would find: a directory with a
// plugin.md under <town>/plugins and under each rig's plugins directory. Dot
// directories are not plugins (the daemon disables one by renaming it so). A
// directory that cannot be listed, or a plugin.md that cannot be read for a
// reason other than not existing, is a problem returned beside the list: the
// check that stops an unclassified plugin from slipping through must not pass
// when it could not look.
func (r *rehearsal) discoverSteps() (found, problems []string) {
	roots := []string{filepath.Join(r.f.liveRoot, "plugins")}
	rigs, err := r.d.readDir(r.f.liveRoot)
	if err != nil {
		problems = append(problems, "the live town "+r.f.liveRoot+" cannot be listed, so its rigs' plugins are unknown: "+err.Error())
	}
	for _, e := range rigs {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			roots = append(roots, filepath.Join(r.f.liveRoot, e.Name(), "plugins"))
		}
	}
	for _, root := range roots {
		entries, err := r.d.readDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a rig without plugins
		}
		if err != nil {
			problems = append(problems, "the plugins directory "+root+" cannot be listed: "+err.Error())
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			_, err := r.d.readFile(filepath.Join(root, e.Name(), "plugin.md"))
			switch {
			case err == nil:
				found = append(found, e.Name())
			case !errors.Is(err, fs.ErrNotExist):
				problems = append(problems, "the plugin "+filepath.Join(root, e.Name())+" cannot be read: "+err.Error())
			}
		}
	}
	return found, problems
}

// missingStepPrograms checks the image carries every program a stepRuns row
// execs, naming the steps that need each.
func (r *rehearsal) missingStepPrograms(ctx context.Context) []string {
	needed := map[string][]string{}
	for _, s := range daemonSteps {
		if s.class == stepRuns {
			for _, p := range s.needs {
				needed[p] = append(needed[p], s.name)
			}
		}
	}
	progs := make([]string, 0, len(needed))
	for p := range needed {
		progs = append(progs, p)
	}
	sort.Strings(progs)
	var fails []string
	for _, p := range progs {
		err := r.inImage(ctx, "none", "bash", "-c", "command -v "+p)
		switch {
		case err == nil:
		case probeSaidNo(err, 1):
			fails = append(fails, fmt.Sprintf("%s: not on the image's PATH, needed by %s", p, strings.Join(needed[p], ", ")))
		default:
			fails = append(fails, p+": the probe did not run: "+err.Error())
		}
	}
	return fails
}

// knownPatrolKeys are the patrol keys the daemon's own config type declares,
// retired ones (kept only as raw JSON so old files decode) excluded. A patrol the
// daemon enables by its default appears here whether or not the settings name it.
func knownPatrolKeys() []string {
	t := reflect.TypeOf(config.PatrolsConfig{})
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || f.Type == reflect.TypeOf(json.RawMessage(nil)) {
			continue
		}
		keys = append(keys, name)
	}
	return keys
}

// unclassifiedNames are the names the audit table does not classify.
func unclassifiedNames(names []string) []string {
	var out []string
	for _, n := range names {
		if !stepNamed(n, kindPatrol) {
			out = append(out, n)
		}
	}
	return out
}
