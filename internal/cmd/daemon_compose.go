package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/workspace"
)

// daemonComposeFlags are the values `gt daemon compose write` parsed.
type daemonComposeFlags struct {
	image string // gt-daemon:<rev>, the image the rendered file names
	print bool   // print the render and write no file
	// omTokenFile is the om subscription token file's path on the host, which
	// the render mounts read-only into the container; "" renders neither the
	// mount nor the flag. The command never reads the file, so its contents are
	// in no output and no error (gt-yaqct).
	omTokenFile string
	// omTokenFileSet is whether --om-token-file was given at all. A blank value
	// on a flag the operator named is refused rather than read as unset: it
	// names no file, so the render would silently carry no token mount
	// (gt-0pb64).
	omTokenFileSet bool
	// forgeHost and forgeTokenFile are --forge-host and --forge-token-file: the
	// Forgejo's host:port and the host path of the landing bot's token file,
	// which the render mounts read-only into the container and passes to the
	// entrypoint. Both or neither; the render refuses one alone (gt-rpx80).
	forgeHost      string
	forgeTokenFile string
	// kubeconfig is the host kubeconfig the container's kubectl and openshell
	// use, under the town; the render derives a copy that reaches the cluster on
	// host.docker.internal and lists it first in KUBECONFIG (gt-yj0vh).
	kubeconfig string
	// openShellConfigDir is the host's openshell config directory (the gateway's
	// mTLS client certificates), mounted read-only.
	openShellConfigDir string
	// extraHosts are --extra-host values, one "<hostname>:<ipv4 or host-gateway>"
	// each, in the order given. The render sorts them into the daemon service's
	// extra_hosts and refuses a malformed entry by name (gt-gg9hv.2).
	extraHosts []string
	// configDir is --config-dir: the town's config directory on the host, an
	// absolute path. The render mounts it read-only at that path and at the
	// container's XDG path, or with --secrets-manifest only the files the
	// manifest lists (gt-gg9hv.3, gt-gg9hv.66).
	configDir string
	// secretsManifest is --secrets-manifest: the path of the enumerated list of
	// files the daemon reads from the config directory. With it the render
	// mounts those files read-only and no directory of the tree; without it the
	// render is unchanged (gt-gg9hv.66).
	secretsManifest string
	// secretsManifestSet is whether --secrets-manifest was given at all, so a
	// blank value is refused rather than read as unset.
	secretsManifestSet bool
	// backupDir is --backup-dir: the host's Dolt backup directory, an absolute
	// path. The render mounts it read-write at that path and sets
	// GT_DOLT_BACKUP_ROOT to it; a relative path is refused by the render
	// (gt-gg9hv.17).
	backupDir string
	// bindRO are --bind-ro values: host directories the render mounts read-only at
	// the same path, in the order given. Each is checked to be a directory here;
	// the render checks the rest (gt-vqexg).
	bindRO []string
}

// daemonComposeDeps is what the command takes from the host: where the town is
// found, its daemon settings, the Dolt endpoint its container reaches, the
// filesystem seam the render is written through, and where output goes. A test
// supplies fakes, so no unit test reads a live town or writes its file.
type daemonComposeDeps struct {
	findTown     func() (string, error)
	daemonConfig func(townRoot string) (*config.DaemonPatrolConfig, error)
	// doltHost is the host the daemon in the container reaches the town's Dolt
	// through, and whether the town names an endpoint at all.
	doltHost func(townRoot string) (string, bool)
	// containerEnv is the environment the container carries from the town's
	// daemon.env; readFile reads the kubeconfig the derived one comes from.
	containerEnv func(townRoot string) (map[string]string, error)
	readFile     func(path string) ([]byte, error)
	// evalSymlinks resolves the kubeconfig and the town for the under-the-town
	// check.
	evalSymlinks func(path string) (string, error)
	// isDir reports whether path is a directory; an error is a path it cannot
	// stat. A --bind-ro path must be one (gt-vqexg).
	isDir func(path string) (bool, error)
	// townZone is the IANA zone name for the container's TZ ("" for none).
	townZone func(townRoot string) (string, error)
	// omConfig derives the om config the container reads from the default
	// settings, which the render takes; a test stands in a failure for it.
	omConfig func() ([]byte, error)
	// secretsFS is the seam the secrets render reads the config directory
	// through. Nil is the host's filesystem; a test supplies a fake that records
	// every call and refuses a content read (gt-gg9hv.66).
	secretsFS templates.SecretFS
	// hmConfig reads the Mac's hm config the container's copy is derived from.
	// configDir is --config-dir; empty means the config directory's default.
	hmConfig func(configDir string) ([]byte, error)
	files    composeFiles
	out      io.Writer
	// errOut is where a warning goes, apart from the compose file on out: the
	// rendered file must stay the whole of what --print prints.
	errOut io.Writer
}

