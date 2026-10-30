package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/templates"
)

// daemonSupervisor is the service manager a provisioned daemon runs under
// (gt daemon enable-supervisor). Starting the daemon by hand while one of
// these is loaded is the trap gt-3jrm closes: the hand-started process
// holds daemon.lock, and the KeepAlive job then respawns every ~10 s, loses
// the flock and exits 1 for as long as the manual daemon lives —
// runDaemonEnableSupervisor refuses to provision in that state for the same
// reason, and start has to honor the same rule from the other side.
type daemonSupervisor struct {
	name string // "launchd" / "systemd" / "container", for messages
	// start, stop and bootstrap build their argv when used, so the container
	// kind's settings refusal (containerComposeProject) comes before any
	// command runs rather than after an argv built on the live town's default.
	start func() ([]string, error)
	// restart replaces a daemon that is running. A nil restart means the kind's
	// start restarts in place (launchd kickstart -k, systemctl restart); the
	// container kind sets it, because `up -d` does not recreate an unchanged
	// service and so a restart on the same image would do nothing (gt-qpeei.39).
	restart func() ([]string, error)
	// stop leaves the job unloaded, so the daemon stays down: signaling the
	// process alone leaves a KeepAlive job to respawn it (gt-sq9e).
	stop func() ([]string, error)
	// bootstrap loads the job when the service manager does not know it — the
	// state a stop leaves behind on macOS, where bootout unloads and
	// kickstart then cannot reach the job.
	bootstrap func() ([]string, error)
	// logs is the command that shows why the daemon did not come up. Only the
	// container sets it: its restart is proven by state.json rather than a pid,
	// so its logs are where a failed restart is explained.
	logs string
	load string // how to load the job when start says it is not loaded
	// fallback ends the start and restart errors, after load: how the
	// operator gets back to a hand-run daemon. A file kind has a file to
	// remove; the container kind has a town setting to unset.
	fallback string
	// state reads this job's live state. The file kinds leave it nil and read
	// through the control, which needs the kind alone; the container kind sets
	// it, because its container is named by the town's settings.
	state templates.SupervisorReader
	// owner is the owner the daemon this supervisor starts will name for
	// itself (daemon.DetectSelf in that process), which the start guard needs
	// to judge a start the way Run will. The container kind refuses here when
	// the town does not set both container fields, so no guard ever reads the
	// live container's name off a default.
	owner func() (daemon.Owner, error)

	ctl *daemonControl // what runs the commands above and reads the job
}

// jobState reads the live state of this supervisor's job.
func (s *daemonSupervisor) jobState() templates.SupervisorState {
	if s.state != nil {
		return s.state(s.name)
	}
	return s.ctl.stateFor(s.name)
}

// jobIsMissing reports whether a failed command left the job in the state the
// caller recovers from by loading it. The container kind never reads as
// missing: its daemon lives in the container, so an absent container runs no
// daemon to replace, and loading the whole project is not the repair a failed
// container command asks for.
func (s *daemonSupervisor) jobIsMissing() bool {
	if s.name == config.SupervisorContainer {
		return false
	}
	st := s.jobState()
	return st.Err == nil && !st.Loaded
}

// errSupervisorRefused marks a builder's refusal, in which no command ran.
// The start and restart waits and loadJob's retry special-case it: waiting on
// or retrying something that never ran can only misreport what happened.
var errSupervisorRefused = errors.New("supervisor refused to run")

// runArgv builds one of the supervisor's argv and runs it.
func (s *daemonSupervisor) runArgv(build func() ([]string, error)) error {
	argv, err := build()
	if err != nil {
		return fmt.Errorf("%w: %w", errSupervisorRefused, err)
	}
	return s.ctl.run(argv)
}

// runArgvEnv builds one of the supervisor's argv and runs it with extra
// environment. Only the one start --takeover makes uses it; every other
// command goes through runArgv and the plain argv seam, which carries none.
func (s *daemonSupervisor) runArgvEnv(build func() ([]string, error), env []string) error {
	argv, err := build()
	if err != nil {
		return fmt.Errorf("%w: %w", errSupervisorRefused, err)
	}
	return s.ctl.runEnv(argv, env)
}

// runStartArgv runs a start-side argv with env when there is any, through the
// env-carrying seam, and through the plain argv seam otherwise.
func (s *daemonSupervisor) runStartArgv(build func() ([]string, error), env []string) error {
	if len(env) > 0 {
		return s.runArgvEnv(build, env)
	}
	return s.runArgv(build)
}

// daemonControl is what the daemon's start, stop, restart and status paths
// take from the host: supervisor detection (the town's daemon.supervisor
// setting first, then GOOS and the file paths), running the supervisor's
// command, reading the job's live state, the direct spawn, the stop, the
// running check and the waits between polls. realDaemonControl wires the real
// ones; unit tests build their own.
type daemonControl struct {
	goos      string
	plistPath func() (string, error)
	unitPath  func() (string, error)
	uid       int
	// patrolConfig reads the town's daemon settings, of which the supervisor
	// kind is what picks the container supervisor: GOOS cannot. A nil seam
	// (the host-detection unit tests) reads as a town that sets none.
	patrolConfig func(townRoot string) (*config.DaemonPatrolConfig, error)
	// dockerContext is the --context every container argv carries. Empty
	// means defaultDockerContext: the Docker Desktop context the live town
	// runs in, never the global one, which nothing here may change.
	dockerContext string
	run           func(argv []string) error
	// runEnv runs argv with env added to the process environment: the env
	// carrying seam beside run, for the one start --takeover makes (the
	// compose file passes GT_DAEMON_TAKEOVER through to the container).
	runEnv func(argv []string, env []string) error
	// guard is the startup guard every start path asks before it launches
	// anything: daemon.GuardStart, with self the owner the started daemon will
	// name. nil reads as the real guard, so a control built without one can
	// never skip it.
	guard func(townRoot string, self daemon.Owner, takeover bool) error
	// readState reads state.json for the container restart's wait: nil when the
	// file is missing, an error when it cannot be read or does not parse. nil
	// reads the real file (readCutoverState).
	readState func(townRoot string) (*daemon.State, error)
	// syncFile brings the supervisor file up to date and reports whether it
	// rewrote it (syncSupervisorFile); nil reads as that. Only start reads it
	// through this seam, so a test can say the file was rewritten.
	syncFile func(townRoot string) bool
	// dockerInspect runs one docker command and returns its stdout; a failure
	// is a *templates.DockerInspectError carrying its stderr, so the container
	// kind's state read never parses the two streams as one.
	dockerInspect templates.ContainerDocker
	stateFor      templates.SupervisorReader
	spawn         func(townRoot string, env []string) (int, error)
	stopDirect    func(townRoot string) error
	isRunning     func(townRoot string) (bool, int, error)
	// install writes a repaired supervisor file (installSupervisorFile).
	install func(path, content string) error
	// statusLine renders the Supervised: line of gt daemon status
	// (supervisedLine).
	statusLine func(townRoot string, lockPID int) string
	// sleep waits pollInterval between lock checks and loadRetryInterval
	// between bootstrap attempts.
	sleep             func(time.Duration)
	pollInterval      time.Duration
	loadRetryInterval time.Duration
}

