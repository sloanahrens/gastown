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
// launchd or its files. The audit in internal/config classifies every one the
// town has, so the container's daemon.container_off can be checked for a name
// the audit does not know (gt-1oz0t); the settings validator reads the same
// table (gt-yjzepk.4). The host-side and needs-a-seam names are
// daemon.container_off in the container's settings; the host agent runs the
// host-side ones on the host (gt-w9nux).

type (
	stepClass  = config.DaemonStepClass
	stepKind   = config.DaemonStepKind
	daemonStep = config.DaemonStep
)

const (
	// stepRuns: the step runs in the container once the parity beads have
	// landed; needs lists the programs it execs, which the image must carry.
	stepRuns = config.DaemonStepRuns
	// stepHost: the step needs the host and runs there.
	stepHost = config.DaemonStepHost
	// stepSeam: the step could run in the container with a seam that does not
	// exist yet; until it does it is off in the container.
	stepSeam = config.DaemonStepSeam
)

const (
	kindPlugin = config.DaemonStepPlugin
	kindPatrol = config.DaemonStepPatrol
)

// daemonSteps is the audit internal/config owns, so the rehearsal and the
// settings validator classify a name the same way.
var daemonSteps = config.DaemonSteps()

// stepNamed reports whether the audit table, or the jev_ prefix rule for
// patrols, classifies name as a step of the given kind.
func stepNamed(name string, kind stepKind) bool { return config.DaemonStepNamed(name, kind) }

// stepKnown reports whether name is a classified step of either kind, which is
// what a container_off entry may name.
func stepKnown(name string) bool { return config.DaemonStepKnown(name) }

// containerOffNames are the names daemon.container_off must list in the
// container's settings: every step that is not stepRuns, sorted.
func containerOffNames() []string {
	var names []string
	for _, s := range daemonSteps {
		if s.Class != stepRuns {
			names = append(names, s.Name)
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
		if s.Class == stepRuns || slices.Contains(off, s.Name) {
			continue
		}
		if p, ok := st.Daemon.Patrols[s.Name]; ok && p.Enabled != nil && !*p.Enabled {
			continue // already off for every supervisor
		}
		fails = append(fails, fmt.Sprintf("%s (%s) is not in daemon.container_off: %s", s.Name, s.Class, s.Why))
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
		if s.Class == stepRuns {
			for _, p := range s.Needs {
				needed[p] = append(needed[p], s.Name)
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
