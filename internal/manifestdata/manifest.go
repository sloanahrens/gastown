// Package manifestdata holds the data the cluster renderer trusts: the town
// root's ownership split, the daemon's secrets manifest, and the container
// paths the live compose writer and the renderer both name. Its imports are
// the standard library and the YAML module alone, so a package that renders
// from it reaches no process and no socket through it (gt-t5tf9g).
package manifestdata

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Owner names who a town-root path belongs to.
type Owner string

const (
	// OwnerDaemon marks a path the daemon writes; it moves with the town-state volume.
	OwnerDaemon Owner = "daemon"
	// OwnerSession marks a path a session writes; it stays on the host.
	OwnerSession Owner = "session"
	// OwnerShared marks a path both sides reach through a command, not a mount.
	OwnerShared Owner = "shared"
)

// Placeholders stand for the parts of a town the manifest cannot name once:
// the rigs the town registers and the bird names the pool hands out.
const (
	RigPlaceholder  = "<rig>"
	BirdPlaceholder = "<bird>"
)

// Entry is one row of the split: a town-root-relative path, its owner and why.
type Entry struct {
	Path  string `yaml:"path" json:"path"`
	Owner Owner  `yaml:"owner" json:"owner"`
	Why   string `yaml:"why" json:"why"`
	// Rollback marks the daemon-owned rows that hold one-way state: what the
	// daemon writes while it owns the town and the Mac tree cannot rebuild.
	// `gt town state export --rollback` of this tagged set, imported with
	// --merge-newer, is the replay half of a rollback (gt-gg9hv.68). Only a
	// daemon row can carry it, because only the daemon-owned set travels to
	// the volume.
	Rollback bool `yaml:"rollback,omitempty" json:"rollback,omitempty"`
	// Private marks a daemon-owned row whose files carry modes that must
	// survive a round trip (a kubeconfig, a config an agent may hold): import
	// keeps the archive's mode instead of masking it to 0755/0644.
	Private bool `yaml:"private,omitempty" json:"private,omitempty"`
}

// Manifest is the ordered list of split rows.
type Manifest struct {
	Entries []Entry `yaml:"entries"`
}

// DirNames are the town directory names the manifest's rig rows are built
// from. They are a parameter so this package reads no other package for them;
// the caller passes the names its town builds paths from, and each is required.
type DirNames struct {
	// RepoGit is the rig's shared bare repository directory name.
	RepoGit string
	// Mayor is the directory holding mayor configuration and state.
	Mayor string
	// Rig is the subdirectory holding the canonical clone.
	Rig string
}