// realDaemonControl is the daemonControl of this host.
func realDaemonControl() *daemonControl {
	c := &daemonControl{
		goos:         runtime.GOOS,
		plistPath:    templates.LaunchdPlistPath,
		unitPath:     templates.SystemdUnitPath,
		uid:          os.Getuid(),
		patrolConfig: daemon.ReadPatrolConfig,
		run: func(argv []string) error {
			out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed argv from detectDaemonSupervisor
			return commandError(out, err)
		},
		runEnv: func(argv, env []string) error {
			out, err := commandWithEnv(argv, env).CombinedOutput()
			return commandError(out, err)
		},
		dockerInspect:     templates.RunDockerInspect,
		stateFor:          supervisorJobState,
		stopDirect:        daemon.StopDaemon,
		isRunning:         daemon.IsRunning,
		install:           installSupervisorFile,
		sleep:             clockwork.NewRealClock().Sleep,
		pollInterval:      daemonPollInterval,
		loadRetryInterval: supervisorLoadRetryInterval,
	}
	c.spawn = func(townRoot string, env []string) (int, error) { return spawnDaemonProcess(c, townRoot, env) }
	c.statusLine = c.supervisedLine
	return c
}

// commandError is the error a supervisor command's failure reports: the exit
// error with the command's combined output after it.
func commandError(out []byte, err error) error {
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// commandWithEnv builds the command the env-carrying seam runs: argv with env
// added to this process's environment.
func commandWithEnv(argv, env []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // fixed argv from detectDaemonSupervisor
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

// supervisorJobState is the live job read of this host, keyed by kind alone.
// The container kind is not one it can read: which container a town runs is
// said only by that town's settings, so a container town's supervisor carries
// its own reader (containerJobState). Unknown, never "not loaded", is the
// honest answer where no town was named.
func supervisorJobState(kind string) templates.SupervisorState {
	if kind == config.SupervisorContainer {
		return templates.SupervisorState{Kind: kind, LastExit: -1, Err: errContainerStateUntowned}
	}
	return templates.SupervisorJobState(kind)
}

// errContainerStateUntowned is the container kind's state where the read was
// asked by kind alone and no town's settings named its container.
var errContainerStateUntowned = errors.New("the container kind's state is read per town: no town's settings name its container here")

// containerJobState is the state reader of the container town whose settings
// are cfg: the container cfg names, inspected under this control's docker
// context. The town's own settings are the only source of that name, so a
// scratch town inspects its scratch container and never the live one.
func (c *daemonControl) containerJobState(cfg *config.DaemonPatrolConfig) templates.SupervisorReader {
	if _, err := containerComposeProject(cfg); err != nil {
		return unreadContainerState(err)
	}
	return templates.ContainerJobState(c.dockerInspect, c.dockerCLIContext(), cfg.ContainerName())
}

// unreadContainerState is the reader of a container town whose settings do not
// name a container this binary may act on: unknown, and no docker command run.
func unreadContainerState(err error) templates.SupervisorReader {
	return func(kind string) templates.SupervisorState {
		return templates.SupervisorState{Kind: kind, LastExit: -1, Err: err}
	}
}

// supervisedLine renders the "Supervised:" value gt daemon status prints for
// townRoot. A container town reads its own container, which its settings name;
// every other town is the file reading, which no per-user file can supply for a
// container.
func (c *daemonControl) supervisedLine(townRoot string, lockPID int) string {
	cfg, err := c.townDaemonConfig(townRoot)
	if err != nil {
		return unverifiedSupervisorLine(fmt.Errorf("reading the town's daemon settings: %w", err))
	}
	kind, err := cfg.SupervisorKind()
	if err != nil {
		return unverifiedSupervisorLine(err)
	}
	if kind != config.SupervisorContainer {
		return templates.SupervisorStatusLine(townRoot, lockPID, c.stateFor)
	}
	return c.containerJobState(cfg)(config.SupervisorContainer).StatusLine(lockPID)
}

// unverifiedSupervisorLine is what status prints where the town's settings
// cannot say which supervisor it has: "none" would claim it has none.
func unverifiedSupervisorLine(err error) string {
	return fmt.Sprintf("unknown (unverified - %v)", err)
}

// defaultDockerContext is the Docker context the container supervisor's argv
// carry unless the control names another: Docker Desktop's Linux engine, the
// one the live town runs in. Nothing here reads or changes the global context.
const defaultDockerContext = "desktop-linux"

// townDaemonConfig reads the town's daemon settings, the document that holds
// daemon.supervisor, container_name and compose_project. It is read for every
// supervisor decision because only it says a town is a container town, and an
// unreadable file is refused for every kind rather than guessed at. A nil seam
// (the unit tests of host detection) reads as a town that sets none.
func (c *daemonControl) townDaemonConfig(townRoot string) (*config.DaemonPatrolConfig, error) {
	if c.patrolConfig == nil {
		return nil, nil
	}
	return c.patrolConfig(townRoot)
}

// supervisorFilePath returns the file this town's supervisor is described by
// and the kind it belongs to ("launchd" / "systemd" / "container"), or
// ("", "") on a host with no supported supervisor. The plist or unit need not
// exist — whether it does is what the detection reads — but the compose file
// of a container town is checked when that supervisor is built, because a
// missing one there is an error rather than "no supervisor". Both the
// detection below and the file reconciliation use this, so the two never
// disagree about which file they are talking about.
//
// A town that sets daemon.supervisor names the kind, which is the only way a
// container town gets one: GOOS cannot tell it apart from the Mac it runs on.
// A setting this host has no manager for is an error rather than a silent
// fallback to the host's own, which would drive a manager the operator did
// not ask for.
func (c *daemonControl) supervisorFilePath(townRoot string, cfg *config.DaemonPatrolConfig) (path, kind string, err error) {
	configured, err := cfg.SupervisorKind()
	if err != nil {
		return "", "", err
	}
	if configured == config.SupervisorContainer {
		return composeFilePath(townRoot), config.SupervisorContainer, nil
	}
	hostPath, hostKind := c.hostSupervisorFile()
	if configured != "" && configured != hostKind {
		return "", "", fmt.Errorf("daemon.supervisor is %q, but this host has no %s: leave it unset to detect the host's service manager", configured, configured)
	}
	return hostPath, hostKind, nil
}

// hostSupervisorFile returns the supervisor file this host would use for a
// daemon and the kind it belongs to, from GOOS alone.
func (c *daemonControl) hostSupervisorFile() (path, kind string) {
	switch c.goos {
	case "darwin":
		if p, err := c.plistPath(); err == nil {
			return p, config.SupervisorLaunchd
		}
	case "linux":
		if p, err := c.unitPath(); err == nil {
			return p, config.SupervisorSystemd
		}
	}
	return "", ""
}

// hostSupervisorJob is the supervisor enable-supervisor is about to provision
// on this host, as far as the start guard needs it: its kind and owner, none of
// its commands. nil on a host with no supported supervisor, which the guard
// then judges as a hand-run start.
func (c *daemonControl) hostSupervisorJob() *daemonSupervisor {
	path, kind := c.hostSupervisorFile()
	if kind == "" {
		return nil
	}
	return c.fileSupervisor(path, kind)
}

// composeFilePath is where a container town's compose file lives: rendered
// when the town is set up, read by the container supervisor's argv.
func composeFilePath(townRoot string) string {
	return config.ContainerComposeFile(townRoot)
}

// detectDaemonSupervisor reports the supervisor provisioned FOR townRoot, or
// nil when the daemon is meant to be run by hand (no plist / unit file
// installed) or the installed one belongs to another workspace: the file is
// per user, and its WorkingDirectory is the town it was provisioned for —
// kickstarting it from a different town would restart that town's daemon
// and then wait for this town's lock in vain. A file that exists but cannot
// be read is an error, never "no supervisor": the whole point of the check
// is to never hand-spawn beside a provisioned one.
//
// A town that sets daemon.supervisor to container gets the container
// supervisor and never the host's: the setting is the one fact GOOS cannot
// supply, so a launchd job the host still carries is not what this town runs
// under.
func (c *daemonControl) detectDaemonSupervisor(townRoot string) (*daemonSupervisor, error) {
	cfg, err := c.townDaemonConfig(townRoot)
	if err != nil {
		return nil, fmt.Errorf("reading the town's daemon settings: %w", err)
	}
	p, kind, err := c.supervisorFilePath(townRoot, cfg)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "":
		// No resolvable home means nowhere a plist could have been
		// provisioned; same reading as templates.SupervisorStatus.
		return nil, nil
	case config.SupervisorContainer:
		return c.containerSupervisor(p, cfg)
	case config.SupervisorLaunchd, config.SupervisorSystemd:
		isFor, err := templates.SupervisorFileIsFor(p, townRoot)
		if err != nil || !isFor {
			return nil, err
		}
		return c.fileSupervisor(p, kind), nil
	}
	return nil, nil
}

// containerSupervisor is the supervisor a town with daemon.supervisor
// "container" runs its daemon under: docker compose, against the compose file
// at <townRoot>/daemon/container/compose.yml.
func (c *daemonControl) containerSupervisor(composeFile string, cfg *config.DaemonPatrolConfig) (*daemonSupervisor, error) {
	// A missing compose file is an error, not "no supervisor": the town asked
	// for the container kind, and a nil supervisor would hand-spawn a daemon
	// beside the container the file describes.
	if _, err := os.Stat(composeFile); err != nil {
		return nil, fmt.Errorf("daemon.supervisor is container, so its compose file is required: %w", err)
	}
	compose := func(service ...string) func() ([]string, error) {
		return func() ([]string, error) { return c.composeArgv(composeFile, cfg, service...) }
	}
	// The load hint is printed to the operator, so a refused town gets a
	// placeholder for its project rather than the live default.
	project, err := containerComposeProject(cfg)
	if err != nil {
		project = "<compose_project>"
	}
	return &daemonSupervisor{
		name:  config.SupervisorContainer,
		start: compose("up", "-d", "daemon"),
		// --force-recreate stops the old container on its stop_grace_period
		// (SIGTERM, so a clean stop is recorded) and starts a new one that
		// re-reads the compose file and its environment, under the same owner
		// id. `compose restart` would keep the container and do neither.
		restart:   compose("up", "-d", "--force-recreate", "daemon"),
		stop:      compose("stop", "daemon"),
		bootstrap: compose("up", "-d"),
		load:      fmt.Sprintf("docker --context %s compose -p %s -f %s up -d daemon", c.dockerCLIContext(), project, composeFile),
		fallback:  "or unset daemon.supervisor to run the daemon by hand",
		logs:      fmt.Sprintf("docker --context %s logs %s", c.dockerCLIContext(), cfg.ContainerName()),
		state:     c.containerJobState(cfg),
		owner: func() (daemon.Owner, error) {
			if _, err := containerComposeProject(cfg); err != nil {
				return daemon.Owner{}, err
			}
			return daemon.Owner{Kind: config.SupervisorContainer, ID: cfg.ContainerName()}, nil
		},
		ctl: c,
	}, nil
}

// composeArgv builds one container-supervisor argv: the explicit context, the
// town's compose project and file, then the compose subcommand.
func (c *daemonControl) composeArgv(composeFile string, cfg *config.DaemonPatrolConfig, service ...string) ([]string, error) {
	project, err := containerComposeProject(cfg)
	if err != nil {
		return nil, err
	}
	argv := []string{"docker", "--context", c.dockerCLIContext(), "compose", "-p", project, "-f", composeFile}
	return append(argv, service...), nil
}

// dockerCLIContext is the --context the container argv carry: the field, or
// the default when a control does not set one.
func (c *daemonControl) dockerCLIContext() string {
	if c.dockerContext == "" {
		return defaultDockerContext
	}
	return c.dockerContext
}

// townDaemonKind reads the town's daemon settings and reports whether they name
// the container supervisor, whether or not that supervisor could be built: a
// stop needs the kind even when detection failed (a missing compose file is a
// detection error). The settings come back with it for the caller that has to
// build the compose command detection could not. An unreadable read or a kind
// this package does not know answers "cannot tell" as an error, and the caller
// refuses rather than guesses.
func (c *daemonControl) townDaemonKind(townRoot string) (*config.DaemonPatrolConfig, bool, error) {
	cfg, err := c.townDaemonConfig(townRoot)
	if err != nil {
		return nil, false, err
	}
	kind, err := cfg.SupervisorKind()
	if err != nil {
		return cfg, false, err
	}
	return cfg, kind == config.SupervisorContainer, nil
}

// containerStopError is what a stop reports instead of signaling a container
// town's lock-file PID. cause is why the supervisor did not settle the daemon
// (an unknown job state, a stopped command that failed), or nil when the
// supervisor ran and something else still holds the lock.
//
// The signal is the one thing that must not happen here: daemon.lock's PID
// comes from the container's namespace, so on this host it names nothing — or,
// once the number is reused, an unrelated process. The error carries the
// compose command that does stop the container, built from the town's settings
// when detection could not supply the supervisor.
func (c *daemonControl) containerStopError(townRoot string, cfg *config.DaemonPatrolConfig, sup *daemonSupervisor, cause error) error {
	lines := []string{"the daemon is still running, and a container town's daemon is stopped through its supervisor, never by a signal: daemon.lock's PID comes from the container's namespace and names no process on this host"}
	if cause != nil {
		lines = append(lines, cause.Error())
	}
	if argv, err := c.containerStopArgv(townRoot, cfg, sup); err != nil {
		lines = append(lines, err.Error())
	} else {
		lines = append(lines, "stop it with: "+strings.Join(argv, " "))
	}
	return errors.New(strings.Join(lines, "\n  "))
}

// containerStopArgv builds the compose command that stops a container town's
// daemon: the detected supervisor's own argv, or the same argv from the
// settings townDaemonKind read when detection failed and left no supervisor.
func (c *daemonControl) containerStopArgv(townRoot string, cfg *config.DaemonPatrolConfig, sup *daemonSupervisor) ([]string, error) {
	if sup != nil {
		return sup.stop()
	}
	return c.composeArgv(composeFilePath(townRoot), cfg, "stop", "daemon")
}

// containerComposeProject returns the compose project the container supervisor
// runs under, and is where that supervisor refuses a town whose settings it
// cannot act on. The project is the field the argv carry, so the refusal keys
// on it: a town that left it unset would run under the live project's default
// and could stop or replace the live container (gt-ix1kz). Both fields must
// then be set, in the shapes ValidateContainerNames accepts.
func containerComposeProject(cfg *config.DaemonPatrolConfig) (string, error) {
	if cfg == nil || cfg.ComposeProjectSetting == "" {
		return "", errContainerSettingsUnset
	}
	if err := cfg.ValidateContainerNames(); err != nil {
		return "", err
	}
	return cfg.ComposeProject(), nil
}

// errContainerSettingsUnset is the refusal of a container town that does not
// set both fields. It names both, since either one left out inherits the live
// default.
var errContainerSettingsUnset = errors.New("daemon.supervisor is container, but the town does not set both daemon.container_name and daemon.compose_project: set both, so a scratch town cannot act on the live daemon's container")

// supervisorState reads one supervisor job's live state through this control's
// state seam, and through the host's own reader when none is set, so a test
// that fakes the state never runs systemctl.
func (c *daemonControl) supervisorState(kind string) templates.SupervisorState {
	if c.stateFor == nil {
		return supervisorJobState(kind)
	}
	return c.stateFor(kind)
}

// fileSupervisor is the launchd or systemd supervisor, whose argv are fixed
// once the file path is known.
func (c *daemonControl) fileSupervisor(p, kind string) *daemonSupervisor {
	argv := func(a ...string) func() ([]string, error) {
		return func() ([]string, error) { return a, nil }
	}
	switch kind {
	case config.SupervisorLaunchd:
		job := fmt.Sprintf("gui/%d/%s", c.uid, templates.LaunchdLabel)
		return &daemonSupervisor{
			name:      config.SupervisorLaunchd,
			start:     argv("launchctl", "kickstart", "-k", job),
			stop:      argv("launchctl", "bootout", job),
			bootstrap: argv("launchctl", "bootstrap", fmt.Sprintf("gui/%d", c.uid), p),
			load:      fmt.Sprintf("launchctl bootstrap gui/%d %s", c.uid, p),
			fallback:  "or remove its file to run the daemon by hand",
			owner: func() (daemon.Owner, error) {
				return daemon.Owner{Kind: config.SupervisorLaunchd, ID: templates.LaunchdLabel}, nil
			},
			ctl: c,
		}
	case config.SupervisorSystemd:
		return &daemonSupervisor{
			name:      config.SupervisorSystemd,
			start:     argv("systemctl", "--user", "restart", templates.SystemdUnit),
			stop:      argv("systemctl", "--user", "stop", templates.SystemdUnit),
			bootstrap: argv("systemctl", "--user", "enable", "--now", templates.SystemdUnit),
			load:      "systemctl --user daemon-reload && systemctl --user enable --now " + templates.SystemdUnit,
			fallback:  "or remove its file to run the daemon by hand",
			// A systemd daemon names itself by the unit's invocation id, so
			// the guard must be told the id the unit runs now: read before
			// any restart, it makes the daemon that comes back this unit's
			// own rather than a second daemon's start beside it (gt-h7iy7).
			// An id that cannot be read leaves the owner id empty, which
			// equals no recorded owner: the guard then judges the recorded
			// owner on its heartbeat and probe, as it did before.
			owner: func() (daemon.Owner, error) {
				owner := daemon.Owner{Kind: config.SupervisorSystemd}
				if st := c.supervisorState(config.SupervisorSystemd); st.Err == nil {
					owner.ID = st.InvocationID
				}
				return owner, nil
			},
			ctl: c,
		}
	}
	return nil
}

// syncSupervisorFile brings an installed supervisor file up to date with the
// binary now running, and reports whether it had to rewrite it (gt-x872).
//
// A supervisor file is derived state, and one part of it is compiled into the
// binary that wrote it: launchd's ExitTimeOut, from daemon.ShutdownBudget. Only
// a provision writes one and a provision only runs on request, so a town
// provisioned before ShutdownBudget grew keeps the old value for as long as
// nobody rewrites the file — and launchd SIGKILLs the daemon part-way through
// its shutdown on every restart in the meantime, which is the failure the key
// exists to prevent. What is repairable, and what is deliberately left alone,
// is templates.SupervisorFileRepair's call; this is the installation half.
//
// A repair that cannot be made is warned about and then dropped: a job file
// that is behind is a reason to restart the daemon under a worse ExitTimeOut,
// never a reason to refuse to start it at all.
//
// A rewrite only changes the file. Making the service manager act on it is
// separate and deliberate: both managers cache a job definition at load time,
// so a changed file reaches the running job only through the unload and load
// that startFromFile does, never through an in-place restart.
//
// A container town is left out of this: its supervisor file is the compose
// file, and templates.SupervisorFileRepair repairs launchd's ExitTimeOut only,
// so there is nothing here to rewrite. A launchd plist the host still carries is
// not this town's supervisor, and is left as it is.
func (c *daemonControl) syncSupervisorFile(townRoot string) (rewritten bool) {
	cfg, err := c.townDaemonConfig(townRoot)
	if err != nil {
		style.PrintWarning("could not read the town's daemon settings: %v", err)
		return false
	}
	path, kind, err := c.supervisorFilePath(townRoot, cfg)
	if err != nil {
		style.PrintWarning("could not tell which supervisor file this town uses: %v", err)
		return false
	}
	if kind == "" {
		return false
	}
	content, repair, err := templates.SupervisorFileRepair(path, kind, townRoot, daemon.ShutdownBudget)
	if err == nil && repair {
		err = c.install(path, content)
	}
	if err != nil {
		style.PrintWarning("could not bring the %s job file up to date: %v", kind, err)
		return false
	}
	return repair
}

// installSupervisorFile writes content over the supervisor file at path,
// whole and moved into place: a truncate-and-write leaves a half-written plist
// behind if this process dies mid-write, and the next bootstrap would then
// fail on it — no daemon, and nothing to say why. Same directory, so the
// rename is atomic.
func installSupervisorFile(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".com.gastown.daemon.*")
	if err != nil {
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename below succeeded
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing supervisor file: %w", err)
	}
	return nil
}

