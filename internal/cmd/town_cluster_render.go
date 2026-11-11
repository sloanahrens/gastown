package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/manifestdata"
	"github.com/steveyegge/gastown/internal/workspace"
)

// secretsManifestRelPath is the secrets manifest under the repo directory: the
// same file the compose render reads, so the pod mounts the list the container
// mounts.
var secretsManifestRelPath = filepath.Join("deploy", "daemon", "secrets.yaml")

var townClusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Render the daemon's control-cluster manifests",
	Long: `Turn the image values file and the daemon's manifests into the files the
operator applies to the control cluster. The render is a file write: nothing
is applied and no cluster is contacted.`,
	RunE: requireSubcommand,
}

var townClusterRenderCmd = &cobra.Command{
	Use:   "render",
	Short: "Write the daemon's manifests in apply order and print the Secret commands",
	Long: `Write one numbered file per object into --out, in apply order: the namespace,
the claims, the service account, the Forgejo Service, the bindings, the seed
helper Pod (with --with-helper) and the Deployment. With --dashboard, the
dashboard's Service and Deployment are written too, in the same order.

Secret documents are not written. The command prints, for each Secret the
Deployment mounts, one kubectl line that creates it from the operator's existing
file, so no agent reads a secret. No file is opened to print those lines.

The image values file is read from under --dir, where imagevalues places it;
a missing file is refused, since a render needs a digest. A live render also
writes that pair to the town's own values file, where the pair-ready check
reads the running commit, and prints the one kubectl line that copies it into
the daemon pod's town copy. The line is printed, never run. A rehearsal writes
no values file. --cpu and --memory are required: no source records a measured
share. A live render also reads the derived hm config the compose write leaves
under the town root, and refuses when it is absent: the pod's hm would read its
defaults, which name its own loopback, so run gt daemon compose write first.

Examples:
  gt town cluster render --dir ~/gt/gastown --out ./manifests \
    --cpu 4 --memory 8Gi --host-gateway 192.168.64.1 \
    --proxy-image registry.example/gastown/proxy@sha256:<digest> \
    --confirm-container-stopped
  gt town cluster render --dir ~/gt/gastown --out ./rehearsal --rehearsal \
    --namespace gt-rehearsal --cpu 1 --memory 2Gi`,
	Args: cobra.NoArgs,
	RunE: runTownClusterRender,
}

// clusterRenderFlags are the flags of `gt town cluster render`.
type clusterRenderFlags struct {
	dir                string
	out                string
	namespace          string
	rehearsal          bool
	cpu                string
	memory             string
	hostGateway        string
	confirmStopped     bool
	withHelper         bool
	proxyImage         string
	polecatImage       string
	configDir          string
	openShellConfigDir string
	forgeHost          string
	doltBackupHostPath string
	dashboard          bool
	dashboardClaudeDir string
	daemonClaudeDir    string
}

var clusterRender clusterRenderFlags

// clusterRenderDeps is what the render takes from the host: the town it runs in,
// the environment the pod carries, the reads of the repo's files, the defaults
// the config directories come from, and the steps that write the manifests. A
// test supplies its own, so no unit test reads the live town or fails a real
// write to find out what the render does then.
type clusterRenderDeps struct {
	findTown     func() (string, error)
	containerEnv func(townRoot string) (map[string]string, error)
	readFile     func(path string) ([]byte, error)
	lstat        func(path string) (fs.FileInfo, error)
	getenv       func(key string) string
	userHome     func() (string, error)
	build        func(k8sdaemon.Values) (k8sdaemon.Manifests, error)
	render       func(io.Writer, k8sdaemon.Manifests) error
	buildHelper  func(k8sdaemon.Values) (k8sdaemon.Helper, error)
	encode       func(io.Writer, any) error
	mkdirAll     func(path string) error
	writeFile    func(path string, data []byte) error
	out          io.Writer
}

