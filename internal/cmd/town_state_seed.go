package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/townstate"
)

// seedClaim is the cluster claim the seed fills: the rehearsal's town-state
// volume. The pod engine answers only for this name.
const seedClaim = "town-state"

// seedLiveNamespace is the namespace the live daemon runs in. A rehearsal seed
// never writes into it.
const seedLiveNamespace = "gt"

// seedProfileArgv is what a rehearsal runs in the pod after the stream. The town
// is townstate.VolumeMount, which the pod engine maps to the claim's mount path.
var seedProfileArgv = []string{"gt", "town", "state", "profile", "--town", townstate.VolumeMount, "--rehearsal"}

// seedNamespacePattern and seedPodPattern are the shapes a namespace and a pod
// name take. A name outside them is refused before it reaches a kubectl argv.
var (
	seedNamespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	seedPodPattern       = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

var townStateSeedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Copy the daemon-owned set into a cluster town-state claim through the seed helper Pod",
	Long: `Stream the daemon-owned set of the town root into the town-state claim the seed
helper Pod mounts, then compare both sides by sha256 and link target, as
"gt town state migrate verify" does.

The stream is "gt town state export" on this host, piped into "gt town state
import" in the pod through kubectl exec. The secret-named files are left out of
the stream and of the listing. The host tree is only read. The claim is mounted
in the pod at the --town path, so the pod's import and listing name the same
path the host does.

With --rehearsal the command runs "gt town state profile --rehearsal" in the pod
after the stream, which turns every daemon patrol off in the seeded settings
file. That settings file is compared before the profile runs, so a difference in
it still fails, and it is then reported as "profiled", not as a difference. A
rehearsal refuses the live namespace "gt", and it refuses while the town's split
names the settings file shared: a shared row is not streamed, so there is no
settings file in the claim to profile.

Without --rehearsal (the live copy) the command refuses unless the dispatch hold
file exists and the daemon container is stopped.

A failure after the stream starts exits non-zero and says the claim is partial
and not to be used.

Examples:
  gt town state seed --town /Users/sloan/gt --namespace rehearsal --pod seed-helper \
    --secrets-manifest deploy/daemon/secrets.yaml --rehearsal`,
	Args: cobra.NoArgs,
	RunE: runTownStateSeed,
}

func init() {
	townStateCmd.AddCommand(townStateSeedCmd)
	townStateSeedCmd.Flags().String("town", "", "the town root to copy from, an absolute path the pod mounts the claim at (required)")
	townStateSeedCmd.Flags().String("namespace", "", "the namespace of the seed helper Pod (required)")
	townStateSeedCmd.Flags().String("pod", "", "the seed helper Pod that mounts the town-state claim (required)")
	townStateSeedCmd.Flags().Bool("rehearsal", false, "seed a rehearsal: refuse namespace gt, and profile the seeded settings in the pod")
	townStateSeedCmd.Flags().String("since", "", "keep only regular files modified after this RFC 3339 time (optional)")
	townStateSeedCmd.Flags().Bool("allow-no-rigs", false, "copy a town with no registered rigs (every rig-scoped row would be missing)")
	townStateSeedCmd.Flags().String("secrets-manifest", "", "deploy/daemon/secrets.yaml: its file names are left out of the stream and the listing (required)")
	_ = townStateSeedCmd.MarkFlagRequired("town")
	_ = townStateSeedCmd.MarkFlagRequired("namespace")
	_ = townStateSeedCmd.MarkFlagRequired("pod")
	_ = townStateSeedCmd.MarkFlagRequired("secrets-manifest")
}

// kubectlRun runs one kubectl argv with the streams given. Production is
// execKubectl; a test supplies a fake and starts no process.
type kubectlRun func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error

// podEngine is the cluster townstate.Engine: each command is one kubectl exec
// into the seed helper Pod, which mounts the claim at the town's own path.
type podEngine struct {
	namespace string
	pod       string
	claim     string
	// town is the path the pod mounts the claim at. Each argv entry that is
	// townstate.VolumeMount names it.
	town string
	run  kubectlRun
	// timeout bounds one exec. Zero means migrateHelperTimeout; a test sets it
	// short to drive the timeout without waiting ten minutes.
	timeout time.Duration
}