// reconcileSupervisorJob brings the supervisor job for townRoot in line with
// the binary now running, for callers that found the daemon already up:
// startDaemon and restartDaemon do the same reconciliation on their own paths,
// so this is the town-boot version of it. The returned note is "" when there
// was nothing to do — no supervisor, no file, or a current one.
//
// It is a restart, not just a file write: a service manager caches a job
// definition when it loads it, so a rewritten file does not reach the running
// job by itself (gt-x872). That is an interruption the caller did not ask for
// — bounded, and paid once per change of the file's derived values, since the
// rewrite is what makes the next boot find the file current. The alternative
// is an ExitTimeOut that never takes effect on any town that is not
// re-provisioned, which is the whole of the bug.
func (c *daemonControl) reconcileSupervisorJob(townRoot string, daemonPID int) (note string, err error) {
	sup, err := c.detectDaemonSupervisor(townRoot)
	if err != nil || sup == nil {
		return "", err
	}
	if !c.syncSupervisorFile(townRoot) {
		return "", nil
	}

	// The reload below starts a daemon, so it asks the guard first. This is
	// after the file sync on purpose: a gt up that reloads nothing never asks,
	// and a daemon already running under its own owner is not judged here
	// (gt-qpeei.28).
	if err := c.guardStart(townRoot, sup, false); err != nil {
		return "", fmt.Errorf("not reloading the %s job from its rewritten file: %w", sup.name, err)
	}
	if err := sup.startFromFile(); err != nil {
		return "", fmt.Errorf("reloading the %s job from its rewritten file: %w", sup.name, err)
	}
	// The job is back but the daemon it starts needs a moment to take the
	// lock, and the caller reads that lock for its own status line. Unlike the
	// restart paths, the PID that must be replaced is the one the caller
	// already read: a wait that stopped at "the lock is held" would report the
	// daemon on its way out and call this a success.
	if _, err := c.waitForRestart(townRoot, daemonPID); err != nil {
		return "", fmt.Errorf("daemon did not come back after the %s job was reloaded from its file: %w", sup.name, err)
	}
	return fmt.Sprintf("%s job reloaded from an updated file", sup.name), nil
}

