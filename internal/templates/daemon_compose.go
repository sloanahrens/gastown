package templates

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/manifestdata"
)

// ComposeSpec is the identity of one town's daemon service, which
// RenderDaemonCompose writes into the compose file the container supervisor
// drives. Every field is per town: a scratch town passes its own project,
// container name and root, so its render shares nothing with the live one.
type ComposeSpec struct {
	// Project is the compose project the supervisor passes to `docker compose
	// -p`. The file itself does not carry it, so the flag is the only place
	// the project is named; it is still required here so a caller cannot
	// render a file that belongs to no project.
	Project string

	// ContainerName is the container's name, the daemon's owner id and the
	// `docker inspect` target (internal/daemon's owner, internal/cmd's
	// supervisor), so all three always name one container.
	ContainerName string

	// Image is the tag the deployer built on this host, gt-daemon:<rev>.
	Image string

	// TownRoot is the directory mounted at its own absolute path, so the daemon
	// inside the container reads and writes the town's own files. It is a bind of
	// the Mac's directory unless TownStateVolume names a volume.
	TownRoot string

	// TownStateVolume is `gt daemon compose write --town-state-volume`: the name
	// of an external docker volume mounted at TownRoot in place of the Mac's
	// directory, so the daemon's state lives in the volume and a compose `down`
	// never removes it. It is declared external because the volume outlives the
	// project. Only the live render takes it, and a name that is not a docker
	// volume name is refused. With it set, no bind may mount a path the
	// town-state manifest gives a session: the volume carries the daemon's
	// paths, and a session's path would be a host bind shadowing it
	// (gt-yjzepk.6).
	TownStateVolume string

	// SessionOwned is the seam the render reads the town-state manifest through:
	// it reports whether a town-root-relative path falls under a session-owned
	// entry and names that entry. It is required when TownStateVolume is set, so
	// a caller that does not wire the manifest is refused rather than left with
	// no check at all; the command wires it to townstate.Default().
	SessionOwned func(rel string) (entry string, ok bool)

	// StopGrace is docker's stop_grace_period: the time between SIGTERM and
	// SIGKILL on `docker compose stop` and on a restart. It may not be shorter
	// than the daemon's own worst-case graceful shutdown; see MinimumStopGrace.
	StopGrace time.Duration

	// DoltHost is the host the daemon reaches the town's Dolt through from
	// inside the container. The rendered file does not carry it: the daemon
	// reads the town config over the bind mount, which keeps the endpoint in
	// one source.
	//
	// It is the resolved host, not town.json's container_host alone: the caller
	// passes container_host when the town sets it and dolt.host when it does
	// not, because only the town config holds both. It is required here, and
	// must name the Mac on the far side of the mount. An empty host is refused
	// because town.json's dolt.host may be empty, and an empty one means the
	// local machine, which inside the container is the container itself; so is
	// a loopback address, for the same reason.
	//
	// A scratch spec does not set DoltHost, and a scratch render ignores it. The
	// scratch daemon's Dolt host comes from the scratch town's own settings under
	// the bind mount, which the spec does not carry. The scratch render is tested
	// to carry none of the live render's names, the live Dolt's among them.
	DoltHost string

	// Scratch renders the scratch project: its own engine, networks, volumes and
	// Dolt server, and the --scratch-town flag that lets the container
	// entrypoint install the scratch town. No name in it is one the live render
	// carries.
	Scratch bool

	// ScratchDoltImage is the Dolt server image the scratch project runs. It is
	// required when Scratch is set; the scratch spec passes DoltServerImage.
	ScratchDoltImage string

	// DoltPort is the port the scratch Dolt server listens on, which the
	// healthcheck probes and the daemon's command passes to the entrypoint. Zero
	// means ScratchDoltPort, the pinned image's default. Only the scratch render
	// takes it: a live spec that sets it is refused.
	DoltPort int

	// OMTokenFile is the file the om subscription token is read from, and it is
	// absolute on both renders; where it lives is the one thing that differs.
	// The scratch render takes a path inside the container, which the scratch
	// runbook mounts there, and passes it to the entrypoint as --om-token-file
	// beside --scratch-town (gt-qpeei.36). The live render takes the file's path
	// on the host, mounts it read-only at LiveOMTokenPath, and passes that path
	// to the entrypoint, so the live container's om has a credential (gt-yaqct).
	// Unset, neither render carries the flag or the mount.
	OMTokenFile string

	// OMConfigFile is the host path of the om config RenderOMConfig derives for
	// the live container, which the command writes beside the compose file. The
	// render mounts it read-only at LiveOMConfigPath and sets XDG_CONFIG_HOME, so
	// om's backend is the container's claude-exec and carries the token mount.
	// A live spec with an om token file must carry it, since a token file with no
	// config leaves om a bare claude with no credential; a config without a token
	// file, or on a scratch spec, is refused (gt-gg9hv.13).
	OMConfigFile string

	// HMConfigFile is the host path of the hm config RenderHMConfig derives for
	// the live container, which the command writes beside the compose file. The
	// render mounts it read-only at LiveHMConfigPath and sets HM_CONFIG to that
	// path, so the container's hm reaches the Mac's database instead of its own
	// loopback. Unset, neither the mount nor the variable is rendered; a scratch
	// spec may not set it (gt-yjzepk.13).
	HMConfigFile string

	// ForgeHost and ForgeTokenFile give the live container's git the Forgejo
	// credential helper: ForgeHost is the forge's bare host:port, and
	// ForgeTokenFile is the host path of the landing bot's token file. A live
	// spec carries both or neither. The render mounts the host file read-only at
	// LiveForgeTokenPath and passes the pair to the entrypoint as --forge-host
	// and --forge-token-file, so the landing worker's pushes carry the bot's
	// token; the file's path, never its contents, reaches the compose file. A
	// scratch spec sets neither: its render takes the forge credential from
	// ScratchForge and the scratch token directory (gt-rpx80).
	ForgeHost      string
	ForgeTokenFile string

	// ExtraHosts are host aliases the live container resolves through its own
	// /etc/hosts, each "<hostname>:<ipv4 or host-gateway>", each hostname named
	// once. The rigs' git remotes name forgejo:3000, which a container cannot
	// resolve, so the render writes them under the daemon service's extra_hosts,
	// sorted, and writes no line at all when the list is empty. A malformed or
	// repeated entry is refused, named in the error, rather than written for
	// docker to reject later. A scratch spec sets none: its project reaches
	// nothing outside itself (gt-gg9hv.2).
	ExtraHosts []string

	// Env, KubeconfigFiles, OpenShellConfigDir and ConfigDir are the live
	// container's client configuration (gt-yj0vh), the counterpart of what the
	// launchd plist and the operator's home give the host daemon. Env is rendered
	// into the daemon's environment, sorted; it is the caller's already filtered
	// daemon.env, and the render refuses a name it sets itself. KubeconfigFiles
	// are absolute paths joined into KUBECONFIG, first file first; the render checks
	// they are absolute, and the caller sees to it that they lie under the town bind. OpenShellConfigDir is the host's openshell config directory,
	// mounted read-only at LiveOpenShellConfigPath, with XDG_CONFIG_HOME set to
	// its parent. ConfigDir is the town's config directory (~/.config/gt). With
	// no Secrets it is mounted read-only both at its own path and at
	// LiveConfigDirPath, so a setting that names either path finds its file;
	// with Secrets only the listed files are mounted, because the directory
	// holds far more than the daemon reads (gt-gg9hv.66). A scratch spec sets
	// none of them.
	Env                map[string]string
	KubeconfigFiles    []string
	OpenShellConfigDir string
	ConfigDir          string

	// Secrets is the enumerated manifest of files the daemon reads from
	// ConfigDir (deploy/daemon/secrets.yaml, whose header defines the fields).
	// Non-empty, the render mounts each listed file read-only at its own path
	// and at LiveConfigDirPath and mounts no directory of the config tree.
	// Empty, the render mounts ConfigDir as a directory, as it always has.
	Secrets []SecretFile

	// SecretsFS is the seam the render lstats and stats the listed files
	// through. Unset, it is the host's filesystem; a test supplies a fake that
	// records every call and refuses a content read (gt-gg9hv.66).
	SecretsFS SecretFS

	// BackupDir is the host's Dolt backup directory, an absolute path. The Dolt
	// server container writes each night's backup there at the same path, so the
	// live daemon mounts it read-write at that path too, and GT_DOLT_BACKUP_ROOT
	// is the variable that points the daemon at it (gt-gg9hv.17).
	// Unset, the render carries neither the mount nor the variable.
	BackupDir string

	// BindRO are host directories the live daemon mounts read-only at the same
	// path, from `gt daemon compose write --bind-ro` (gt-vqexg). Each is absolute
	// and cleaned, does not lie inside, contain or equal the town root, does not
	// equal a path the render already mounts, and is given once. The render sorts
	// them by path. Unset, the render carries none. A scratch spec sets none.
	BindRO []string

	// OMTokenMetaFile is the host path of the om token's expiry sidecar
	// (daemon.om_token_meta); the render mounts it read-only at the same path, so
	// the setting reads it unchanged. LandingWorkRoot is the landing worker's work
	// directory (patrols.landing_worker.work_root), bound read-write at the same
	// path so its clones outlive a redeploy. TimeZone is an IANA zone name for TZ,
	// so the daemon's local windows fall at the host's local times; a name only,
	// never zone data (gt-vi1p3).
	OMTokenMetaFile string
	LandingWorkRoot string
	TimeZone        string

	// LiveTownRoot is the live town's root, which a scratch spec must not
	// mount, nor mount anything under. It is required when Scratch is set, and
	// absolute, so a scratch render is always checked against the live town it
	// could otherwise reach.
	LiveTownRoot string

	// ScratchForge adds a throwaway Forgejo stack to the scratch project: the
	// forge, its Postgres, a runner and the engine the runner drives, for the L2
	// window. Only a scratch spec takes it. Its names and volumes are the scratch
	// project's own, and its two host ports are the fields below, so the stack
	// never takes the live Forgejo's 3000 and 2222.
	ScratchForge bool

	// ScratchForgeHTTPPort and ScratchForgeSSHPort are the host ports the scratch
	// Forgejo publishes, on 127.0.0.1 as the live project does. They are required
	// when ScratchForge is set, must be valid, distinct ports, and may not be the
	// live Forgejo's 3000 and 2222: a start on those ports would fight the live
	// forge for them, or worse, find it already there.
	ScratchForgeHTTPPort int
	ScratchForgeSSHPort  int
}

