package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/townstate"
)

// seedNamespace and seedPod are the target a seed test names; the fake pod checks
// the exec argv carries them.
const (
	seedNamespace = "rehearsal"
	seedPod       = "seed-helper"
)

// fakeKubectl is the kubectl runner a seed test injects. It checks the exec argv
// the pod engine built, then runs the command in the fake pod: the import and the
// listing through fakeVolume, and the profile through the real profile command
// over the pod's directory.
type fakeKubectl struct {
	fv   *fakeVolume
	town string
	// manifest is the split the pod's import checks each archive entry against.
	manifest *townstate.Manifest
	// calls records each argv the runner was given, from the start.
	calls [][]string
	// profileFails makes the pod's profile command fail.
	profileFails error
}

func (k *fakeKubectl) run(_ context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) error {
	k.calls = append(k.calls, slices.Clone(args))
	prefix := []string{"exec", "-i", "-n", seedNamespace, seedPod, "--"}
	if len(args) < len(prefix) || !slices.Equal(args[:len(prefix)], prefix) {
		return errors.New("fake kubectl: unexpected exec argv")
	}
	argv := slices.Clone(args[len(prefix):])
	// The pod sees its own town path; map it back to the mount the fake volume
	// answers to.
	for i, a := range argv {
		if a == k.town {
			argv[i] = townstate.VolumeMount
		}
	}
	// The import checks each entry against the split the pod was given, not the
	// default one, which the fake volume's own import would use. A fault is the
	// fake volume's to make.
	if slices.Equal(argv, townstate.ImportArgv) && k.fv.fault == nil {
		s, err := townstate.Import(stdin, k.fv.dir, k.manifest, townstate.ImportOptions{})
		fmt.Fprintln(stdout, s.String())
		return err
	}
	if slices.Equal(argv, seedProfileArgv) {
		if k.profileFails != nil {
			return k.profileFails
		}
		return runTownStateProfileIn(townStateProfileEnv{
			town:        k.fv.dir,
			rehearsal:   true,
			getenv:      inPod,
			files:       hostProfileFiles(),
			readPatrols: daemon.ReadPatrolConfig,
			out:         stdout,
		})
	}
	return k.fv.Run(seedClaim, argv, stdin, stdout)
}

// seedSession is one seed run's fixtures: the env it runs with, the fake pod's
// volume and runner, and the output.
type seedSession struct {
	env  townStateSeedEnv
	fv   *fakeVolume
	kube *fakeKubectl
	out  *bytes.Buffer
}

// seedManifest is the town's default split with the settings row daemon-owned. The
// rehearsal profile rewrites settings/config.json in the claim, so these tests
// make the claim carry that file. The default split names settings shared, so the
// stream leaves it out.
func seedManifest(t *testing.T) *townstate.Manifest {
	t.Helper()
	m := townstate.Default()
	found := false
	for i := range m.Entries {
		if m.Entries[i].Path == "settings" {
			m.Entries[i].Owner = townstate.OwnerDaemon
			found = true
		}
	}
	if !found {
		t.Fatal("the default split has no settings row")
	}
	return m
}

// newSeedSession is a live-shaped session over town with a fake pod whose claim
// is empty. The town has no rigs registered, so the copy allows it.
func newSeedSession(t *testing.T, town string) seedSession {
	t.Helper()
	fv := &fakeVolume{dir: t.TempDir(), exists: true, free: 1 << 30}
	m := seedManifest(t)
	kube := &fakeKubectl{fv: fv, town: town, manifest: m}
	var out bytes.Buffer
	return seedSession{
		env: townStateSeedEnv{
			town:            town,
			namespace:       seedNamespace,
			allowNoRigs:     true,
			exclude:         migrateSecretExclusion(t),
			manifest:        m,
			patrolWritePath: config.PatrolWritePath,
			engine:          podEngine{namespace: seedNamespace, pod: seedPod, claim: seedClaim, town: town, run: kube.run},
			probes:          readyProbes(),
			out:             &out,
		},
		fv:   fv,
		kube: kube,
		out:  &out,
	}
}