// realClusterRenderDeps is the host's. The environment is the one the compose
// writer renders into its ComposeSpec.Env, read through the same function.
func realClusterRenderDeps(out io.Writer) clusterRenderDeps {
	return clusterRenderDeps{
		findTown:     workspace.FindFromCwdOrError,
		containerEnv: realDaemonComposeDeps(out).containerEnv,
		readFile:     os.ReadFile,
		lstat:        os.Lstat,
		getenv:       os.Getenv,
		userHome:     os.UserHomeDir,
		build:        k8sdaemon.Build,
		render:       k8sdaemon.Render,
		buildHelper:  k8sdaemon.BuildHelper,
		encode:       k8sdaemon.YAML,
		mkdirAll:     func(path string) error { return os.MkdirAll(path, 0o755) },
		writeFile: func(path string, data []byte) error {
			return os.WriteFile(path, data, 0o644) //nolint:gosec // manifests hold no secret; the operator reads them
		},
		out: out,
	}
}

func init() {
	townClusterRenderCmd.Flags().StringVar(&clusterRender.dir, "dir", "",
		"the repo directory holding the image values file (see imagevalues) and deploy/daemon/secrets.yaml (required)")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.out, "out", "",
		"the directory the manifests are written to; it must be empty or absent (required)")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.namespace, "namespace", "",
		"the namespace every object lands in; the default when empty. A rehearsal must name one")
	townClusterRenderCmd.Flags().BoolVar(&clusterRender.rehearsal, "rehearsal", false,
		"render the rehearsal pod, which runs beside the live container: no promote key, no sweep binding")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.cpu, "cpu", "",
		"the container's CPU share, requests equal limits (required)")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.memory, "memory", "",
		"the container's memory share, requests equal limits (required)")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.hostGateway, "host-gateway", "",
		"the IPv4 address the control cluster's nodes reach the host at; the live pod maps the Dolt host to it")
	townClusterRenderCmd.Flags().BoolVar(&clusterRender.confirmStopped, "confirm-container-stopped", false,
		"acknowledge that the Docker Desktop container is stopped; a live render is refused without it")
	townClusterRenderCmd.Flags().BoolVar(&clusterRender.withHelper, "with-helper", false,
		"also write the seed helper Pod, applied before the Deployment")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.proxyImage, "proxy-image", "",
		"the socat relay image by digest (repo@sha256:<64 hex>); set, the live pod runs the registry and gateway relays")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.polecatImage, "polecat-image", "",
		"the polecat image by digest (repo@sha256:<64 hex>); set, the pod template carries it as the gastown.dev/polecat-image annotation and a read-only file")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.configDir, "config-dir", "",
		"the town's config directory the operator's secret files are in, an absolute path (default $XDG_CONFIG_HOME/gt or ~/.config/gt)")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.openShellConfigDir, "openshell-config-dir", "",
		"the operator's openshell config directory, an absolute path (default $XDG_CONFIG_HOME/openshell or ~/.config/openshell)")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.forgeHost, config.ForgeHostFlag, "",
		"the forge's host:port the pod's git asks for a credential (required for a live render, the flag `gt daemon compose write` takes); a rehearsal takes none")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.doltBackupHostPath, "dolt-backup-host-path", "",
		"the host directory the live pod mounts at the Dolt backup path, read-write, in place of the dolt-backup claim; it must be "+k8sdaemon.DoltBackupPath+" (a live render only)")
	townClusterRenderCmd.Flags().BoolVar(&clusterRender.dashboard, "dashboard", false,
		"also write the dashboard's Service and Deployment beside the daemon's; the secrets manifest must name "+k8sdaemon.ViewerTokenFile+" (a live render only)")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.dashboardClaudeDir, "dashboard-claude-dir", "",
		"the Mac's ~/.claude directory, an absolute path, mounted read-only in the dashboard at /root/.claude; it needs --dashboard")
	townClusterRenderCmd.Flags().StringVar(&clusterRender.daemonClaudeDir, "daemon-claude-dir", "",
		"the Mac's ~/.claude directory, an absolute path, whose projects directory is mounted read-only in the daemon at /root/.claude-host/projects, beside the pod's own /root/.claude, where the usage scan reads the host's transcripts")

	townClusterCmd.AddCommand(townClusterRenderCmd)
	townCmd.AddCommand(townClusterCmd)
}