// composeFiles is the filesystem the compose file is written through: a
// directory is made, then the file is written into it. The host's is the real
// one; a test stands in its own, so no unit test touches the real filesystem.
type composeFiles struct {
	mkdirAll func(dir string) error
	write    func(path string, content []byte) error
	// writePrivate writes a file its owner alone reads, which the derived hm
	// config needs: its database URL holds the password.
	writePrivate func(path string, content []byte) error
}

// doltEndpointResolver resolves a town's Dolt endpoint for a process in or out
// of container mode. config.ResolveDoltEndpointFor is the host's.
type doltEndpointResolver func(townRoot string, inContainer bool) (config.DoltEndpoint, bool)

// containerDoltHost is the host the daemon in the container reaches Dolt
// through. The process asking is not the container, but the daemon this file
// starts is: the endpoint is resolved in container mode whichever process asks
// (gt-qpeei.17).
func containerDoltHost(resolve doltEndpointResolver) func(townRoot string) (string, bool) {
	return func(townRoot string) (string, bool) {
		ep, ok := resolve(townRoot, true)
		return ep.Host, ok
	}
}

// realDaemonComposeDeps is the host's.
func realDaemonComposeDeps(out io.Writer) daemonComposeDeps {
	deps := daemonComposeDeps{
		findTown: workspace.FindFromCwdOrError,
		// The settings are read through the control the supervisor and the
		// cutover refuse from, so this command refuses the same towns they do.
		daemonConfig: realDaemonControl().townDaemonConfig,
		doltHost:     containerDoltHost(config.ResolveDoltEndpointFor),
		containerEnv: func(townRoot string) (map[string]string, error) {
			return containerEnvFor(townRoot, config.LoadDaemonEnv, settingsFS{readDir: os.ReadDir, read: os.ReadFile, eval: filepath.EvalSymlinks})
		},
		readFile:     os.ReadFile,
		evalSymlinks: filepath.EvalSymlinks,
		isDir: func(path string) (bool, error) {
			info, err := os.Stat(path)
			if err != nil {
				return false, err
			}
			return info.IsDir(), nil
		},
		townZone: func(townRoot string) (string, error) {
			return containerZone(townRoot, config.LoadTownSettings, os.Readlink, "/etc/localtime")
		},
		omConfig: func() ([]byte, error) {
			return templates.RenderOMConfig(templates.DefaultOMConfigSettings())
		},
		files: composeFiles{
			mkdirAll:     func(dir string) error { return os.MkdirAll(dir, 0o755) },
			write:        daemonimage.WriteComposeFile,
			writePrivate: templates.WriteHMConfigFile,
		},
		out:    out,
		errOut: os.Stderr,
	}
	// The Mac's hm config holds the database password, so it is read through the
	// same seam the command reads every host file through (gt-gg9hv.66).
	deps.hmConfig = func(configDir string) ([]byte, error) {
		path, err := hostHMConfigPath(configDir, os.Getenv, os.UserHomeDir)
		if err != nil {
			return nil, err
		}
		return deps.readFile(path)
	}
	return deps
}