// seedTown is migrateTown with a settings file carrying patrols on, so a
// rehearsal has something to profile.
func seedTown(t *testing.T) string {
	t.Helper()
	town := migrateTown(t)
	writeProfileFile(t, filepath.Join(town, "settings", "config.json"), profileTestSettings)
	return town
}

// TestTownStateSeedStreamsThenVerifies: the export reaches the pod's import and the
// verify passes; a changed, a missing and an extra file each fail the verify and
// name their path.
func TestTownStateSeedStreamsThenVerifies(t *testing.T) {
	t.Parallel()
	t.Run("the stream reaches the pod's import and the verify passes", func(t *testing.T) {
		t.Parallel()
		town := seedTown(t)
		s := newSeedSession(t, town)
		if err := runTownStateSeedIn(s.env); err != nil {
			t.Fatalf("seed: %v\n%s", err, s.out.String())
		}
		assertVolumeFile(t, s.fv.dir, "daemon/nested/notes.json", hostFileText(t, town, "daemon/nested/notes.json"))
		if !slices.ContainsFunc(s.kube.calls, func(a []string) bool { return slices.Contains(a, town) && slices.Contains(a, "import") }) {
			t.Errorf("no kubectl exec ran the import against %s: %q", town, s.kube.calls)
		}
		if !strings.Contains(s.out.String(), "claim town-state matches the host copy set") {
			t.Errorf("output does not report the verify passed:\n%s", s.out.String())
		}
		if strings.Contains(s.out.String(), secretContent) {
			t.Errorf("output carries file contents:\n%s", s.out.String())
		}
		if _, err := os.Stat(filepath.Join(s.fv.dir, "daemon", "forgejo-landing.env")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the secret file reached the claim (stat %v)", err)
		}
	})
	// Each divergence is made in the pod after the stream, before the listing.
	cases := []struct {
		name   string
		change func(t *testing.T, dir string)
		want   string
	}{
		{
			name: "a changed file",
			change: func(t *testing.T, dir string) {
				writeHostFile(t, filepath.Join(dir, "daemon", "nested", "notes.json"), "edited in the pod\n", time.Now())
			},
			want: "changed daemon/nested/notes.json",
		},
		{
			name: "a missing file",
			change: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "daemon", "maintenance_state.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: "missing daemon/maintenance_state.json",
		},
		{
			name: "an extra file",
			change: func(t *testing.T, dir string) {
				writeHostFile(t, filepath.Join(dir, "daemon", "late.json"), "x\n", time.Now())
			},
			want: "extra daemon/late.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+" fails the verify and names its path", func(t *testing.T) {
			t.Parallel()
			town := seedTown(t)
			s := newSeedSession(t, town)
			s.fv.onRun = func(argv []string) {
				if slices.Equal(argv, townstate.ListArgv) {
					tc.change(t, s.fv.dir)
				}
			}
			err := runTownStateSeedIn(s.env)
			var exitErr *ExitCodeError
			if !errors.As(err, &exitErr) || exitErr.Code != 1 {
				t.Fatalf("seed = %v, want exit 1\n%s", err, s.out.String())
			}
			if !strings.Contains(s.out.String(), tc.want) {
				t.Errorf("output does not name %q:\n%s", tc.want, s.out.String())
			}
			if !strings.Contains(err.Error(), "partial and not to be used") {
				t.Errorf("error does not say the claim is partial: %v", err)
			}
		})
	}
}