func runTownClusterRender(cmd *cobra.Command, _ []string) error {
	return runClusterRender(realClusterRenderDeps(cmd.OutOrStdout()), clusterRender)
}

// xdgConfigSubdir is the directory sub lives in under the XDG config home, which
// is $XDG_CONFIG_HOME when it is set, else ~/.config.
func xdgConfigSubdir(sub string, getenv func(string) string, home func() (string, error)) (string, error) {
	if base := getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, sub), nil
	}
	dir, err := home()
	if err != nil {
		return "", fmt.Errorf("HOME cannot be found, so the default ~/.config/%s cannot be read; pass the directory's absolute path: %w", sub, err)
	}
	return filepath.Join(dir, ".config", sub), nil
}

// validate refuses the flags a render cannot do without, before anything is
// read or written.
func (f clusterRenderFlags) validate() error {
	switch {
	case strings.TrimSpace(f.dir) == "":
		return errors.New("--dir is required: name the repo directory that holds the image values file (see imagevalues)")
	case strings.TrimSpace(f.out) == "":
		return errors.New("--out is required: name the directory the manifests are written to, empty or absent")
	case strings.TrimSpace(f.cpu) == "":
		return errors.New("--cpu is required: no source records a measured share, so the render takes no default")
	case strings.TrimSpace(f.memory) == "":
		return errors.New("--memory is required: no source records a measured share, so the render takes no default")
	case !f.rehearsal && !f.confirmStopped:
		return errors.New("a live render needs --confirm-container-stopped: the Docker Desktop container owns the town until it is stopped, and only one of it and the pod may own the town")
	case !f.rehearsal && strings.TrimSpace(f.forgeHost) == "":
		return fmt.Errorf("--%s is required for a live render: the pod's git has no credential helper without it, and every Forgejo call fails (the live compose command passes the same flag)", config.ForgeHostFlag)
	case f.rehearsal && strings.TrimSpace(f.forgeHost) != "":
		return fmt.Errorf("--%s is for a live render: a rehearsal mounts only the read-only viewer token and carries no forge host", config.ForgeHostFlag)
	case f.rehearsal && f.doltBackupHostPath != "":
		return errors.New("--dolt-backup-host-path is for a live render: a rehearsal mounts the dolt-backup claim read-only and never owns the live backups")
	case f.doltBackupHostPath != "" && !filepath.IsAbs(f.doltBackupHostPath):
		return fmt.Errorf("--dolt-backup-host-path %q must be an absolute path", f.doltBackupHostPath)
	case f.doltBackupHostPath != "" && f.doltBackupHostPath != k8sdaemon.DoltBackupPath:
		return fmt.Errorf("--dolt-backup-host-path %q: want %q, the host directory the Dolt container writes its backups to", f.doltBackupHostPath, k8sdaemon.DoltBackupPath)
	case f.rehearsal && f.dashboard:
		return errors.New("--dashboard is for a live render: a rehearsal renders no dashboard, so drop --rehearsal or --dashboard")
	case f.dashboardClaudeDir != "" && !f.dashboard:
		return errors.New("--dashboard-claude-dir is for the dashboard: name --dashboard with it, or drop the flag")
	case f.dashboardClaudeDir != "" && !filepath.IsAbs(f.dashboardClaudeDir):
		return fmt.Errorf("--dashboard-claude-dir %q must be an absolute path: the pod names the host directory", f.dashboardClaudeDir)
	case f.daemonClaudeDir != "" && !filepath.IsAbs(f.daemonClaudeDir):
		return fmt.Errorf("--daemon-claude-dir %q must be an absolute path: the pod names the host directory", f.daemonClaudeDir)
	case !filepath.IsAbs(f.configDir):
		return fmt.Errorf("--config-dir %q must be an absolute path: the create commands are run from another directory", f.configDir)
	case !filepath.IsAbs(f.openShellConfigDir):
		return fmt.Errorf("--openshell-config-dir %q must be an absolute path: the create commands are run from another directory", f.openShellConfigDir)
	}
	return nil
}