// hostHMConfigPath is where the Mac's hm config lives: <config-dir>/hm-config.json
// when the operator names the config directory, else $XDG_CONFIG_HOME/gt's and
// else ~/.config/gt's, the directory the render mounts at LiveConfigDirPath.
func hostHMConfigPath(configDir string, getenv func(string) string, homeDir func() (string, error)) (string, error) {
	var path string
	switch {
	case configDir != "":
		path = filepath.Join(configDir, "hm-config.json")
	case getenv("XDG_CONFIG_HOME") != "":
		path = filepath.Join(getenv("XDG_CONFIG_HOME"), "gt", "hm-config.json")
	default:
		home, err := homeDir()
		if err != nil {
			return "", fmt.Errorf("the hm config directory cannot be found without HOME: %w", err)
		}
		path = filepath.Join(home, ".config", "gt", "hm-config.json")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("the Mac's hm config path %q is not absolute, so it would be read from the working directory", path)
	}
	return path, nil
}

// containerOMConfigRP is where the derived om config is written: under the town's
// .runtime beside the derived kubeconfig, which the town bind carries into the
// container for the read-only mount to name (gt-gg9hv.13).
const containerOMConfigRP = ".runtime/container/om-config.json"

// containerOMConfigPath is the host path of the derived om config.
func containerOMConfigPath(townRoot string) string {
	return filepath.Join(townRoot, filepath.FromSlash(containerOMConfigRP))
}

// containerHMConfigRP is where the derived hm config is written, beside the om
// config the same write derives. The town bind carries it into the container,
// and the render mounts it at the path HM_CONFIG names (gt-yjzepk.13).
const containerHMConfigRP = ".runtime/container/hm-config.json"

// containerHMConfigPath is the host path of the derived hm config.
func containerHMConfigPath(townRoot string) string {
	return filepath.Join(townRoot, filepath.FromSlash(containerHMConfigRP))
}

// writeDaemonComposeFile writes content to path, making the compose file's
// directory first: it lives under the town's daemon/ directory, which a town
// that has never rendered it does not have.
func writeDaemonComposeFile(files composeFiles, path string, content []byte) error {
	if err := files.mkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	return files.write(path, content)
}

// writeDaemonPrivateFile writes content to path through the owner-only seam,
// making its directory first: the one file it writes is the hm config, whose
// database URL carries the password.
func writeDaemonPrivateFile(files composeFiles, path string, content []byte) error {
	if err := files.mkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	return files.writePrivate(path, content)
}