// startFromFile (re)starts the daemon from the job's file on disk, for the
// case where that file was just rewritten: a service manager reads a job
// definition when it loads it and caches it from then on, so the only way a
// rewritten file reaches the job is to unload it and load it again. That is
// what this does — sup.stop, then sup.bootstrap.
//
// It is deliberately not how a restart normally goes. Unloading leaves the
// daemon unsupervised between the two commands, the window restartDaemon
// exists to avoid (gt-sq9e), and spending it is worth it only when the job
// definition actually changed — at most once per binary upgrade.
//
// Because the unload leaves nothing to bring the daemon back, it is never
// allowed to be the last thing this does (gt-4k3fj.11: launchd removed the
// job during a `gt up` and nothing loaded it again). launchd finishes removing
// a job after bootout has returned, and a bootstrap that lands in that window
// fails; so the job is confirmed gone first and the load is retried. An unload
// that reports failure is checked against the job's live state too, because
// launchd reports errors for a job it did remove.
func (s *daemonSupervisor) startFromFile() error { return s.startFromFileEnv(nil) }

// startFromFileEnv is startFromFile with env added to the load command, so the
// one start --takeover makes keeps its variable when the supervisor file was
// rewritten first. The unload carries none: only the start needs it.
func (s *daemonSupervisor) startFromFileEnv(env []string) error {
	// Unload only a job the manager knows: bootout on a job that is not there
	// fails, and startFromFile is also reached from start, where "not loaded"
	// (the state `gt daemon stop` leaves) is ordinary.
	if st := s.jobState(); st.Err == nil && st.Loaded {
		if err := s.runArgv(s.stop); err != nil {
			// Only a job that is still loaded means the unload did not
			// happen, and then that job is still supervising the daemon.
			if st := s.jobState(); st.Err != nil || st.Loaded {
				return fmt.Errorf("unloading the %s job to load its rewritten file: %w", s.name, err)
			}
		}
		s.waitUnloaded()
	}
	return s.loadJobEnv(env)
}