// checkOutDir refuses an --out that holds anything, so the render never mixes
// its files with another apply set. A missing directory is fine: it is made.
func checkOutDir(path string) error {
	entries, err := os.ReadDir(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("--out %s cannot be read as a directory: %w", path, err)
	case len(entries) > 0:
		return fmt.Errorf("--out %s is not empty: the render writes into an empty or absent directory, so it refuses to mix its files with another apply set", path)
	}
	return nil
}

// runClusterRender writes the manifests for f into f.out and prints the
// Secret-creating commands. Every refusal happens before the first write.
func runClusterRender(d clusterRenderDeps, f clusterRenderFlags) error {
	if f.configDir == "" {
		dir, err := xdgConfigSubdir("gt", d.getenv, d.userHome)
		if err != nil {
			return err
		}
		f.configDir = dir
	}
	if f.openShellConfigDir == "" {
		dir, err := xdgConfigSubdir("openshell", d.getenv, d.userHome)
		if err != nil {
			return err
		}
		f.openShellConfigDir = dir
	}
	if err := f.validate(); err != nil {
		return err
	}
	if err := checkOutDir(f.out); err != nil {
		return err
	}
	pair, ok, err := imagevalues.LoadIn(f.dir)
	if err != nil {
		return fmt.Errorf("reading the image values under %s: %w", f.dir, err)
	}
	if !ok {
		return fmt.Errorf("--dir %s holds no image pair at %s, and a render needs the daemon's digest: run gt update images first", f.dir, imagevalues.PathIn(f.dir))
	}
	townRoot, err := d.findTown()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace, so the pod's environment cannot be read: %w", err)
	}
	env, err := d.containerEnv(townRoot)
	if err != nil {
		return fmt.Errorf("reading the town's daemon.env for the pod's environment: %w", err)
	}
	manifestPath := filepath.Join(f.dir, secretsManifestRelPath)
	data, err := d.readFile(manifestPath)
	if err != nil {
		return fmt.Errorf("reading the secrets manifest %s: %w", manifestPath, err)
	}
	secrets, err := manifestdata.ParseSecretsManifest(data)
	if err != nil {
		return fmt.Errorf("the secrets manifest %s is refused: %w", manifestPath, err)
	}
	// The dashboard's token is the viewer entry's Secret. The manifest marks that
	// entry optional, and a pod that mounts a Secret that does not exist stays in
	// ContainerCreating, so the render refuses before it writes anything.
	if f.dashboard && !slices.ContainsFunc(secrets.Files, func(s manifestdata.SecretFile) bool {
		return s.Name == k8sdaemon.ViewerTokenFile
	}) {
		return fmt.Errorf("--dashboard needs the %s entry in %s: the dashboard reads its Forgejo token from that Secret, and a pod that mounts a missing Secret stays in ContainerCreating; no file was written", k8sdaemon.ViewerTokenFile, manifestPath)
	}
	cloudCheckKey, err := cloudCheckKeyConfigured(f.configDir, d.lstat)
	if err != nil {
		return err
	}
	hmConfigPath := filepath.Join(townRoot, filepath.FromSlash(k8sdaemon.HMConfigSource))
	hmConfig := false
	if !f.rehearsal {
		if hmConfig, err = hmConfigConfigured(hmConfigPath, d.lstat); err != nil {
			return err
		}
		// A pod whose hm mounts no config reads its defaults, which name the pod's
		// own loopback, so it reaches neither the town's database nor the host's
		// embedder and reranker. The render refuses rather than write a pod that
		// cannot work (gt-fg6ami).
		if !hmConfig {
			return fmt.Errorf("the derived hm config is absent at %s: without it the pod's hm reads its defaults, which name the pod's own loopback; run gt daemon compose write first, and no file was written", hmConfigPath)
		}
	}
	namespace := f.namespace
	if namespace == "" {
		namespace = k8sdaemon.DefaultNamespace
	}
	v := k8sdaemon.Values{
		Namespace:               f.namespace,
		Rehearsal:               f.rehearsal,
		Secrets:                 secrets.Files,
		CloudCheckKey:           cloudCheckKey,
		HMConfig:                hmConfig,
		Image:                   pair.Daemon,
		ProxyImage:              f.proxyImage,
		PolecatImage:            f.polecatImage,
		Env:                     env,
		CPU:                     f.cpu,
		Memory:                  f.memory,
		ConfirmContainerStopped: f.confirmStopped,
		HostGatewayIP:           f.hostGateway,
		Dashboard:               f.dashboard,
		DashboardClaudeDir:      f.dashboardClaudeDir,
		DaemonClaudeDir:         f.daemonClaudeDir,
	}
	if !f.rehearsal {
		// The live pod's arguments equal the live compose command's: the om token
		// file and the forge host with the landing bot's token (gt-22wubb).
		v.OMToken = true
		v.ForgeHost = strings.TrimSpace(f.forgeHost)
		v.DoltBackupHostPath = f.doltBackupHostPath
	}
	m, err := d.build(v)
	if err != nil {
		return fmt.Errorf("cannot render the manifests, and no file was written: %w", err)
	}
	var stream bytes.Buffer
	if err := d.render(&stream, m); err != nil {
		return fmt.Errorf("cannot encode the manifests, and no file was written: %w", err)
	}
	objects, err := applyObjects(stream.Bytes(), yaml.Marshal)
	if err != nil {
		return err
	}
	if f.withHelper {
		helper, err := d.buildHelper(v)
		if err != nil {
			return fmt.Errorf("cannot render the seed helper, and no file was written: %w", err)
		}
		var body bytes.Buffer
		if err := d.encode(&body, helper); err != nil {
			return fmt.Errorf("cannot encode the seed helper, and no file was written: %w", err)
		}
		objects = append(objects, applyObject{rank: applyRank["Pod"], kind: "Pod", name: k8sdaemon.HelperName, body: body.Bytes()})
	}
	sort.SliceStable(objects, func(i, j int) bool { return objects[i].rank < objects[j].rank })
	lines, err := secretCreateLines(m, namespace, f.configDir, f.openShellConfigDir, townRoot)
	if err != nil {
		return err
	}
	if err := d.mkdirAll(f.out); err != nil {
		return fmt.Errorf("cannot create --out %s: %w", f.out, err)
	}
	for i := range objects {
		objects[i].file = fmt.Sprintf("%02d-%s-%s.yaml", i+1, strings.ToLower(objects[i].kind), objects[i].name)
		path := filepath.Join(f.out, objects[i].file)
		if err := d.writeFile(path, objects[i].body); err != nil {
			return fmt.Errorf("cannot write %s: %w", path, err)
		}
	}
	// The pair-ready check reads the running commit from the town's own values
	// file, and nothing else writes it. A live render writes it after the
	// manifests, so a failed manifest write leaves the running commit as it was.
	townValues := imagevalues.PathIn(filepath.Join(townRoot, "gastown"))
	if !f.rehearsal {
		if err := imagevalues.Write(townValues, pair); err != nil {
			return fmt.Errorf("cannot write the running values file %s, and the manifests are written: %w", townValues, err)
		}
	}
	fmt.Fprintf(d.out, "Wrote %d manifests to %s, in apply order:\n", len(objects), f.out)
	for _, o := range objects {
		fmt.Fprintf(d.out, "  %s\n", o.file)
	}
	fmt.Fprintf(d.out, "Values: commit %s, daemon image %s\n", pair.Commit, pair.Daemon)
	fmt.Fprintln(d.out, "Apply the namespace file first so it exists. Then create each Secret from its file, and apply the rest in the order listed:")
	for _, l := range lines {
		fmt.Fprintf(d.out, "  %s\n", l)
	}
	if !f.rehearsal {
		fmt.Fprintf(d.out, "Wrote the running values file to %s. The daemon pod reads its own town copy, so copy it there too:\n", townValues)
		fmt.Fprintf(d.out, "  %s\n", podValuesCopyLine(namespace, townValues))
	}
	return nil
}