// TestTownStateSeedRehearsalProfilesTheSettings: a rehearsal runs the profile in
// the pod after the stream, and the settings file is reported as profiled, not as
// a difference. The Mac's settings file is not written.
func TestTownStateSeedRehearsalProfilesTheSettings(t *testing.T) {
	t.Parallel()
	town := seedTown(t)
	settingsPath := filepath.Join(town, "settings", "config.json")
	hostBefore := hostFileText(t, town, "settings/config.json")
	s := newSeedSession(t, town)
	s.env.rehearsal = true
	if err := runTownStateSeedIn(s.env); err != nil {
		t.Fatalf("rehearsal seed: %v\n%s", err, s.out.String())
	}
	out := s.out.String()
	if !strings.Contains(out, "profiled settings/config.json") {
		t.Errorf("output does not report the settings as profiled:\n%s", out)
	}
	if strings.Contains(out, "changed settings/config.json") || strings.Contains(out, "missing settings/config.json") {
		t.Errorf("the profiled settings file is reported as a difference:\n%s", out)
	}
	last := s.kube.calls[len(s.kube.calls)-1]
	if !slices.Equal(last[len(last)-len(seedProfileArgv):], withTown(seedProfileArgv, town)) {
		t.Errorf("the last pod command is %q, want the profile after the stream", last)
	}
	podSettings := readJSONFile(t, filepath.Join(s.fv.dir, "settings", "config.json"))
	if got := podSettings["daemon"].(map[string]any)["patrols"].(map[string]any)["landing_worker"].(map[string]any)["enabled"]; got != false {
		t.Errorf("the pod's landing_worker patrol is %v after the profile, want false", got)
	}
	if got := hostFileText(t, town, "settings/config.json"); got != hostBefore {
		t.Errorf("the host settings file changed:\n%s", got)
	}
	if _, err := os.Stat(settingsPath + ".pre-rehearsal"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a rehearsal wrote a backup on the host (stat %v)", err)
	}
}

// TestTownStateSeedRefusesTheLiveNamespace: a rehearsal in namespace gt is refused,
// and a live copy without the hold file or with the daemon container running is
// refused naming which. Each refusal happens before any stream.
func TestTownStateSeedRefusesTheLiveNamespace(t *testing.T) {
	t.Parallel()
	t.Run("a rehearsal in namespace gt is refused before any stream", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.env.rehearsal = true
		s.env.namespace = seedLiveNamespace
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), `namespace "gt"`) {
			t.Fatalf("seed = %v, want the live namespace refused", err)
		}
		if len(s.kube.calls) != 0 {
			t.Errorf("a refused seed ran %d kubectl commands", len(s.kube.calls))
		}
	})
	t.Run("a live copy without the dispatch hold is refused naming it", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.env.probes.holdPresent = func(string) (bool, error) { return false, nil }
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), "dispatch hold missing") {
			t.Fatalf("seed = %v, want the hold named", err)
		}
		if len(s.kube.calls) != 0 {
			t.Errorf("a refused seed ran %d kubectl commands", len(s.kube.calls))
		}
	})
	t.Run("a live copy with the daemon container running is refused naming it", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.env.probes.containerRunning = func(string) (bool, error) { return true, nil }
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), "daemon container running (gt-town-daemon-1)") {
			t.Fatalf("seed = %v, want the running container named", err)
		}
		if len(s.kube.calls) != 0 {
			t.Errorf("a refused seed ran %d kubectl commands", len(s.kube.calls))
		}
	})
	t.Run("a live copy whose container docker cannot find is refused naming it", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.env.probes.containerRunning = func(string) (bool, error) { return false, errDaemonContainerAbsent }
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), `daemon container "gt-town-daemon-1" not found`) {
			t.Fatalf("seed = %v, want the absent container named", err)
		}
		if len(s.kube.calls) != 0 {
			t.Errorf("a refused seed ran %d kubectl commands", len(s.kube.calls))
		}
	})
	t.Run("a rehearsal whose settings cannot be named is refused before any stream", func(t *testing.T) {
		t.Parallel()
		town := seedTown(t)
		writeProfileFile(t, filepath.Join(town, "settings", "config.json"), `{"daemon":`)
		s := newSeedSession(t, town)
		s.env.rehearsal = true
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), "naming the settings") {
			t.Fatalf("seed = %v, want the settings refusal", err)
		}
		if len(s.kube.calls) != 0 {
			t.Errorf("a refused seed ran %d kubectl commands", len(s.kube.calls))
		}
	})
	t.Run("a rehearsal whose settings path lies outside the town is refused before any stream", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.env.rehearsal = true
		s.env.patrolWritePath = func(string) (string, error) { return "/elsewhere/daemon.json", nil }
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), "not under the town root") {
			t.Fatalf("seed = %v, want the settings path refused", err)
		}
		if len(s.kube.calls) != 0 {
			t.Errorf("a refused seed ran %d kubectl commands", len(s.kube.calls))
		}
	})
	t.Run("a live copy with both prerequisites met streams", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		if err := runTownStateSeedIn(s.env); err != nil {
			t.Fatalf("live seed: %v\n%s", err, s.out.String())
		}
		if len(s.kube.calls) == 0 {
			t.Error("a live copy with both prerequisites met ran no kubectl command")
		}
	})
}