// Run runs argv in the pod with stdin streamed in and stdout streamed out. A
// volume other than the claim is refused, so the engine never reaches a claim the
// pod does not mount. The pod's stderr goes to the operator's terminal, never into
// an error.
func (e podEngine) Run(volume string, argv []string, stdin io.Reader, stdout io.Writer) error {
	if volume != e.claim {
		return fmt.Errorf("the seed helper Pod mounts claim %s, not volume %s", e.claim, volume)
	}
	args := []string{"exec", "-i", "-n", e.namespace, e.pod, "--"}
	for _, a := range argv {
		if a == townstate.VolumeMount {
			a = e.town
		}
		args = append(args, a)
	}
	timeout := e.timeout
	if timeout == 0 {
		timeout = migrateHelperTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := e.run(ctx, args, stdin, stdout, os.Stderr); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("kubectl exec into pod %s did not finish within %s; the pod may still be running it", e.pod, timeout)
		}
		return fmt.Errorf("kubectl exec into pod %s exited: %w", e.pod, err)
	}
	return nil
}

// HasVolume is not answered: the seed helper Pod mounts the claim, and the seed
// neither looks it up nor creates it.
func (e podEngine) HasVolume(string) (bool, error) {
	return false, errors.New("the seed helper Pod mounts the claim; the seed does not look it up")
}

// CreateVolume is refused for the same reason as HasVolume.
func (e podEngine) CreateVolume(string) error {
	return errors.New("the seed helper Pod mounts the claim; the seed does not create it")
}

// execKubectl is the production kubectlRun: the kubectl binary with args, on the
// operator's own kubeconfig.
func execKubectl(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...) //nolint:gosec // fixed binary; the namespace and pod names are validated before they are built
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// townStateSeedEnv is one seed session: the town, the target, the engine, the
// cut-over probes and the output. Production fills it from flags and the pod
// engine; tests fill it with fakes.
type townStateSeedEnv struct {
	town        string
	namespace   string
	rehearsal   bool
	since       time.Time
	allowNoRigs bool
	exclude     func(string) bool
	// manifest is the copy set. Nil means the town's default split.
	manifest *townstate.Manifest
	// patrolWritePath names the file the daemon patrol config is written to,
	// which the rehearsal profile rewrites. Production is config.PatrolWritePath.
	patrolWritePath func(string) (string, error)
	engine          townstate.Engine
	probes          migrateProbes
	out             io.Writer
}

func runTownStateSeed(cmd *cobra.Command, _ []string) error {
	env, err := townStateSeedEnvFromFlags(cmd)
	if err != nil {
		return err
	}
	return runTownStateSeedIn(env)
}

// seedFlags is the seed command's flag values, read once.
type seedFlags struct {
	town, namespace, pod, since, secretsPath string
	rehearsal, allowNoRigs                   bool
}

// readSeedFlags reads every flag the seed command defines. A flag the command
// does not define, or one of the other type, is an error.
func readSeedFlags(cmd *cobra.Command) (seedFlags, error) {
	var f seedFlags
	strs := []struct {
		name string
		dst  *string
	}{
		{"town", &f.town},
		{"namespace", &f.namespace},
		{"pod", &f.pod},
		{"since", &f.since},
		{"secrets-manifest", &f.secretsPath},
	}
	for _, s := range strs {
		v, err := cmd.Flags().GetString(s.name)
		if err != nil {
			return seedFlags{}, fmt.Errorf("reading --%s: %w", s.name, err)
		}
		*s.dst = v
	}
	bools := []struct {
		name string
		dst  *bool
	}{
		{"rehearsal", &f.rehearsal},
		{"allow-no-rigs", &f.allowNoRigs},
	}
	for _, b := range bools {
		v, err := cmd.Flags().GetBool(b.name)
		if err != nil {
			return seedFlags{}, fmt.Errorf("reading --%s: %w", b.name, err)
		}
		*b.dst = v
	}
	return f, nil
}