// waitUnloaded polls until the service manager no longer knows the job, for
// up to handStopConfirmBudget. It does not fail on timeout: the load that
// follows retries, and is the one that reports a job that would not go.
func (s *daemonSupervisor) waitUnloaded() {
	for range handStopConfirmAttempts {
		if st := s.jobState(); st.Err == nil && !st.Loaded {
			return
		}
		s.ctl.sleep(s.ctl.pollInterval)
	}
}

// supervisorLoadAttempts is how many times loadJob tries to bootstrap the job;
// supervisorLoadRetryInterval separates the tries.
const (
	supervisorLoadAttempts      = 6
	supervisorLoadRetryInterval = time.Second
)

// loadJob bootstraps the job, retrying a failed attempt, and returns nil once
// the manager knows the job — including when another caller (a concurrent
// gt up) loaded it between attempts. It is the half of an unload-and-load
// that must not be given up on early: a failure here leaves the town with no
// loaded job and no daemon.
func (s *daemonSupervisor) loadJob() error { return s.loadJobEnv(nil) }

// loadJobEnv is loadJob with env added to the bootstrap command (see
// runStartArgv).
func (s *daemonSupervisor) loadJobEnv(env []string) error {
	var err error
	for attempt := range supervisorLoadAttempts {
		if attempt > 0 {
			s.ctl.sleep(s.ctl.loadRetryInterval)
		}
		if err = s.runStartArgv(s.bootstrap, env); err == nil {
			return nil
		}
		if errors.Is(err, errSupervisorRefused) {
			// Nothing ran, so another attempt would refuse identically.
			return err
		}
		if st := s.jobState(); st.Err == nil && st.Loaded {
			return nil
		}
	}
	return fmt.Errorf("loading the %s job failed %d times, so the daemon has no supervisor: %w", s.name, supervisorLoadAttempts, err)
}