// MinimumStopGrace is the shortest stop_grace_period RenderDaemonCompose
// accepts, and the shortest that is ever correct: a grace period shorter than
// the daemon's own graceful shutdown gets it SIGKILLed mid-shutdown on every
// stop, which for the Dolt SQL server step risks its append-only journal
// instead of letting it exit cleanly.
//
// The value is daemon.ShutdownBudget. internal/daemon imports this package, so
// the renderer cannot read that constant here; TestMinimumStopGrace_CoversShutdownBudget
// pins the two together, and a change to either that unties them fails it.
const MinimumStopGrace = 30 * time.Second

// The sweep engine's names are fixed, not per town: the compose project scopes
// them, which is what keeps a scratch render off the live town's engine (E1
// renders its own).
const (
	dindService  = "sweep-dind"
	dindNetwork  = "sweep"
	dindVolume   = "sweep_dind_data"
	dindDataPath = "/var/lib/docker"

	// defaultNetwork is compose's implicit network, declared here because a
	// service that lists its networks explicitly gets no implicit one, and the
	// daemon needs a way out to host.docker.internal.
	defaultNetwork = "default"
)

// composeNames are the names one project's engine and networks take. The live
// project's are the sweep names above. A scratch project takes its own, so no
// name in its render is one the live render carries.
type composeNames struct {
	engine  string // the engine's service name, which DOCKER_HOST reaches
	network string // the internal network the engine, the daemon and the scratch Dolt share
	volume  string // the engine's data volume
	egress  string // the network the daemon reaches the outside through
}

var (
	liveNames    = composeNames{engine: dindService, network: dindNetwork, volume: dindVolume, egress: defaultNetwork}
	scratchNames = composeNames{engine: "scratch-dind", network: "scratch-internal", volume: "scratch_dind_data", egress: "scratch-egress"}
)

// ScratchDoltService is the scratch Dolt server's name on the internal network,
// and the only DoltHost a scratch spec may name: the live Dolt is on the Mac.
const ScratchDoltService = "scratch-dolt"

// ScratchDoltPort is the port the scratch Dolt server listens on: the pinned
// image's default, which the service does not change. The healthcheck probes
// it and the daemon's command passes it to the entrypoint, so the install
// dials the port the server serves rather than gt's own default, 3307.
const ScratchDoltPort = 3306

// LiveOMTokenPath is where the live render mounts the om token file inside the
// container, and the path its command line passes to the entrypoint
// (gt-rpx80). The path lives in manifestdata, which the cluster renderer may
// import.
const LiveOMTokenPath = manifestdata.LiveOMTokenPath //nolint:gosec // a mount path, not a credential

// LiveOMConfigPath is where om reads the derived config beneath the container's
// XDG home (gt-gg9hv.13).
const LiveOMConfigPath = manifestdata.LiveOMConfigPath

// LiveForgeTokenPath is the fixed path where the live render mounts the forge
// token file and names it on the entrypoint's command line.
const LiveForgeTokenPath = manifestdata.LiveForgeTokenPath //nolint:gosec // a mount path, not a credential

// The scratch project's Dolt server, which only the scratch render has. It
// joins the internal network alone and publishes no port.
const (
	scratchDoltVolume   = "scratch_dolt_data"
	scratchDoltDataPath = "/var/lib/dolt"
)

// The live Forgejo project's host ports. A scratch forge may take neither.
const (
	liveForgeHTTPPort = 3000
	liveForgeSSHPort  = 2222
)

// The scratch Forgejo stack's service names, which a compose command takes to
// start them: the forge, its database, the engine the runner drives and the
// runner. Each is the scratch project's own: none is a name the live Forgejo
// project declares (the list is the test's, in
// daemon_compose_scratch_forge_test.go, so it is written once), and the compose
// project scopes them again.
const (
	ScratchForgejoService     = "scratch-forgejo"
	ScratchForgejoDBService   = "scratch-forgejo-db"
	ScratchForgeDindService   = "scratch-forge-dind"
	ScratchForgeRunnerService = "scratch-forge-runner"
)

// ScratchForgeServices are the services the scratch forge adds to the file, in
// the order a compose command names them to start them: none is a dependency of
// daemon, so naming daemon alone would leave the forge stopped. The list is
// empty unless s.ScratchForge is set, the only case that renders the body, and
// daemon_compose_scratch_forge_test.go holds it to the body's service set.
func ScratchForgeServices(s ComposeSpec) []string {
	if !s.ScratchForge {
		return nil
	}
	return []string{
		ScratchForgejoService,
		ScratchForgejoDBService,
		ScratchForgeDindService,
		ScratchForgeRunnerService,
	}
}

// The stack's networks, the alias the runner reaches its engine by, and its
// volumes: no command names one from outside this package, so they stay
// unexported.
const (
	scratchForgeNetwork   = "scratch-forge"
	scratchForgeCINetwork = "scratch-forge-ci"
	scratchForgeDindAlias = "docker"

	scratchForgeDBVolume     = "scratch_forgejo_pgdata"
	scratchForgeDataVolume   = "scratch_forgejo_data"
	scratchForgeCertsVolume  = "scratch_forge_certs"
	scratchForgeDindVolume   = "scratch_forge_dind_data"
	scratchForgeRunnerVolume = "scratch_forge_runner_data"
)

