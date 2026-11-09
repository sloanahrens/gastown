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
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/dispatch"
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
file exists and the daemon container is stopped. The live copy also sends the
town's settings/config.json, with daemon.supervisor removed and every patrol
left as the Mac has it, and each registered rig's <rig>/settings/config.json byte
for byte, after the daemon-owned stream. The claim is listed afterwards and each
file's sha256 is compared with the bytes sent; a difference leaves the claim
partial. Last, the cut-over record (the time the copy ended, the claim and the
seconds the copy took) is written into the claim alone, in the shape
"gt town state migrate copy --cutover" writes. A record the claim does not hold
is removed from it, and the claim is partial. After the record, the empty dispatch
hold file (seat-refill.hold) is written into the claim, so the pod starts held; the
overseer removes it after the day-one checks. Nothing is written to the Mac tree.

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
		args = append(args, e.podPath(a))
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

// podPath is the argv entry the pod sees: the mount path, or a path under it, is
// the town path the pod mounts the claim at. Any other entry passes unchanged.
func (e podEngine) podPath(a string) string {
	if a == townstate.VolumeMount {
		return e.town
	}
	if strings.HasPrefix(a, townstate.VolumeMount+"/") {
		return e.town + a[len(townstate.VolumeMount):]
	}
	return a
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
	// readHost reads the town's settings files on the host for the live copy. Nil
	// means os.ReadFile.
	readHost func(string) ([]byte, error)
	// patrolWritePath names the file the daemon patrol config is written to,
	// which the rehearsal profile rewrites. Production is config.PatrolWritePath.
	patrolWritePath func(string) (string, error)
	engine          townstate.Engine
	probes          migrateProbes
	// now is the command's clock: the cut-over record's time and its copy window.
	// Nil means time.Now.
	now func() time.Time
	// encode writes the cut-over record's bytes. Nil means encodeRecord.
	encode func(any) ([]byte, error)
	out    io.Writer
}

// clock reads the session's clock, time.Now when none is set.
func (env townStateSeedEnv) clock() time.Time {
	if env.now == nil {
		return time.Now()
	}
	return env.now()
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
		readHost:        os.ReadFile,
		engine:          podEngine{namespace: f.namespace, pod: f.pod, claim: seedClaim, town: f.town, run: execKubectl},
		probes:          productionMigrateProbes(),
		now:             time.Now,
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
	// The live copy reads every file it sends before the claim is touched, so a
	// refusal here leaves the claim as it was.
	var cutover seedCutover
	if !env.rehearsal {
		c, err := seedCutoverPrepare(env.town, env.readHost)
		if err != nil {
			return fmt.Errorf("refused: %w", err)
		}
		cutover = c
	}

	m := env.manifest
	if m == nil {
		m = townstate.Default()
	}
	opts := townstate.ExportOptions{Since: env.since, AllowNoRigs: env.allowNoRigs, Exclude: env.exclude}
	// The copy window opens at the first read of the copy, as migrate copy's does.
	start := env.clock()
	sum, err := townstate.SeedVolume(env.engine, seedClaim, env.town, m, opts, townstate.ImportArgv, env.out)
	if err != nil {
		return fmt.Errorf("stream into claim %s: %w", seedClaim, err)
	}
	// The copy window closes when the copy returns, before any verify step.
	copied := env.clock().Sub(start)
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

	// The live copy's settings step runs after the daemon verify, as the rehearsal's
	// does, and its byte check runs before the success line.
	if !env.rehearsal {
		if err := seedCutoverStep(env, cutover); err != nil {
			return err
		}
		if err := seedCutoverRecord(env, copied); err != nil {
			return err
		}
		if err := seedHoldStep(env); err != nil {
			return err
		}
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
// live copy sends its settings in seedCutoverStep, not here.
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
// name is found with an empty sum, so it never matches a host file.
func claimFileSum(eng townstate.Engine, claim, rel string) (string, bool, error) {
	sums, err := claimFileSums(eng, claim, []string{rel})
	if err != nil {
		return "", false, err
	}
	sum, found := sums[rel]
	return sum, found, nil
}

// claimFileSums lists the claim once and returns, for each name in rels the
// listing has, the sha256 of its bytes; a non-regular entry under the name maps
// to an empty sum. A name the listing lacks is absent from the result. The listing
// is read to its end, so a listing that fails part way is an error.
func claimFileSums(eng townstate.Engine, claim string, rels []string) (map[string]string, error) {
	want := make(map[string]bool, len(rels))
	for _, rel := range rels {
		want[rel] = true
	}
	sums := make(map[string]string, len(rels))
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(eng.Run(claim, townstate.ListArgv, nil, pw)) }()
	// A listing still writing when this returns fails on the closed pipe; its own
	// error is never the answer here.
	defer pr.CloseWithError(io.ErrClosedPipe)
	tr := tar.NewReader(pr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return sums, nil
		}
		if err != nil {
			return nil, fmt.Errorf("listing claim %s: %w", claim, err)
		}
		name := path.Clean(hdr.Name)
		if !want[name] {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			sums[name] = ""
			continue
		}
		// The copy is bounded by the entry's own size, and a short read is an error.
		h := sha256.New()
		if _, err := io.CopyN(h, tr, hdr.Size); err != nil {
			return nil, fmt.Errorf("listing claim %s: %w", claim, err)
		}
		sums[name] = hex.EncodeToString(h.Sum(nil))
	}
}