// startDaemon starts the daemon for townRoot: through the provisioned
// supervisor when there is one, directly otherwise. It reports how the
// daemon was started ("launchd", "systemd" or "" for a direct spawn) and its
// PID; callers print. When the supervisor command fails the daemon is NOT
// spawned by hand — a job that is still loaded would respawn against the
// manual daemon forever, the very loop this path exists to prevent — the
// error says how to load the job, or that removing the file returns the
// daemon to manual operation.
func startDaemon(townRoot string) (via string, pid int, err error) {
	return realDaemonControl().startDaemon(townRoot)
}

func (c *daemonControl) startDaemon(townRoot string) (via string, pid int, err error) {
	return c.startDaemonWith(townRoot, false)
}

// startDaemonWith is startDaemon with the operator's --takeover. Whatever the
// supervisor, the startup guard is asked first, before the supervisor file is
// touched and before any command runs: a refusal reaches the operator with
// nothing launched (gt-qpeei.28). A supervisor error refuses the start and
// never falls through to a direct spawn.
func (c *daemonControl) startDaemonWith(townRoot string, takeover bool) (via string, pid int, err error) {
	running, pid, err := c.isRunning(townRoot)
	if err != nil {
		return "", 0, fmt.Errorf("checking daemon status: %w", err)
	}
	if running {
		return "", pid, fmt.Errorf("daemon already running (PID %d)", pid)
	}
	sup, err := c.detectDaemonSupervisor(townRoot)
	if err != nil {
		return "", 0, err
	}
	if err := c.refuseFileKindTakeover(sup, takeover); err != nil {
		return "", 0, err
	}
	if err := c.guardStart(townRoot, sup, takeover); err != nil {
		return "", 0, err
	}
	// Only a start the operator asked to take over carries the variable; every
	// other start goes through the plain argv seam, which carries none.
	var takeoverEnv []string
	if takeover {
		takeoverEnv = []string{config.TakeoverEnv + "=1"}
	}
	if sup != nil {
		sync := c.syncSupervisorFile
		if c.syncFile != nil {
			sync = c.syncFile
		}
		rewritten := sync(townRoot)
		var runErr error
		if rewritten {
			// The file the job will start from is the one just rewritten, so
			// the job has to be loaded from it rather than restarted in place.
			// The takeover variable rides the load too, or a start the guard
			// approved over a fresh owner would run without it.
			runErr = sup.startFromFileEnv(takeoverEnv)
		} else {
			runErr = sup.runStartArgv(sup.start, takeoverEnv)
			if runErr != nil {
				// kickstart / restart reach only a job the service manager knows.
				// One that gt daemon stop unloaded is bootstrapped instead, so that
				// a stop-then-start pair leaves a supervised daemon rather than the
				// hand spawn that recreates the crash loop (gt-3jrm).
				if sup.jobIsMissing() {
					runErr = sup.runArgv(sup.bootstrap)
				}
			}
		}
		if runErr != nil {
			// The command may have failed after starting the daemon. A refusal
			// is not that: nothing ran.
			if !errors.Is(runErr, errSupervisorRefused) {
				if pid, err := c.waitForDaemon(townRoot); err == nil {
					return sup.name, pid, nil
				}
			}
			return sup.name, 0, fmt.Errorf("%s is provisioned for this town but could not start the daemon: %v\n  load it with: %s\n  %s", sup.name, runErr, sup.load, sup.fallback)
		}
		pid, err = c.waitForDaemon(townRoot)
		if err != nil {
			return sup.name, 0, fmt.Errorf("daemon did not come up under %s: %w", sup.name, err)
		}
		return sup.name, pid, nil
	}
	pid, err = c.spawn(townRoot, takeoverEnv)
	return "", pid, err
}

// guardStart asks the startup guard whether the daemon sup is about to start
// (or a hand-run one, when sup is nil) may take townRoot. The guard is
// daemon.GuardStart, the one function Run asks in the daemon itself, so a
// refusal here is the refusal the daemon would give after launch.
func (c *daemonControl) guardStart(townRoot string, sup *daemonSupervisor, takeover bool) error {
	self := daemon.Owner{Kind: daemon.OwnerKindManual}
	if sup != nil && sup.owner != nil {
		owner, err := sup.owner()
		if err != nil {
			return err
		}
		self = owner
	}
	guard := c.guard
	if guard == nil {
		guard = realStartGuard
	}
	return guard(townRoot, self, takeover)
}

// realStartGuard is the guard of this host: docker, launchd and systemd read
// for real, the wall clock.
func realStartGuard(townRoot string, self daemon.Owner, takeover bool) error {
	return daemon.GuardStart(townRoot, self, takeover, nil, time.Now())
}

// refuseFileKindTakeover refuses --takeover under launchd or systemd. Their
// job is the one thing a takeover must not start beside a recorded owner, and
// neither can carry the variable into the daemon (the job's own environment is
// fixed in its file), so the operator boots the job out and starts without it.
func (c *daemonControl) refuseFileKindTakeover(sup *daemonSupervisor, takeover bool) error {
	if !takeover || sup == nil || sup.name == config.SupervisorContainer {
		return nil
	}
	hint := "stop the job first"
	if argv, err := sup.stop(); err == nil {
		hint = "boot the job out first: " + strings.Join(argv, " ")
	}
	return fmt.Errorf("--takeover cannot start the daemon under %s: %s, then run gt daemon start without --takeover", sup.name, hint)
}

// daemonPollInterval is how often waitForDaemon/waitForRestart re-check the
// lock. The attempt counts below are chosen for their real-time products
// (interval * attempts).
const daemonPollInterval = 100 * time.Millisecond