// Default returns the town root's split as this town defines it, with the rig
// rows spelled from dirs.
//
// The rows tagged rollback: true are the one-way state the pod writes while it
// owns the town: the red-main statuses, the landing records, the spec dispatcher
// state, the events and feeds, and the maintenance and patrol state under
// daemon/. The rollback replays them onto the Mac tree with gt town state export
// --rollback and import --merge-newer. The tag sits on whole rows, so the replay
// carries all of daemon/, not only the two files named above (gt-gg9hv.68).
func Default(dirs DirNames) *Manifest {
	return &Manifest{Entries: []Entry{
		// Town-level state the daemon writes.
		{Path: "daemon", Owner: OwnerDaemon, Rollback: true, Why: "the daemon's own state, settings and patrol records; rollback replays it"},
		{Path: "logs", Owner: OwnerDaemon, Why: "daemon and session logs the daemon rotates"},
		{Path: "events", Owner: OwnerDaemon, Rollback: true, Why: "the town event store the daemon appends; rollback replays it"},
		{Path: ".events.jsonl", Owner: OwnerDaemon, Rollback: true, Why: "the town event feed the daemon appends; rollback replays it"},
		{Path: ".feed.jsonl", Owner: OwnerDaemon, Rollback: true, Why: "the feed the daemon appends; rollback replays it"},
		{Path: "mayor", Owner: OwnerDaemon, Why: "town config and the pool and roster records it writes"},
		{Path: ".beads", Owner: OwnerDaemon, Why: "workspace metadata the daemon reads and writes"},
		{Path: "plugins", Owner: OwnerDaemon, Why: "installed plugins the daemon loads"},
		{Path: ".claude-town", Owner: OwnerDaemon, Why: "the daemon's liveness probe reads session transcripts under it (internal/liveness)"},
		{Path: "settings", Owner: OwnerShared, Why: "edited by gt config daemon, read by the daemon (gt-yjzepk.4)"},
		{Path: "seat-refill.hold", Owner: OwnerShared, Why: "the operator's dispatch hold, written on the Mac and read by the pod each tick; internal/dispatch/hold.go HoldFilePath is its path and OperatorHold reads it"},
		{Path: "ESTOP", Owner: OwnerShared, Why: "the town-wide ESTOP sentinel, written on the Mac and read by the pod each tick; internal/estop/estop.go FilePath is its path and internal/dispatch/hold.go estopHold reads it"},
		{Path: "ESTOP.<rig>", Owner: OwnerShared, Why: "the per-rig ESTOP sentinel, written on the Mac and read by the pod each tick; internal/estop/estop.go RigFilePath is its path and internal/dispatch/hold.go RigHold reads it"},
		{Path: ".events.jsonl.lock", Owner: OwnerDaemon, Why: "the flock internal/events/events.go takes beside .events.jsonl before an append, so two writers cannot interleave"},
		{Path: ".dolt-archive", Owner: OwnerDaemon, Why: "the JSONL git backup repository internal/daemon/jsonl_git_backup.go pushes each database's history into"},
		{Path: ".beads-wisp", Owner: OwnerDaemon, Why: "the wisp config layer internal/wisp/config.go writes and internal/rig/config.go reads"},

		// Rig repositories the landing worker and promoter run git in.
		{Path: "<rig>/config.json", Owner: OwnerDaemon, Why: "the rig's registry record the daemon and landing worker read"},
		{Path: "<rig>/" + dirs.RepoGit, Owner: OwnerDaemon, Why: "the rig's bare repository the landing worker fetches and merges into; the name is constants.DirRepoGit, which internal/rig and the daemon build the path from"},
		{Path: "<rig>/" + dirs.Mayor + "/" + dirs.Rig, Owner: OwnerDaemon, Why: "the rig's canonical clone the landing worker and promoter land on; the name is constants.DirMayor and constants.DirRig, which rig.RepoPath resolves (all nine rigs of this town have it, read 2026-11-06)"},
		{Path: "<rig>/.beads", Owner: OwnerDaemon, Why: "the rig's beads workspace internal/rig/manager.go InitBeads creates and bd writes, as the town-level .beads row"},
		{Path: "<rig>/plugins", Owner: OwnerDaemon, Why: "the plugins the daemon's scanner loads from each rig (internal/plugin/scanner.go DiscoverAll); internal/cmd/daemon_rehearse_plugins.go reads it"},
		{Path: "<rig>/settings", Owner: OwnerShared, Why: "the rig's behavioral settings gt config writes (internal/config/loader.go SaveRigSettings) and the daemon's landing worker reads (internal/daemon/landing_worker.go rigPostLandCommand), as the town-level settings row"},
		{Path: "<rig>/directives", Owner: OwnerShared, Why: "directives a person writes and internal/config/directives.go reads as the rig's layer (two rigs of this town have it)"},
		{Path: "<rig>/scripts", Owner: OwnerShared, Why: "harness scripts a person writes by running the om repo's deploy.sh (contrib/gastown), which internal/doctor/harness_drift_check.go hashes; the refinery it deployed to is retired"},
		{Path: "<rig>/.gastown-harness-manifest.json", Owner: OwnerShared, Why: "the deploy-managed file list the om repo's deploy.sh (contrib/gastown) writes and internal/doctor/harness_drift_check.go reads; the refinery it deployed to is retired"},
		{Path: "<rig>/.runtime/namepool-state.json", Owner: OwnerDaemon, Why: "the name pool's state internal/polecat/namepool.go writes when a polecat is named"},
		{Path: "<rig>/.runtime/locks", Owner: OwnerDaemon, Why: "rig lock files a polecat session takes (internal/polecat/manager.go) as the town-level .runtime/locks row; export skips the files themselves and keeps the directory"},
		{Path: "<rig>/.runtime/session_id", Owner: OwnerSession, Why: "the session ID gt prime writes in the cwd of a session started at the rig root (internal/cmd/prime_session.go persistSessionID)"},
		{Path: "<rig>/.runtime/session_start_emitted", Owner: OwnerSession, Why: "the ID whose session_start gt prime has emitted, written in that session's cwd (internal/cmd/prime_session.go recordSessionStartEmitted)"},
		{Path: "<rig>/.runtime/last_handoff_ts", Owner: OwnerSession, Why: "the handoff cooldown gt handoff writes in the session's cwd (internal/cmd/handoff.go recordHandoffTimeIn)"},
		{Path: "<rig>/.runtime/agent.lock", Owner: OwnerSession, Why: "the identity lock gt prime takes for a worker directory (internal/lock/lock.go New)"},
		{Path: "<rig>/.runtime/agent.lock.flock", Owner: OwnerSession, Why: "the flock internal/lock/lock.go takes beside agent.lock so two Acquire calls cannot race"},
		{Path: "<rig>/.runtime/handoff_to_successor", Owner: OwnerSession, Why: "the marker gt handoff leaves for the successor's gt prime (internal/cmd/handoff.go writeHandoffMarker)"},

		// The daemon's subsystem directories under .runtime, and the state only
		// gt commands write. The .runtime/agents row states the ownership reason
		// for the gt-command rows, which name that row.
		{Path: ".runtime/jev", Owner: OwnerDaemon, Why: "jev's dataset the daemon writes"},
		{Path: ".runtime/polecat-spans", Owner: OwnerDaemon, Why: "polecat span records the daemon writes"},
		{Path: ".runtime/agents", Owner: OwnerDaemon, Why: "agent intent records and the bird registry. Every writer is the daemon or a gt command (gt done, gt sling, the bird commands). Owner daemon because after the wrapper cutover every gt command runs in the container, so the volume holds the only copy of what a gt command writes; until then a gt command on the Mac also writes the host's tree, a cutover prerequisite (gt-yjzepk.10). The gt-command rows below say their owner is as this row says"},
		{Path: ".runtime/landings", Owner: OwnerDaemon, Rollback: true, Why: "landing ledgers the daemon writes; rollback replays the landing records"},
		{Path: ".runtime/usage", Owner: OwnerDaemon, Why: "usage records the daemon writes"},
		{Path: ".runtime/sweep-runs", Owner: OwnerDaemon, Why: "tier sweep output the daemon writes"},
		{Path: ".runtime/overrun-guard", Owner: OwnerDaemon, Why: "overrun guard state the daemon writes"},
		{Path: ".runtime/metrics", Owner: OwnerDaemon, Why: "resource samples the daemon writes"},
		{Path: ".runtime/landing-om", Owner: OwnerDaemon, Why: "landing om evidence the daemon writes"},
		{Path: ".runtime/spec_dispatch", Owner: OwnerDaemon, Rollback: true, Why: "spec dispatcher state the daemon writes; rollback replays it"},
		{Path: ".runtime/heartbeats", Owner: OwnerDaemon, Why: "daemon heartbeats"},
		{Path: ".runtime/sandboxes", Owner: OwnerDaemon, Why: "sandbox records the daemon writes"},
		{Path: ".runtime/landing-logs", Owner: OwnerDaemon, Why: "landing stage logs the daemon writes"},
		{Path: ".runtime/attention", Owner: OwnerDaemon, Why: "the attention queue the daemon writes"},
		{Path: ".runtime/red-main", Owner: OwnerDaemon, Rollback: true, Why: "red-main status the landing worker writes; rollback replays the statuses"},
		{Path: ".runtime/locks", Owner: OwnerDaemon, Why: "town lock files the daemon takes; export skips the files themselves and keeps the directory"},
		{Path: ".runtime/container", Owner: OwnerDaemon, Private: true, Why: "the container's kubeconfig and state the daemon writes; the kubeconfig and the 0600 configs beside it must keep their modes through a copy"},
		{Path: ".runtime/daemon-image", Owner: OwnerDaemon, Why: "the daemon image build record"},
		{Path: ".runtime/reports", Owner: OwnerDaemon, Why: "the published bird reports gt report publish writes, one directory per bird (gt-yjzepk.3)"},
		{Path: ".runtime/ci-failures.jsonl", Owner: OwnerDaemon, Why: "the CI failure ledger internal/daemon/cifailures.go writes, one JSON line per failure"},
		{Path: ".runtime/cloud-check", Owner: OwnerDaemon, Why: "the cloud-check plugin's status.json, read by internal/cmd/dashboard.go dashboardCloudChecksPath"},
		{Path: ".runtime/deploy-watch.json", Owner: OwnerDaemon, Why: "the deploy watch's state internal/daemon/deploy_watch.go writes"},
		{Path: ".runtime/diskmaint", Owner: OwnerDaemon, Why: "the disk maintenance ledger gt maint writes (internal/cmd/maint.go) and internal/polecatimage reads before it prunes a registry"},
		{Path: ".runtime/dolt-archive", Owner: OwnerDaemon, Why: "the history archive internal/daemon/scheduled_maintenance_archive.go appends each database's dolt_log rows to, as dolt-log-<db>-<UTC date>.csv.gz; the path is the town's configured archive_dir (internal/config/daemon_config.go ArchiveDir), not a code default"},
		{Path: ".runtime/gate-logs", Owner: OwnerDaemon, Private: true, Why: "the landing worker's gate logs, one directory per rig (internal/land/gatelog.go); captured gate output can echo a token"},
		{Path: ".runtime/hm-src", Owner: OwnerDaemon, Private: true, Why: "the daemon's hm skew monitor's clone (internal/daemon/hm_skew.go hmSkewCloneDir); the clone's .git/config holds the rig's origin URL, which can carry credentials"},
		{Path: ".runtime/om-src", Owner: OwnerDaemon, Private: true, Why: "the om install's checkout, the default internal/ominstaller/resolve.go uses; the clone's .git/config holds the origin URL rebuild_om.go copies, which can carry credentials"},
		{Path: ".runtime/host", Owner: OwnerDaemon, Why: "the host probe file, written by the Mac's gt-mac-watch (internal/hostprobe/probes.go) and read by the daemon (internal/daemon/attention_host_file.go, disk_maintenance_disks.go)"},
		{Path: ".runtime/nudge_poller", Owner: OwnerDaemon, Why: "the nudge poller's PID files, written by internal/nudge/poller.go startPoller and pruned by the daemon (internal/daemon/pollution.go)"},
		{Path: ".runtime/openshell", Owner: OwnerDaemon, Private: true, Why: "the OpenShell gateway's log directory, made by the gt-installer gateway step (internal/installer/gateway.go writeUnit); the gateway holds provider credentials (internal/doctor/openshell_gateway_check.go lists them), so the directory is private"},
		{Path: ".runtime/patrol_scan", Owner: OwnerDaemon, Why: "the witness patrol's ledger internal/patrolscan/ledger.go writes and the daemon's patrol_scan step reads"},
		{Path: ".runtime/pids", Owner: OwnerDaemon, Why: "the pid files internal/session/pidtrack.go TrackSessionPID writes when a session starts; internal/doctor/stale_runtime_files_check.go reads them"},
		{Path: ".runtime/polecat-image-install.log", Owner: OwnerDaemon, Why: "the install log internal/installer/image.go writes when gt-installer installs the polecat image"},
		{Path: ".runtime/polecat-image.lock.flock", Owner: OwnerDaemon, Why: "the flock internal/polecatimage/lock.go takes beside the polecat-image lock directory so two image runs cannot claim it"},
		{Path: ".runtime/polecat-image.published", Owner: OwnerDaemon, Why: "the publish record internal/polecatimage/program.go writes and reads, so a push that did not finish is retried"},
		{Path: ".runtime/rigs", Owner: OwnerDaemon, Why: "the per-rig ready-to-land stamp internal/land/readystamp.go TouchReadyStamp writes and the dock stamp internal/rig/dockstamp.go writes"},
		{Path: ".runtime/sandbox-seat-ends", Owner: OwnerDaemon, Why: "the pending sandbox seat ends internal/daemon/sandbox_seat_end_pending.go writes"},
		{Path: ".runtime/sbx-k8s", Owner: OwnerDaemon, Private: true, Why: "the gt-sbx kind cluster's kubeconfig and manifests, written by deploy/k8s/kind/common.sh and read by internal/diskmaint/kubectl.go and internal/cmd/maint.go; the kubeconfig must keep its mode through a copy"},
		{Path: ".runtime/scheduler-dispatch.lock", Owner: OwnerDaemon, Why: "the dispatch lock internal/schedulerrun/schedulerrun.go takes"},
		{Path: ".runtime/signing", Owner: OwnerDaemon, Private: true, Why: "the keychain, its 0600 password file and the identity gt-installer signs with (internal/installer/sign.go ResolveSigning), which internal/binaryinstall/sign.go reads; the password file must keep its mode through a copy"},
		{Path: ".runtime/sling-startup-failures", Owner: OwnerDaemon, Why: "the per-bead startup failure records gt sling writes (internal/dispatch/startup_backoff.go), which the daemon's dispatchers read before re-slinging"},
		{Path: ".runtime/spec-lint", Owner: OwnerDaemon, Why: "the manual lint ledger gt spec lint appends (internal/specdispatch/lintlog.go)"},
		{Path: ".runtime/supervisor", Owner: OwnerDaemon, Why: "the action log internal/supervisor/supervisor.go writes when it restarts a session"},
		{Path: ".runtime/tier-sweep", Owner: OwnerDaemon, Why: "the tier sweep's per-rig records internal/daemon/tier_sweep.go writes"},
		{Path: ".runtime/tiertiming", Owner: OwnerShared, Why: "the shell timing log shelltier.TimingPath names (internal/shelltier/timing.go), which the shelltier runner appends to; the daemon's tier sweep (internal/daemon/tier_sweep.go tierSweepShellRunner) and make test-slow (Makefile SHELL_TESTS) both run that runner, so both write"},
		{Path: ".runtime/tiertrigger", Owner: OwnerDaemon, Why: "the tier trigger's state and per-tier run files cmd/tiertrigger writes through internal/tiertrigger/state.go"},
		{Path: ".runtime/town-metrics.lock", Owner: OwnerDaemon, Why: "the lock gt metrics takes while it samples (internal/cmd/metrics_run.go), run by the town-metrics daemon plugin"},
		{Path: ".runtime/townhealth.json", Owner: OwnerDaemon, Why: "the town health snapshot internal/townhealth/file.go records, the one file the daemon writes"},
		{Path: ".runtime/update", Owner: OwnerDaemon, Why: "the update run's state and history internal/update/state.go writes"},
		{Path: ".runtime/daemon-liveness.json", Owner: OwnerDaemon, Why: "the daemon liveness baseline the doctor check writes (internal/doctor/daemon_liveness_check.go), run by the daemon's doctor_dog"},
		{Path: ".runtime/daemon-respawn.json", Owner: OwnerDaemon, Why: "the daemon respawn baseline a retired doctor check wrote; that check is deleted (docs/design/gt-in-container.md lists the launchd-era daemon checks), and no code writes the file now, so a town may still hold it"},

		// .runtime state only gt commands write; each owner is as the
		// .runtime/agents row says.
		{Path: ".runtime/polecat-admission", Owner: OwnerDaemon, Why: "admission reservations gt sling writes (internal/cmd/polecat_capacity.go writePolecatAdmissionReservation); owner as the .runtime/agents row says"},
		{Path: ".runtime/polecat-pool-claims", Owner: OwnerDaemon, Why: "seat claims gt sling writes (internal/cmd/sling_pool.go poolSeatClaimDir); owner as the .runtime/agents row says"},
		{Path: ".runtime/polecat-prompts", Owner: OwnerDaemon, Why: "the polecat system prompts gt sling stores (internal/session/prompt_store.go storePolecatPrompt), read back by the landing worker for a rejection (internal/land/promptstamp.go); owner as the .runtime/agents row says"},
		{Path: ".runtime/rehearsal", Owner: OwnerDaemon, Why: "the container rehearsal's run directories, written and deleted by gt daemon rehearse (internal/cmd/daemon_rehearse.go chooseRunDir)"},
		{Path: ".runtime/hooks", Owner: OwnerDaemon, Why: "the hooks sync report gt hooks sync writes (internal/hooks/sync_report.go) and the hooks-sync doctor check reads"},
		{Path: ".runtime/composer_pending", Owner: OwnerDaemon, Why: "the composer stall clock the daemon heartbeat and the witness patrol write (internal/tmux/pending_input_clock.go)"},
		{Path: ".runtime/nudge_queue", Owner: OwnerDaemon, Why: "the queued nudges gt nudge writes (internal/nudge/queue.go Enqueue) and the target session's hook drains; owner as the .runtime/agents row says"},
		{Path: ".runtime/seat-refill.json", Owner: OwnerDaemon, Why: "the seat-refill plugin's episode state, read by internal/report/hour.go refillEpisodes"},
		{Path: ".runtime/bead-respawn-counts.json", Owner: OwnerDaemon, Why: "the per-bead respawn counts gt sling records (internal/polecat/respawn_count.go RecordBeadRespawnMark), read by the jev scorecard (internal/jev/scorecard/thrash.go)"},
		{Path: ".runtime/bead-respawn-counts.json.flock", Owner: OwnerDaemon, Why: "the flock internal/polecat/respawn_count.go takes beside bead-respawn-counts.json"},
		{Path: ".runtime/claude-plan", Owner: OwnerDaemon, Why: "the Claude plan samples ~/.claude/statusline-command.sh appends, with the closeouts and trim gt dashboard writes (internal/dashboard/claude_plan_month.go)"},
		{Path: ".runtime/watch", Owner: OwnerDaemon, Why: "the watch feed gt tail and gt report read (internal/cmd/tail_sources.go); gt dashboard rewrites spend.json there while a page is open (internal/cmd/dashboard.go)"},
		{Path: ".runtime/jev-dataset", Owner: OwnerDaemon, Why: "the dataset tables gt jev dataset writes (internal/jev/dataset/dataset.go)"},
		{Path: ".runtime/dashboard-load.jsonl", Owner: OwnerDaemon, Why: "the dashboard's host load history, appended by internal/cmd/dashboard_trend.go dashLoads.append"},
		{Path: ".runtime/sling-context-scan", Owner: OwnerDaemon, Why: "the full sling-context scan's outcome marker, recorded by the heartbeat (internal/schedulerrun/contexts.go) through internal/beads/sling_context_stamp.go RecordSlingContextScan"},

		// Session state a session writes in its own cwd, which is the town root
		// for a session started there.
		{Path: ".runtime/session_id", Owner: OwnerSession, Why: "the session ID gt prime writes in the cwd of a session started at the town root (internal/cmd/prime_session.go persistSessionID)"},
		{Path: ".runtime/session_start_emitted", Owner: OwnerSession, Why: "the ID whose session_start gt prime has emitted, written in that session's cwd (internal/cmd/prime_session.go recordSessionStartEmitted)"},
		{Path: ".runtime/last_handoff_ts", Owner: OwnerSession, Why: "the handoff cooldown gt handoff writes in the session's cwd (internal/cmd/handoff.go recordHandoffTimeIn)"},

		// Session-owned trees.
		{Path: "<rig>/crew", Owner: OwnerSession, Why: "each crew worker's persistent workspace"},
		{Path: "<rig>/polecats", Owner: OwnerSession, Why: "each polecat's worktree"},
		{Path: "<rig>/witness", Owner: OwnerSession, Why: "the witness session's own tree"},
		{Path: "<rig>/refinery", Owner: OwnerSession, Why: "the refinery session's own tree"},
		{Path: ".runtime/<bird>", Owner: OwnerSession, Why: "each bird session's own directory; gt report publish writes its daily report under .runtime/reports instead (gt-yjzepk.3)"},
	}}
}