// townStateSeedEnvFromFlags is the production session the command's flags name:
// the target is checked, --since and the secrets manifest are read, and the pod
// engine is built over the namespace and pod.
func townStateSeedEnvFromFlags(cmd *cobra.Command) (townStateSeedEnv, error) {
	f, err := readSeedFlags(cmd)
	if err != nil {
		return townStateSeedEnv{}, err
	}
	if err := validSeedTarget(f.namespace, f.pod, f.town); err != nil {
		return townStateSeedEnv{}, err
	}
	sinceAt, err := parseSeedSince(f.since)
	if err != nil {
		return townStateSeedEnv{}, err
	}
	exclude, err := seedSecretExclusion(f.secretsPath)
	if err != nil {
		return townStateSeedEnv{}, err
	}
	return townStateSeedEnv{
		town:            f.town,
		namespace:       f.namespace,
		rehearsal:       f.rehearsal,
		since:           sinceAt,
		allowNoRigs:     f.allowNoRigs,
		exclude:         exclude,
		patrolWritePath: config.PatrolWritePath,
		engine:          podEngine{namespace: f.namespace, pod: f.pod, claim: seedClaim, town: f.town, run: execKubectl},
		probes:          productionMigrateProbes(),
		out:             cmd.OutOrStdout(),
	}, nil
}

// parseSeedSince reads --since: empty keeps every file, else an RFC 3339 time.
func parseSeedSince(since string) (time.Time, error) {
	if since == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q is not an RFC 3339 time", since)
	}
	return at, nil
}

// seedSecretExclusion reads the secrets manifest at path and returns the
// exclusion for the stream and the listing. A manifest that cannot be read or
// parsed is an error: a copy without its exclusion would carry the secrets.
func seedSecretExclusion(path string) (func(string) bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the secrets manifest: %w", err)
	}
	return secretExclusion(data)
}

// validSeedTarget refuses a namespace, a pod name or a town root that cannot be
// used in a kubectl argv or as the path a pod mounts the claim at.
func validSeedTarget(namespace, pod, town string) error {
	if !seedNamespacePattern.MatchString(namespace) {
		return fmt.Errorf("--namespace %q is not a Kubernetes namespace name", namespace)
	}
	if !seedPodPattern.MatchString(pod) {
		return fmt.Errorf("--pod %q is not a pod name", pod)
	}
	if !filepath.IsAbs(town) {
		return fmt.Errorf("--town %q is not an absolute path: the pod mounts the claim at that same path", town)
	}
	return nil
}

