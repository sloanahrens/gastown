package config

import (
	"slices"
	"strings"
)

// DaemonStepClass is where a daemon step can run.
type DaemonStepClass string

const (
	// DaemonStepRuns: the step runs in the container once the parity beads
	// have landed; Needs lists the programs it execs, which the image must
	// carry.
	DaemonStepRuns DaemonStepClass = "runs-in-container"
	// DaemonStepHost: the step needs the host and runs there.
	DaemonStepHost DaemonStepClass = "host-side"
	// DaemonStepSeam: the step could run in the container with a seam that
	// does not exist yet; until it does it is off in the container.
	DaemonStepSeam DaemonStepClass = "needs-a-seam"
)

// DaemonStepKind says which namespace a name is in: a plugin is a directory
// with a plugin.md, a patrol is a key of daemon.patrols.
type DaemonStepKind string

const (
	DaemonStepPlugin DaemonStepKind = "plugin"
	DaemonStepPatrol DaemonStepKind = "patrol"
)

// DaemonStep is one row of the audit: a plugin (a directory with plugin.md) or
// a patrol (a key of daemon.patrols), what it needs, and why it is classed so.
type DaemonStep struct {
	Name  string
	Kind  DaemonStepKind
	Class DaemonStepClass
	Needs []string // programs it execs, for DaemonStepRuns
	Why   string
}