// TestTownStateSeedPartialStreamIsReported: an import that ends early, an import
// that fails and a rehearsal profile that fails each exit non-zero and say the
// claim is partial and not to be used.
func TestTownStateSeedPartialStreamIsReported(t *testing.T) {
	t.Parallel()
	const partial = "partial"
	const notUsed = "not to be used"
	t.Run("an import that ends early says the claim is partial", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.fv.fault = &importFault{after: 10, fail: false}
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), partial) || !strings.Contains(err.Error(), notUsed) {
			t.Fatalf("seed = %v, want the claim named partial", err)
		}
	})
	t.Run("a failing exec says the claim is partial", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.fv.fault = &importFault{after: 10, fail: true}
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), partial) || !strings.Contains(err.Error(), notUsed) {
			t.Fatalf("seed = %v, want the claim named partial", err)
		}
	})
	t.Run("a rehearsal profile that fails says the claim is partial", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.env.rehearsal = true
		s.kube.profileFails = errors.New("exec refused")
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), partial) || !strings.Contains(err.Error(), notUsed) {
			t.Fatalf("seed = %v, want the claim named partial", err)
		}
		if strings.Contains(s.out.String(), "profiled ") {
			t.Errorf("a failed profile was reported as profiled:\n%s", s.out.String())
		}
	})
}