// runTownStateSeedIn is what `gt town state seed` runs. Every refusal happens
// before the stream starts, so a refused run leaves the claim untouched.
func runTownStateSeedIn(env townStateSeedEnv) error {
	if env.rehearsal && env.namespace == seedLiveNamespace {
		return fmt.Errorf("refused: --rehearsal in namespace %q, the live daemon's namespace", seedLiveNamespace)
	}
	if !env.rehearsal {
		if unmet := seedCutoverUnmet(env); len(unmet) > 0 {
			return fmt.Errorf("refused: the live copy needs the cut-over prerequisites: %s", strings.Join(unmet, "; "))
		}
	}
	var settings string
	if env.rehearsal {
		rel, err := seedSettingsRel(env)
		if err != nil {
			return fmt.Errorf("refused: naming the settings the rehearsal profile writes: %w", err)
		}
		settings = rel
	}

	m := env.manifest
	if m == nil {
		m = townstate.Default()
	}
	if env.rehearsal {
		if err := seedSettingsDaemonOwned(m, settings); err != nil {
			return fmt.Errorf("refused: %w", err)
		}
	}
	opts := townstate.ExportOptions{Since: env.since, AllowNoRigs: env.allowNoRigs, Exclude: env.exclude}
	sum, err := townstate.SeedVolume(env.engine, seedClaim, env.town, m, opts, townstate.ImportArgv, env.out)
	if err != nil {
		return fmt.Errorf("stream into claim %s: %w", seedClaim, err)
	}
	fmt.Fprintln(env.out, sum.String())

	// The verify runs before the profile, so the settings file is compared as
	// the stream left it.
	res, err := townstate.VerifyVolume(env.engine, seedClaim, env.town, m, opts)
	if err != nil {
		return fmt.Errorf("verifying claim %s: the claim is partial and not to be used: %w", seedClaim, err)
	}
	if problems := printVerifyProblems(env.out, res); problems > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("claim %s does not match the host copy set (%d problems): the claim is partial and not to be used", seedClaim, problems)}
	}

	// The success line comes last, after the profile, and says whether the profile
	// changed the claim: the profile writes the settings file only when it finds
	// something still on.
	profileNote := ""
	if env.rehearsal {
		var profileOut bytes.Buffer
		if err := env.engine.Run(seedClaim, seedProfileArgv, nil, io.MultiWriter(env.out, &profileOut)); err != nil {
			return fmt.Errorf("the rehearsal profile in claim %s failed: the claim is partial and not to be used: %w", seedClaim, err)
		}
		fmt.Fprintf(env.out, "profiled %s\n", settings)
		profileNote = "; the profile changed nothing"
		if strings.Contains(profileOut.String(), rehearsalWrittenPrefix) {
			profileNote = "; the profile changed the claim"
		}
	}
	fmt.Fprintf(env.out, "claim %s matches the host copy set (%d files) as streamed%s\n", seedClaim, res.Files, profileNote)
	return nil
}

// seedSettingsDaemonOwned refuses a rehearsal whose split does not stream the
// settings file. Only daemon-owned rows are copied and imported, so a settings row
// classed shared leaves the file out of the claim, and the profile would have
// nothing to rewrite. The refusal comes before any stream, so the claim is left as
// it was.
func seedSettingsDaemonOwned(m *townstate.Manifest, rel string) error {
	row, ok := townstate.Lookup(m.Entries, rel)
	if !ok || row.Owner != townstate.OwnerDaemon {
		return fmt.Errorf("the split does not copy %s into the claim (its row is not daemon-owned): a rehearsal cannot copy or profile it, and no claim is written", rel)
	}
	return nil
}

// seedCutoverUnmet reads the two cut-over prerequisites a live seed needs: the
// dispatch hold file and a stopped daemon container. Each one that is not met,
// or cannot be read, is named. A container docker cannot find is unmet too: a
// removed container and a misnamed one read the same.
func seedCutoverUnmet(env townStateSeedEnv) []string {
	var unmet []string
	present, err := env.probes.holdPresent(env.town)
	switch {
	case err != nil:
		unmet = append(unmet, fmt.Sprintf("dispatch hold unreadable: %v", err))
	case !present:
		unmet = append(unmet, "dispatch hold missing")
	}

	name, err := env.probes.daemonContainer(env.town)
	if err != nil {
		return append(unmet, fmt.Sprintf("daemon container state unreadable: %v", err))
	}
	running, err := env.probes.containerRunning(name)
	switch {
	case errors.Is(err, errDaemonContainerAbsent):
		unmet = append(unmet, fmt.Sprintf("daemon container %q not found", name))
	case err != nil:
		unmet = append(unmet, fmt.Sprintf("daemon container state unreadable: %v", err))
	case running:
		unmet = append(unmet, fmt.Sprintf("daemon container running (%s)", name))
	}
	return unmet
}

// seedSettingsRel names, relative to the town root, the settings file the
// rehearsal profile rewrites. It reads the host's file and never writes it.
func seedSettingsRel(env townStateSeedEnv) (string, error) {
	path, err := env.patrolWritePath(daemon.PatrolConfigFile(env.town))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(env.town, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%s is not under the town root", path)
	}
	return filepath.ToSlash(rel), nil
}