// podValuesCopyLine is the one command that copies the town's values file into
// the daemon pod's town copy. The daemon container runs with the town root at
// TownRootPath, so the pod reads the file at TownRootPath/gastown. The copy
// goes to a .tmp file first; test -s refuses an empty copy, which is what a
// missing -i leaves, and mv replaces the pod's file only after that check. The
// line is printed for the operator and never run.
func podValuesCopyLine(namespace, src string) string {
	dst := k8sdaemon.TownRootPath + "/gastown/" + imagevalues.DefaultRelPath
	dir := k8sdaemon.TownRootPath + "/gastown/" + filepath.Dir(imagevalues.DefaultRelPath)
	tmp := dst + ".tmp"
	inner := fmt.Sprintf("mkdir -p %s && cat > %s && test -s %s && mv %s %s",
		shellQuote(dir), shellQuote(tmp), shellQuote(tmp), shellQuote(tmp), shellQuote(dst))
	return fmt.Sprintf("kubectl -n %s exec -i deploy/%s -c %s -- sh -c %s < %s",
		shellQuote(namespace), k8sdaemon.Name, k8sdaemon.Name, shellQuote(inner), shellQuote(src))
}

// applyRank orders the kinds a render writes. A seed helper Pod fills the
// claims before the daemon starts, so it applies ahead of the Deployment.
var applyRank = map[string]int{
	"Namespace":             1,
	"PersistentVolumeClaim": 2,
	"ServiceAccount":        3,
	"Service":               4,
	"RoleBinding":           5,
	"ClusterRoleBinding":    5,
	"Pod":                   6,
	"Deployment":            7,
}