// scratchForgeBody is the throwaway Forgejo stack. %[1]d and %[2]d are the host
// ports for http and ssh, bound to 127.0.0.1 as the live project binds its own.
// SSH_PORT is the host port, as the live project's is: it is the port clone URLs
// advertise, and the port a client reaches the forge's ssh on is the host one.
//
// Three networks keep the privileged engine away from the daemon. The forge and
// its Postgres share an internal one, so the database has no way in or out but
// the forge. The forge, the runner and the runner's engine share the CI network,
// which is not internal because the engine pulls its job images through it; the
// daemon is not on it. The forge also joins the scratch egress network, the
// daemon's, so the daemon reaches the forge by its service name without ever
// reaching the engine. Postgres trusts connections on its network because the
// stack holds nothing but scratch data, and a password would be one more secret
// for the render to carry or interpolate. The engine speaks TLS with its own
// certs volume, as the live one does.
//
// The runner reaches its engine as "docker": the engine's cert is valid for that
// name, and the CI network gives the engine that alias explicitly, because a
// container's hostname is not a name compose's DNS resolves.
//
// The runner names this forge only: the one forge URL the file carries is
// http://scratch-forgejo:3000/. compose starts it with the rest of the project;
// without a config.yml in its data volume it exits, and restart "no" leaves it
// stopped. The L2 window registers it against this forge and starts it again.
const scratchForgeBody = `  ` + ScratchForgejoDBService + `:
    image: %[3]q
    restart: unless-stopped
    environment:
      POSTGRES_USER: forgejo
      POSTGRES_DB: forgejo
      POSTGRES_HOST_AUTH_METHOD: trust
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U forgejo -d forgejo"]
      interval: 5s
      timeout: 3s
      retries: 20
    volumes:
      - type: volume
        source: ` + scratchForgeDBVolume + `
        target: /var/lib/postgresql/data
    networks:
      - ` + scratchForgeNetwork + `
  ` + ScratchForgejoService + `:
    image: %[4]q
    restart: unless-stopped
    depends_on:
      ` + ScratchForgejoDBService + `:
        condition: service_healthy
    environment:
      FORGEJO__database__DB_TYPE: postgres
      FORGEJO__database__HOST: ` + ScratchForgejoDBService + `:5432
      FORGEJO__database__NAME: forgejo
      FORGEJO__database__USER: forgejo
      FORGEJO__server__DOMAIN: ` + ScratchForgejoService + `
      FORGEJO__server__ROOT_URL: http://` + ScratchForgejoService + `:3000/
      FORGEJO__server__SSH_DOMAIN: ` + ScratchForgejoService + `
      FORGEJO__server__SSH_PORT: "%[2]d"
      FORGEJO__security__INSTALL_LOCK: "true"
      FORGEJO__service__DISABLE_REGISTRATION: "true"
      FORGEJO__actions__ENABLED: "true"
      FORGEJO__repository__DEFAULT_REPO_UNITS: "repo.code,repo.actions,repo.pulls,repo.issues"
    volumes:
      - type: volume
        source: ` + scratchForgeDataVolume + `
        target: /data
    ports:
      - "127.0.0.1:%[1]d:3000"
      - "127.0.0.1:%[2]d:22"
    networks:
      - ` + scratchForgeNetwork + `
      - ` + scratchForgeCINetwork + `
      - %[6]s
  ` + ScratchForgeDindService + `:
    image: %[5]q
    restart: unless-stopped
    privileged: true
    command: ["--insecure-registry=` + ScratchForgejoService + `:3000"]
    environment:
      DOCKER_TLS_CERTDIR: /certs
    volumes:
      - type: volume
        source: ` + scratchForgeCertsVolume + `
        target: /certs
      - type: volume
        source: ` + scratchForgeDindVolume + `
        target: /var/lib/docker
    networks:
      ` + scratchForgeCINetwork + `:
        aliases:
          - ` + scratchForgeDindAlias + `
  ` + ScratchForgeRunnerService + `:
    image: %[7]q
    restart: "no"
    depends_on:
      ` + ScratchForgejoService + `:
        condition: service_started
      ` + ScratchForgeDindService + `:
        condition: service_started
    working_dir: /data
    command: ["forgejo-runner", "--config", "config.yml", "daemon"]
    environment:
      DOCKER_HOST: tcp://` + scratchForgeDindAlias + `:2376
      DOCKER_CERT_PATH: /certs/client
      DOCKER_TLS_VERIFY: "1"
    volumes:
      - type: volume
        source: ` + scratchForgeRunnerVolume + `
        target: /data
      - type: volume
        source: ` + scratchForgeCertsVolume + `
        target: /certs
    networks:
      - ` + scratchForgeCINetwork + `
`

// scratchForgeNetworkDecl and scratchForgeVolumeDecls are the stack's entries in
// the file's networks and volumes sections.
const scratchForgeNetworkDecl = "  " + scratchForgeNetwork + ":\n    internal: true\n" +
	"  " + scratchForgeCINetwork + ":\n"

var scratchForgeVolumeDecls = "  " + scratchForgeDBVolume + ":\n" +
	"  " + scratchForgeDataVolume + ":\n" +
	"  " + scratchForgeCertsVolume + ":\n" +
	"  " + scratchForgeDindVolume + ":\n" +
	"  " + scratchForgeRunnerVolume + ":\n"

// scratchOperatorNote is what the scratch daemon's compose file tells the
// operator about a failed install. It is written once, here: scratchDaemonCommand
// turns each line into a YAML comment, so the file cannot carry a second copy
// that drifts from this one.
var scratchOperatorNote = []string{
	"After a failed scratch install the entrypoint refuses every start until",
	"the operator removes .gt-install-incomplete and what the install left,",
	"from the host side of the scratch directory.",
}

// scratchDaemonCommand is the daemon's command in the scratch project: the
// operator note, then the flags the container entrypoint is passed. --scratch-town
// is the one way a container asks for its scratch town's install, and
// --dolt-host ScratchDoltService rides beside it, so the install writes the
// endpoint of the Dolt server this project runs rather than starting one inside
// the daemon container. When omTokenFile is set, --om-token-file <path> follows
// them. The entrypoint is the image's ENTRYPOINT, so the command carries its
// flags alone. The live project passes only the om token flag, when it has a
// token file (liveDaemonCommand).
func scratchDaemonCommand(omTokenFile string, doltPort int, forgeTokenFile string) string {
	var b strings.Builder
	for _, line := range scratchOperatorNote {
		b.WriteString("    # " + line + "\n")
	}
	argv := []string{"--" + config.ScratchTownFlag, "--" + config.DoltHostFlag, ScratchDoltService,
		"--" + config.DoltPortFlag, strconv.Itoa(doltPort)}
	if omTokenFile != "" {
		argv = append(argv, "--"+config.OMTokenFileFlag, composeLiteral(omTokenFile))
	}
	if forgeTokenFile != "" {
		argv = append(argv, "--"+config.ForgeHostFlag, ScratchForgejoService+":3000",
			"--"+config.ForgeTokenFileFlag, composeLiteral(forgeTokenFile))
	}
	b.WriteString(composeCommand(argv))
	return b.String()
}

// composeCommand renders the daemon's command entry: each flag quoted, so a
// value cannot change the YAML's shape. The entrypoint is the image's
// ENTRYPOINT, so the list carries the flags alone.
func composeCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = strconv.Quote(arg)
	}
	return fmt.Sprintf("    command: [%s]\n", strings.Join(quoted, ", "))
}

// liveDaemonCommand is the live daemon's command line: the flags the entrypoint
// reads the container's credentials from, each naming the path the render
// mounts the host file at rather than the host's own. --om-token-file carries
// the om token file, whose contents the entrypoint turns into the daemon's
// environment; --forge-host and --forge-token-file make git ask the forge
// credential helper, and the helper reads the token file at push time. Either
// credential may be absent, and a command line carrying neither is no command
// line at all, so a live spec with neither renders as it always has (gt-yaqct,
// gt-rpx80). No token reaches this file or an argv.
func liveDaemonCommand(s ComposeSpec) string {
	argv := make([]string, 0, 5)
	if s.OMTokenFile != "" {
		argv = append(argv, "--"+config.OMTokenFileFlag, LiveOMTokenPath)
	}
	if s.ForgeHost != "" {
		argv = append(argv, "--"+config.ForgeHostFlag, s.ForgeHost,
			"--"+config.ForgeTokenFileFlag, LiveForgeTokenPath)
	}
	if len(argv) == 0 {
		return ""
	}
	return composeCommand(argv)
}

