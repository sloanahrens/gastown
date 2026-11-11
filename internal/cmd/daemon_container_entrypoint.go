package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/version"
)

// The environment the entrypoint reads beyond the container-mode flag.
// GT_TOWN_ROOT is the bind mount the compose file names (templates
// daemon_compose.go); GIT_USER and GIT_EMAIL are the commit identity.
const (
	containerTownRootEnv = "GT_TOWN_ROOT"
	containerGitUserEnv  = "GIT_USER"
	containerGitEmailEnv = "GIT_EMAIL"

	// containerMountinfo is the table of mounts this process sees.
	containerMountinfo = "/proc/self/mountinfo"

	// containerOMTokenFileFlag names the file holding the subscription token om
	// authenticates with inside the container. It is a flag, not an environment
	// variable: the GT_ environment-read guard forbids a new one, and the compose
	// project's command line is where the scratch flag already travels. The
	// entrypoint reads the file and puts the token into the environment of the
	// daemon it execs as omTokenEnv; with the flag unset no token is loaded
	// (gt-qpeei.36).
	containerOMTokenFileFlag = config.OMTokenFileFlag
	// omTokenEnv is the variable the claude CLI reads a subscription token from.
	omTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN" //nolint:gosec // an environment variable name, not a credential

	// containerImagePluginsDir is where the image carries the Linux plugin
	// binaries, one directory per plugin holding the binary of the same name.
	// The town's plugin directories live on the shared town volume, so the
	// entrypoint copies each binary into the town at start (gt-gg9hv.10).
	containerImagePluginsDir = "/usr/local/lib/gt/plugins"

	// containerImageMangoDir is where the image carries the mango build for the
	// platform the daemon runs on, with the commit it was built from
	// (deploy/daemon/Dockerfile). The town's runtime directory lives on the
	// shared town volume, so the entrypoint copies both files into the town at
	// start (gt-eijn2z).
	containerImageMangoDir = "/usr/local/lib/gt/mango"

	// mangoBinName and mangoShaName are the two files the image's mango
	// directory holds, and the names they take in the town's runtime directory:
	// the binary and the stamp of the commit it was built from, which the
	// metrics analysis reads before it decides to rebuild.
	mangoBinName = "mango"
	mangoShaName = "mango.sha"
)

// entrypointFlags are the values the command's flags parsed.
type entrypointFlags struct {
	scratch     bool   // --scratch-town
	omTokenFile string // --om-token-file
	// doltHost is --dolt-host, the Dolt server the scratch town's install uses,
	// and doltHostSet says the flag was given: an empty value and no flag are
	// not the same thing, since an empty host is refused.
	doltHost    string
	doltHostSet bool
	// doltPort is --dolt-port, the port that Dolt server listens on; doltPortSet
	// says the flag was given, since port 0 is refused and so must be told apart
	// from no port at all.
	doltPort    int
	doltPortSet bool
	// forgeHost and forgeTokenFile are --forge-host and --forge-token-file; each
	// Set says its flag was given, since an empty value is refused and so must be
	// told apart from no flag.
	forgeHost         string
	forgeHostSet      bool
	forgeTokenFile    string
	forgeTokenFileSet bool
}