// seedCutoverTownRel is the town's settings file the live copy sends, relative to
// the town root. Its rig counterpart is <rig>/settings/config.json.
var seedCutoverTownRel = path.Join(constants.DirSettings, "config.json")

// seedRigNamePattern is the shape a registered rig name takes in a path the copy
// builds. A name outside it is refused before it names a host or claim file.
var seedRigNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// seedCutover is the live copy's settings step: each file to send, with the bytes
// and their sha256, and the settings files that are absent on the host.
type seedCutover struct {
	files  []seedCutoverFile
	absent []string
}

// seedCutoverFile is one file the live copy writes into the claim. sum is the
// sha256 of data, the bytes the step meant to write, and mode the permission bits
// the claim's copy carries.
type seedCutoverFile struct {
	rel  string
	data []byte
	sum  string
	mode int64
}

// seedCutoverPrepare reads what the live copy's settings step writes, before the
// claim is touched: the town's settings file with its supervisor removed, and each
// registered rig's settings file byte for byte. The host is only read. The town's
// file is required, because a pod without it runs on compiled-in defaults. A rig
// with no settings file is listed in absent, not refused.
func seedCutoverPrepare(town string, read func(string) ([]byte, error)) (seedCutover, error) {
	if read == nil {
		read = os.ReadFile
	}
	var c seedCutover
	data, found, err := seedHostSettings(town, seedCutoverTownRel, read)
	if err != nil {
		return seedCutover{}, err
	}
	if !found {
		return seedCutover{}, fmt.Errorf("the town's settings file %s is not on the host: the pod would run on compiled-in defaults, and no claim is written", seedCutoverTownRel)
	}
	townOut, _, err := townstate.RemoveSupervisor(data)
	if err != nil {
		return seedCutover{}, fmt.Errorf("the town's settings file %s: %w", seedCutoverTownRel, err)
	}
	c.files = append(c.files, newSeedCutoverFile(seedCutoverTownRel, townOut))

	rigs, err := townstate.RegisteredRigs(town)
	if err != nil {
		return seedCutover{}, fmt.Errorf("reading the town's rigs: %w", err)
	}
	for _, rig := range rigs {
		if !seedRigNamePattern.MatchString(rig) {
			return seedCutover{}, fmt.Errorf("registered rig %q is not a plain name, and no claim is written", rig)
		}
		rel := path.Join(rig, constants.DirSettings, "config.json")
		data, found, err := seedHostSettings(town, rel, read)
		if err != nil {
			return seedCutover{}, err
		}
		if !found {
			c.absent = append(c.absent, rel)
			continue
		}
		c.files = append(c.files, newSeedCutoverFile(rel, data))
	}
	return c, nil
}

// newSeedCutoverFile is the file the step sends for rel, with the sha256 of data.
func newSeedCutoverFile(rel string, data []byte) seedCutoverFile {
	sum := sha256.Sum256(data)
	return seedCutoverFile{rel: rel, data: data, sum: hex.EncodeToString(sum[:]), mode: 0o644}
}