// liveTokenMounts is the live daemon's volumes beyond the town bind mount: each
// credential file the spec names, bind-mounted read-only at the fixed path its
// flag names. A read-only mount is what keeps the container from writing the
// host file, and the file is the host's own: nothing copies it into the image,
// the town or this file.
func liveTokenMounts(s ComposeSpec) string {
	var b strings.Builder
	if s.OMTokenFile != "" {
		b.WriteString(liveTokenMount(s.OMTokenFile, LiveOMTokenPath))
	}
	if s.OMConfigFile != "" {
		b.WriteString(liveTokenMount(s.OMConfigFile, LiveOMConfigPath))
	}
	if s.HMConfigFile != "" {
		b.WriteString(liveTokenMount(s.HMConfigFile, LiveHMConfigPath))
	}
	if s.ForgeTokenFile != "" {
		b.WriteString(liveTokenMount(s.ForgeTokenFile, LiveForgeTokenPath))
	}
	return b.String()
}

// OMConfigSettings are the reviewer settings the live container's derived om
// config carries: the model its claude-exec backend runs, and the threshold,
// depth and timeout om applies beside the backend. RenderOMConfig takes them as
// its input, so the file's values come from here and not from literals in the
// render.
type OMConfigSettings struct {
	Model     string
	Threshold float64
	Depth     string
	Timeout   int
}

// DefaultOMConfigSettings is the reviewer settings the container runs with
// unless a caller changes them.
func DefaultOMConfigSettings() OMConfigSettings {
	return OMConfigSettings{Model: "haiku", Threshold: 0.75, Depth: "standard", Timeout: 300}
}

// omSettingName is a model or depth name om and the reviewer read as one plain
// word, so it needs no JSON escaping.
var omSettingName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// RenderOMConfig is the om config the live container reads at LiveOMConfigPath.
// Its backend is the container's claude-exec, which reads the token from
// LiveOMTokenPath, so the file names the mount's path and never the host's, and
// it carries no credential. A setting om cannot run with is refused here,
// rather than written for om to fail on later.
func RenderOMConfig(o OMConfigSettings) ([]byte, error) {
	switch {
	case o.Model == "":
		return nil, errors.New("om config: Model is empty; the claude-exec backend needs a model")
	case !omSettingName.MatchString(o.Model):
		return nil, fmt.Errorf("om config: Model %q is not a plain name such as claude-haiku-5-5", o.Model)
	case o.Depth == "":
		return nil, errors.New("om config: Depth is empty")
	case !omSettingName.MatchString(o.Depth):
		return nil, fmt.Errorf("om config: Depth %q is not a plain name such as standard", o.Depth)
	case o.Timeout <= 0:
		return nil, fmt.Errorf("om config: Timeout %d is not a positive number of seconds", o.Timeout)
	case !(o.Threshold > 0 && o.Threshold <= 1):
		return nil, fmt.Errorf("om config: Threshold %v is not in (0, 1]", o.Threshold)
	}
	// Written by hand, not marshaled, so the render has no encoder error to
	// refuse: each value is a checked plain name or a finite number.
	argv := []string{"gt", "daemon", "claude-exec", "--" + config.OMTokenFileFlag, LiveOMTokenPath, "--", "-p", "--model", o.Model}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = strconv.Quote(arg)
	}
	return []byte(fmt.Sprintf("{\n  \"backend\": [%s],\n  \"threshold\": %s,\n  \"depth\": %s,\n  \"timeout\": %d\n}\n",
		strings.Join(quoted, ", "), strconv.FormatFloat(o.Threshold, 'f', -1, 64), strconv.Quote(o.Depth), o.Timeout)), nil
}

// validateOMConfig pairs the derived om config with the token it reads. A live
// token file without its config leaves om running a bare claude with no
// credential, the failure gt-gg9hv.13 exists to end, so the pair is given
// together, and a config is refused on a scratch spec, which runs no om.
func (s ComposeSpec) validateOMConfig() error {
	switch {
	case s.OMConfigFile == "" && s.OMTokenFile != "" && !s.Scratch:
		return fmt.Errorf("compose spec: OMTokenFile %q is set on a live spec without OMConfigFile; om would run with no config and no credential", s.OMTokenFile)
	case s.OMConfigFile != "" && (s.Scratch || s.OMTokenFile == ""):
		return fmt.Errorf("compose spec: OMConfigFile %q is set without a live OMTokenFile; the derived config names the token mount, and only a live token file has one", s.OMConfigFile)
	case s.OMConfigFile != "" && !filepath.IsAbs(s.OMConfigFile):
		return fmt.Errorf("compose spec: OMConfigFile %q is not an absolute path; the live render mounts it on the host", s.OMConfigFile)
	}
	return nil
}