// newDaemonComposeCmd builds `gt daemon compose` over run, which receives the
// flags `write` parsed.
func newDaemonComposeCmd(run func(out io.Writer, f daemonComposeFlags) error) *cobra.Command {
	var f daemonComposeFlags
	parent := &cobra.Command{
		Use:   "compose",
		Short: "Render the town's live daemon compose file",
		Long: `Render the compose file the town's daemon container runs from, at
<town>/daemon/container/compose.yml.

gt daemon cutover --to container starts the container from this file, so it must
name the town's own container, compose project and image. The file is rendered
from the town's settings, never by hand.`,
		RunE: requireSubcommand,
	}
	write := &cobra.Command{
		Use:   "write --image gt-daemon:<rev>",
		Short: "Write the live compose file and start nothing",
		Long: `Write the live compose file from the town in the working directory and start
nothing. The town must set both daemon.container_name and daemon.compose_project:
a field left unset would render the live defaults into a file this town's
cutover then starts.

Dolt is resolved as the container reaches it: dolt.container_host when the town
sets it, dolt.host when it does not, and a loopback or empty host is refused,
because inside the container that host is the container itself.

--om-token-file names the om subscription token file on the host, an absolute
path. The render mounts it read-only into the container and passes its path to
the entrypoint, which loads the token into the daemon's environment; the file is
never read here, so its contents appear in no output, and without the flag the
render carries neither the mount nor the flag. A flag given with a blank value
is refused, since it names no file and would otherwise render as if unset.

The same flag also writes the om config derived for the container, at
<town>/.runtime/container/om-config.json. The render mounts it read-only at the
XDG path om reads, so the reviewer runs its backend through the container's
claude-exec with the token mount, and never a bare claude with no credential.
The config names the token's container path, not the host's, and carries no
credential.

Every write derives the container's hm config from the Mac's, which must exist:
hm has no environment override for its database URL, so a copy still naming
localhost would reach the container itself. The Mac's file is
<config-dir>/hm-config.json when --config-dir names the directory, else
$XDG_CONFIG_HOME/gt's, else ~/.config/gt's. The derived file is
<town>/.runtime/container/hm-config.json, mode 0600; the render mounts it
read-only where the container's HM_CONFIG points and sets that variable. A file
that cannot be read or parsed refuses the write, and its contents never reach
this command's output or an error; --print derives it and writes nothing.

--forge-host and --forge-token-file give the container's git the Forgejo
credential helper, so the landing worker's pushes carry the landing bot's token.
The render mounts the token file read-only and passes the pair to the
entrypoint, which writes the helper line; the file's path, never its contents,
reaches the compose file. They are accepted only together.

--kubeconfig and --openshell-config-dir give the container's kubectl and openshell
the cluster and the gateway. The render writes a derived kubeconfig inside the
town's .runtime (the original's cluster entries on host.docker.internal with
tls-server-name localhost, no user, no credential), lists it before the original
in KUBECONFIG, and mounts the openshell directory read-only; only paths appear in
the compose file. The daemon.env entries that are not host-only (containerEnvDeniedNames
in internal/cmd/daemon_compose_client.go is the list), not secret-named and not
referenced as $NAME or ${NAME} by any file under the town's settings directory (links
that stay under the town are followed; one that leaves it refuses the render; other
bound directories are not scanned) go into the container's environment.

--extra-host's aliases reach the daemon service's extra_hosts list sorted,
directly after init. Without the flag the render is unchanged.

--config-dir names the town's config directory on the host, an absolute path
(usually ~/.config/gt). The render mounts it read-only at that same path and at
the container's XDG_CONFIG_HOME path, because the town's settings name the
directory both ways; without the flag the render is unchanged.

--secrets-manifest names deploy/daemon/secrets.yaml, the enumerated list of files
in the config directory that the daemon reads. Its header defines the fields and
what rotating a listed file costs. With it the render mounts each listed file
read-only at its own path and at the container's XDG path, and mounts no
directory of the config tree, so the town's other config files stay on the host.
The flag requires --config-dir, since its entries are files in that directory,
and a blank value is refused. The command reads the manifest and opens none of
the files it lists.

--backup-dir names the host's Dolt backup directory, an absolute path. The Dolt
server container writes each night's backup there at the same path, so the render
mounts it read-write at that same path on the daemon and sets GT_DOLT_BACKUP_ROOT
to it; a relative path is refused. Without the flag the render is unchanged.

--bind-ro names a host directory, an absolute path, that the daemon mounts
read-only at that same path; repeat it for more. A path that is not a directory
is refused, and so is one the render cannot mount read-only (relative, not
cleaned, under .., inside or containing the town, already mounted, or given
twice). The mounts are sorted by path. Without the flag the render is unchanged.

--print prints the render and writes no file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.omTokenFileSet = cmd.Flags().Changed(config.OMTokenFileFlag)
			f.secretsManifestSet = cmd.Flags().Changed("secrets-manifest")
			return run(cmd.OutOrStdout(), f)
		},
	}
	write.Flags().StringVar(&f.image, "image", "", "daemon image tag, gt-daemon:<rev>")
	write.Flags().StringVar(&f.omTokenFile, config.OMTokenFileFlag, "",
		"om token file's absolute path on the host, mounted read-only into the container; unset renders neither the mount nor the flag, and a blank value is refused")
	write.Flags().StringVar(&f.kubeconfig, "kubeconfig", "",
		"the host kubeconfig (an absolute path under the town, checked on resolved paths); the render adds a derived copy on host.docker.internal with tls-server-name localhost and sets KUBECONFIG; unset renders neither")
	write.Flags().StringVar(&f.openShellConfigDir, "openshell-config-dir", "",
		"the host's openshell config directory (gateway metadata and mTLS client certificates), an absolute path mounted read-only; unset renders neither the mount nor XDG_CONFIG_HOME")
	write.Flags().BoolVar(&f.print, "print", false, "print the render and write no file")
	write.Flags().StringVar(&f.forgeHost, config.ForgeHostFlag, "",
		"Give the container's git this Forgejo host:port; accepted only with --"+config.ForgeTokenFileFlag)
	write.Flags().StringVar(&f.forgeTokenFile, config.ForgeTokenFileFlag, "",
		"The host path of the landing bot's token file, mounted read-only into the container; accepted only with --"+config.ForgeHostFlag)
	write.Flags().StringArrayVar(&f.extraHosts, "extra-host", nil,
		"A host alias the container resolves, as <hostname>:<ipv4 or host-gateway>; repeat for more, and a malformed entry is refused by name")
	write.Flags().StringVar(&f.configDir, "config-dir", "",
		"the town's config directory on the host (an absolute path), mounted read-only at that path and at the container's XDG path, or with --secrets-manifest only the files the manifest lists")
	write.Flags().StringVar(&f.secretsManifest, "secrets-manifest", "",
		"the secrets manifest listing the config-directory files the daemon reads; with it the render mounts those files and no directory of the tree, and requires --config-dir")
	write.Flags().StringVar(&f.backupDir, "backup-dir", "",
		"the host's Dolt backup directory (an absolute path), mounted read-write at that path and named by GT_DOLT_BACKUP_ROOT; unset renders neither")
	write.Flags().StringArrayVar(&f.bindRO, "bind-ro", nil,
		"a host directory (an absolute path) mounted read-only at that same path in the daemon; repeat for more, and a path that is not a directory is refused")
	parent.AddCommand(write)
	return parent
}

var daemonComposeCmd = newDaemonComposeCmd(func(out io.Writer, f daemonComposeFlags) error {
	return runDaemonCompose(f, realDaemonComposeDeps(out))
})

func init() {
	daemonCmd.AddCommand(daemonComposeCmd)
}

// runDaemonCompose renders the town's live compose file and, unless --print,
// writes it. Every refusal comes before the file is written, so a town the
// render or the settings refuse keeps the file it had.
func runDaemonCompose(f daemonComposeFlags, d daemonComposeDeps) error {
	image := strings.TrimSpace(f.image)
	if _, err := daemonimage.ImageTagRev(image); err != nil {
		return err
	}
	// The token file is passed to the render as the host's path, and the render
	// refuses a relative one. The command never opens the file, so no contents
	// reach its output or an error.
	omTokenFile := strings.TrimSpace(f.omTokenFile)
	if f.omTokenFileSet && omTokenFile == "" {
		return fmt.Errorf("--%s was given blank; name the om token file's absolute path on the host, or leave the flag out to render no token mount", config.OMTokenFileFlag)
	}
	// The forge pair is passed the same way: the render refuses a host that is
	// not a bare host:port and a token file path it cannot mount, and the
	// command opens neither.
	forgeHost := strings.TrimSpace(f.forgeHost)
	forgeTokenFile := strings.TrimSpace(f.forgeTokenFile)
	configDir := strings.TrimSpace(f.configDir)
	// The manifest names files in the config directory, so the two flags are
	// given together. The manifest file itself is read; none of the files it
	// lists is, so no contents reach the render or an error.
	secretsManifest := strings.TrimSpace(f.secretsManifest)
	if f.secretsManifestSet && secretsManifest == "" {
		return fmt.Errorf("--secrets-manifest was given blank; name the manifest file, or leave the flag out to mount the config directory")
	}
	var secretFiles []templates.SecretFile
	if secretsManifest != "" {
		if configDir == "" {
			return fmt.Errorf("--secrets-manifest lists files in the config directory, so --config-dir must name it; no compose file was written")
		}
		data, err := d.readFile(secretsManifest)
		if err != nil {
			return fmt.Errorf("reading the secrets manifest %s, and no compose file was written: %w", secretsManifest, err)
		}
		manifest, err := templates.ParseSecretsManifest(data)
		if err != nil {
			return fmt.Errorf("the secrets manifest %s is refused: %w", secretsManifest, err)
		}
		secretFiles = manifest.Files
	}
	// Each --bind-ro must name a directory the command can stat. A blank value
	// names nothing, so it is refused rather than rendered as unset. The render
	// refuses the rest of what a read-only mount cannot carry (gt-vqexg).
	bindRO := make([]string, 0, len(f.bindRO))
	for _, p := range f.bindRO {
		p = strings.TrimSpace(p)
		if p == "" {
			return fmt.Errorf("--bind-ro was given blank; name the host directory's absolute path, or leave the flag out")
		}
		dir, err := d.isDir(p)
		if err != nil {
			return fmt.Errorf("--bind-ro %s cannot be read, and no compose file was written: %w", p, err)
		}
		if !dir {
			return fmt.Errorf("--bind-ro %s is not a directory, and no compose file was written", p)
		}
		bindRO = append(bindRO, p)
	}
	townRoot, err := d.findTown()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	cfg, err := d.daemonConfig(townRoot)
	if err != nil {
		return fmt.Errorf("reading the town's daemon settings: %w", err)
	}
	project, err := containerComposeProject(cfg)
	if err != nil {
		return err
	}
	host, ok := d.doltHost(townRoot)
	if !ok {
		return fmt.Errorf("the town's Dolt endpoint cannot be resolved, so the daemon in the container has no host to reach it on: set mayor/town.json's dolt.container_host (or dolt.host)")
	}
	env, err := d.containerEnv(townRoot)
	if err != nil {
		return fmt.Errorf("reading the town's daemon.env for the container's environment: %w", err)
	}
	zone, err := d.townZone(townRoot)
	if err != nil {
		return err
	}
	// The om config is derived from the default settings and names the token
	// mount, never the token: it is written only when the token file is named,
	// so a render without the flag writes no om config (gt-gg9hv.13).
	var omConfig []byte
	var omConfigFile string
	if omTokenFile != "" {
		omConfigFile = containerOMConfigPath(townRoot)
		if omConfig, err = d.omConfig(); err != nil {
			return fmt.Errorf("deriving the om config, and no compose file was written: %w", err)
		}
	}
	// The hm config is derived from the Mac's, whichever directory names it: an
	// unreadable or unparsable one refuses, since a render that left hm the Mac's
	// file would send it to the container's own loopback (gt-yjzepk.13).
	hostHMConfig, err := d.hmConfig(configDir)
	if err != nil {
		return fmt.Errorf("reading the Mac's hm config, and no compose file was written: %w", err)
	}
	hmConfig, err := templates.RenderHMConfig(hostHMConfig)
	if err != nil {
		return fmt.Errorf("deriving the hm config, and no compose file was written: %w", err)
	}
	hmConfigFile := containerHMConfigPath(townRoot)
	kubeconfig := strings.TrimSpace(f.kubeconfig)
	var overlay []byte
	var kubeconfigFiles []string
	if kubeconfig != "" {
		if err := requireUnderTown(townRoot, kubeconfig, d.evalSymlinks); err != nil {
			return err
		}
		original, err := d.readFile(kubeconfig)
		if err != nil {
			return fmt.Errorf("reading the kubeconfig %s: %w", kubeconfig, err)
		}
		if overlay, err = containerKubeconfig(original, filepath.Dir(kubeconfig), townRoot); err != nil {
			return fmt.Errorf("deriving the container kubeconfig from %s: %w", kubeconfig, err)
		}
		kubeconfigFiles = []string{containerKubeconfigPath(townRoot), kubeconfig}
	}
	spec := templates.ComposeSpec{
		Env:                env,
		TimeZone:           zone,
		OMTokenMetaFile:    cfg.OMTokenMeta(),
		LandingWorkRoot:    landingWorkRoot(cfg),
		KubeconfigFiles:    kubeconfigFiles,
		OpenShellConfigDir: strings.TrimSpace(f.openShellConfigDir),
		ConfigDir:          configDir,
		Secrets:            secretFiles,
		SecretsFS:          d.secretsFS,
		BackupDir:          strings.TrimSpace(f.backupDir),
		BindRO:             bindRO,
		Project:            project,
		ContainerName:      cfg.ContainerName(),
		Image:              image,
		TownRoot:           townRoot,
		StopGrace:          templates.MinimumStopGrace,
		DoltHost:           host,
		OMTokenFile:        omTokenFile,
		OMConfigFile:       omConfigFile,
		HMConfigFile:       hmConfigFile,
		ForgeHost:          forgeHost,
		ForgeTokenFile:     forgeTokenFile,
		ExtraHosts:         f.extraHosts,
	}
	content, err := templates.RenderDaemonCompose(spec)
	if err != nil {
		return fmt.Errorf("cannot render the live compose file, and no file was written: %w", err)
	}
	if len(secretFiles) > 0 {
		// Every manifest render says what rotating a listed secret costs (the
		// manifest's header says the same). The warning stream, not out: with
		// --print, out must stay the compose file alone.
		warn := d.errOut
		if warn == nil {
			warn = io.Discard
		}
		fmt.Fprintln(warn, templates.SecretsRecreateNotice)
	}
	if f.print {
		_, err = d.out.Write(content)
		return err
	}
	if overlay != nil {
		if err := writeDaemonComposeFile(d.files, containerKubeconfigPath(townRoot), overlay); err != nil {
			return fmt.Errorf("cannot write the container kubeconfig, and no compose file was written: %w", err)
		}
	}
	if omConfigFile != "" {
		if err := writeDaemonComposeFile(d.files, omConfigFile, omConfig); err != nil {
			return fmt.Errorf("cannot write the om config, and no compose file was written: %w", err)
		}
	}
	if err := writeDaemonPrivateFile(d.files, hmConfigFile, hmConfig); err != nil {
		return fmt.Errorf("cannot write the hm config %s, and no compose file was written: %w", hmConfigFile, err)
	}
	path := config.ContainerComposeFile(townRoot)
	if err := writeDaemonComposeFile(d.files, path, content); err != nil {
		return fmt.Errorf("cannot write %s, and the old file is as it was: %w", path, err)
	}
	fmt.Fprintf(d.out, "%s Wrote %s: container %s, compose project %s, image %s.\n",
		style.Bold.Render("✓"), path, spec.ContainerName, project, spec.Image)
	return nil
}

// requireUnderTown refuses a kubeconfig that is not inside the town on resolved
// paths: the container sees only the town bind, so a file elsewhere would be
// listed in KUBECONFIG and not be there.
func requireUnderTown(townRoot, path string, eval func(string) (string, error)) error {
	town, err := eval(townRoot)
	if err != nil {
		return fmt.Errorf("the town root %s cannot be resolved: %w", townRoot, err)
	}
	real, err := eval(path)
	if err != nil {
		return fmt.Errorf("--kubeconfig %s cannot be resolved: %w", path, err)
	}
	if !filepath.IsAbs(path) || !pathWithin(real, town) {
		return fmt.Errorf("--kubeconfig %s resolves to %s, which is not an absolute path under the town %s; the container reads only the town bind", path, real, town)
	}
	return nil
}

// landingWorkRoot is patrols.landing_worker.work_root, "" when the town sets
// none. The render binds it at the same path, so a relative one is refused
// there with its reason.
func landingWorkRoot(cfg *config.DaemonPatrolConfig) string {
	if cfg.Patrols == nil || cfg.Patrols.LandingWorker == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Patrols.LandingWorker.WorkRoot)
}