// daemonSteps is the audit, read from the code and the daemon log on 2026-11-01.
// The rows the container daemon's own log shows running on 2026-11-08 are
// stepRuns, and their reasons cite that log line (gt-0rqae4). A plugin or patrol
// the town has that this table does not name fails the rehearsal until it is
// classified.
//
// The container daemon cannot run everything the launchd daemon runs: some
// plugins and patrols need the Mac's toolchain, its Docker Desktop engine, its
// launchd or its files. The table classifies every one this town has, so a
// daemon.container_off name can be checked against it (gt-1oz0t). It lives
// beside the patrol schema because the settings validator and the daemon
// rehearsal both read it, so a name one knows the other knows (gt-yjzepk.4).
// The rows and their reasons describe this town (its plugins, its Mac and its
// Docker Desktop), so a fork that contributes upstream keeps them out of the
// shared diff.
var daemonSteps = []DaemonStep{
	{"polecat-image", DaemonStepPlugin, DaemonStepHost, nil, "built by hand each cycle: its directory is the image's build program, and go, make, crane and the Docker Desktop engine build and publish the polecat image; a stale image refuses every sandbox sling"},
	{"hm-sync", DaemonStepPlugin, DaemonStepHost, nil, "hm, make and go against hm-postgres on Docker Desktop"},
	{"cloud-check", DaemonStepPlugin, DaemonStepHost, nil, "launchctl, the Docker Desktop engine and /Users/Shared/gt-cloud"},
	{"app-promote", DaemonStepPlugin, DaemonStepRuns, []string{"gt"}, "runs in the container: daemon.log 2026-11-08 12:52:16 'Handler: script plugin app-promote skipped (exit 0 after 0s); nothing to do this run', a clean skip, so the log shows no promotion; its one exec is gt"},
	{"devops-reporter", DaemonStepPlugin, DaemonStepRuns, []string{"bd", "gt"}, "runs in the container: daemon.log 2026-11-08 12:46:00 'Handler: script plugin devops-reporter ok (exit 0 after 0s)', a clean exit with no work shown; the log does not show its reads of /Users/Shared/gt-cloud/reports, which the live compose does not mount"},
	{"release-reporter", DaemonStepPlugin, DaemonStepRuns, []string{"bd", "gt"}, "runs in the container: daemon.log 2026-11-08 12:46:00 'Handler: script plugin release-reporter ok (exit 0 after 0s)', a clean exit with no work shown; the log does not show its reads of /Users/Shared/gt-cloud/releases, which the live compose does not mount, or of two files under HOME"},
	{"town-metrics", DaemonStepPlugin, DaemonStepRuns, []string{"docker", "lsof", "ps"}, "runs in the container: daemon.log 2026-11-08 12:52:20 'Handler: script plugin town-metrics ok (exit 0 after 5s)'; its lsof, ps and docker probes read the container, not the host"},

	{"landing_worker", DaemonStepPatrol, DaemonStepRuns, []string{"om"}, "runs in the container: daemon.log 2026-11-08 12:55:40 'landing_worker: Landing of gt-gg9hv.68 ... left for the overseer', 1239 of its lines in the last 20000; reviews with om when review is on. A rig that sets post_land_command needs a toolchain this image does not carry, so the verdict stays main's Forgejo status (gt-gg9hv.7.1)"},
	{"rebuild_gt", DaemonStepPatrol, DaemonStepHost, nil, "make install and launchctl"},
	{"update", DaemonStepPatrol, DaemonStepHost, nil, "make install and launchctl install the binary on the host and reload the host's daemon"},
	{"rebuild_om", DaemonStepPatrol, DaemonStepHost, nil, "go build"},
	{"rebuild_hm", DaemonStepPatrol, DaemonStepHost, nil, "go build"},
	{"disk_maintenance", DaemonStepPatrol, DaemonStepRuns, []string{"kubectl"}, "runs in the container: daemon.log 2026-11-08 12:53:42 'disk_maintenance: cache trim gt-cache in openshell applied: route=shards n=15'; the cache trim execs kubectl (internal/daemon/cache_trim.go)"},
	{"main_branch_test", DaemonStepPatrol, DaemonStepHost, nil, "go test"},
	{"doctor_dog", DaemonStepPatrol, DaemonStepRuns, nil, "in process, so it runs in the container: daemon.log 2026-11-08 12:53:18 'doctor_dog: all clear (latency 14ms, connections 22/1000, orphans 0)'; its probes (internal/daemon/doctor_dog.go) exec no program"},
	{"health_notify", DaemonStepPatrol, DaemonStepSeam, nil, "the macOS notify scripts (deferred by the operator)"},

	{"dolt_server", DaemonStepPatrol, DaemonStepHost, nil, "starts and watches the Dolt server, which runs on the host; the container reaches it over host.docker.internal"},
	{"hm_sync", DaemonStepPatrol, DaemonStepHost, nil, "hm on the host (the plugin of the same name does the same)"},
	{"landing_infra", DaemonStepPatrol, DaemonStepHost, nil, "the landing worker's companion: watches Forgejo and its run queue for a landing worker that runs on the host"},
	{"resource_sampler", DaemonStepPatrol, DaemonStepSeam, nil, "ps rows for the host's components would describe the container"},

	{"handler", DaemonStepPatrol, DaemonStepRuns, []string{"bash"}, "dispatches the plugins that run here"},
	{"tier_sweep", DaemonStepPatrol, DaemonStepRuns, []string{"docker"}, "drives the sweep engine over DOCKER_HOST"},
	{"spec_dispatch", DaemonStepPatrol, DaemonStepRuns, []string{"openshell", "kubectl"}, "gt sling through the gateway and the cluster"},
	{"jsonl_git_backup", DaemonStepPatrol, DaemonStepRuns, []string{"dolt", "git"}, "dolt and git"},
	{"patrol_scan", DaemonStepPatrol, DaemonStepRuns, []string{"git"}, "git and gt"},
	{"checkpoint_dog", DaemonStepPatrol, DaemonStepRuns, []string{"git"}, "git"},
	{"overrun_guard", DaemonStepPatrol, DaemonStepRuns, []string{"ps"}, "ps"},
	{"overseer_report", DaemonStepPatrol, DaemonStepRuns, nil, "in process"},
	{"refinery", DaemonStepPatrol, DaemonStepRuns, nil, "retired and ignored; the key stays in the schema so old files load"},
	{"bird_reports", DaemonStepPatrol, DaemonStepRuns, nil, "in process"},
	{"findings_filer", DaemonStepPatrol, DaemonStepRuns, nil, "in process"},
	{"events_prune", DaemonStepPatrol, DaemonStepRuns, nil, "in process"},
	{"git_hygiene", DaemonStepPatrol, DaemonStepRuns, []string{"git"}, "git and git gc in the rigs' repositories under the town"},
	{"compactor_dog", DaemonStepPatrol, DaemonStepRuns, nil, "in process"},
	{"scheduled_maintenance", DaemonStepPatrol, DaemonStepRuns, nil, "in process"},
	{"scheduled_slings", DaemonStepPatrol, DaemonStepRuns, nil, "gt sling"},
	{"wisp_reaper", DaemonStepPatrol, DaemonStepRuns, nil, "in process"},
}

// daemonPatrolJevPrefix names in-process readers of the town's own files. They
// are patrols the audit classifies without a row each.
const daemonPatrolJevPrefix = "jev_"

// DaemonSteps returns the audit's rows.
func DaemonSteps() []DaemonStep {
	return append([]DaemonStep(nil), daemonSteps...)
}

// DaemonStepNamed reports whether the audit table, or the jev_ prefix rule for
// patrols, classifies name as a step of the given kind.
func DaemonStepNamed(name string, kind DaemonStepKind) bool {
	if kind == DaemonStepPatrol && strings.HasPrefix(name, daemonPatrolJevPrefix) {
		return true
	}
	return slices.ContainsFunc(daemonSteps, func(s DaemonStep) bool { return s.Name == name && s.Kind == kind })
}

// DaemonStepKnown reports whether name is a classified daemon step of either
// kind, which is what a daemon.container_off entry may name.
func DaemonStepKnown(name string) bool {
	return DaemonStepNamed(name, DaemonStepPlugin) || DaemonStepNamed(name, DaemonStepPatrol)
}