// validateHMConfig pairs the derived hm config with the mount and the variable
// that name it. The config is mounted at LiveHMConfigPath and HM_CONFIG names
// that path, so a daemon.env entry naming another path is refused rather than
// left pointing hm at a file no mount provides; the render writes HM_CONFIG
// itself when it mounts the config, so an entry naming the same path is not
// repeated (gt-yjzepk.13).
func (s ComposeSpec) validateHMConfig() error {
	if s.HMConfigFile == "" {
		return nil
	}
	switch {
	case s.Scratch:
		return fmt.Errorf("compose spec: HMConfigFile %q is set on a scratch spec; only the live render mounts the container's hm config", s.HMConfigFile)
	case !filepath.IsAbs(s.HMConfigFile):
		return fmt.Errorf("compose spec: HMConfigFile %q is not an absolute path; the live render mounts it on the host", s.HMConfigFile)
	case strings.ContainsFunc(s.HMConfigFile, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return fmt.Errorf("compose spec: HMConfigFile %q holds a control character, which neither a mount nor a compose value can carry", s.HMConfigFile)
	}
	if v, ok := s.Env[hmConfigEnv]; ok && v != LiveHMConfigPath {
		return fmt.Errorf("compose spec: Env names %s as %q, but the render mounts the container's hm config at %s; name that path or leave the entry out", hmConfigEnv, v, LiveHMConfigPath)
	}
	return nil
}

// townStateVolumeName is a docker volume name: a letter or digit, then letters,
// digits, dots, underscores and hyphens. An external volume is reached by the
// name the host gives it, so a name docker would refuse is refused here rather
// than written into a file whose `up` then fails.
var townStateVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// validateTownState refuses a town-state volume the file cannot declare and a
// mount that would bind a session's path beside it. The volume carries the town
// root's state; every other mount the file writes stays as it is, so only a path
// the manifest gives a session is refused, and a path it does not class is kept
// as before. A session-owned path is refused, never silently bound over the
// volume (gt-yjzepk.6).
func (s ComposeSpec) validateTownState() error {
	if s.TownStateVolume == "" {
		return nil
	}
	switch {
	case s.Scratch:
		return fmt.Errorf("compose spec: TownStateVolume %q is set on a scratch spec; only the live render replaces the town bind with a volume", s.TownStateVolume)
	case !townStateVolumeName.MatchString(s.TownStateVolume):
		return fmt.Errorf("compose spec: --town-state-volume %q is not a valid volume name; a name starts with a letter or digit and holds letters, digits, dots, underscores and hyphens", s.TownStateVolume)
	case s.TownStateVolume == dindVolume:
		return fmt.Errorf("compose spec: --town-state-volume %q is the engine's data volume %q; the file declares each volume name once", s.TownStateVolume, dindVolume)
	case s.SessionOwned == nil:
		return fmt.Errorf("compose spec: TownStateVolume %q is set without SessionOwned; the town-state manifest is what refuses a session's path, so a render without it would bind one in silence", s.TownStateVolume)
	}
	return s.checkTownStateMounts()
}

// checkTownStateMounts refuses a bind of the town root, which would mount over
// the volume at its own target, and a bind whose host path falls under a
// session-owned town-state entry. The paths are resolved first, because the
// mount follows a link: a directory that names itself daemon but resolves into
// a rig's polecats tree is still a session's path. A path that cannot be
// resolved is refused rather than taken as outside the town.
func (s ComposeSpec) checkTownStateMounts() error {
	for _, hostPath := range s.liveBindSources() {
		if hostPath == "" {
			continue
		}
		rel, inside, err := townRelative(s.TownRoot, hostPath)
		if err != nil {
			return fmt.Errorf("compose spec: --town-state-volume %q is set, and the mount %q cannot be resolved to check it against the town-state manifest: %w", s.TownStateVolume, hostPath, err)
		}
		if !inside {
			continue
		}
		if rel == "" {
			return fmt.Errorf("compose spec: --town-state-volume %q is set, and the mount %q is the town root itself, where the volume is mounted; a bind there would shadow the volume", s.TownStateVolume, hostPath)
		}
		if entry, owned := s.SessionOwned(rel); owned {
			return fmt.Errorf("compose spec: --town-state-volume %q is set, and the mount %q falls under %q, a session-owned town-state entry; the volume carries the daemon's paths, so a session's path is not bound beside it", s.TownStateVolume, hostPath, entry)
		}
	}
	return nil
}

// liveBindSources are the host paths the live render binds, the paths the
// town-state check reads. A scratch render binds none of them, and the field
// validation refuses each on a scratch spec.
func (s ComposeSpec) liveBindSources() []string {
	paths := []string{s.OMTokenFile, s.OMConfigFile, s.HMConfigFile, s.ForgeTokenFile,
		s.OpenShellConfigDir, s.ConfigDir, s.BackupDir, s.OMTokenMetaFile, s.LandingWorkRoot}
	paths = append(paths, s.KubeconfigFiles...)
	return append(paths, s.BindRO...)
}

// townRelative returns hostPath relative to townRoot on resolved paths, and
// whether it lies at or under the town at all. A path outside is not a problem
// here: only a bind at or beneath the town can shadow or sit beside the
// town-state volume. The town root itself is inside with an empty relative
// path, so the caller can refuse it: a bind there mounts over the volume's own
// target. Both paths are cleaned and absolute after resolution, so the town's
// own prefix, with its one trailing separator, is what says the mount is
// beneath it.
func townRelative(townRoot, hostPath string) (string, bool, error) {
	town, err := resolvedHostPath(townRoot)
	if err != nil {
		return "", false, err
	}
	real, err := resolvedHostPath(hostPath)
	if err != nil {
		return "", false, err
	}
	if real == town {
		return "", true, nil
	}
	prefix := strings.TrimSuffix(town, string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(real, prefix) {
		return "", false, nil
	}
	return filepath.ToSlash(strings.TrimPrefix(real, prefix)), true, nil
}

// liveTokenMount is one credential file mounted read-only at one path inside the
// container. Both values are escaped as every other spec value is, so a dollar
// sign in the host path is the literal the host has.
func liveTokenMount(hostFile, containerPath string) string {
	return fmt.Sprintf("      - type: bind\n        source: %s\n        target: %s\n        read_only: true\n",
		strconv.Quote(composeLiteral(hostFile)), strconv.Quote(containerPath))
}

// scratchDoltBody is the scratch project's Dolt server service. It has no
// ports, so the only way in is the internal network the daemon also joins. Its
// healthcheck holds the daemon until the server accepts a connection, which is
// the wait the engine's own healthcheck gives the daemon (gt-3bp5q): the port
// is ScratchDoltPort, the Dolt server's default, which the service does not
// change; the probe reads the constant, so it cannot drift from the command.
//
// DOLT_ROOT_HOST is "%" because the image creates root only as root@localhost
// otherwise, and the daemon reaches this server from its own container (L2,
// 2026-10-31: Error 1045 Access denied for user 'root'). A wildcard root host is
// acceptable here and only here: the service publishes no port, joins the
// scratch internal network alone, holds throwaway data, and runs root with no
// password as the live Dolt does. The live render has no such service. The %%
// is doubled in the source because the body is a format string.
//
// The probe runs bash's /dev/tcp, so it needs bash in the image. The pinned
// DoltServerImage has it: a container run from that digest on 2026-10-30
// (linux/arm64) printed /usr/bin/bash and /usr/local/bin/dolt on a Debian 12
// base. The live L1 check still runs the probe before the scratch project is
// trusted.
const scratchDoltBody = `  %[1]s:
    image: %[2]q
    restart: unless-stopped
    environment:
      DOLT_ROOT_HOST: "%%"
    healthcheck:
      test: ["CMD", "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/%[6]d"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 30s
    volumes:
      - type: volume
        source: %[3]s
        target: %[4]s
    networks:
      - %[5]s
`

// ScratchForgeTokenDir is the directory, under the scratch town root, where the
// runbook's provisioning step writes the bots' token files. The landing bot's
// file in it is what the daemon's git credential helper reads (gt-2d6f2).
const ScratchForgeTokenDir = "forge-tokens"

// scratchForgeTokenFile is the landing bot's token file the daemon's git
// credential helper reads, or "" when the scratch project has no forge.
func (s ComposeSpec) scratchForgeTokenFile() string {
	if !s.ScratchForge {
		return ""
	}
	return filepath.Join(s.TownRoot, ScratchForgeTokenDir, "forgejo-landing.env")
}

// scratchDoltPort is the port the scratch Dolt server listens on: DoltPort, or
// ScratchDoltPort when the spec leaves it zero.
func (s ComposeSpec) scratchDoltPort() int {
	if s.DoltPort != 0 {
		return s.DoltPort
	}
	return ScratchDoltPort
}

// DaemonComposeHeader is the first line of every file RenderDaemonCompose
// writes, and the mark the command reads to tell its own file from one the
// operator wrote by hand: it refuses to overwrite a file that does not carry it
// unless --force is given.
const DaemonComposeHeader = "# Generated by gt for the container supervisor; every deploy rewrites this file.\n"

// IsDaemonComposeFile reports whether data is a compose file this package
// renders, which the header at its first line marks. Empty data, and a file
// whose first line is anything else, are not the package's to overwrite.
func IsDaemonComposeFile(data []byte) bool {
	return bytes.HasPrefix(data, []byte(DaemonComposeHeader))
}

// daemonComposeBody renders the daemon service and the Docker engine its sweep
// drives. Every spec value takes a %q, quoted so a town root with a space or a
// colon cannot change the YAML's shape, and passes through composeLiteral so a
// dollar sign in one is not interpolated away; the remaining lines are fixed by
// the design and not per town. GT_DAEMON_TAKEOVER is the file's one intentional
// reference to the environment, which is why it alone keeps a single "${...}".
//
// The engine listens without TLS by design, so its only guard is the network:
// declared internal, no published port, and joined by the daemon alone. The
// daemon keeps the project's default network beside it because an internal
// network has no way out, and the daemon reaches the town's Dolt through
// host.docker.internal.
//
// The daemon waits on the engine's health rather than on its start: compose
// starts a dependency's container and moves on, so a plain depends_on can hand
// the daemon an engine whose dockerd is not listening yet, and the first sweep
// then fails on a connection refused. `docker info` answers only once dockerd
// is up, so the health condition holds the daemon until the engine can take a
// request. A scratch daemon waits the same way on the scratch Dolt server, which
// has its own healthcheck, so the daemon never starts before its Dolt answers.
//
// The daemon service runs under docker's init so a finished child is reaped. The
// daemon is PID 1 in its container and reaps nothing, so every dolt and sh it
// spawns and finishes would stand as a defunct process for the daemon's life.
//
// %[12]s is the daemon's command: empty on the live project without an om token
// file, the scratch flags on the scratch project, and the om token flag alone on
// a live project that has one. %[17]s is the token file's read-only mount, which
// only that live project carries, so both are the empty string where the file
// has no such line and the golden is unchanged. %[19]s is the daemon's
// extra_hosts block, which sits directly after init so a reader sees the aliases
// beside the container they belong to; it is the empty string when the spec names
// no alias, so every other render stays byte-identical. %[20]s is the mount at
// the town root: the bind of the Mac's directory, or the town-state volume when
// the spec names one. %[21]s declares that volume external, and is the empty
// string without it, so the bind render keeps every byte it had.
const daemonComposeBody = DaemonComposeHeader + `services:
  daemon:
    image: %[1]q
    container_name: %[2]q
    restart: unless-stopped
    init: true
%[19]s    stop_grace_period: %[3]q
    working_dir: %[4]q
%[12]s    depends_on:
      %[5]s:
        condition: service_healthy
%[15]s    environment:
      DOCKER_HOST: "tcp://%[5]s:2375"
      GT_DAEMON_CONTAINER_NAME: %[2]q
      GT_DAEMON_SUPERVISOR: %[6]q
      GT_DAEMON_TAKEOVER: "${GT_DAEMON_TAKEOVER:-}"
      GT_TOWN_ROOT: %[4]q
%[18]s    volumes:
%[20]s%[17]s    networks:
      - %[11]s
      - %[7]s
  %[5]s:
    image: %[8]q
    restart: unless-stopped
    privileged: true
    command: ["--tls=false"]
    environment:
      DOCKER_TLS_CERTDIR: ""
    healthcheck:
      test: ["CMD", "docker", "info"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 30s
    volumes:
      - type: volume
        source: %[9]s
        target: %[10]s
    networks:
      - %[7]s
%[13]snetworks:
  %[11]s:
  %[7]s:
    internal: true
%[16]svolumes:
  %[9]s:
%[14]s%[21]s`

// RenderDaemonCompose renders the compose file the container supervisor
// starts, stops and inspects: the daemon service and the Docker engine its
// tier sweep drives.
//
// Neither service publishes a port and neither mounts a Docker socket: the
// daemon reaches a Docker engine only through the sweep's own engine on the
// project's internal network (C2). That engine is privileged, so the internal
// network is its only guard and only the daemon joins it. GT_DAEMON_SUPERVISOR
// puts the process in container mode, so it writes no pid file, and
// GT_DAEMON_CONTAINER_NAME is the owner id it records.
// GT_DAEMON_TAKEOVER is passed through empty, so a `--takeover` start is the
// operator's one start and never a standing setting. working_dir and
// GT_TOWN_ROOT both name the mount point, as the launchd plist and the systemd
// unit do, so the process finds the town whichever of the two it reads.
//
// A live spec's OMTokenFile is the host's token file: the render mounts it
// read-only at LiveOMTokenPath and passes that path to the entrypoint, which
// loads it into the daemon's environment. The file's contents never enter this
// file, an argv or an error; only its host path, and the fixed mount target,
// do (gt-yaqct).
//
// A live spec's HMConfigFile is mounted read-only at LiveHMConfigPath, and
// HM_CONFIG names that path, so the container's hm reads the derived config
// rather than the Mac's copy under the config directory mount (gt-yjzepk.13).
//
// When a live spec carries ForgeHost and ForgeTokenFile, the render mounts the
// host's token file read-only at LiveForgeTokenPath and passes both to the
// entrypoint, which sets the container's git credential helper from them; with
// neither, the file carries no mount and no command line, as before (gt-rpx80).
//
// A live spec's TownStateVolume replaces the daemon's bind of the Mac's town
// root with a volume of that name, declared external so compose never creates
// or removes it, and mounted at the town root path. Every other mount stays as
// it is, and a bind whose path the town-state manifest gives a session is
// refused, because that path belongs to a session and the volume does not carry
// it (gt-yjzepk.6).
//
// It refuses a spec whose project or container name is empty rather than
// falling back to the live town's defaults (gt-ix1kz): a scratch render that
// inherited them could stop or replace the live daemon's container.
func RenderDaemonCompose(s ComposeSpec) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}

	names := liveNames
	var command, dolt, doltVolume, doltWait, forgeNetwork, tokenMounts string
	if s.Scratch {
		names = scratchNames
		command = scratchDaemonCommand(s.OMTokenFile, s.scratchDoltPort(), s.scratchForgeTokenFile())
		dolt = fmt.Sprintf(scratchDoltBody,
			ScratchDoltService,
			composeLiteral(s.ScratchDoltImage),
			scratchDoltVolume,
			scratchDoltDataPath,
			names.network,
			s.scratchDoltPort(),
		)
		doltVolume = "  " + scratchDoltVolume + ":\n"
		doltWait = "      " + ScratchDoltService + ":\n        condition: service_healthy\n"
		if s.ScratchForge {
			dolt += fmt.Sprintf(scratchForgeBody,
				s.ScratchForgeHTTPPort,
				s.ScratchForgeSSHPort,
				composeLiteral(ForgePostgresImage),
				composeLiteral(ForgejoImage),
				composeLiteral(DindImage),
				names.egress,
				composeLiteral(ForgejoRunnerImage),
			)
			doltVolume += scratchForgeVolumeDecls
			forgeNetwork = scratchForgeNetworkDecl
		}
	} else {
		command = liveDaemonCommand(s)
		clientMounts, err := liveClientMounts(s)
		if err != nil {
			return nil, err
		}
		tokenMounts = liveTokenMounts(s) + clientMounts
	}

	out := fmt.Sprintf(daemonComposeBody,
		composeLiteral(s.Image),
		composeLiteral(s.ContainerName),
		s.StopGrace.String(),
		composeLiteral(s.TownRoot),
		names.engine,
		config.SupervisorContainer,
		names.network,
		composeLiteral(DindImage),
		names.volume,
		dindDataPath,
		names.egress,
		command,
		dolt,
		doltVolume,
		doltWait,
		forgeNetwork,
		tokenMounts,
		liveClientEnv(s),
		extraHostsBlock(s.ExtraHosts),
		townRootMount(s),
		townStateVolumeDecl(s),
	)
	return []byte(out), nil
}