// TestTownStateSeedRefusesAnUnusableTarget: a namespace, a pod name or a town root
// that cannot be used in the kubectl argv or as the mount path is refused.
func TestTownStateSeedRefusesAnUnusableTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, namespace, pod, town, want string
	}{
		{name: "a namespace that reads as a flag", namespace: "--rm", pod: seedPod, town: "/Users/sloan/gt", want: "--namespace"},
		{name: "an upper-case namespace", namespace: "Rehearsal", pod: seedPod, town: "/Users/sloan/gt", want: "--namespace"},
		{name: "an empty pod name", namespace: seedNamespace, pod: "", town: "/Users/sloan/gt", want: "--pod"},
		{name: "a pod name with a slash", namespace: seedNamespace, pod: "a/b", town: "/Users/sloan/gt", want: "--pod"},
		{name: "a relative town root", namespace: seedNamespace, pod: seedPod, town: "gt", want: "absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validSeedTarget(tc.namespace, tc.pod, tc.town)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validSeedTarget = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}

// TestTownStateSeedEngineNamesOnlyItsClaim: the pod engine answers for the claim
// the pod mounts and runs its argv with the claim's mount path, and refuses any
// other volume before it execs anything.
func TestTownStateSeedEngineNamesOnlyItsClaim(t *testing.T) {
	t.Parallel()
	town := seedTown(t)
	kube := &fakeKubectl{fv: &fakeVolume{dir: t.TempDir()}, town: town}
	eng := podEngine{namespace: seedNamespace, pod: seedPod, claim: seedClaim, town: town, run: kube.run}
	if err := eng.Run("gt-state", townstate.ListArgv, nil, io.Discard); err == nil {
		t.Fatal("a volume other than the claim was run")
	}
	if len(kube.calls) != 0 {
		t.Errorf("a refused volume ran %d kubectl commands", len(kube.calls))
	}
	if err := eng.Run(seedClaim, townstate.ListArgv, nil, io.Discard); err != nil {
		t.Fatalf("listing the claim: %v", err)
	}
	want := []string{"exec", "-i", "-n", seedNamespace, seedPod, "--", "tar", "-c", "-C", town, "."}
	if !slices.Equal(kube.calls[0], want) {
		t.Errorf("listing argv = %q, want %q", kube.calls[0], want)
	}
}

// TestTownStateSeedListingFailureIsReported: a claim listing that fails leaves the
// verify unmade, and the seed says so as partial rather than passing.
func TestTownStateSeedListingFailureIsReported(t *testing.T) {
	t.Parallel()
	s := newSeedSession(t, seedTown(t))
	s.fv.listErr = errors.New("listing refused")
	err := runTownStateSeedIn(s.env)
	if err == nil || !strings.Contains(err.Error(), "verifying claim town-state") || !strings.Contains(err.Error(), "partial and not to be used") {
		t.Fatalf("seed = %v, want the verify named partial", err)
	}
	if strings.Contains(s.out.String(), "matches the host copy set") {
		t.Errorf("a failed listing was reported as a pass:\n%s", s.out.String())
	}
}

// seedFlagValues is what each flag of the seed command is given in a test.
type seedFlagValues struct {
	town, namespace, pod, since, secrets string
	rehearsal                            bool
}

// seedFlagCommand is a command with every flag the seed reads, each given its
// value from v. A flag named in mistyped is defined with the other type.
func seedFlagCommand(v seedFlagValues, mistyped string) *cobra.Command {
	c := &cobra.Command{Use: "seed"}
	strs := map[string]string{"town": v.town, "namespace": v.namespace, "pod": v.pod, "since": v.since, "secrets-manifest": v.secrets}
	for _, name := range []string{"town", "namespace", "pod", "since", "secrets-manifest"} {
		if name == mistyped {
			c.Flags().Bool(name, true, "")
			continue
		}
		c.Flags().String(name, strs[name], "")
	}
	if mistyped == "rehearsal" {
		c.Flags().String("rehearsal", "", "")
	} else {
		c.Flags().Bool("rehearsal", v.rehearsal, "")
	}
	if mistyped == "allow-no-rigs" {
		c.Flags().String("allow-no-rigs", "", "")
	} else {
		c.Flags().Bool("allow-no-rigs", false, "")
	}
	return c
}

// TestTownStateSeedReadsItsFlags: a command whose flags are valid builds the
// session they name, with --since parsed.
func TestTownStateSeedReadsItsFlags(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	secrets := filepath.Join(t.TempDir(), "secrets.yaml")
	writeProfileFile(t, secrets, secretsYAML)
	env, err := townStateSeedEnvFromFlags(seedFlagCommand(seedFlagValues{
		town: town, namespace: seedNamespace, pod: seedPod, since: "2026-11-06T14:30:00Z", secrets: secrets, rehearsal: true,
	}, ""))
	if err != nil {
		t.Fatalf("townStateSeedEnvFromFlags: %v", err)
	}
	if env.town != town || env.namespace != seedNamespace || !env.rehearsal {
		t.Errorf("session = town %q namespace %q rehearsal %v, want the flags' values", env.town, env.namespace, env.rehearsal)
	}
	if want := time.Date(2026, 11, 6, 14, 30, 0, 0, time.UTC); !env.since.Equal(want) {
		t.Errorf("since = %v, want %v", env.since, want)
	}
	if env.exclude == nil || env.engine == nil || env.patrolWritePath == nil {
		t.Error("the session is missing its exclusion, engine or settings path")
	}
}

// TestTownStateSeedRefusesABadFlag: a flag value or a flag of the wrong type that
// the seed cannot use is refused before anything is copied.
func TestTownStateSeedRefusesABadFlag(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	secrets := filepath.Join(t.TempDir(), "secrets.yaml")
	writeProfileFile(t, secrets, secretsYAML)
	valid := seedFlagValues{town: town, namespace: seedNamespace, pod: seedPod, secrets: secrets}
	with := func(f func(v *seedFlagValues)) seedFlagValues {
		v := valid
		f(&v)
		return v
	}
	cases := []struct {
		name     string
		v        seedFlagValues
		mistyped string
		want     string
	}{
		{name: "a namespace that reads as a flag", v: with(func(v *seedFlagValues) { v.namespace = "--rm" }), want: "--namespace"},
		{name: "a pod name with a slash", v: with(func(v *seedFlagValues) { v.pod = "a/b" }), want: "--pod"},
		{name: "a relative town root", v: with(func(v *seedFlagValues) { v.town = "gt" }), want: "absolute path"},
		{name: "a since that is not RFC 3339", v: with(func(v *seedFlagValues) { v.since = "yesterday" }), want: "RFC 3339"},
		{name: "a secrets manifest that is missing", v: with(func(v *seedFlagValues) { v.secrets = filepath.Join(t.TempDir(), "missing.yaml") }), want: "reading the secrets manifest"},
		{name: "a secrets manifest that does not parse", v: with(func(v *seedFlagValues) {
			v.secrets = filepath.Join(t.TempDir(), "bad.yaml")
			writeProfileFile(t, v.secrets, "files: [\n")
		})},
		{name: "a string flag of the other type", v: valid, mistyped: "town", want: "reading --town"},
		{name: "a bool flag of the other type", v: valid, mistyped: "rehearsal", want: "reading --rehearsal"},
		{name: "a bool flag allow-no-rigs of the other type", v: valid, mistyped: "allow-no-rigs", want: "reading --allow-no-rigs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := townStateSeedEnvFromFlags(seedFlagCommand(tc.v, tc.mistyped))
			if err == nil || (tc.want != "" && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("townStateSeedEnvFromFlags = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// TestTownStateSeedCommandRefusesBeforeAnyExec: the command's run stops at a flag
// it cannot read, before it builds a session, so no kubectl is started.
func TestTownStateSeedCommandRefusesBeforeAnyExec(t *testing.T) {
	t.Parallel()
	err := runTownStateSeed(seedFlagCommand(seedFlagValues{}, "town"), nil)
	if err == nil || !strings.Contains(err.Error(), "reading --town") {
		t.Fatalf("runTownStateSeed = %v, want the unreadable --town named", err)
	}
}

// TestTownStateSeedEngineStopsAHungExec: an exec that outlives its bound is
// stopped, and the error says the pod may still be running it.
func TestTownStateSeedEngineStopsAHungExec(t *testing.T) {
	t.Parallel()
	hang := func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}
	eng := podEngine{namespace: seedNamespace, pod: seedPod, claim: seedClaim, town: t.TempDir(), run: hang, timeout: time.Millisecond}
	err := eng.Run(seedClaim, townstate.ListArgv, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "did not finish within") || !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("Run = %v, want the hung exec named", err)
	}
}

// TestTownStateSeedEngineLeavesTheClaimToThePod: the seed never asks the engine to
// look up or create the claim, and if it did the answer is an error, never a pass.
func TestTownStateSeedEngineLeavesTheClaimToThePod(t *testing.T) {
	t.Parallel()
	eng := podEngine{namespace: seedNamespace, pod: seedPod, claim: seedClaim}
	if _, err := eng.HasVolume(seedClaim); err == nil {
		t.Error("HasVolume answered for a claim the engine does not manage")
	}
	if err := eng.CreateVolume(seedClaim); err == nil {
		t.Error("CreateVolume created a claim the engine does not manage")
	}
}

// withTown returns argv with each townstate.VolumeMount replaced by town, the
// argv the pod engine runs.
func withTown(argv []string, town string) []string {
	out := slices.Clone(argv)
	for i, a := range out {
		if a == townstate.VolumeMount {
			out[i] = town
		}
	}
	return out
}

// readJSONFile decodes a JSON object file, failing the test when it cannot.
func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return doc
}