// applyObject is one document of the apply set: its kind and name, its rank in
// the apply order, the numbered file it is written to, and its bytes.
type applyObject struct {
	rank int
	kind string
	name string
	file string
	body []byte
}

// cloudCheckKeyConfigured reports whether the operator's config directory holds
// the key file the gt-cloud-check-gcp Secret is created from. The patrol runs
// without the key (one log line), and the pod must not mount a Secret that was
// never created, so the mount and its create line are rendered only for a
// configured key. A path that cannot be read for any other reason refuses the
// render rather than guessing the file away.
func cloudCheckKeyConfigured(configDir string, lstat func(string) (fs.FileInfo, error)) (bool, error) {
	path := filepath.Join(configDir, k8sdaemon.CloudCheckKeyFile)
	switch _, err := lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("the cloud check's key file %s cannot be read, so the render cannot tell whether to mount it: %w", path, err)
	}
	return true, nil
}

// hmConfigConfigured reports whether the derived hm config exists under the town
// root, the file the Secret gt-hm-config is created from. The pod reads its
// database and its embedder and reranker from that file, so a live render mounts
// it and sets HM_CONFIG only when it is there, and refuses when it is absent: a
// pod without it reads its defaults, which name the pod's own loopback. A path
// that cannot be read for any other reason refuses the render too (gt-7vrjuo).
func hmConfigConfigured(path string, lstat func(string) (fs.FileInfo, error)) (bool, error) {
	switch _, err := lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("the derived hm config %s cannot be read, so the render cannot tell whether to mount it: %w", path, err)
	}
	return true, nil
}