// townRootMount is the daemon's volume at the town root: the bind of the Mac's
// directory, which is today's render, or the town-state volume when the spec
// names one. The volume's target is the town root, so the container finds the
// town at the same path either way. The name is quoted as every other spec
// value is: a volume name the regexp admits can still be a bare YAML scalar
// that reads back as a number or a bool ("123", "true"), and the file must
// name the volume the host has (gt-i0h1x6).
func townRootMount(s ComposeSpec) string {
	if s.TownStateVolume != "" {
		return fmt.Sprintf("      - type: volume\n        source: %s\n        target: %s\n",
			strconv.Quote(composeLiteral(s.TownStateVolume)), strconv.Quote(composeLiteral(s.TownRoot)))
	}
	q := strconv.Quote(composeLiteral(s.TownRoot))
	return fmt.Sprintf("      - type: bind\n        source: %s\n        target: %s\n", q, q)
}

// townStateVolumeDecl is the town-state volume's entry in the file's top-level
// volumes section, or "" without the flag. `external: true` makes compose use a
// volume it did not create and leave it in place on `down`, which is what keeps
// the daemon's state across a project teardown. The key is quoted for the same
// reason the mount's source is.
func townStateVolumeDecl(s ComposeSpec) string {
	if s.TownStateVolume == "" {
		return ""
	}
	return "  " + strconv.Quote(composeLiteral(s.TownStateVolume)) + ":\n    external: true\n"
}

