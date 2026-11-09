package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
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

// seedSettingsArgv is what the pod runs to unpack the rehearsal's settings file
// into the claim. The pod's gt import refuses a shared row, so the file arrives as
// a plain tar stream, unpacked under the mount.
var seedSettingsArgv = []string{"tar", "-x", "-f", "-", "-C", townstate.VolumeMount}

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

With --rehearsal the command also copies the town's settings file into the claim
in its own step after the daemon-owned stream. The split names that file shared,
so the daemon stream leaves it out; the rehearsal's copy is one file, compared by
sha256 before the profile runs, and a difference in it still fails. Then the
command runs "gt town state profile --rehearsal" in the pod, which turns every
daemon patrol off in that file, and the file is reported as "profiled", not as a
difference. A rehearsal refuses the live namespace "gt".

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
		if err := seedSettingsHostFile(env.town, rel); err != nil {
			return fmt.Errorf("refused: %w", err)
		}
		settings = rel
	}

	m := env.manifest
	if m == nil {
		m = townstate.Default()
	}
	opts := townstate.ExportOptions{Since: env.since, AllowNoRigs: env.allowNoRigs, Exclude: env.exclude}
	sum, err := townstate.SeedVolume(env.engine, seedClaim, env.town, m, opts, townstate.ImportArgv, env.out)
	if err != nil {
		return fmt.Errorf("stream into claim %s: %w", seedClaim, err)
	}
	fmt.Fprintln(env.out, sum.String())

	// The verify runs before the settings step, so the claim holds only the
	// daemon-owned set it compares.
	res, err := townstate.VerifyVolume(env.engine, seedClaim, env.town, m, opts)
	if err != nil {
		return fmt.Errorf("verifying claim %s: the claim is partial and not to be used: %w", seedClaim, err)
	}
	if problems := printVerifyProblems(env.out, res, nil); problems > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("claim %s does not match the host copy set (%d problems): the claim is partial and not to be used", seedClaim, problems)}
	}

	// The success line comes last, after the profile, and says whether the profile
	// changed the claim: the profile writes the settings file only when it finds
	// something still on.
	profileNote := ""
	if env.rehearsal {
		if err := seedSettingsStream(env, settings); err != nil {
			return fmt.Errorf("stream settings into claim %s: the claim is partial and not to be used: %w", seedClaim, err)
		}
		mismatches, err := seedSettingsMismatches(env, settings)
		if err != nil {
			return fmt.Errorf("verifying settings in claim %s: the claim is partial and not to be used: %w", seedClaim, err)
		}
		if problems := printVerifyProblems(env.out, townstate.VerifyResult{Mismatches: mismatches}, nil); problems > 0 {
			return &ExitCodeError{Code: 1, Err: fmt.Errorf("claim %s does not match the host's %s (%d problems): the claim is partial and not to be used", seedClaim, settings, problems)}
		}
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

// seedSettingsHostFile refuses a rehearsal whose settings file is not a regular
// file on the host. It only reads the host, and it runs before any stream, so a
// refused run leaves the claim as it was.
func seedSettingsHostFile(town, rel string) error {
	info, err := os.Lstat(filepath.Join(town, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("the settings file %s cannot be read on the host: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("the settings file %s is not a regular file on the host, and no claim is written", rel)
	}
	return nil
}

// seedSettingsStream copies the rehearsal's settings file into the claim, after
// the daemon-owned stream and before the profile. The split names settings shared,
// so the daemon stream leaves the file out; this is the rehearsal's own copy of
// that one file, sent through the same kubectl exec as the daemon stream. The
// live cut-over's way of giving the pod its settings is a separate question,
// tracked outside this bead.
func seedSettingsStream(env townStateSeedEnv, rel string) error {
	m := &townstate.Manifest{Entries: []townstate.Entry{{
		Path:  rel,
		Owner: townstate.OwnerDaemon,
		Why:   "the rehearsal's own copy of the settings file the profile rewrites",
	}}}
	opts := townstate.ExportOptions{Exclude: env.exclude}
	if _, err := townstate.SeedVolume(env.engine, seedClaim, env.town, m, opts, seedSettingsArgv, env.out); err != nil {
		return err
	}
	fmt.Fprintf(env.out, "settings %s streamed into claim %s (the rehearsal's own copy)\n", rel, seedClaim)
	return nil
}

// seedSettingsMismatches compares the settings file as the claim holds it with the
// host's, by sha256, before the profile rewrites it. A file the claim lacks, or
// holds with other bytes, is a mismatch naming rel. A listing that fails is an
// error, never a pass.
func seedSettingsMismatches(env townStateSeedEnv, rel string) ([]townstate.Mismatch, error) {
	host, err := seedHostFileSum(filepath.Join(env.town, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("reading the host's %s: %w", rel, err)
	}
	vol, found, err := claimFileSum(env.engine, seedClaim, rel)
	if err != nil {
		return nil, err
	}
	switch {
	case !found:
		return []townstate.Mismatch{{Path: rel, Reason: townstate.ReasonMissing}}, nil
	case vol != host:
		return []townstate.Mismatch{{Path: rel, Reason: townstate.ReasonChanged}}, nil
	}
	return nil, nil
}

// seedHostFileSum is the sha256 of the host file at p, read and never written.
func seedHostFileSum(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// claimFileSum lists the claim through the engine and returns the sha256 of the
// entry named rel and whether the listing has one. A non-regular entry under that
// name is found with an empty sum, so it never matches a host file. The listing is
// read to its end, so a listing that fails part way is an error.
func claimFileSum(eng townstate.Engine, claim, rel string) (string, bool, error) {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(eng.Run(claim, townstate.ListArgv, nil, pw)) }()
	defer pr.Close()
	tr := tar.NewReader(pr)
	var sum string
	found := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return sum, found, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("listing claim %s: %w", claim, err)
		}
		if path.Clean(hdr.Name) != rel {
			continue
		}
		found = true
		if hdr.Typeflag != tar.TypeReg {
			sum = ""
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil {
			return "", false, fmt.Errorf("listing claim %s: %w", claim, err)
		}
		sum = hex.EncodeToString(h.Sum(nil))
	}
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