// DaemonRuntimeNames returns the names of the daemon-owned directories
// directly under .runtime — jev, landings, usage, agents and the rest — sorted.
// A bird's published name may not equal one: the split under .runtime is read
// by name, so a session directory carrying a daemon name would be owned by two
// (gt-yjzepk.3).
func (m *Manifest) DaemonRuntimeNames() []string {
	names := DaemonSubsystemNames(m.Entries)
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// DaemonSubsystemNames returns the names of the daemon-owned directories
// directly under .runtime, such as usage for .runtime/usage.
func DaemonSubsystemNames(entries []Entry) map[string]bool {
	names := map[string]bool{}
	for _, e := range entries {
		if e.Owner != OwnerDaemon {
			continue
		}
		if name, ok := strings.CutPrefix(e.Path, ".runtime/"); ok && name != "" && !strings.Contains(name, "/") {
			names[name] = true
		}
	}
	return names
}

// Parse reads a manifest from its YAML mirror and refuses any row that cannot
// name a town-root path.
func Parse(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("reading the town-state manifest: %w", err)
	}
	for _, e := range m.Entries {
		if err := e.Validate(); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// Validate refuses an entry that cannot name a town-root path or owner.
func (e Entry) Validate() error {
	switch e.Owner {
	case OwnerDaemon, OwnerSession, OwnerShared:
	default:
		return fmt.Errorf("town-state entry %q: unknown owner %q (want %s, %s or %s)",
			e.Path, e.Owner, OwnerDaemon, OwnerSession, OwnerShared)
	}
	if e.Path == "" {
		return fmt.Errorf("town-state entry with owner %s has an empty path", e.Owner)
	}
	if path.IsAbs(e.Path) {
		return fmt.Errorf("town-state entry %q: the path must be relative to the town root", e.Path)
	}
	if strings.TrimSpace(e.Why) == "" {
		return fmt.Errorf("town-state entry %q: why is empty; say what the path holds", e.Path)
	}
	if e.Rollback && e.Owner != OwnerDaemon {
		return fmt.Errorf("town-state entry %q: rollback: true needs a daemon owner; only the daemon-owned set travels to the volume and back", e.Path)
	}
	if e.Private && e.Owner != OwnerDaemon {
		return fmt.Errorf("town-state entry %q: private: true needs a daemon owner; only the daemon-owned set is copied", e.Path)
	}
	for _, seg := range strings.Split(e.Path, "/") {
		switch seg {
		case "":
			return fmt.Errorf("town-state entry %q: empty path segment", e.Path)
		case ".", "..":
			return fmt.Errorf("town-state entry %q: %q is not a town-root path segment", e.Path, seg)
		}
		if strings.ContainsAny(seg, "<>") && !validPlaceholderSeg(seg) {
			return fmt.Errorf("town-state entry %q: unknown placeholder %q (want %s or %s, one per segment)",
				e.Path, seg, RigPlaceholder, BirdPlaceholder)
		}
	}
	return nil
}

// Validate refuses every entry that cannot name a town-root path or owner.
func (m *Manifest) Validate() error {
	for _, e := range m.Entries {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Rollback returns the rows tagged rollback: true, in manifest order. It is the
// one-way state list a rollback replays from the volume onto the Mac tree;
// `gt town state export --rollback` takes it from here so seed, rollback and
// the guard cannot disagree about what it is.
func (m *Manifest) Rollback() []Entry {
	var out []Entry
	for _, e := range m.Entries {
		if e.Rollback {
			out = append(out, e)
		}
	}
	return out
}

// Expand replaces the rig and bird placeholders with the names a town has. A
// placeholder may be a whole segment (<rig>) or part of one (ESTOP.<rig>); only
// the placeholder text is replaced, so the text beside it stays.
func (m *Manifest) Expand(rigs, birds []string) []Entry {
	out := make([]Entry, 0, len(m.Entries))
	for _, e := range m.Entries {
		segs := strings.Split(e.Path, "/")
		var expanded []Entry
		switch {
		case carries(segs, RigPlaceholder):
			for _, rig := range rigs {
				expanded = append(expanded, e.withPath(replaceSeg(segs, RigPlaceholder, rig)))
			}
		case carries(segs, BirdPlaceholder):
			for _, name := range birds {
				expanded = append(expanded, e.withPath(replaceSeg(segs, BirdPlaceholder, name)))
			}
		default:
			expanded = append(expanded, e)
		}
		out = append(out, expanded...)
	}
	return out
}

// withPath returns the entry with a different path.
func (e Entry) withPath(p string) Entry {
	e.Path = p
	return e
}

// replaceSeg returns segs with every occurrence of old swapped for name, inside
// a segment as well as for a whole one.
func replaceSeg(segs []string, old, name string) string {
	out := slices.Clone(segs)
	for i, s := range out {
		out[i] = strings.ReplaceAll(s, old, name)
	}
	return strings.Join(out, "/")
}

// carries reports whether a segment of segs holds ph, whole or with text beside it.
func carries(segs []string, ph string) bool {
	return slices.ContainsFunc(segs, func(s string) bool { return strings.Contains(s, ph) })
}

// validPlaceholderSeg reports whether every < and > in seg belongs to one rig or
// bird placeholder, and seg carries at most one of them.
func validPlaceholderSeg(seg string) bool {
	n := strings.Count(seg, RigPlaceholder) + strings.Count(seg, BirdPlaceholder)
	rest := strings.ReplaceAll(strings.ReplaceAll(seg, RigPlaceholder, ""), BirdPlaceholder, "")
	return n <= 1 && !strings.ContainsAny(rest, "<>")
}

// segMatches reports whether the path segment name fits pattern. A pattern with
// no placeholder fits only its own text. A placeholder fits any non-empty text in
// its place, so ESTOP.<rig> fits ESTOP.gastown but not ESTOP or ESTOPX.gastown.
func segMatches(pattern, name string) bool {
	for _, ph := range []string{RigPlaceholder, BirdPlaceholder} {
		if prefix, suffix, ok := strings.Cut(pattern, ph); ok {
			return len(name) > len(prefix)+len(suffix) && strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
		}
	}
	return pattern == name
}

// Lookup returns the deepest entry covering p, and whether one covers it.
func Lookup(entries []Entry, p string) (Entry, bool) {
	var best Entry
	found := false
	for _, e := range entries {
		if p != e.Path && !strings.HasPrefix(p, e.Path+"/") {
			continue
		}
		if !found || strings.Count(e.Path, "/") > strings.Count(best.Path, "/") {
			best, found = e, true
		}
	}
	return best, found
}

// SessionOwned returns the session-owned entry covering rel, a town-root-relative
// path, and whether one covers it. The rig and bird placeholders stand for any
// non-empty name in their segment, so a caller that does not know the town's
// rigs still finds a path under any rig's session tree. The deepest entry wins, and an
// entry with no placeholder wins over a placeholder at the same depth, so
// .runtime/usage stays the daemon's even though .runtime/<bird> also matches it.
// Letters are folded as the default macOS volume folds them, so the same path in
// another case is the same directory.
func (m *Manifest) SessionOwned(rel string) (Entry, bool) {
	rel = strings.Trim(path.Clean("/"+rel), "/")
	if rel == "" || rel == "." {
		return Entry{}, false
	}
	var best Entry
	found := false
	for _, e := range m.Entries {
		if !coversEntry(e.Path, rel) {
			continue
		}
		switch {
		case !found:
		case entryDepth(e.Path) > entryDepth(best.Path):
		case entryDepth(e.Path) == entryDepth(best.Path) && hasPlaceholder(best.Path) && !hasPlaceholder(e.Path):
		default:
			continue
		}
		best, found = e, true
	}
	if !found || best.Owner != OwnerSession {
		return Entry{}, false
	}
	return best, true
}

// coversEntry reports whether rel is pattern or lies under it, with the rig and
// bird placeholders matching a non-empty part of one segment (see segMatches).
func coversEntry(pattern, rel string) bool {
	ps := strings.Split(strings.ToLower(pattern), "/")
	rs := strings.Split(strings.ToLower(rel), "/")
	if len(rs) < len(ps) {
		return false
	}
	for i, seg := range ps {
		if !segMatches(seg, rs[i]) {
			return false
		}
	}
	return true
}

// entryDepth is how many segments an entry path has.
func entryDepth(p string) int { return strings.Count(p, "/") }

// hasPlaceholder reports whether an entry path holds the rig or bird placeholder,
// whole or in part of a segment.
func hasPlaceholder(p string) bool {
	segs := strings.Split(p, "/")
	return carries(segs, RigPlaceholder) || carries(segs, BirdPlaceholder)
}

// Names reports whether an entry path is exactly p.
func (m *Manifest) Names(p string) bool {
	for _, e := range m.Entries {
		if e.Path == p {
			return true
		}
	}
	return false
}

// IsContainer reports whether p is a directory that only holds entries.
func IsContainer(entries []Entry, p string) bool {
	for _, e := range entries {
		if strings.HasPrefix(e.Path, p+"/") {
			return true
		}
	}
	return false
}