// newDaemonContainerEntrypointCmd builds the command over run, which receives
// the flag values the command parsed. A test passes a run that records them, so
// the flags the shell sets are the values the decision reads.
func newDaemonContainerEntrypointCmd(run func(flags entrypointFlags) error) *cobra.Command {
	var flags entrypointFlags
	cmd := &cobra.Command{
		Use:   "container-entrypoint",
		Short: "Start the daemon as the container's entrypoint (internal)",
		Long: `Start the Gas Town daemon as the container's entrypoint.

Runs only in container mode. It starts the daemon on the town root in its
environment and refuses everything else, so a missing or wrong bind mount never
turns into a fresh town:

  - mayor/town.json present as a regular file: exec 'gt daemon run'.
  - mayor/town.json present as any other kind of file, a symlink included:
    refuse.
  - mayor/town.json absent, --scratch-town not given: refuse.
  - mayor/town.json absent, --scratch-town given: run 'gt install <townRoot>
    --git' once, with --dolt-host <host> when that flag is given, only when the
    town root is a mount point, holds the scratch marker that internal/config
    defines and the scratch runbook creates, and holds nothing else; then exec
    'gt daemon run'. Otherwise refuse.
  - an install-incomplete marker (see below) present, whatever else is there:
    refuse.

The scratch install is bracketed by an install-incomplete marker, created
before the install starts and removed only after it succeeded. If the install
fails, the entries it wrote are removed on a best effort, the scratch marker is
kept, the incomplete marker stays, and the command refuses; so does every later
start, until the operator removes the incomplete marker (and anything the
install left) from the host side of the scratch directory.

With --dolt-host, the scratch town's install is given that host as its own
--dolt-host, so the scratch town uses the Dolt server the scratch project runs
instead of a local one. The flag is accepted only with --scratch-town, and an
empty host or one beginning with a dash is refused: the live project's command
line stays empty. Only an install writes the host into the town, so a start
that runs none refuses a host the installed town does not already name.

With --om-token-file, the file's contents (spaces, tabs and line breaks removed)
become CLAUDE_CODE_OAUTH_TOKEN in the daemon's environment; a file unreadable or
empty after that refuses. A container that sets ANTHROPIC_API_KEY or
ANTHROPIC_AUTH_TOKEN is refused, since either outranks the token and bills API
rates.

With --forge-host and --forge-token-file, the container's git is told to ask
'gt git-credential-forgejo' for that Forgejo's credentials, so the daemon's
pushes carry the landing bot's token. The pair is taken together or not at all,
by the live and the scratch container alike, and the config line names the token
file's path and never a token: the helper reads the file when git asks, so the
file need not exist yet at start.

Every check runs before the first command, so a refusal runs nothing.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags.doltHostSet = cmd.Flags().Changed(config.DoltHostFlag)
			flags.doltPortSet = cmd.Flags().Changed(config.DoltPortFlag)
			flags.forgeHostSet = cmd.Flags().Changed(config.ForgeHostFlag)
			flags.forgeTokenFileSet = cmd.Flags().Changed(config.ForgeTokenFileFlag)
			return run(flags)
		},
	}
	cmd.Flags().BoolVar(&flags.scratch, config.ScratchTownFlag, false,
		"Install a town at the town root when it holds only the scratch marker")
	cmd.Flags().StringVar(&flags.omTokenFile, containerOMTokenFileFlag, "",
		"Read the om subscription token from this file and give it to the daemon; the file's mode is not checked")
	cmd.Flags().StringVar(&flags.doltHost, config.DoltHostFlag, "",
		"Give this Dolt host to the scratch town's install; accepted only with --"+config.ScratchTownFlag)
	cmd.Flags().IntVar(&flags.doltPort, config.DoltPortFlag, 0,
		"Give this port of that Dolt host to the scratch town's install; accepted only with --"+config.DoltHostFlag)
	cmd.Flags().StringVar(&flags.forgeHost, config.ForgeHostFlag, "",
		"Make git ask the forge credential helper for this Forgejo host:port; accepted only with --"+config.ForgeTokenFileFlag)
	cmd.Flags().StringVar(&flags.forgeTokenFile, config.ForgeTokenFileFlag, "",
		"The landing bot's token file the credential helper reads at push time (absolute path); accepted only with --"+config.ForgeHostFlag)
	return cmd
}

var daemonContainerEntrypointCmd = newDaemonContainerEntrypointCmd(runDaemonContainerEntrypoint)

// containerEntrypoint is what the entrypoint reads and runs. Every command it
// runs goes through run or exec, so a test records the whole sequence and no
// process starts. runDaemonContainerEntrypoint wires the real ones.
type containerEntrypoint struct {
	self   func() (string, error)    // the gt binary's path, argv[0] of each command it runs
	getenv func(key string) string   // the environment
	run    func(argv []string) error // runs a command and waits for it
	// exec replaces this process with argv and env added to its environment;
	// it returns only on error. The token travels in env, never in argv.
	exec     func(argv []string, env []string) error
	readFile func(path string) (string, error) // reads a whole file
	// patrols reads the town's daemon patrol settings the way the daemon does;
	// an absent settings file reads as nil, with no error.
	patrols   func(townRoot string) (*config.DaemonPatrolConfig, error)
	mountinfo func() (string, error) // text of /proc/self/mountinfo
	dirNames  func(dir string) ([]string, error)
	lstat     func(path string) (entryKind, error) // what sits at mayor/town.json, a link not followed
	exists    func(path string) (bool, error)
	create    func(path string) error // creates an empty file, failing if the path exists (O_EXCL)
	remove    func(path string) error // removes an entry and what it holds
	// installPlugins puts the image's plugin binaries into the town's plugin
	// directories; it runs after a scratch install has made them and before the
	// daemon is exec'd.
	installPlugins func(townRoot string) error
	// installMango puts the image's mango binary and its commit stamp into the
	// town's runtime directory for this platform; it runs after installPlugins
	// and before the daemon is exec'd.
	installMango func(townRoot string) error
	// deployDirectives writes the town's central directives from the gastown bare
	// repo at the image's commit; it runs after installMango and before the daemon
	// is exec'd. Its missing-commit error is a warning, not a refusal.
	deployDirectives func(townRoot string) ([]string, error)
	// logf writes one line of start-up progress to the container's log.
	logf    func(format string, args ...any)
	scratch bool // --scratch-town: the scratch project asked for a scratch town
	// omTokenFile is --om-token-file: the file the om subscription token is read
	// from; "" loads no token.
	omTokenFile string
	// doltHost is --dolt-host, the Dolt server the scratch town's install uses;
	// doltHostSet says the flag was given, since an empty host is refused and
	// so must be told apart from no host at all.
	doltHost    string
	doltHostSet bool
	// doltPort is --dolt-port, the port that server listens on; doltPortSet says
	// the flag was given.
	doltPort    int
	doltPortSet bool
	// forgeHost and forgeTokenFile are --forge-host and --forge-token-file, and
	// each Set says its flag was given.
	forgeHost         string
	forgeHostSet      bool
	forgeTokenFile    string
	forgeTokenFileSet bool
}

// entryKind is what a path is, as Lstat reports it. A symbolic link is
// entryOther: it is not the regular file the town needs.
type entryKind int

const (
	entryAbsent entryKind = iota
	entryRegular
	entryOther
)

// runDaemonContainerEntrypoint runs the entrypoint with the real seams.
func runDaemonContainerEntrypoint(flags entrypointFlags) error {
	return runContainerEntrypoint(containerEntrypoint{
		self:      os.Executable,
		getenv:    os.Getenv,
		run:       runEntrypointCommand,
		exec:      execEntrypointArgv,
		readFile:  readTextFile,
		patrols:   daemon.ReadPatrolConfig,
		mountinfo: func() (string, error) { return readTextFile(containerMountinfo) },
		dirNames:  listDirNames,
		lstat:     entrypointLstat,
		exists:    entrypointPathExists,
		create:    entrypointCreateExclusive,
		remove:    os.RemoveAll,
		installPlugins: func(townRoot string) error {
			return installPluginBinaries(containerImagePluginsDir, townRoot, entrypointLogf)
		},
		installMango: func(townRoot string) error {
			return installMangoBinary(containerImageMangoDir, townRoot)
		},
		deployDirectives: func(townRoot string) ([]string, error) {
			return config.DeployCentralDirectives(townRoot, "", version.BuildCommit)
		},
		logf:        entrypointLogf,
		scratch:     flags.scratch,
		omTokenFile: flags.omTokenFile,
		doltHost:    flags.doltHost,
		doltHostSet: flags.doltHostSet,
		doltPort:    flags.doltPort,
		doltPortSet: flags.doltPortSet,

		forgeHost:         flags.forgeHost,
		forgeHostSet:      flags.forgeHostSet,
		forgeTokenFile:    flags.forgeTokenFile,
		forgeTokenFileSet: flags.forgeTokenFileSet,
	})
}

// entrypointLstat reports what sits at path without following a symbolic link.
// A failure other than the path being absent is an error, not "absent".
func entrypointLstat(path string) (entryKind, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return entryAbsent, nil
	case err != nil:
		return entryAbsent, err
	case info.Mode().IsRegular():
		return entryRegular, nil
	default:
		return entryOther, nil
	}
}

// entrypointCreateExclusive creates an empty file at path and fails if anything,
// a symbolic link included, is already there.
func entrypointCreateExclusive(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// entrypointLogf writes one line of start-up progress to stderr, where the
// container's log collects it.
func entrypointLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// readTextFile returns the text of one file.
func readTextFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// listDirNames returns the names of the entries in dir.
func listDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

// entrypointPathExists reports whether path exists. A stat that fails for any reason
// other than the path being absent is an error, not a "no".
func entrypointPathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// runContainerEntrypoint starts the daemon, or refuses. It decides first, with
// no command run, then sets the git identity, installs a scratch town if the
// decision says so, and execs the daemon last.
func runContainerEntrypoint(e containerEntrypoint) error {
	if e.getenv(config.ContainerModeEnv) != config.SupervisorContainer {
		return fmt.Errorf("gt daemon container-entrypoint runs only in container mode (%s=%s); nothing was started",
			config.ContainerModeEnv, config.SupervisorContainer)
	}
	// The Dolt host belongs to the scratch town's install alone, and an empty
	// one names nothing: both are refused here, before any command runs.
	if err := e.refuseDoltHost(); err != nil {
		return err
	}
	if err := e.refuseForgeCredential(); err != nil {
		return err
	}
	// An API credential outranks the subscription token and bills API rates, so
	// a container that carries one never starts: this runs before anything else
	// does, and runs nothing.
	if err := e.refuseAPICredentials(); err != nil {
		return err
	}
	townRoot := filepath.Clean(e.getenv(containerTownRootEnv))
	if !filepath.IsAbs(townRoot) {
		return fmt.Errorf("%s must name the town root as an absolute path; nothing was started", containerTownRootEnv)
	}
	self, err := e.self()
	if err != nil {
		return fmt.Errorf("find the gt binary to exec: %w; nothing was started", err)
	}
	// The om token is read before any command runs, so an unreadable or empty
	// file runs nothing. Its value goes only into the daemon's environment.
	daemonEnv, err := e.omTokenEnv()
	if err != nil {
		return err
	}
	// First, before town.json is looked at: an earlier install that did not
	// finish leaves the town unstartable, even when its cleanup failed and a
	// regular mayor/town.json is still on disk.
	if err := e.refuseIncompleteInstall(townRoot); err != nil {
		return err
	}
	install, err := e.decideStart(townRoot)
	if err != nil {
		return err
	}
	// The install path execs the daemon too, so the tier sweep check runs on
	// both paths and before the install writes anything.
	if err := e.refuseTierSweepWithoutRegistry(townRoot); err != nil {
		return err
	}
	if !install {
		if err := e.refuseDoltHostTheTownDoesNotName(townRoot); err != nil {
			return err
		}
	}
	if err := e.setGitIdentity(); err != nil {
		return err
	}
	if err := e.setForgeCredentialHelper(self); err != nil {
		return err
	}
	if install {
		if err := e.installScratchTown(self, townRoot); err != nil {
			return err
		}
	}
	// The daemon runs the town's plugin binaries itself, so the image's Linux
	// builds are in place before it starts; a scratch install has made the
	// plugin directories by now. A copy that fails refuses rather than let the
	// daemon run the Mac binaries the town volume also holds (gt-gg9hv.10).
	if err := e.installPlugins(townRoot); err != nil {
		return fmt.Errorf("install the image's plugin binaries under %s: %w; the daemon was not started", townRoot, err)
	}
	// The metrics lookup runs the town's mango binary; the town runtime is on
	// the shared volume, so the image's build and its commit stamp are in place
	// before the daemon starts. An image without mango is older than the
	// per-platform build and is not an error (gt-eijn2z).
	if err := e.installMango(townRoot); err != nil {
		return fmt.Errorf("install the image's mango under %s: %w; the daemon was not started", townRoot, err)
	}
	if err := e.deployTownDirectives(townRoot); err != nil {
		return err
	}
	if err := e.exec([]string{self, "daemon", "run"}, daemonEnv); err != nil {
		return fmt.Errorf("exec gt daemon run: %w", err)
	}
	return nil
}

// deployTownDirectives writes the town's central directives from the gastown
// bare repo at the image's commit. A town with no gastown rig yet has no repo to
// deploy from, so the step is skipped with one line. A commit the repo does not
// hold yet is a warning that names it: the previous deployed copy stays. Any
// other failure refuses, as the plugin install does.
func (e containerEntrypoint) deployTownDirectives(townRoot string) error {
	present, err := e.exists(config.CentralDirectiveRepo(townRoot))
	if err != nil {
		return fmt.Errorf("check the gastown rig under %s for central directives: %w; the daemon was not started", townRoot, err)
	}
	if !present {
		e.logf("no gastown rig yet, central directives not deployed")
		return nil
	}
	_, err = e.deployDirectives(townRoot)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, config.ErrDirectiveCommitMissing):
		e.logf("warning: central directives not redeployed: %v; the previous copy stays until the bare repo fetches the image's commit", err)
		return nil
	default:
		return fmt.Errorf("deploy the central directives under %s: %w; the daemon was not started", townRoot, err)
	}
}

// refuseDoltHost refuses a --dolt-host that no install would act on. Only the
// scratch town's install takes a Dolt host, so one given without --scratch-town
// is a mistake; and a host that is empty or begins with a dash names nothing,
// which the flag alone cannot tell from no host at all. A host of spaces is
// empty too, as `gt install` reads it. Every refusal runs before the first
// command.
func (e containerEntrypoint) refuseDoltHost() error {
	if e.doltPortSet {
		if !e.doltHostSet {
			return fmt.Errorf("--%s is given without --%s; the port belongs to the Dolt server the host names, so nothing was started",
				config.DoltPortFlag, config.DoltHostFlag)
		}
		if e.doltPort < 1 || e.doltPort > 65535 {
			return fmt.Errorf("--%s %d is not a port (1 to 65535), so nothing was started", config.DoltPortFlag, e.doltPort)
		}
	}
	if !e.doltHostSet {
		return nil
	}
	host := strings.TrimSpace(e.doltHost)
	if host == "" {
		return fmt.Errorf("--%s is empty; the scratch town's install needs the Dolt server's host, so nothing was started", config.DoltHostFlag)
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("--%s %q begins with a dash, so it names a flag rather than a host; name the Dolt server's host, and nothing was started", config.DoltHostFlag, host)
	}
	if !e.scratch {
		return fmt.Errorf("--%s is given without --%s; only the scratch town's install takes a Dolt host, and the live project's command line stays empty, so nothing was started",
			config.DoltHostFlag, config.ScratchTownFlag)
	}
	return nil
}

// refuseDoltHostTheTownDoesNotName refuses a --dolt-host this start would
// never write. The host reaches a town only through the scratch install, which
// writes it into mayor/town.json; a start that finds the town already there
// runs no install, so the flag would be dropped without a word unless the town
// already names that host, and a value that disagrees is refused instead. A
// host the town already names is the one the install wrote, so the restart the
// compose file's unchanged command line gives keeps starting. A town whose
// config cannot be read or parsed is refused rather than taken as naming
// nothing, since that would drop the flag in silence too.
func (e containerEntrypoint) refuseDoltHostTheTownDoesNotName(townRoot string) error {
	if !e.doltHostSet {
		return nil
	}
	path := filepath.Join(townRoot, "mayor", "town.json")
	text, err := e.readFile(path)
	if err != nil {
		return fmt.Errorf("read the town's %s: %w; --%s is written only by an install, and this start runs none, so nothing was started",
			path, err, config.DoltHostFlag)
	}
	var town config.TownConfig
	if err := json.Unmarshal([]byte(text), &town); err != nil {
		return fmt.Errorf("parse the town's %s: %w; the Dolt server it names cannot be told from --%s, so nothing was started",
			path, err, config.DoltHostFlag)
	}
	named := ""
	if town.Dolt != nil {
		named = strings.TrimSpace(town.Dolt.Host)
	}
	host := strings.TrimSpace(e.doltHost)
	if strings.EqualFold(named, host) {
		// The port this start would have installed: --dolt-port, else the
		// default `gt install` falls back to. A restart that omits the flag is
		// checked against that default, so the flag cannot be dropped to skip
		// the check.
		want := doltserver.DefaultPort
		if e.doltPortSet {
			want = e.doltPort
		}
		if town.Dolt == nil || town.Dolt.Port != want {
			return fmt.Errorf("--%s %d (the port this start would install) is not the port the town at %s already names for %q, and this start runs no install that would write it; nothing was started",
				config.DoltPortFlag, want, townRoot, named)
		}
		return nil
	}
	if named == "" {
		return fmt.Errorf("--%s %q names a Dolt server, but the town at %s names none, and this start runs no install that would write it; nothing was started",
			config.DoltHostFlag, host, townRoot)
	}
	return fmt.Errorf("--%s %q is not the Dolt server the town at %s already names (%q), and this start runs no install that would write it, so the flag would be ignored; nothing was started",
		config.DoltHostFlag, host, townRoot, named)
}

// refuseTierSweepWithoutRegistry refuses a pod start whose daemon patrol
// settings enable tier_sweep with no runner image registry. A pod has no
// container engine to load the runner image into, so a sweep with no registry
// to ask instead fails on its first run. A settings file that cannot be read
// or parsed is refused too, since the sweep cannot be ruled out without it.
// Outside a pod nothing is checked.
func (e containerEntrypoint) refuseTierSweepWithoutRegistry(townRoot string) error {
	if e.getenv(podEnv) == "" {
		return nil
	}
	cfg, err := e.patrols(townRoot)
	if err != nil {
		return fmt.Errorf("read the daemon patrol settings %s: %w; a pod cannot rule out the tier sweep without them, so nothing was started",
			daemon.PatrolConfigSource(townRoot), err)
	}
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.TierSweep == nil || !cfg.Patrols.TierSweep.Enabled {
		return nil
	}
	if runner := cfg.Patrols.TierSweep.Runner; runner != nil && runner.ImageRegistry != "" {
		return nil
	}
	return fmt.Errorf("daemon.patrols.tier_sweep.runner.image_registry is not set while tier_sweep is enabled: this pod has no container engine to load the runner image into, so the first sweep would fail; set the key to a registry the daemon reaches, and nothing was started")
}

// apiCredentialEnvs are the variables that outrank CLAUDE_CODE_OAUTH_TOKEN in
// the claude CLI and bill API rates; the container carries neither.
var apiCredentialEnvs = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// refuseAPICredentials refuses a container environment that sets an API
// credential, naming the variable and never its value. A variable set to the
// empty string is not set: a compose pass-through of an unset host variable
// arrives empty.
func (e containerEntrypoint) refuseAPICredentials() error {
	for _, name := range apiCredentialEnvs {
		if e.getenv(name) != "" {
			return fmt.Errorf("%s is set in the container environment; it outranks the subscription token and bills API rates, so nothing was started", name)
		}
	}
	return nil
}

// omTokenEnv returns the environment the daemon is exec'd with beyond this
// process's own: the subscription token from the file --om-token-file names, or
// nothing when it is unset. A token is one run of printable characters, so the
// space, tab, CR and LF characters are removed from the file's contents, not
// only the ones at the ends: a file saved hard-wrapped holds a line break inside
// the token, which the claude CLI rejects. The token is never put into an error,
// a message or an argv; a file unreadable or empty after the strip is refused
// naming the path alone. The file's mode is not checked: a bind
// mount through Docker Desktop reports ownership and modes unreliably.
func (e containerEntrypoint) omTokenEnv() ([]string, error) {
	path := e.omTokenFile
	if path == "" {
		return nil, nil
	}
	text, err := e.readFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the om token file %s: it could not be read; nothing was started", path)
	}
	token := strings.Map(dropASCIIWhitespace, text)
	if token == "" {
		return nil, fmt.Errorf("the om token file %s is empty; nothing was started", path)
	}
	return []string{omTokenEnv + "=" + token}, nil
}

// dropASCIIWhitespace returns r unless it is an ASCII whitespace character a
// hard-wrapped file can put inside a token: space, tab, CR or LF, which is
// dropped.
func dropASCIIWhitespace(r rune) rune {
	switch r {
	case ' ', '\t', '\r', '\n':
		return -1
	}
	return r
}

// mergeEnv returns base with every variable add names replaced by add's value,
// so a token already in the environment never beats the one just loaded and no
// name appears twice for the exec to choose between.
func mergeEnv(base, add []string) []string {
	replaced := make(map[string]bool, len(add))
	for _, kv := range add {
		name, _, _ := strings.Cut(kv, "=")
		replaced[name] = true
	}
	merged := make([]string, 0, len(base)+len(add))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !replaced[name] {
			merged = append(merged, kv)
		}
	}
	return append(merged, add...)
}

// refuseIncompleteInstall refuses when the install-incomplete marker is there.
// It is looked at with Lstat, so a link planted at that name counts as the
// marker rather than being followed or ignored, and a read that fails refuses:
// a marker that cannot be ruled out is treated as present.
func (e containerEntrypoint) refuseIncompleteInstall(townRoot string) error {
	path := filepath.Join(townRoot, config.ScratchInstallIncompleteMarker)
	kind, err := e.lstat(path)
	if err != nil {
		return fmt.Errorf("read %s: %w; nothing was started", path, err)
	}
	if kind == entryAbsent {
		return nil
	}
	return fmt.Errorf("%s exists: an earlier scratch install did not finish, so the town under %s is not safe to start; "+
		"nothing was started. Remove %s and every entry but %s from the scratch directory on the host, then start again",
		path, townRoot, config.ScratchInstallIncompleteMarker, config.ScratchTownMarker)
}

// installScratchTown runs the one install a scratch container may run, between
// the install-incomplete marker's creation and its removal. The marker is
// created first and exclusively, so an install never starts without it, and is
// removed only after the install succeeded, so a failure of any kind, the
// cleanup's included, leaves a town the next start refuses. The marker stays
// after every failed install and only the operator clears it; a failure to
// remove it after a good install refuses too, for the same reason.
func (e containerEntrypoint) installScratchTown(self, townRoot string) error {
	incomplete := filepath.Join(townRoot, config.ScratchInstallIncompleteMarker)
	if err := e.create(incomplete); err != nil {
		return fmt.Errorf("create the %s marker under %s: %w; nothing was installed or started",
			config.ScratchInstallIncompleteMarker, townRoot, err)
	}
	if err := e.run(e.installArgv(self, townRoot)); err != nil {
		return e.undoScratchInstall(townRoot, fmt.Errorf("install the scratch town at %s: %w", townRoot, err))
	}
	if err := e.remove(incomplete); err != nil {
		return fmt.Errorf("the scratch install at %s succeeded, but its %s marker could not be removed: %w; "+
			"the next start refuses until it is removed, and nothing was started", townRoot, config.ScratchInstallIncompleteMarker, err)
	}
	return nil
}

// installArgv is the install the scratch town gets: the town root and --git
// always, and --dolt-host with its value when the flag was given, so the town
// writes that server's endpoint instead of starting a Dolt of its own. The
// flag's name is the one the entrypoint registers and the render passes
// (config.DoltHostFlag), so the two cannot drift.
func (e containerEntrypoint) installArgv(self, townRoot string) []string {
	argv := []string{self, "install", townRoot, "--git"}
	if e.doltHostSet {
		argv = append(argv, "--"+config.DoltHostFlag, e.doltHost)
	}
	if e.doltPortSet {
		argv = append(argv, "--"+config.DoltPortFlag, strconv.Itoa(e.doltPort))
	}
	return argv
}

// undoScratchInstall removes, on a best effort, every entry the failed install
// left in the scratch town except the two markers. The directory held only the
// scratch marker before the install, so everything else is the install's own
// work. The incomplete marker stays whatever happens here, so a removal that
// fails still leaves a town the next start refuses.
func (e containerEntrypoint) undoScratchInstall(townRoot string, cause error) error {
	names, err := e.dirNames(townRoot)
	if err != nil {
		return fmt.Errorf("%w; and could not list %s to remove the partial install: %v; the %s marker stays, so the next start refuses; remove every entry but %s by hand",
			cause, townRoot, err, config.ScratchInstallIncompleteMarker, config.ScratchTownMarker)
	}
	for _, name := range names {
		if name == config.ScratchTownMarker || name == config.ScratchInstallIncompleteMarker {
			continue
		}
		if err := e.remove(filepath.Join(townRoot, name)); err != nil {
			return fmt.Errorf("%w; and could not remove %s from the partial install: %v; the %s marker stays, so the next start refuses; remove every entry but %s by hand",
				cause, name, err, config.ScratchInstallIncompleteMarker, config.ScratchTownMarker)
		}
	}
	return fmt.Errorf("%w; the partial install was removed, the %s marker stays and the next start refuses until it is removed, and nothing was started",
		cause, config.ScratchInstallIncompleteMarker)
}

// decideStart says whether this start installs the town first, or returns the
// refusal. It runs no command, so every refusal records nothing.
func (e containerEntrypoint) decideStart(townRoot string) (bool, error) {
	kind, err := e.lstat(filepath.Join(townRoot, "mayor", "town.json"))
	if err != nil {
		return false, fmt.Errorf("read mayor/town.json under %s: %w; nothing was started", townRoot, err)
	}
	switch kind {
	case entryRegular:
		return false, nil
	case entryOther:
		return false, fmt.Errorf("mayor/town.json under %s is not a regular file (a link or another kind of file), so nothing was started", townRoot)
	}
	if !e.scratch {
		return false, fmt.Errorf("%s has no mayor/town.json, so the bind mount is missing or the path is wrong; a town is not installed here, and nothing was started", townRoot)
	}

	mountinfo, err := e.mountinfo()
	if err != nil {
		return false, fmt.Errorf("read %s: %w; nothing was started", containerMountinfo, err)
	}
	if !isMountPoint(mountinfo, townRoot) {
		return false, fmt.Errorf("%s is not a mount point, so it is not the scratch town's bind mount; nothing was started", townRoot)
	}

	hasMarker, err := e.exists(filepath.Join(townRoot, config.ScratchTownMarker))
	if err != nil {
		return false, fmt.Errorf("read the %s marker under %s: %w; nothing was started", config.ScratchTownMarker, townRoot, err)
	}
	if !hasMarker {
		return false, fmt.Errorf("%s holds no %s marker, which the runbook creates in the scratch town; nothing was started", townRoot, config.ScratchTownMarker)
	}

	names, err := e.dirNames(townRoot)
	if err != nil {
		return false, fmt.Errorf("list %s: %w; nothing was started", townRoot, err)
	}
	for _, name := range names {
		if name != config.ScratchTownMarker {
			return false, fmt.Errorf("%s holds files besides %s, so it is not an empty scratch town; nothing was started", townRoot, config.ScratchTownMarker)
		}
	}
	return true, nil
}

// setGitIdentity sets the commit identity the daemon's git work uses. An empty
// variable leaves git's own setting as it is.
func (e containerEntrypoint) setGitIdentity() error {
	for _, id := range []struct{ key, env string }{
		{"user.name", containerGitUserEnv},
		{"user.email", containerGitEmailEnv},
	} {
		value := e.getenv(id.env)
		if value == "" {
			continue
		}
		if err := e.run([]string{"git", "config", "--global", id.key, value}); err != nil {
			return fmt.Errorf("set git %s from %s: %w", id.key, id.env, err)
		}
	}
	return nil
}

// runEntrypointCommand runs one command and waits for it. Its output goes to
// this process's streams and never into an error, since output can hold a secret.
func runEntrypointCommand(argv []string) error {
	c := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv is built in this file from fixed names and the bind-mount path
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// isMountPoint reports whether path is a mount point in the text of
// /proc/self/mountinfo. The mount point is field 5 of each line, with space,
// tab, newline and backslash written as octal escapes. A parent of a mount
// point is a mount point only if the table lists it.
func isMountPoint(mountinfo string, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	want := filepath.Clean(path)
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescapeMountPoint(fields[4]) == want {
			return true
		}
	}
	return false
}

// unescapeMountPoint decodes the \ooo octal escapes mountinfo writes into a
// mount point. A backslash that starts no valid escape is kept as it is.
func unescapeMountPoint(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// forgeCredentialPathRe is what a forge token file path may hold. The path is
// written into a git config helper line that git runs through a shell, so it
// carries no space, quote or shell metacharacter.
var forgeCredentialPathRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// refuseForgeCredential refuses a --forge-host or --forge-token-file no start
// could use: one without the other, an empty value, a host that is not a bare
// host:port, and a token file path that is not absolute or holds a character a
// shell would read. Both containers take the pair: the scratch project's git
// pushes to its throwaway Forgejo, and the live container's landing worker
// pushes to the live one (gt-rpx80). Every refusal runs before the first
// command.
func (e containerEntrypoint) refuseForgeCredential() error {
	if !e.forgeHostSet && !e.forgeTokenFileSet {
		return nil
	}
	if e.forgeHostSet != e.forgeTokenFileSet {
		return fmt.Errorf("--%s and --%s are given together or not at all, so nothing was started",
			config.ForgeHostFlag, config.ForgeTokenFileFlag)
	}
	host := strings.TrimSpace(e.forgeHost)
	m := config.ForgeHostPattern.FindStringSubmatch(host)
	if m == nil {
		return fmt.Errorf("--%s %q is not a bare host:port (no scheme, path, login, space or leading dash), so nothing was started",
			config.ForgeHostFlag, e.forgeHost)
	}
	if port, _ := strconv.Atoi(m[1]); port < 1 || port > 65535 {
		return fmt.Errorf("--%s %q has port %s, which is not a port (1 to 65535), so nothing was started",
			config.ForgeHostFlag, e.forgeHost, m[1])
	}
	if !forgeCredentialPathRe.MatchString(e.forgeTokenFile) {
		return fmt.Errorf("--%s %q must be an absolute path of letters, digits, dot, dash, underscore and slash, since git runs the helper line through a shell, so nothing was started",
			config.ForgeTokenFileFlag, e.forgeTokenFile)
	}
	return nil
}

// setForgeCredentialHelper tells the container's git to ask `gt
// git-credential-forgejo` for the credentials of the forge host, so the daemon's
// pushes (the landing worker's land/<bead> candidates) carry the landing bot's
// token. The config line names the token file's path and never a token; the
// helper reads the file when git asks, so the file may not exist yet at start
// (the runbook provisions it after the first start). No flag, no change.
func (e containerEntrypoint) setForgeCredentialHelper(self string) error {
	if !e.forgeHostSet {
		return nil
	}
	if !forgeCredentialPathRe.MatchString(self) {
		return fmt.Errorf("the gt binary's path %q holds a character a shell would read, and git runs the credential helper line through a shell, so nothing was started", self)
	}
	host := strings.TrimSpace(e.forgeHost)
	helper := "!" + self + " " + config.GitCredentialForgejoCommand + " --host " + host + " --token-file " + e.forgeTokenFile
	if err := e.run([]string{"git", "config", "--global", "credential.http://" + host + ".helper", helper}); err != nil {
		return fmt.Errorf("set the git credential helper for %s: %w", host, err)
	}
	return nil
}

// installMangoBinary copies the image's mango binary and the stamp of the
// commit it was built from into the town's runtime directory for this
// platform, where the metrics analysis reads them. An image older than the
// per-platform build carries no mango directory and is not an error; a mango
// directory without its files is a broken image and is refused rather than
// leave the town running the Mac binary the shared runtime also holds. Both
// files are read before either is written, so a broken image writes nothing,
// and the pair already installed is read before it is replaced and put back
// when either write fails, so a failed install never leaves the image's new
// binary under the stamp of the build it replaced (gt-50mngy). The roots are
// parameters, so a test uses temporary directories.
func installMangoBinary(imageDir, townRoot string) error {
	if _, err := os.Stat(imageDir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read the image's mango directory %s: %w", imageDir, err)
	}
	// The destination is the directory the metrics lookup reads, so the install
	// and the lookup cannot drift apart.
	dir := filepath.Dir(metrics.PlatformMangoBin(townRoot))
	type mangoFile struct {
		name string
		mode os.FileMode
		data []byte
	}
	files := make([]mangoFile, 0, 2)
	for _, spec := range []struct {
		name string
		mode os.FileMode
	}{
		{mangoBinName, 0o755},
		{mangoShaName, 0o644},
	} {
		source := filepath.Join(imageDir, spec.name)
		data, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("read the image's %s: %w", source, err)
		}
		files = append(files, mangoFile{name: spec.name, mode: spec.mode, data: data})
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create the town's mango directory %s: %w", dir, err)
	}
	bin, stamp := files[0], files[1]
	binDest := filepath.Join(dir, bin.name)
	stampDest := filepath.Join(dir, stamp.name)
	previousBin, err := readInstalledMango(binDest)
	if err != nil {
		return fmt.Errorf("read the town's mango binary %s: %w", binDest, err)
	}
	previousStamp, err := readInstalledMango(stampDest)
	if err != nil {
		return fmt.Errorf("read the town's mango stamp %s: %w", stampDest, err)
	}
	// The binary is written before its stamp: the stamp is what the metrics
	// analysis compares with origin/main, so a stamp put in place first would
	// describe a binary that is not there yet. A failed write puts the pair
	// back, so the stamp never describes a binary it was not written with.
	if err := atomicfile.WriteFile(binDest, bin.data, bin.mode); err != nil {
		return undoInstall(fmt.Errorf("install the image's mango at %s: %w", binDest, err), previousBin)
	}
	if err := atomicfile.WriteFile(stampDest, stamp.data, stamp.mode); err != nil {
		return undoInstall(fmt.Errorf("install the image's mango stamp at %s: %w", stampDest, err), previousBin, previousStamp)
	}
	return nil
}

// undoInstall puts back the files an install replaced after a write failed, and
// reports the write failure together with any restore failure: a restore that
// went wrong leaves the town partly installed, and dropping it would report the
// failure as handled.
func undoInstall(installErr error, files ...installedMango) error {
	errs := []error{installErr}
	for _, file := range files {
		if err := file.putBack(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// installedMango is one file of the town's mango directory as it was before an
// install replaced it, so a failed install can put the pair back.
type installedMango struct {
	path    string
	data    []byte
	mode    os.FileMode
	regular bool // a regular file held the path, and its contents are in data
	present bool // something held the path; only a regular file is put back
}

// readInstalledMango records path as it is now.
func readInstalledMango(path string) (installedMango, error) {
	installed := installedMango{path: path}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return installed, nil
	}
	if err != nil {
		return installedMango{}, err
	}
	installed.present = true
	if !info.Mode().IsRegular() {
		return installed, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return installedMango{}, err
	}
	installed.regular, installed.data, installed.mode = true, data, info.Mode().Perm()
	return installed, nil
}

// putBack returns path to the state readInstalledMango found: a regular file is
// written back, and a path that held nothing is removed if the install created
// it. A path that held anything else is not put back, because the town's mango
// files are regular files and the install does not own that occupant.
func (m installedMango) putBack() error {
	if m.regular {
		if err := atomicfile.WriteFile(m.path, m.data, m.mode); err != nil {
			return fmt.Errorf("put the installed mango back at %s: %w", m.path, err)
		}
		return nil
	}
	if m.present {
		return nil
	}
	info, err := os.Lstat(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the path the install wrote at %s: %w", m.path, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the image's file at %s: %w", m.path, err)
	}
	return nil
}

// installPluginBinaries copies each plugin binary the image carries into the
// town's plugin directory for this platform, where the plugin runner looks for
// it: bin/<goos>-<goarch>/<name> (gt-gg9hv.6). The town's plugin set comes from
// the shared volume, so a plugin the town carries no plugin.md for is skipped
// and logged once, and an image directory that does not exist is not an error.
// The roots are parameters, so a test uses temporary directories.
func installPluginBinaries(imageDir, townRoot string, logf func(format string, args ...any)) error {
	entries, err := os.ReadDir(imageDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the image's plugin directory %s: %w", imageDir, err)
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	for _, entry := range entries {
		// os.Stat follows a symlink, so a plugin directory the image links in is
		// installed like a real one. A dangling link is a broken image: refuse.
		entryPath := filepath.Join(imageDir, entry.Name())
		info, err := os.Stat(entryPath)
		if err != nil {
			return fmt.Errorf("stat the image's plugin entry %s: %w", entryPath, err)
		}
		if !info.IsDir() {
			continue
		}
		name := entry.Name()
		townPluginDir := filepath.Join(townRoot, "plugins", name)
		hasDefinition, err := townPluginHasDefinition(townPluginDir)
		if err != nil {
			return fmt.Errorf("read the town's plugin.md under %s: %w", townPluginDir, err)
		}
		if !hasDefinition {
			logf("plugin %q is not in the town's plugin directory, so the image's binary for it was not installed", name)
			continue
		}
		source := filepath.Join(imageDir, name, name)
		dest := filepath.Join(townPluginDir, "bin", platform, name)
		if err := installPluginBinary(source, dest); err != nil {
			return err
		}
	}
	return nil
}

// townPluginHasDefinition reports whether pluginDir holds the plugin.md the
// scanner loads. A plugin.md that is not a regular file is not one: the scanner
// cannot read it and skips the plugin.
func townPluginHasDefinition(pluginDir string) (bool, error) {
	info, err := os.Stat(filepath.Join(pluginDir, "plugin.md"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// installPluginBinary copies source to dest with mode 0755. atomicfile.WriteFile
// writes a temporary file in dest's own directory and renames it over dest, so
// a reader sees the old binary or the new one and never a partial file, and an
// existing binary is replaced.
func installPluginBinary(source, dest string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read the image's plugin binary %s: %w", source, err)
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create the plugin binary directory %s: %w", dir, err)
	}
	if err := atomicfile.WriteFile(dest, data, 0o755); err != nil {
		return fmt.Errorf("install the plugin binary %s: %w", dest, err)
	}
	return nil
}
