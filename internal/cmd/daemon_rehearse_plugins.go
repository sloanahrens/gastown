package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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

// daemonStep is one row of the audit: a plugin (a directory with plugin.md) or
// a patrol (a key of daemon.patrols), what it needs, and why it is classed so.
type daemonStep struct {
	name   string
	plugin bool // a plugin; otherwise a patrol
	class  stepClass
	needs  []string // programs it execs, for stepRuns
	why    string
}

// daemonSteps is the audit, read from the code and the daemon log on 2026-11-01
// (seagull and dipper). A plugin or patrol the town has that this table does not
// name fails the rehearsal until it is classified.
var daemonSteps = []daemonStep{
	{"polecat-image", true, stepHost, nil, "go, make, crane and the Docker Desktop engine build and publish the polecat image; a stale image refuses every sandbox sling"},
	{"hm-sync", true, stepHost, nil, "hm, make and go against hm-postgres on Docker Desktop"},
	{"tool-updater", true, stepHost, nil, "manual plugin that runs brew"},
	{"cloud-check", true, stepHost, nil, "launchctl, the Docker Desktop engine and /Users/Shared/gt-cloud"},
	{"app-promote", true, stepSeam, nil, "needs the Forgejo viewer token, the deploy key, outbound ssh to GitHub and the Forgejo API address"},
	{"devops-reporter", true, stepSeam, nil, "bd and gt only, but reads /Users/Shared/gt-cloud/reports"},
	{"release-reporter", true, stepSeam, nil, "bd and gt only, but reads /Users/Shared/gt-cloud/releases and two files under HOME"},
	{"town-metrics", true, stepSeam, nil, "its lsof, ps and desktop-linux docker probes would measure the container, not the host"},

	{"landing_worker", false, stepHost, nil, "gates with make gate: go, make and golangci-lint"},
	{"rebuild_gt", false, stepHost, nil, "make install and launchctl"},
	{"rebuild_om", false, stepHost, nil, "go build"},
	{"rebuild_hm", false, stepHost, nil, "go build"},
	{"disk_maintenance", false, stepHost, nil, "every target is the Mac, Colima or Docker Desktop"},
	{"main_branch_test", false, stepHost, nil, "go test"},
	{"doctor_dog", false, stepSeam, nil, "gt doctor does not finish in the image; Dolt and the backup directory need the same addresses and mounts"},
	{"health_notify", false, stepSeam, nil, "the macOS notify scripts (deferred by the operator)"},

	{"handler", false, stepRuns, []string{"bash"}, "dispatches the plugins that run here"},
	{"tier_sweep", false, stepRuns, []string{"docker"}, "drives the sweep engine over DOCKER_HOST"},
	{"spec_dispatch", false, stepRuns, []string{"openshell", "kubectl"}, "gt sling through the gateway and the cluster"},
	{"jsonl_git_backup", false, stepRuns, []string{"dolt", "git"}, "dolt and git"},
	{"patrol_scan", false, stepRuns, []string{"git"}, "git and gt"},
	{"checkpoint_dog", false, stepRuns, []string{"git"}, "git"},
	{"overrun_guard", false, stepRuns, []string{"ps"}, "ps"},
	{"overseer_report", false, stepRuns, nil, "in process"},
	{"compactor_dog", false, stepRuns, nil, "in process"},
	{"scheduled_maintenance", false, stepRuns, nil, "in process"},
	{"scheduled_slings", false, stepRuns, nil, "gt sling"},
	{"wisp_reaper", false, stepRuns, nil, "in process"},
}

// jev_ patrols are in-process readers of the town's own files.
const jevPatrolPrefix = "jev_"

func stepNamed(name string) (daemonStep, bool) {
	for _, s := range daemonSteps {
		if s.name == name {
			return s, true
		}
	}
	if strings.HasPrefix(name, jevPatrolPrefix) {
		return daemonStep{name: name, class: stepRuns, why: "in process"}, true
	}
	return daemonStep{}, false
}

// ContainerOffNames are the names the container's settings must list in
// daemon.container_off: every step that is not stepRuns, sorted. The host agent
// runs the host-side subset, so the two lists come from this one table.
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
		if _, ok := stepNamed(name); !ok {
			fails = append(fails, name+": in daemon.container_off but unknown to the audit table (a typo, or a step to classify)")
		}
	}
	for _, name := range r.discoverSteps() {
		if _, ok := stepNamed(name.name); !ok {
			fails = append(fails, fmt.Sprintf("%s: a %s the audit table does not classify; add it to daemonSteps", name.name, name.kind()))
		}
	}
	for name := range st.Daemon.Patrols {
		if _, ok := stepNamed(name); !ok {
			fails = append(fails, name+": a patrol the audit table does not classify; add it to daemonSteps")
		}
	}
	return append(fails, r.missingStepPrograms(ctx)...)
}

// discoveredStep is a plugin directory found in the live town.
type discoveredStep struct {
	name   string
	plugin bool
}

func (d discoveredStep) kind() string { return "plugin" }

// discoverSteps lists the plugins the daemon would find: a directory with a
// plugin.md under <town>/plugins and under each rig's plugins directory. Dot
// directories are not plugins (the daemon disables one by renaming it so).
func (r *rehearsal) discoverSteps() []discoveredStep {
	var found []discoveredStep
	roots := []string{filepath.Join(r.f.liveRoot, "plugins")}
	if rigs, err := r.d.readDir(r.f.liveRoot); err == nil {
		for _, e := range rigs {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				roots = append(roots, filepath.Join(r.f.liveRoot, e.Name(), "plugins"))
			}
		}
	}
	for _, root := range roots {
		entries, err := r.d.readDir(root)
		if err != nil {
			continue // a rig without plugins
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if _, err := r.d.readFile(filepath.Join(root, e.Name(), "plugin.md")); err == nil {
				found = append(found, discoveredStep{name: e.Name(), plugin: true})
			}
		}
	}
	return found
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