// waitForDaemon polls the lock for up to 3 s and returns the daemon's PID.
//
// pid <= 0 never satisfies the wait: IsRunning reports running with pid 0
// while the daemon holds the lock but has not written its PID file yet
// (daemon.Run locks first, then runs its startup preflight), and returning
// then would make the callers announce "Daemon started (PID 0)".
func (c *daemonControl) waitForDaemon(townRoot string) (int, error) {
	for range 30 {
		c.sleep(c.pollInterval)
		running, pid, err := c.isRunning(townRoot)
		if err != nil {
			return 0, fmt.Errorf("checking daemon status: %w", err)
		}
		if running && pid > 0 {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("daemon failed to start (check logs with 'gt daemon logs')")
}

// handStopConfirmBudget bounds waitForDaemonGone: how long restartDaemon's
// hand path waits, after c.stopDirect returns, for the OS to actually
// finish tearing the old process down and releasing daemon.lock. SIGKILL
// itself is near-instant, so this is confirming the OS finished, not waiting
// out a graceful shutdown — a short budget, unlike restartWaitBudget above.
const handStopConfirmBudget = 5 * time.Second

// handStopConfirmAttempts is handStopConfirmBudget expressed as a poll count
// the same way restartWaitAttempts is (see its doc).
const handStopConfirmAttempts = int(handStopConfirmBudget / daemonPollInterval)

// waitForDaemonGone polls the lock for up to handStopConfirmBudget and
// reports an error if the daemon is still running at the end of it.
func (c *daemonControl) waitForDaemonGone(townRoot string) error {
	for range handStopConfirmAttempts {
		running, _, err := c.isRunning(townRoot)
		if err != nil {
			return fmt.Errorf("checking daemon status: %w", err)
		}
		if !running {
			return nil
		}
		c.sleep(c.pollInterval)
	}
	return fmt.Errorf("daemon still held its lock %s after stopping (check logs with 'gt daemon logs')", handStopConfirmBudget)
}

// restartDaemon brings the daemon back up on the program now on disk: through
// the provisioned supervisor when there is one, by hand when there is not. It
// is the primitive that makes a freshly installed binary take effect in a
// process that is already running, and it is deliberately NOT a stop
// followed by a start.
//
// On macOS the supervisor's stop is `launchctl bootout`, which unloads the
// job: between a stop and a start the daemon is not supervised at all, so
// anything that fails in that window — a rebuild-gt run killed by the very
// restart it asked for, a crash, a reboot — leaves the town with no daemon
// and no loaded job to bring one back (gt-sq9e). kickstart -k restarts the
// job in place: no unsupervised gap, and no dependence on a second command
// succeeding afterwards.
//
// A daemon that is not running is started rather than restarted, so callers
// that want "the daemon is running the current binary" do not have to check
// first. When no supervisor is provisioned the daemon is only ever run by
// hand, so the hand stop/start pair is the whole of it there.
//
// The supervisor's file is reconciled first (syncSupervisorFile). A restart is
// both the act a stale ExitTimeOut endangers — it is what asks the service
// manager to stop the daemon — and the one gt-driven moment the file can be
// loaded afresh: when the reconciliation finds the file stale it rewrites it
// and restarts the job from it, so the new value is in force from this restart
// onward rather than at some later one.
//
// The outgoing PID is read before the restart so the wait can tell the new
// daemon from the old one: waitForDaemon returns as soon as the lock reads as
// running, and throughout a restart that is true of the process on its way
// out as well.
func restartDaemon(townRoot string) (via string, pid int, err error) {
	return realDaemonControl().restartDaemon(townRoot)
}

func (c *daemonControl) restartDaemon(townRoot string) (via string, pid int, err error) {
	running, oldPID, err := c.isRunning(townRoot)
	if err != nil {
		return "", 0, fmt.Errorf("checking daemon status: %w", err)
	}
	if !running {
		return c.startDaemon(townRoot)
	}
	sup, err := c.detectDaemonSupervisor(townRoot)
	if err != nil {
		return "", 0, err
	}
	if sup == nil {
		if err := c.stopDirect(townRoot); err != nil {
			return "", 0, fmt.Errorf("stopping the daemon: %w", err)
		}
		// c.stopDirect (daemon.StopDaemon) returns once the daemon exited
		// or after sending SIGKILL, without confirming the kill took: its
		// own caller (`gt daemon stop`) has
		// nothing further to do with the process either way. This caller
		// does — it is about to spawn a new daemon into the same lock — so
		// starting that race before the old holder is confirmed gone risks
		// the new process losing the flock to a not-quite-dead old one.
		if err := c.waitForDaemonGone(townRoot); err != nil {
			return "", 0, err
		}
		return c.startDaemon(townRoot)
	}
	// A restart under a supervisor starts a daemon too, so it asks the same
	// guard before the file is rewritten or the job touched (gt-qpeei.28).
	if err := c.guardStart(townRoot, sup, false); err != nil {
		return "", 0, err
	}
	// A container's lock pid is from the container's own pid namespace (a
	// recreated daemon is pid 1 again), so whether the restart worked is read
	// from state.json instead: the start time recorded before the restart is
	// the one the new daemon has to be later than (gt-qpeei.40).
	var proof *containerRestartProof
	if sup.name == config.SupervisorContainer {
		var err error
		if proof, err = c.containerRestartProofFor(townRoot, sup); err != nil {
			return "", 0, err
		}
	}
	var runErr error
	if c.syncSupervisorFile(townRoot) {
		// This is the restart the ExitTimeOut it carries exists for: the job
		// is reloaded from the file, so the new value is in force before the
		// next stop this restart causes — and for every one after it.
		runErr = sup.startFromFile()
	} else {
		// A kind with a restart verb of its own runs it; the others restart
		// with their start (kickstart -k and systemctl restart replace the
		// process in place).
		restart := sup.start
		if sup.restart != nil {
			restart = sup.restart
		}
		runErr = sup.runArgv(restart)
		if runErr != nil {
			// kickstart reaches only a job the manager knows. The daemon that
			// is up is then running outside its supervisor (the job was
			// unloaded and never loaded again, gt-4k3fj.11), and the job
			// cannot be loaded beside it: the loaded job would respawn
			// against the lock the hand-run daemon holds (gt-3jrm). So it
			// goes, and the job comes up in its place.
			if sup.jobIsMissing() {
				runErr = c.replaceUnsupervisedDaemon(townRoot, sup)
			}
		}
	}
	if runErr != nil {
		// The command may have failed after the job came up; a live daemon
		// that is not the old one is what the caller asked for either way. A
		// refusal is not that: nothing ran.
		if !errors.Is(runErr, errSupervisorRefused) {
			if pid, waitErr := c.waitForRestartOf(townRoot, oldPID, proof); waitErr == nil {
				return sup.name, pid, nil
			}
		}
		return sup.name, 0, fmt.Errorf("%s is provisioned for this town but could not restart the daemon: %v\n  load it with: %s\n  %s", sup.name, runErr, sup.load, sup.fallback)
	}
	pid, err = c.waitForRestartOf(townRoot, oldPID, proof)
	if err != nil {
		return sup.name, 0, fmt.Errorf("daemon did not come back under %s: %w", sup.name, err)
	}
	return sup.name, pid, nil
}

// containerRestartProof is what a container restart is judged against: the
// owner the restarted daemon will name, the start time state.json recorded
// before the restart, and the supervisor that reads the container and names its
// logs command.
type containerRestartProof struct {
	owner daemon.Owner
	since time.Time
	sup   *daemonSupervisor
}

// containerRestartProofFor records, before a container restart, what state.json
// says: a missing file records no start time, and one that cannot be read
// refuses the restart, since the new daemon could not then be told from the old.
func (c *daemonControl) containerRestartProofFor(townRoot string, sup *daemonSupervisor) (*containerRestartProof, error) {
	owner, err := sup.owner()
	if err != nil {
		return nil, err
	}
	read := c.readState
	if read == nil {
		read = readCutoverState
	}
	st, err := read(townRoot)
	if err != nil {
		return nil, fmt.Errorf("reading state.json before the restart: %w; nothing was restarted", err)
	}
	proof := &containerRestartProof{owner: owner, sup: sup}
	if st != nil {
		proof.since = st.StartedAt
	}
	return proof, nil
}

// waitForRestartOf waits for the restarted daemon: by its recorded start for a
// container (and then there is no pid to return), by its lock pid for the rest.
func (c *daemonControl) waitForRestartOf(townRoot string, oldPID int, proof *containerRestartProof) (int, error) {
	if proof == nil {
		return c.waitForRestart(townRoot, oldPID)
	}
	return 0, c.waitForContainerRestart(townRoot, proof)
}

// waitForContainerRestart polls, within the restart budget, until the container
// has taken the town: state.json names it as the owner, records it running
// with a start time later than the one recorded before the restart, and docker
// reports the container running. A reading that cannot be taken (state.json
// unreadable, the container's state unknown) is a wait that goes on and ends in
// the timeout, never success.
func (c *daemonControl) waitForContainerRestart(townRoot string, proof *containerRestartProof) error {
	last := "no reading was taken"
	for range restartWaitAttempts {
		c.sleep(c.pollInterval)
		ok, why := c.containerTookTheTown(townRoot, proof)
		if ok {
			return nil
		}
		last = why
	}
	return fmt.Errorf("the container did not take the town within %s: %s\n  its logs: %s", restartWaitBudget, last, proof.sup.logs)
}

// containerTookTheTown is one reading of the restart's proof, with the reason it
// fails.
func (c *daemonControl) containerTookTheTown(townRoot string, proof *containerRestartProof) (bool, string) {
	read := c.readState
	if read == nil {
		read = readCutoverState
	}
	st, err := read(townRoot)
	switch {
	case err != nil:
		return false, fmt.Sprintf("state.json cannot be read: %v", err)
	case st == nil:
		return false, "state.json is missing"
	case st.Owner == nil || *st.Owner != proof.owner:
		return false, fmt.Sprintf("state.json names owner %s, not %s %q", ownerText(st.Owner), proof.owner.Kind, proof.owner.ID)
	case !st.Running:
		return false, "state.json records the daemon not running"
	case !st.StartedAt.After(proof.since):
		return false, fmt.Sprintf("state.json still records the start of %s, not a later one", proof.since.Format(time.RFC3339))
	}
	job := proof.sup.jobState()
	switch {
	case job.Err != nil:
		return false, fmt.Sprintf("the container's state cannot be read: %v", job.Err)
	case !job.Loaded || job.Status != "running":
		return false, fmt.Sprintf("docker does not report the container running (loaded %v, status %q)", job.Loaded, job.Status)
	}
	return true, ""
}

// replaceUnsupervisedDaemon stops the daemon holding daemon.lock outside its
// supervisor and loads the job, which starts the daemon under it.
func (c *daemonControl) replaceUnsupervisedDaemon(townRoot string, sup *daemonSupervisor) error {
	if err := c.stopDirect(townRoot); err != nil {
		return fmt.Errorf("stopping the daemon running outside %s: %w", sup.name, err)
	}
	if err := c.waitForDaemonGone(townRoot); err != nil {
		return err
	}
	return sup.loadJob()
}

// daemonStartupMargin is how long waitForRestart allows the INCOMING daemon
// to run its own preflight (Dolt metadata repair, opening beads stores) and
// acquire daemon.lock, on top of the time the OUTGOING one takes to let go of
// it. It has no measured source the way ShutdownBudget's steps do; it is a
// deliberately generous margin.
const daemonStartupMargin = 30 * time.Second

// restartWaitBudget bounds waitForRestart's poll. It is derived from the two
// real enforcement mechanisms that bound how long the OLD daemon can hold
// daemon.lock after a restart is asked for — not from Daemon.shutdown's own
// step budgets, which neither restart path actually lets run to completion:
//
//   - Under a provisioned launchd job (the only path that uses this budget;
//     see restartDaemon), `gt daemon restart` is `launchctl kickstart -k`,
//     which sends SIGTERM and — critically — launchd itself SIGKILLs the job
//     if it has not exited within ExitTimeOut. ProvisionSupervisor sets that
//     to daemon.ShutdownBudget (see internal/templates), so the old daemon is
//     guaranteed gone within ShutdownBudget of the restart, whatever its own
//     shutdown steps add up to.
//   - The hand path (no supervisor) does not use this function at all:
//     restartDaemon calls c.stopDirect (daemon.StopDaemon) instead, which
//     polls for the daemon to exit and SIGKILLs it only after
//     daemon.StopWaitBudget (ShutdownBudget plus a margin), so a daemon that
//     finishes shutdown() is never killed and it returns with the old one gone.
//
// So ShutdownBudget is the bound this function actually has to plan around;
// daemonStartupMargin covers the incoming daemon on top of it.
const restartWaitBudget = daemon.ShutdownBudget + daemonStartupMargin

// restartWaitAttempts is how many times waitForRestart polls: chosen so that
// restartWaitAttempts * daemonPollInterval equals restartWaitBudget.
const restartWaitAttempts = int(restartWaitBudget / daemonPollInterval)

// waitForRestart polls the lock for up to restartWaitBudget and returns the
// PID of a daemon other than oldPID. The longer budget than waitForDaemon's
// is for the restart's extra step: the outgoing process has to release the
// lock and the supervisor has to bring the job back before there is anything
// to see, and a wait that times out early would report a restart that did
// happen as a failure.
//
// pid <= 0 never satisfies the wait, even when running is true and 0 !=
// oldPID: IsRunning reports running with pid 0 when the lock is held but the
// PID file has not been written yet (the same race daemon.StopDaemon's own
// comment calls out) — a start still in flight, not the new daemon this is
// waiting for.
func (c *daemonControl) waitForRestart(townRoot string, oldPID int) (int, error) {
	for range restartWaitAttempts {
		c.sleep(c.pollInterval)
		running, pid, err := c.isRunning(townRoot)
		if err != nil {
			return 0, fmt.Errorf("checking daemon status: %w", err)
		}
		if running && pid > 0 && pid != oldPID {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("daemon failed to restart (check logs with 'gt daemon logs')")
}