// newSeedHoldFile is the empty dispatch hold file the live copy places at rel, mode
// 0600.
func newSeedHoldFile(rel string) seedCutoverFile {
	f := newSeedCutoverFile(rel, nil)
	f.mode = 0o600
	return f
}

// seedHostSettings reads the host file at rel under town. A file that is not there
// is reported as not found; one that is not a regular file is an error.
func seedHostSettings(town, rel string, read func(string) ([]byte, error)) ([]byte, bool, error) {
	p := filepath.Join(town, filepath.FromSlash(rel))
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("the settings file %s cannot be read on the host: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("the settings file %s is not a regular file on the host, and no claim is written", rel)
	}
	data, err := read(p)
	if err != nil {
		return nil, false, fmt.Errorf("reading the host's %s: %w", rel, err)
	}
	return data, true, nil
}

// seedCutoverStep is the live copy's settings step: the prepared files are sent
// into the claim as one tar stream, unpacked by the pod, then the claim is listed
// and each file's sha256 is compared with the bytes sent. A difference, or a
// stream or listing that fails, leaves the claim partial.
func seedCutoverStep(env townStateSeedEnv, c seedCutover) error {
	for _, rel := range c.absent {
		fmt.Fprintf(env.out, "%s is absent on the host: the claim has no file for it\n", rel)
	}
	if err := seedCutoverStream(env, c.files); err != nil {
		return fmt.Errorf("stream settings into claim %s: the claim is partial and not to be used: %w", seedClaim, err)
	}
	mismatches, err := seedCutoverMismatches(env.engine, c.files)
	if err != nil {
		return fmt.Errorf("verifying settings in claim %s: the claim is partial and not to be used: %w", seedClaim, err)
	}
	if problems := printVerifyProblems(env.out, townstate.VerifyResult{Mismatches: mismatches}, nil); problems > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("claim %s does not match the host's settings files (%d problems): the claim is partial and not to be used", seedClaim, problems)}
	}
	fmt.Fprintf(env.out, "settings %d files streamed into claim %s\n", len(c.files), seedClaim)
	return nil
}

// seedCutoverRecord writes the cut-over record into the claim, after the daemon
// verify and the settings verify have both passed. The record is one more file
// through the settings stream, so the host tree is not written. Its cutover_at is
// the time of this write; copied is the copy window, from the copy's first read to
// the copy's return, so the verify steps are not in it. A record that does not reach
// the claim, or a claim that does not hold it afterwards, is removed from the claim.
func seedCutoverRecord(env townStateSeedEnv, copied time.Duration) error {
	at := env.clock()
	encode := encodeRecord
	if env.encode != nil {
		encode = env.encode
	}
	data, err := encode(cutoverRecord{CutoverAt: at.UTC().Format(time.RFC3339), Volume: seedClaim, CopySeconds: copied.Seconds()})
	if err != nil {
		return seedFileUnwind(env, cutoverRecordRel, "cut-over record", fmt.Errorf("encoding the cut-over record: %w", err))
	}
	file := newSeedCutoverFile(cutoverRecordRel, data)
	if err := seedCutoverStream(env, []seedCutoverFile{file}); err != nil {
		return seedFileUnwind(env, cutoverRecordRel, "cut-over record", fmt.Errorf("stream the cut-over record: %w", err))
	}
	mismatches, err := seedCutoverMismatches(env.engine, []seedCutoverFile{file})
	if err != nil {
		return seedFileUnwind(env, cutoverRecordRel, "cut-over record", fmt.Errorf("verifying the cut-over record: %w", err))
	}
	if problems := printVerifyProblems(env.out, townstate.VerifyResult{Mismatches: mismatches}, nil); problems > 0 {
		return seedFileUnwind(env, cutoverRecordRel, "cut-over record", fmt.Errorf("the claim does not hold the cut-over record (%d problems)", problems))
	}
	fmt.Fprintf(env.out, "cut-over record written into claim %s\n", seedClaim)
	return nil
}