// extraHostsBlock is the daemon service's extra_hosts block, sorted so a spec
// whose flags arrive in another order renders the same file, or "" when the
// spec names no alias and the file keeps the lines every earlier render had.
func extraHostsBlock(entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	sorted := append([]string(nil), entries...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString("    extra_hosts:\n")
	for _, entry := range sorted {
		fmt.Fprintf(&b, "      - %s\n", strconv.Quote(composeLiteral(entry)))
	}
	return b.String()
}

// composeLiteral makes a spec value a literal in the compose file. `docker
// compose` interpolates $NAME and ${NAME} in every value it reads, before the
// daemon sees the file, so a town root holding a dollar sign would reach the
// container as whatever the environment names instead of itself. A doubled $$
// is compose's escape for one literal $.
func composeLiteral(v string) string {
	return strings.ReplaceAll(v, "$", "$$")
}

// validate reports the first field that would make the rendered file wrong or
// dangerous to apply. An empty name is never filled with a default here; the
// defaults belong to the town config (config.DaemonPatrolConfig).
func (s ComposeSpec) validate() error {
	switch {
	case s.Project == "":
		return fmt.Errorf("compose spec: Project is empty; a render never falls back to the live town's compose project")
	case s.ContainerName == "":
		return fmt.Errorf("compose spec: ContainerName is empty; a render never falls back to the live town's container name")
	case s.Image == "":
		return fmt.Errorf("compose spec: Image is empty; the daemon's image is the local tag gt-daemon:<rev> the deployer built")
	case s.TownRoot == "":
		return fmt.Errorf("compose spec: TownRoot is empty; the bind mount has no directory to mount")
	case !filepath.IsAbs(s.TownRoot):
		return fmt.Errorf("compose spec: TownRoot %q is not absolute; the mount names the same path inside the container, where a relative one resolves against docker's working directory", s.TownRoot)
	case s.StopGrace < MinimumStopGrace:
		return fmt.Errorf("compose spec: StopGrace %s is below MinimumStopGrace %s; a shorter grace period SIGKILLs the daemon mid-shutdown",
			s.StopGrace, MinimumStopGrace)
	case !s.Scratch && (s.ScratchForge || s.ScratchForgeHTTPPort != 0 || s.ScratchForgeSSHPort != 0):
		return fmt.Errorf("compose spec: the scratch forge is set on a live spec; only the scratch render carries a throwaway Forgejo")
	case s.DoltPort != 0 && !s.Scratch:
		return fmt.Errorf("compose spec: DoltPort is set on a live spec; only the scratch render runs a Dolt server of its own")
	case s.DoltPort < 0 || s.DoltPort > 65535:
		return fmt.Errorf("compose spec: DoltPort %d is not a port (1 to 65535)", s.DoltPort)
	case s.OMTokenFile != "" && !filepath.IsAbs(s.OMTokenFile):
		return fmt.Errorf("compose spec: OMTokenFile %q is not absolute; the live render mounts it on the host, and the scratch render names a path inside the container", s.OMTokenFile)
	case strings.ContainsFunc(s.OMTokenFile, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return fmt.Errorf("compose spec: OMTokenFile %q holds a control character, which neither a command line entry nor a mount can carry", s.OMTokenFile)
	case !s.Scratch && s.DoltHost == "":
		return fmt.Errorf("compose spec: DoltHost is empty; pass town.json's dolt.container_host or dolt.host resolved, and an empty dolt.host is the local machine, which inside the container is the container itself")
	}
	if err := s.validateClient(); err != nil {
		return err
	}
	if err := s.validateSecrets(); err != nil {
		return err
	}
	if err := s.validateForge(); err != nil {
		return err
	}
	if err := s.validateOMConfig(); err != nil {
		return err
	}
	if err := s.validateHMConfig(); err != nil {
		return err
	}
	if err := s.validateTownState(); err != nil {
		return err
	}
	if err := s.validateExtraHosts(); err != nil {
		return err
	}
	if s.Scratch {
		return s.validateScratch()
	}
	return checkDoltHost(s.DoltHost)
}

// validateForge checks the live container's forge credential: the pair is given
// together, the token file is an absolute host path a compose value can carry,
// and the host is the bare host:port the entrypoint accepts, since the render
// writes both into its command line. A scratch spec sets neither: the scratch
// render takes its forge credential from ScratchForge and the scratch town's own
// token directory, so a spec that also named one would be dropped in silence.
func (s ComposeSpec) validateForge() error {
	if s.Scratch {
		if s.ForgeHost != "" || s.ForgeTokenFile != "" {
			return fmt.Errorf("compose spec: ForgeHost %q and ForgeTokenFile are set on a scratch spec; the scratch render takes its forge credential from ScratchForge, so it would drop them", s.ForgeHost)
		}
		return nil
	}
	switch {
	case s.ForgeHost == "" && s.ForgeTokenFile == "":
		return nil
	case s.ForgeTokenFile == "":
		return fmt.Errorf("compose spec: ForgeHost %q is set without ForgeTokenFile; the container's git needs the token file beside the host:port, so a live spec carries both or neither", s.ForgeHost)
	case s.ForgeHost == "":
		return fmt.Errorf("compose spec: ForgeTokenFile %q is set without ForgeHost; the container's git needs the host:port beside the token file, so a live spec carries both or neither", s.ForgeTokenFile)
	case !filepath.IsAbs(s.ForgeTokenFile):
		return fmt.Errorf("compose spec: ForgeTokenFile %q is not absolute; it is the host path the render mounts into the container", s.ForgeTokenFile)
	case strings.ContainsFunc(s.ForgeTokenFile, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return fmt.Errorf("compose spec: ForgeTokenFile %q holds a control character, which a compose value cannot carry", s.ForgeTokenFile)
	}
	return checkForgeHost(s.ForgeHost)
}

// checkForgeHost refuses a host the container's entrypoint would refuse: the
// command line carries it, and the entrypoint's own check reads the same
// pattern (config.ForgeHostPattern), so the render cannot write a file that
// starts a container which then refuses.
func checkForgeHost(host string) error {
	m := config.ForgeHostPattern.FindStringSubmatch(host)
	if m == nil {
		return fmt.Errorf("compose spec: ForgeHost %q is not a bare host:port (no scheme, path, login, space or leading dash)", host)
	}
	if port, _ := strconv.Atoi(m[1]); port < 1 || port > 65535 {
		return fmt.Errorf("compose spec: ForgeHost %q has port %q, which is not a port (1 to 65535)", host, m[1])
	}
	return nil
}

// extraHostGateway is docker's own address for the host the container runs on,
// and the one non-IP value an extra host may name.
const extraHostGateway = "host-gateway"

// extraHostName is the hostname half of an extra host: the alias docker writes
// into the container's /etc/hosts. The pattern keeps the entry to characters a
// hostname can have, so the value cannot change the YAML's shape.
var extraHostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateExtraHosts refuses an alias the render cannot write or docker cannot
// use, naming the entry so the flag that carried it is found. Only the live
// render carries aliases; a scratch spec that set one would drop it in silence.
//
// An entry is "<hostname>:<address>", and the render quotes it into one YAML
// list item. The address must be a four-part IPv4 address or host-gateway: a
// bare IPv6 address carries colons of its own, so this one-colon form cannot
// hold it and the entry is refused rather than left for docker to parse another
// way. Each hostname is named once, because a repeat renders two /etc/hosts
// lines and leaves the name's address to their order.
func (s ComposeSpec) validateExtraHosts() error {
	if s.Scratch && len(s.ExtraHosts) > 0 {
		return fmt.Errorf("compose spec: ExtraHosts %q is set on a scratch spec; only the live render carries host aliases, so it would drop them", s.ExtraHosts)
	}
	// /etc/hosts matching ignores case, so "Forgejo" and "forgejo" are one alias.
	seen := make(map[string]string, len(s.ExtraHosts))
	for _, entry := range s.ExtraHosts {
		name, address, ok := strings.Cut(entry, ":")
		switch {
		case !ok:
			return fmt.Errorf("compose spec: ExtraHost %q has no colon; an entry is <hostname>:<ipv4 or %s>", entry, extraHostGateway)
		case name == "":
			return fmt.Errorf("compose spec: ExtraHost %q has an empty hostname, so there is no alias to write", entry)
		case address == "":
			return fmt.Errorf("compose spec: ExtraHost %q has an empty address; name the IPv4 address or %s", entry, extraHostGateway)
		case !extraHostName.MatchString(name):
			return fmt.Errorf("compose spec: ExtraHost %q has hostname %q, which is not a hostname (letters, digits, dot, hyphen, underscore, starting with a letter or digit)", entry, name)
		case !extraHostAddress(address):
			return fmt.Errorf("compose spec: ExtraHost %q has address %q, which is neither an IPv4 address nor %s; a bare IPv6 address carries colons of its own and cannot be written in this <hostname>:<address> form", entry, address, extraHostGateway)
		}
		key := strings.ToLower(name)
		if first, repeated := seen[key]; repeated {
			return fmt.Errorf("compose spec: ExtraHost %q repeats hostname %q from %q; the render writes one line per name, so the second line would leave its address to /etc/hosts order", entry, name, first)
		}
		seen[key] = entry
	}
	return nil
}

// extraHostAddress reports whether address is one docker accepts in extra_hosts.
// net.ParseIP reads an IPv4-mapped IPv6 address such as ::ffff:10.0.0.2 as
// IPv4, but its colons would make this entry mean something other than the
// <hostname>:<address> form the render writes, so only the dotted form passes.
func extraHostAddress(address string) bool {
	if address == extraHostGateway {
		return true
	}
	if strings.Contains(address, ":") {
		return false
	}
	ip := net.ParseIP(address)
	return ip != nil && ip.To4() != nil
}

// validateScratch refuses a scratch spec that could reach the live town: the
// live town's root, container name or compose project. The container name and
// project are the config defaults a live daemon runs under. The live root is the
// caller's, passed as LiveTownRoot, because no constant names it, so an empty
// one is refused rather than compared as "no root". The scratch spec carries no
// DoltHost: its daemon's Dolt host comes from the scratch town's own settings.
func (s ComposeSpec) validateScratch() error {
	switch {
	case s.LiveTownRoot == "":
		return fmt.Errorf("compose spec: LiveTownRoot is empty; a scratch spec is checked against the live town root, so the live root must be named")
	case !filepath.IsAbs(s.LiveTownRoot):
		return fmt.Errorf("compose spec: LiveTownRoot %q is not absolute; the root check compares absolute paths", s.LiveTownRoot)
	case s.ContainerName == config.DefaultContainerName:
		return fmt.Errorf("compose spec: scratch ContainerName %q is the live daemon's container name", s.ContainerName)
	case s.Project == config.DefaultComposeProject:
		return fmt.Errorf("compose spec: scratch Project %q is the live compose project", s.Project)
	case s.ScratchDoltImage == "":
		return fmt.Errorf("compose spec: ScratchDoltImage is empty; the scratch project runs its own Dolt server, at DoltServerImage")
	}
	if err := s.checkScratchForge(); err != nil {
		return err
	}
	return s.checkScratchRoot()
}

// checkScratchForge refuses a forge spec whose ports would reach, or collide
// with, the live Forgejo: the live ports themselves, a port that is not one, or
// one port named for both services. Ports set without ScratchForge are refused
// too, because a render that ignored them would let a caller believe a forge was
// running on them.
func (s ComposeSpec) checkScratchForge() error {
	if !s.ScratchForge {
		if s.ScratchForgeHTTPPort != 0 || s.ScratchForgeSSHPort != 0 {
			return fmt.Errorf("compose spec: scratch forge ports are set but ScratchForge is not; the render would ignore them")
		}
		return nil
	}
	for _, p := range []struct {
		name string
		port int
	}{{"ScratchForgeHTTPPort", s.ScratchForgeHTTPPort}, {"ScratchForgeSSHPort", s.ScratchForgeSSHPort}} {
		switch {
		case p.port < 1 || p.port > 65535:
			return fmt.Errorf("compose spec: %s %d is not a port; a scratch forge names both its host ports", p.name, p.port)
		case p.port == liveForgeHTTPPort || p.port == liveForgeSSHPort:
			return fmt.Errorf("compose spec: %s %d is a host port the live Forgejo publishes (%d and %d); the scratch forge takes its own", p.name, p.port, liveForgeHTTPPort, liveForgeSSHPort)
		}
	}
	if s.ScratchForgeHTTPPort == s.ScratchForgeSSHPort {
		return fmt.Errorf("compose spec: ScratchForgeHTTPPort and ScratchForgeSSHPort are both %d; the two services need different host ports", s.ScratchForgeHTTPPort)
	}
	return nil
}

// checkScratchRoot refuses a scratch town root that resolves to the live root, to
// a path under it, or to an ancestor of it: mounting an ancestor exposes the live
// town just as mounting it does. The check runs on resolved paths, because a
// symlink to the live root, or a directory under one that does not exist yet,
// names the live town under another path. A path that cannot be resolved is
// refused rather than taken as clear.
func (s ComposeSpec) checkScratchRoot() error {
	// Both roots must be absolute here as well as in validate: a relative path
	// cannot be compared with an absolute one, and this check never takes "cannot
	// compare" for "clear".
	if !filepath.IsAbs(s.TownRoot) || !filepath.IsAbs(s.LiveTownRoot) {
		return fmt.Errorf("compose spec: scratch TownRoot %q and LiveTownRoot %q must both be absolute for the root check", s.TownRoot, s.LiveTownRoot)
	}
	scratch, err := resolvePath(s.TownRoot)
	if err != nil {
		return fmt.Errorf("compose spec: resolving scratch TownRoot %q: %w", s.TownRoot, err)
	}
	live, err := resolvePath(s.LiveTownRoot)
	if err != nil {
		return fmt.Errorf("compose spec: resolving LiveTownRoot %q: %w", s.LiveTownRoot, err)
	}
	if within(scratch, live) {
		return fmt.Errorf("compose spec: scratch TownRoot %q is the live town root %q or lies under it; the scratch project mounts its own directory, never the live one", s.TownRoot, s.LiveTownRoot)
	}
	if within(live, scratch) {
		return fmt.Errorf("compose spec: scratch TownRoot %q contains the live town root %q; mounting it would expose the live town, so the scratch project mounts its own directory", s.TownRoot, s.LiveTownRoot)
	}
	return nil
}

// within reports whether path is root or lies under it. Letter case is folded,
// as the default macOS volumes do, so the same directory spelled in another case
// is still the same directory; on a case-sensitive filesystem that folding can
// refuse two distinct directories whose names differ only by case, which is the
// safe side of the error for a check that guards the live town. A pair that
// cannot be compared (filepath.Rel errors when one path is absolute and the
// other is not) counts as inside, so the caller refuses: the answer "cannot
// tell" is never "clear".
func within(path, root string) bool {
	rel, err := filepath.Rel(strings.ToLower(root), strings.ToLower(path))
	if err != nil {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvePath returns path with its symlinks resolved. A path that does not
// exist yet resolves through its nearest existing ancestor and keeps the rest
// of its name, so a directory a scratch install has not created yet, under a
// link to the live root, still resolves to the live root. A dangling link is an
// entry that exists but whose target does not, so it is an error, never a name
// to keep lexically: its target could be the live root. Any other error stops
// the resolution.
func resolvePath(path string) (string, error) {
	clean := filepath.Clean(path)
	dir, rest := clean, ""
	for {
		_, err := os.Lstat(dir)
		parent := filepath.Dir(dir)
		if errors.Is(err, fs.ErrNotExist) && parent != dir {
			rest = filepath.Join(filepath.Base(dir), rest)
			dir = parent
			continue
		}
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return "", err
		}
		return filepath.Join(resolved, rest), nil
	}
}

// checkDoltHost refuses a loopback endpoint: the container's own loopback is
// the container, so the daemon would fail to reach the town's Dolt while the
// file looked right. An empty host is refused in validate, before this, for the
// same reason.
func checkDoltHost(host string) error {
	loopback := strings.EqualFold(host, "localhost")
	if ip := net.ParseIP(host); ip != nil {
		loopback = loopback || ip.IsLoopback()
	}
	if loopback {
		return fmt.Errorf("compose spec: DoltHost %q is a loopback address; inside the container that is the container itself, so set dolt.container_host to the Mac's name in the Docker VM (host.docker.internal)", host)
	}
	return nil
}