// applyObjects splits a rendered stream into its documents, dropping the Secrets:
// an applied placeholder would make the operator's `kubectl create secret` fail
// with AlreadyExists, so the Secrets are created from the operator's files.
// marshal re-encodes each document's node; the render passes yaml.Marshal.
func applyObjects(stream []byte, marshal func(any) ([]byte, error)) ([]applyObject, error) {
	var out []applyObject
	dec := yaml.NewDecoder(bytes.NewReader(stream))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading the rendered manifests back: %w", err)
		}
		var head struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := doc.Decode(&head); err != nil {
			return nil, fmt.Errorf("reading a rendered document's kind: %w", err)
		}
		if head.Kind == "Secret" {
			continue
		}
		rank, ok := applyRank[head.Kind]
		if !ok {
			return nil, fmt.Errorf("the rendered manifests hold a %s, which has no place in the apply order", head.Kind)
		}
		body, err := marshal(&doc)
		if err != nil {
			return nil, fmt.Errorf("encoding a rendered %s: %w", head.Kind, err)
		}
		out = append(out, applyObject{rank: rank, kind: head.Kind, name: head.Metadata.Name, body: body})
	}
}

// secretCreateLines prints one `kubectl create secret generic` line for each
// Secret the Deployment mounts, in the Deployment's volume order. A config
// entry's source is its file in configDir; the openshell tree's keys are each
// file under openShellDir, named by the volume's items. The hm config's source
// is the derived file under townRoot. A mounted Secret the render did not write
// is refused, since its line could not be printed.
func secretCreateLines(m k8sdaemon.Manifests, namespace, configDir, openShellDir, townRoot string) ([]string, error) {
	secrets := make(map[string]k8sdaemon.Secret, len(m.Secrets))
	for _, s := range m.Secrets {
		secrets[s.Metadata.Name] = s
	}
	var lines []string
	for _, vol := range m.Deployment.Spec.Template.Spec.Volumes {
		if vol.Secret == nil {
			continue
		}
		name := vol.Secret.SecretName
		sec, ok := secrets[name]
		if !ok {
			return nil, fmt.Errorf("the Deployment mounts Secret %s, which the render did not write, so no create command can be printed for it", name)
		}
		source := sec.Metadata.Annotations[k8sdaemon.SourceFileAnnotation]
		if source == "" {
			return nil, fmt.Errorf("the Deployment mounts Secret %s, which has no source file annotation, so no create command can be printed for it", name)
		}
		var froms []string
		if source == manifestdata.OpenShellEntry.Name {
			if len(vol.Secret.Items) == 0 {
				return nil, fmt.Errorf("Secret %s is the openshell tree but its volume names no files", name)
			}
			for _, item := range vol.Secret.Items {
				froms = append(froms, fromFileArg(item.Key, filepath.Join(openShellDir, item.Path)))
			}
		} else if source == k8sdaemon.CloudCheckKeyFile {
			// The Secret's data key is the file name the pod mounts, not the
			// operator's config-directory file name it is created from.
			froms = append(froms, fromFileArg(k8sdaemon.CloudCheckKeyKey, filepath.Join(configDir, source)))
		} else if source == k8sdaemon.HMConfigSource {
			// The Secret's data key is the file name the pod mounts; the source is
			// the derived file the compose write leaves under the town root.
			froms = append(froms, fromFileArg(k8sdaemon.HMConfigKey, filepath.Join(townRoot, filepath.FromSlash(source))))
		} else {
			froms = append(froms, fromFileArg(source, filepath.Join(configDir, source)))
		}
		lines = append(lines, fmt.Sprintf("kubectl -n %s create secret generic %s %s", shellQuote(namespace), shellQuote(name), strings.Join(froms, " ")))
	}
	return lines, nil
}

// fromFileArg is one --from-file argument: the Secret key and the operator's
// file it is read from.
func fromFileArg(key, path string) string {
	return "--from-file=" + shellQuote(key) + "=" + shellQuote(path)
}

// shellSafe is a path a shell reads as one word without quoting.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// shellQuote quotes s for a POSIX shell when it holds a character the shell would
// split or expand.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