// seedHoldStep places the dispatch hold file in the claim as the live copy's last
// step, so the pod starts held and its dispatcher does not sling before the day-one
// checks. The file is empty, and its name is dispatch.HoldFileName, the name
// dispatch.HoldFilePath joins to the town root. The overseer removes it after the
// day-one checks. A hold that does not reach the claim, or a claim that does not
// hold it afterwards, is removed from the claim, and the claim is partial.
func seedHoldStep(env townStateSeedEnv) error {
	rel := dispatch.HoldFileName
	file := newSeedHoldFile(rel)
	if err := seedCutoverStream(env, []seedCutoverFile{file}); err != nil {
		return seedFileUnwind(env, rel, "dispatch hold", fmt.Errorf("stream the dispatch hold: %w", err))
	}
	mismatches, err := seedCutoverMismatches(env.engine, []seedCutoverFile{file})
	if err != nil {
		return seedFileUnwind(env, rel, "dispatch hold", fmt.Errorf("verifying the dispatch hold: %w", err))
	}
	if problems := printVerifyProblems(env.out, townstate.VerifyResult{Mismatches: mismatches}, nil); problems > 0 {
		return seedFileUnwind(env, rel, "dispatch hold", fmt.Errorf("the claim does not hold the dispatch hold (%d problems)", problems))
	}
	fmt.Fprintf(env.out, "dispatch hold %s placed in claim %s; the overseer removes it after the day-one checks\n", rel, seedClaim)
	return nil
}

// seedFileUnwind removes the file at rel from the claim and returns cause as the
// claim's partial-copy refusal. what names the file in the removal's error. A
// removal that fails is joined in, never dropped: the claim may still carry the file.
func seedFileUnwind(env townStateSeedEnv, rel, what string, cause error) error {
	err := fmt.Errorf("the claim %s is partial and not to be used: %w", seedClaim, cause)
	if rmErr := townstate.RemoveVolumeFile(env.engine, seedClaim, rel); rmErr != nil {
		return errors.Join(err, fmt.Errorf("removing the %s from claim %s: %w", what, seedClaim, rmErr))
	}
	return err
}

// seedCutoverStream sends the prepared files as one tar stream. Each file's parent
// directories go first, as directory entries, and the pod unpacks the stream with
// the command the rehearsal's settings step uses.
func seedCutoverStream(env townStateSeedEnv, files []seedCutoverFile) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeCutoverTar(pw, files)) }()
	// The pipe is closed on every path: a writer still sending when the exec
	// returns fails on the closed pipe and does not block.
	defer pr.CloseWithError(io.ErrClosedPipe)
	return env.engine.Run(seedClaim, seedSettingsArgv, pr, env.out)
}

// writeCutoverTar writes the prepared files to w as one tar stream. Each file's
// parent directories go first, as directory entries, shallowest first.
func writeCutoverTar(w io.Writer, files []seedCutoverFile) error {
	tw := tar.NewWriter(w)
	made := map[string]bool{}
	for _, f := range files {
		// The walk up the tree finds the deepest missing parent first; the entries are
		// written in reverse so each directory precedes the one under it.
		var missing []string
		for d := path.Dir(f.rel); d != "." && !made[d]; d = path.Dir(d) {
			made[d] = true
			missing = append(missing, d)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			if err := tw.WriteHeader(&tar.Header{Name: missing[i] + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.rel, Typeflag: tar.TypeReg, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	return tw.Close()
}

// seedCutoverMismatches lists the claim once and compares each prepared file's
// sha256 with the sha256 of the bytes the step sent. A file the claim lacks, or
// holds with other bytes, is a mismatch naming its path. A listing that fails is
// an error, never a pass.
func seedCutoverMismatches(eng townstate.Engine, files []seedCutoverFile) ([]townstate.Mismatch, error) {
	rels := make([]string, 0, len(files))
	for _, f := range files {
		rels = append(rels, f.rel)
	}
	sums, err := claimFileSums(eng, seedClaim, rels)
	if err != nil {
		return nil, err
	}
	var mismatches []townstate.Mismatch
	for _, f := range files {
		got, found := sums[f.rel]
		switch {
		case !found:
			mismatches = append(mismatches, townstate.Mismatch{Path: f.rel, Reason: townstate.ReasonMissing})
		case got != f.sum:
			mismatches = append(mismatches, townstate.Mismatch{Path: f.rel, Reason: townstate.ReasonChanged})
		}
	}
	return mismatches, nil
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
