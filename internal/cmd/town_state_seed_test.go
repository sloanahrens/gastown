package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
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
	// calls records each argv the runner was given, from the start.
	calls [][]string
	// profileFails makes the pod's profile command fail.
	profileFails error
	// settingsFails makes the pod's unpack of the settings stream fail after it
	// has read a little of the stream.
	settingsFails error
	// settingsDrop makes the pod discard the settings stream, so the claim never
	// gets the file.
	settingsDrop bool
	// onSettings runs in the pod after the settings file is unpacked, with the
	// pod's directory for the claim.
	onSettings func(dir string)
	// settingsCalls counts the settings streams sent, from the start.
	// failSettingsCall and dropSettingsCall, 1-based, make that numbered stream
	// fail or be discarded; zero never does. The live copy's settings stream is
	// the first, and its cut-over record the second.
	settingsCalls    int
	failSettingsCall int
	dropSettingsCall int
	// onSettingsCall runs after the numbered settings stream is unpacked, with its
	// number and the pod's directory for the claim.
	onSettingsCall func(n int, dir string)
}

func (k *fakeKubectl) run(_ context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) error {
	k.calls = append(k.calls, slices.Clone(args))
	prefix := []string{"exec", "-i", "-n", seedNamespace, seedPod, "--"}
	if len(args) < len(prefix) || !slices.Equal(args[:len(prefix)], prefix) {
		return errors.New("fake kubectl: unexpected exec argv")
	}
	argv := slices.Clone(args[len(prefix):])
	// The pod has the claim only at its town path. A mount-path argument the
	// engine did not map is one this pod cannot see, so it fails here as it
	// would in the pod.
	for _, a := range argv {
		if a == townstate.VolumeMount || strings.HasPrefix(a, townstate.VolumeMount+"/") {
			return fmt.Errorf("fake pod: %s is not mounted in the pod; the claim is at %s", a, k.town)
		}
	}
	// Map the pod's town path back to the mount the fake volume answers to.
	for i, a := range argv {
		if a == k.town || strings.HasPrefix(a, k.town+"/") {
			argv[i] = townstate.VolumeMount + a[len(k.town):]
		}
	}
	if slices.Equal(argv, seedSettingsArgv) {
		return k.unpackSettings(stdin)
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

// unpackSettings is the pod's answer to the settings stream: the stream is unpacked
// under the fake volume's directory, unless a fault or a drop is set.
func (k *fakeKubectl) unpackSettings(stdin io.Reader) error {
	k.settingsCalls++
	if k.settingsCalls == k.failSettingsCall {
		if _, err := io.CopyN(io.Discard, stdin, 10); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return errors.New("pod exec dropped")
	}
	if k.settingsCalls == k.dropSettingsCall {
		_, err := io.Copy(io.Discard, stdin)
		return err
	}
	if k.settingsFails != nil {
		if _, err := io.CopyN(io.Discard, stdin, 10); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return k.settingsFails
	}
	if k.settingsDrop {
		_, err := io.Copy(io.Discard, stdin)
		return err
	}
	if err := podUnpack(k.fv.dir, stdin); err != nil {
		return err
	}
	if k.onSettings != nil {
		k.onSettings(k.fv.dir)
	}
	if k.onSettingsCall != nil {
		k.onSettingsCall(k.settingsCalls, k.fv.dir)
	}
	return nil
}

// podUnpack is the pod's "tar -x -f - -C <mount>", run in-process over the fake
// volume's directory: each directory and regular file of the stream is written
// under dir, and a name that is absolute or climbs out of dir is refused.
func podUnpack(dir string, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("fake tar: refused entry %q", hdr.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return err
			}
		default:
			return fmt.Errorf("fake tar: refused entry %q of type %c", hdr.Name, hdr.Typeflag)
		}
	}
}

// seedSession is one seed run's fixtures: the env it runs with, the fake pod's
// volume and runner, and the output.
type seedSession struct {
	env  townStateSeedEnv
	fv   *fakeVolume
	kube *fakeKubectl
	out  *bytes.Buffer
}

// newSeedSession is a live-shaped session over town with a fake pod whose claim
// is empty. The split is the town's own default, which names settings shared, so
// a rehearsal's settings file reaches the claim only through its own step. The
// town has no rigs registered, so the copy allows it.
func newSeedSession(t *testing.T, town string) seedSession {
	t.Helper()
	m := townstate.Default()
	// The pod's import checks each entry against the split the pod was given, the
	// same split on the happy and the fault path.
	fv := &fakeVolume{dir: t.TempDir(), exists: true, free: 1 << 30, split: m}
	kube := &fakeKubectl{fv: fv, town: town}
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
			now:             seedClock(),
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
	t.Run("a settings stream that fails says the claim is partial and stops before the profile", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.env.rehearsal = true
		s.kube.settingsFails = errors.New("exec refused")
		err := runTownStateSeedIn(s.env)
		if err == nil || !strings.Contains(err.Error(), partial) || !strings.Contains(err.Error(), notUsed) {
			t.Fatalf("seed = %v, want the claim named partial", err)
		}
		if i := slices.IndexFunc(s.kube.calls, func(a []string) bool { return podArgvIs(a, seedProfileArgv) }); i >= 0 {
			t.Errorf("the profile ran after a failed settings stream: %q", s.kube.calls[i])
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

// flagPathSession is the production flag path over the fake runner: the session
// townStateSeedEnvFromFlags builds from the flags, with only the pod engine
// replaced by one that execs into the fake pod.
func flagPathSession(t *testing.T, town string, rehearsal bool) (townStateSeedEnv, *fakeVolume, *fakeKubectl) {
	t.Helper()
	secrets := filepath.Join(t.TempDir(), "secrets.yaml")
	writeProfileFile(t, secrets, secretsYAML)
	// allow-no-rigs lets the export pass its registry check, so only the settings
	// step differs from the stream the other tests run.
	env, err := townStateSeedEnvFromFlags(seedFlagCommand(seedFlagValues{
		town: town, namespace: seedNamespace, pod: seedPod, secrets: secrets, rehearsal: rehearsal, allowNoRigs: true,
	}, ""))
	if err != nil {
		t.Fatalf("townStateSeedEnvFromFlags: %v", err)
	}
	fv := &fakeVolume{dir: t.TempDir(), exists: true, free: 1 << 30}
	kube := &fakeKubectl{fv: fv, town: town}
	env.engine = podEngine{namespace: seedNamespace, pod: seedPod, claim: seedClaim, town: town, run: kube.run}
	env.now = seedClock()
	return env, fv, kube
}

// seedClock is a clock that moves two seconds each time it is read, so a copy
// window that reads it twice has a positive length and no test sleeps.
func seedClock() func() time.Time {
	at := time.Date(2026, 11, 9, 6, 40, 0, 0, time.UTC)
	return func() time.Time {
		at = at.Add(2 * time.Second)
		return at
	}
}

// townTreeSnapshot reads every regular file under town, by slash path, so a test
// can show a run left the host tree as it found it.
func townTreeSnapshot(t *testing.T, town string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(town, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(town, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("reading the host town %s: %v", town, err)
	}
	return files
}

// TestTownStateSeedRehearsalCopiesAndProfilesSettingsThroughTheFlagPath: the
// production flag path, over the town's own split, copies the settings file in its
// own step after the daemon stream, then profiles it. The success line comes after
// the profile, and the split still names settings shared.
func TestTownStateSeedRehearsalCopiesAndProfilesSettingsThroughTheFlagPath(t *testing.T) {
	t.Parallel()
	town := seedTown(t)
	hostBefore := hostFileText(t, town, "settings/config.json")
	env, fv, kube := flagPathSession(t, town, true)
	var out bytes.Buffer
	env.out = &out
	var copied string
	kube.onSettings = func(dir string) {
		data, err := os.ReadFile(filepath.Join(dir, "settings", "config.json"))
		if err != nil {
			t.Errorf("the settings file is not in the claim after its step: %v", err)
		}
		copied = string(data)
	}
	if err := runTownStateSeedIn(env); err != nil {
		t.Fatalf("rehearsal seed: %v\n%s", err, out.String())
	}
	if copied != hostBefore {
		t.Errorf("the claim's settings file before the profile differs from the host's:\n%s", copied)
	}
	if got := readJSONFile(t, filepath.Join(fv.dir, "settings", "config.json"))["daemon"].(map[string]any)["patrols"].(map[string]any)["landing_worker"].(map[string]any)["enabled"]; got != false {
		t.Errorf("the claim's landing_worker patrol is %v after the profile, want false", got)
	}

	settingsAt := slices.IndexFunc(kube.calls, func(a []string) bool { return podArgvIs(a, withTown(seedSettingsArgv, town)) })
	profileAt := slices.IndexFunc(kube.calls, func(a []string) bool { return podArgvIs(a, withTown(seedProfileArgv, town)) })
	if settingsAt < 0 || profileAt < 0 || settingsAt > profileAt {
		t.Fatalf("pod commands %q: want the settings step before the profile", kube.calls)
	}
	text := out.String()
	streamed := strings.Index(text, "settings settings/config.json streamed into claim")
	profiled := strings.Index(text, "profiled settings/config.json")
	success := strings.Index(text, "matches the host copy set")
	if streamed < 0 || profiled < 0 || success < 0 || !(streamed < profiled && profiled < success) {
		t.Errorf("output order is wrong, want streamed, then profiled, then the success line:\n%s", text)
	}
	if !strings.Contains(text, "the profile changed the claim") {
		t.Errorf("success line does not say the profile changed the claim:\n%s", text)
	}
	if row, ok := townstate.Lookup(townstate.Default().Entries, "settings/config.json"); !ok || row.Owner != townstate.OwnerShared {
		t.Errorf("the town's split no longer names settings shared (row %+v)", row)
	}
}

// TestTownStateSeedRehearsalRefusesAnUnreadableSettingsFile: a rehearsal whose
// settings file is missing, or is a link on the host, is refused before any stream,
// so the claim is left as it was.
func TestTownStateSeedRehearsalRefusesAnUnreadableSettingsFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, want string
		setup      func(t *testing.T, town string)
	}{
		{
			name: "a settings file the host does not have",
			want: "cannot be read on the host",
			setup: func(t *testing.T, town string) {
				if err := os.Remove(filepath.Join(town, "settings", "config.json")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a settings file that is a link on the host",
			want: "is not a regular file on the host",
			setup: func(t *testing.T, town string) {
				target := filepath.Join(t.TempDir(), "elsewhere.json")
				writeProfileFile(t, target, profileTestSettings)
				settings := filepath.Join(town, "settings", "config.json")
				if err := os.Remove(settings); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, settings); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := seedTown(t)
			tc.setup(t, town)
			s := newSeedSession(t, town)
			s.env.rehearsal = true
			err := runTownStateSeedIn(s.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("seed = %v, want the refusal %q", err, tc.want)
			}
			if len(s.kube.calls) != 0 {
				t.Errorf("a refused rehearsal ran %d pod commands; the claim must be left as it was", len(s.kube.calls))
			}
		})
	}
}

// TestTownStateSeedRehearsalSettingsCompareFailureIsPartial: a settings compare that
// cannot be made, because the host file is gone or the claim's listing fails, is a
// partial claim, never a pass, and the profile does not run.
func TestTownStateSeedRehearsalSettingsCompareFailureIsPartial(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, s *seedSession)
	}{
		{
			name: "a host settings file removed before the compare says the claim is partial",
			setup: func(t *testing.T, s *seedSession) {
				town := s.env.town
				s.kube.onSettings = func(string) {
					if err := os.Remove(filepath.Join(town, "settings", "config.json")); err != nil {
						t.Errorf("removing the host settings file: %v", err)
					}
				}
			},
		},
		{
			name: "a claim listing that fails at the settings compare says the claim is partial",
			setup: func(t *testing.T, s *seedSession) {
				// The first listing is the daemon verify; the second is the compare.
				s.fv.failListFrom = 2
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSeedSession(t, seedTown(t))
			s.env.rehearsal = true
			tc.setup(t, &s)
			err := runTownStateSeedIn(s.env)
			if err == nil || !strings.Contains(err.Error(), "verifying settings in claim town-state") || !strings.Contains(err.Error(), "partial and not to be used") {
				t.Fatalf("seed = %v, want the settings compare named partial", err)
			}
			if i := slices.IndexFunc(s.kube.calls, func(a []string) bool { return podArgvIs(a, seedProfileArgv) }); i >= 0 {
				t.Errorf("the profile ran after an unmade compare: %q", s.kube.calls[i])
			}
			if strings.Contains(s.out.String(), "profiled ") {
				t.Errorf("an unmade compare was reported as profiled:\n%s", s.out.String())
			}
		})
	}
}

// TestTownStateSeedRehearsalSettingsDifferenceFails: a settings file the claim lacks,
// or holds with other bytes when the compare runs, fails the seed by name before the
// profile, so the profile never runs on a claim the compare has refused.
func TestTownStateSeedRehearsalSettingsDifferenceFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, s *seedSession)
		want  string
	}{
		{
			name:  "a settings file the stream never delivers is missing",
			setup: func(t *testing.T, s *seedSession) { s.kube.settingsDrop = true },
			want:  "missing settings/config.json",
		},
		{
			name: "a settings file the pod changes after the unpack is changed",
			setup: func(t *testing.T, s *seedSession) {
				s.kube.onSettings = func(dir string) {
					if err := os.WriteFile(filepath.Join(dir, "settings", "config.json"), []byte("edited in the pod\n"), 0o644); err != nil {
						t.Errorf("editing the pod's settings: %v", err)
					}
				}
			},
			want: "changed settings/config.json",
		},
		{
			name: "a settings path the pod turns into a directory is changed",
			setup: func(t *testing.T, s *seedSession) {
				s.kube.onSettings = func(dir string) {
					file := filepath.Join(dir, "settings", "config.json")
					if err := os.Remove(file); err != nil {
						t.Errorf("removing the pod's settings file: %v", err)
					}
					if err := os.Mkdir(file, 0o755); err != nil {
						t.Errorf("making the pod's settings path a directory: %v", err)
					}
				}
			},
			want: "changed settings/config.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSeedSession(t, seedTown(t))
			s.env.rehearsal = true
			tc.setup(t, &s)
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
			if i := slices.IndexFunc(s.kube.calls, func(a []string) bool { return podArgvIs(a, seedProfileArgv) }); i >= 0 {
				t.Errorf("the profile ran on a claim the compare refused: %q", s.kube.calls[i])
			}
			if strings.Contains(s.out.String(), "profiled ") {
				t.Errorf("a refused settings file was reported as profiled:\n%s", s.out.String())
			}
		})
	}
}

// TestTownStateSeedReportsWhetherTheProfileChangedTheClaim: the success line comes
// after the profile and says whether the profile changed the claim; a profile that
// fails prints no success line.
func TestTownStateSeedReportsWhetherTheProfileChangedTheClaim(t *testing.T) {
	t.Parallel()
	t.Run("a profile that writes the settings says it changed the claim, after the profile", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.env.rehearsal = true
		if err := runTownStateSeedIn(s.env); err != nil {
			t.Fatalf("rehearsal seed: %v\n%s", err, s.out.String())
		}
		out := s.out.String()
		if !strings.Contains(out, "matches the host copy set") || !strings.Contains(out, "the profile changed the claim") {
			t.Errorf("success line does not say the profile changed the claim:\n%s", out)
		}
		if strings.Index(out, "profiled settings/config.json") > strings.Index(out, "matches the host copy set") {
			t.Errorf("the success line comes before the profile:\n%s", out)
		}
	})
	t.Run("a claim that is already profiled says the profile changed nothing", func(t *testing.T) {
		t.Parallel()
		town := seedTown(t)
		profiled, _, err := townstate.RehearsalProfile([]byte(profileTestSettings), profilePatrolKeys())
		if err != nil {
			t.Fatalf("profiling the test settings: %v", err)
		}
		writeProfileFile(t, filepath.Join(town, "settings", "config.json"), string(profiled))
		s := newSeedSession(t, town)
		s.env.rehearsal = true
		if err := runTownStateSeedIn(s.env); err != nil {
			t.Fatalf("rehearsal seed: %v\n%s", err, s.out.String())
		}
		out := s.out.String()
		if !strings.Contains(out, "the profile changed nothing") || strings.Contains(out, "the profile changed the claim") {
			t.Errorf("success line does not say the profile changed nothing:\n%s", out)
		}
	})
	t.Run("a profile that fails prints no success line", func(t *testing.T) {
		t.Parallel()
		s := newSeedSession(t, seedTown(t))
		s.env.rehearsal = true
		s.kube.profileFails = errors.New("exec refused")
		if err := runTownStateSeedIn(s.env); err == nil {
			t.Fatal("a failed profile was reported as a success")
		}
		if strings.Contains(s.out.String(), "matches the host copy set") {
			t.Errorf("a failed profile printed the success line:\n%s", s.out.String())
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

// TestTownStateSeedEngineMapsTheMountPrefix: a path under the mount reaches the pod
// at the town path the claim is mounted at, so a removal of a claim file reaches
// that file in the pod rather than a path the pod does not have.
func TestTownStateSeedEngineMapsTheMountPrefix(t *testing.T) {
	t.Parallel()
	town := seedTown(t)
	kube := &fakeKubectl{fv: &fakeVolume{dir: t.TempDir()}, town: town}
	eng := podEngine{namespace: seedNamespace, pod: seedPod, claim: seedClaim, town: town, run: kube.run}
	if err := townstate.RemoveVolumeFile(eng, seedClaim, cutoverRecordRel); err != nil {
		t.Fatalf("removing the record from the claim: %v", err)
	}
	want := []string{"exec", "-i", "-n", seedNamespace, seedPod, "--", "rm", "-f", "--", town + "/" + cutoverRecordRel}
	if !slices.Equal(kube.calls[0], want) {
		t.Errorf("removal argv = %q, want %q", kube.calls[0], want)
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

// TestTownStateSeedSettingsHashesFailPartly: the settings hashes read each side to
// its end. A host path that is a directory, a claim listing cut short inside the
// settings entry, and a listing that fails outright are each an error, never a hash
// to compare.
func TestTownStateSeedSettingsHashesFailPartly(t *testing.T) {
	t.Parallel()
	t.Run("a host settings path that is a directory cannot be hashed", func(t *testing.T) {
		t.Parallel()
		if _, err := seedHostFileSum(t.TempDir()); err == nil {
			t.Fatal("a directory was hashed as a file")
		}
	})
	t.Run("a claim listing cut short inside the settings entry is an error", func(t *testing.T) {
		t.Parallel()
		content := strings.Repeat("x", 100)
		var full bytes.Buffer
		tw := tar.NewWriter(&full)
		if err := tw.WriteHeader(&tar.Header{Name: "./settings/config.json", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		// The header is 512 bytes; keep ten bytes of the body.
		cut := listingEngine{listing: full.Bytes()[:512+10]}
		if _, _, err := claimFileSum(cut, seedClaim, "settings/config.json"); err == nil {
			t.Fatal("a listing cut short inside the entry was read as a whole file")
		}
	})
	t.Run("a claim listing that fails outright is an error", func(t *testing.T) {
		t.Parallel()
		if _, _, err := claimFileSum(listingEngine{err: errors.New("listing refused")}, seedClaim, "settings/config.json"); err == nil {
			t.Fatal("a failed listing was read as an empty claim")
		}
	})
}

// listingEngine answers the claim listing with the bytes it is given, or with err.
// It answers nothing else.
type listingEngine struct {
	listing []byte
	err     error
}

func (e listingEngine) Run(_ string, argv []string, _ io.Reader, stdout io.Writer) error {
	if !slices.Equal(argv, townstate.ListArgv) {
		return errors.New("listingEngine: unexpected argv")
	}
	if e.err != nil {
		return e.err
	}
	_, err := stdout.Write(e.listing)
	return err
}

func (listingEngine) HasVolume(string) (bool, error) { return false, errors.New("not used") }

func (listingEngine) CreateVolume(string) error { return errors.New("not used") }

// seedFlagValues is what each flag of the seed command is given in a test.
type seedFlagValues struct {
	town, namespace, pod, since, secrets string
	rehearsal, allowNoRigs               bool
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
		c.Flags().Bool("allow-no-rigs", v.allowNoRigs, "")
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

// podArgvIs reports whether a recorded kubectl argv ends with the pod argv, so a
// test finds one pod command whatever the exec prefix.
func podArgvIs(call, argv []string) bool {
	return len(call) >= len(argv) && slices.Equal(call[len(call)-len(argv):], argv)
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

// cutoverSession is a live-copy session over town through the flag path: the
// prerequisites are met, and the output is kept.
func cutoverSession(t *testing.T, town string) (townStateSeedEnv, *fakeVolume, *fakeKubectl, *bytes.Buffer) {
	t.Helper()
	env, fv, kube := flagPathSession(t, town, false)
	env.probes = readyProbes()
	var out bytes.Buffer
	env.out = &out
	return env, fv, kube, &out
}

// cutoverTown is a town whose registry names alpha and beta, whose settings file
// is profileTestSettings (a supervisor and patrols), and whose alpha has a settings
// file. beta has none, so it is listed as absent.
func cutoverTown(t *testing.T) string {
	t.Helper()
	town := seedTown(t)
	writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
	writeProfileFile(t, filepath.Join(town, "alpha", "settings", "config.json"), `{"landing":{"branch":"main"}}`+"\n")
	return town
}

// TestTownStateSeedCutoverSettingsCopiesTownAndRigFiles: a live copy through the
// flag path writes the town's settings file and each registered rig's settings
// file into the claim, and lists the rig without one as absent.
func TestTownStateSeedCutoverSettingsCopiesTownAndRigFiles(t *testing.T) {
	t.Parallel()
	town := cutoverTown(t)
	env, fv, _, out := cutoverSession(t, town)
	if err := runTownStateSeedIn(env); err != nil {
		t.Fatalf("live seed: %v\n%s", err, out.String())
	}
	for _, rel := range []string{"settings/config.json", "alpha/settings/config.json"} {
		if _, err := os.Stat(filepath.Join(fv.dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("the claim has no %s after the live copy: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fv.dir, "beta", "settings", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the claim has a beta settings file, want none: %v", err)
	}
	if !strings.Contains(out.String(), "beta/settings/config.json is absent on the host") {
		t.Errorf("output does not list beta's settings file as absent:\n%s", out.String())
	}
}

// TestTownStateSeedCutoverSettingsRemovesOnlyTheSupervisor: the claim's town file
// has no daemon.supervisor and every patrol the Mac file has; the rig file is
// byte-equal to the Mac's.
func TestTownStateSeedCutoverSettingsRemovesOnlyTheSupervisor(t *testing.T) {
	t.Parallel()
	town := cutoverTown(t)
	env, fv, _, out := cutoverSession(t, town)
	if err := runTownStateSeedIn(env); err != nil {
		t.Fatalf("live seed: %v\n%s", err, out.String())
	}
	macTown := readJSONFile(t, filepath.Join(town, "settings", "config.json"))
	claimTown := readJSONFile(t, filepath.Join(fv.dir, "settings", "config.json"))
	macDaemon := macTown["daemon"].(map[string]any)
	claimDaemon := claimTown["daemon"].(map[string]any)
	if _, has := claimDaemon["supervisor"]; has {
		t.Errorf("the claim's town file still has daemon.supervisor")
	}
	if !reflect.DeepEqual(claimDaemon["patrols"], macDaemon["patrols"]) {
		t.Errorf("the claim's patrols differ from the Mac's:\nclaim %v\nmac   %v", claimDaemon["patrols"], macDaemon["patrols"])
	}
	if got, want := hostFileText(t, fv.dir, "alpha/settings/config.json"), hostFileText(t, town, "alpha/settings/config.json"); got != want {
		t.Errorf("the claim's rig file is not byte-equal to the Mac's:\n%s", got)
	}
	if got, want := hostFileText(t, town, "settings/config.json"), profileTestSettings; got != want {
		t.Errorf("the Mac's town file changed: %q", got)
	}
}

// TestTownStateSeedCutoverSettingsByteMismatchIsPartial: a write the pod corrupts,
// drops or fails exits 1 and names the claim as partial; the Mac files are unchanged.
func TestTownStateSeedCutoverSettingsByteMismatchIsPartial(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(t *testing.T, k *fakeKubectl)
		// exitOne is whether the failure is a byte difference, which exits 1; a
		// stream or listing that fails is an error with no exit code of its own.
		exitOne bool
	}{
		{"town file corrupted in the claim", func(t *testing.T, k *fakeKubectl) {
			k.onSettings = func(dir string) {
				writeProfileFile(t, filepath.Join(dir, "settings", "config.json"), `{"daemon":{}}`)
			}
		}, true},
		{"rig file dropped from the claim", func(t *testing.T, k *fakeKubectl) {
			k.onSettings = func(dir string) {
				if err := os.Remove(filepath.Join(dir, "alpha", "settings", "config.json")); err != nil {
					t.Errorf("removing the claim's rig file: %v", err)
				}
			}
		}, true},
		{"stream fails part way", func(t *testing.T, k *fakeKubectl) {
			k.settingsFails = errors.New("pod exec dropped")
		}, false},
		{"listing fails after the stream", func(t *testing.T, k *fakeKubectl) {
			// The daemon's verify listing has run by the time the stream unpacks, so
			// the failing listing is the cut-over's own.
			k.onSettings = func(string) { k.fv.listErr = errors.New("listing failed") }
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := cutoverTown(t)
			macTown := hostFileText(t, town, "settings/config.json")
			macRig := hostFileText(t, town, "alpha/settings/config.json")
			env, _, kube, out := cutoverSession(t, town)
			tc.mutate(t, kube)
			err := runTownStateSeedIn(env)
			var exitErr *ExitCodeError
			if err == nil || !strings.Contains(err.Error(), "partial") {
				t.Fatalf("live seed = %v, want an error naming the claim as partial\n%s", err, out.String())
			}
			if tc.exitOne && (!errors.As(err, &exitErr) || exitErr.Code != 1) {
				t.Errorf("live seed error is not exit 1: %v", err)
			}
			if got := hostFileText(t, town, "settings/config.json"); got != macTown {
				t.Errorf("the Mac's town file changed")
			}
			if got := hostFileText(t, town, "alpha/settings/config.json"); got != macRig {
				t.Errorf("the Mac's rig file changed")
			}
		})
	}
}

// TestTownStateSeedCutoverRefusesBeforeTheStream: each refusal the live copy's
// settings step makes happens before the stream, so the pod is never called and
// the claim stays empty.
func TestTownStateSeedCutoverRefusesBeforeTheStream(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(t *testing.T, town string)
		reason string
	}{
		{"town settings file missing", func(t *testing.T, town string) {
			if err := os.Remove(filepath.Join(town, "settings", "config.json")); err != nil {
				t.Fatal(err)
			}
		}, "is not on the host"},
		{"town settings file not JSON", func(t *testing.T, town string) {
			writeProfileFile(t, filepath.Join(town, "settings", "config.json"), "{")
		}, "not valid JSON"},
		{"town settings path is a directory", func(t *testing.T, town string) {
			if err := os.Remove(filepath.Join(town, "settings", "config.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(town, "settings", "config.json"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"rig settings path is a directory", func(t *testing.T, town string) {
			if err := os.Remove(filepath.Join(town, "alpha", "settings", "config.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(town, "alpha", "settings", "config.json"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"rig name is not a plain name", func(t *testing.T, town string) {
			writeTownStateFileContent(t, town, "mayor/rigs.json", `{"version":1,"rigs":{"a b":{"git_url":"x"}}}`)
		}, "not a plain name"},
		{"rig registry is not JSON", func(t *testing.T, town string) {
			writeTownStateFileContent(t, town, "mayor/rigs.json", "{")
		}, "reading the town's rigs"},
		{"rig settings directory is a file", func(t *testing.T, town string) {
			if err := os.RemoveAll(filepath.Join(town, "alpha", "settings")); err != nil {
				t.Fatal(err)
			}
			writeProfileFile(t, filepath.Join(town, "alpha", "settings"), "not a directory")
		}, "cannot be read on the host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := cutoverTown(t)
			tc.setup(t, town)
			env, fv, kube, out := cutoverSession(t, town)
			err := runTownStateSeedIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("live seed = %v, want a refusal naming %q\n%s", err, tc.reason, out.String())
			}
			if len(kube.calls) != 0 {
				t.Errorf("the pod was called %d times, want none before a refusal", len(kube.calls))
			}
			entries, err := os.ReadDir(fv.dir)
			if err != nil {
				t.Fatalf("reading the claim: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("the claim was written by a refused run: %d entries", len(entries))
			}
		})
	}
}

// TestTownStateSeedCutoverRecordIsWrittenToTheClaim: a live seed that verifies
// leaves the cut-over record in the claim, in the shape migrate copy --cutover
// writes, and leaves the host town root as it found it.
func TestTownStateSeedCutoverRecordIsWrittenToTheClaim(t *testing.T) {
	t.Parallel()
	town := cutoverTown(t)
	before := townTreeSnapshot(t, town)
	env, fv, _, out := cutoverSession(t, town)
	if err := runTownStateSeedIn(env); err != nil {
		t.Fatalf("live seed: %v\n%s", err, out.String())
	}
	rec := readJSONFile(t, filepath.Join(fv.dir, filepath.FromSlash(cutoverRecordRel)))
	if len(rec) != 3 {
		t.Errorf("cut-over record has %d keys, want cutover_at, volume and copy_seconds: %v", len(rec), rec)
	}
	cutoverAt, _ := rec["cutover_at"].(string)
	if _, err := time.Parse(time.RFC3339, cutoverAt); err != nil || !strings.HasSuffix(cutoverAt, "Z") {
		t.Errorf("cutover_at = %q, want an RFC 3339 UTC time", cutoverAt)
	}
	if rec["volume"] != seedClaim {
		t.Errorf("volume = %v, want the claim %q", rec["volume"], seedClaim)
	}
	if secs, _ := rec["copy_seconds"].(float64); secs <= 0 {
		t.Errorf("copy_seconds = %v, want a positive copy window", rec["copy_seconds"])
	}
	if got := townTreeSnapshot(t, town); !reflect.DeepEqual(got, before) {
		t.Errorf("the live seed changed the host town root")
	}
}

// TestTownStateSeedCutoverRecordFailureUnwinds: a record that does not reach the
// claim, or a claim that does not hold it afterwards, is removed from the claim,
// and the run exits non-zero naming the claim as partial.
func TestTownStateSeedCutoverRecordFailureUnwinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, k *fakeKubectl)
		// want is the part of the error the failing step names.
		want string
	}{
		{"the record stream fails", func(_ *testing.T, k *fakeKubectl) {
			k.failSettingsCall = 2
		}, "stream the cut-over record"},
		{"the claim drops the record", func(_ *testing.T, k *fakeKubectl) {
			k.dropSettingsCall = 2
		}, "does not hold the cut-over record"},
		{"the claim holds other bytes for the record", func(t *testing.T, k *fakeKubectl) {
			// The record lands, and the claim then holds other bytes for it: only a
			// removal from the claim leaves the record gone.
			k.onSettingsCall = func(n int, dir string) {
				if n == 2 {
					writeProfileFile(t, filepath.Join(dir, filepath.FromSlash(cutoverRecordRel)), "{}\n")
				}
			}
		}, "does not hold the cut-over record"},
		{"the claim listing fails after the record", func(_ *testing.T, k *fakeKubectl) {
			// The listing that checks the record is the one after its stream.
			k.onSettingsCall = func(n int, _ string) {
				if n == 2 {
					k.fv.listErr = errors.New("listing failed")
				}
			}
		}, "verifying the cut-over record"},
		{"the removal fails too", func(_ *testing.T, k *fakeKubectl) {
			k.failSettingsCall = 2
			k.fv.rmErr = errors.New("rm refused")
		}, "removing the cut-over record from claim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := cutoverTown(t)
			env, fv, kube, out := cutoverSession(t, town)
			tc.setup(t, kube)
			err := runTownStateSeedIn(env)
			if err == nil || !strings.Contains(err.Error(), "partial") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("live seed = %v, want an error naming claim %s as partial and %q\n%s", err, seedClaim, tc.want, out.String())
			}
			if _, statErr := os.Stat(filepath.Join(fv.dir, filepath.FromSlash(cutoverRecordRel))); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the claim still holds the cut-over record (stat %v)", statErr)
			}
		})
	}
}

// TestTownStateSeedCutoverRecordEncodeFailureIsPartial: a record the encoder
// refuses is never written, and the claim is named partial for it.
func TestTownStateSeedCutoverRecordEncodeFailureIsPartial(t *testing.T) {
	t.Parallel()
	town := cutoverTown(t)
	env, fv, _, out := cutoverSession(t, town)
	env.encode = func(any) ([]byte, error) { return nil, errors.New("encoder refused") }
	err := runTownStateSeedIn(env)
	if err == nil || !strings.Contains(err.Error(), "encoding the cut-over record") || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("live seed = %v, want the claim named partial for the record's encoding\n%s", err, out.String())
	}
	if _, statErr := os.Stat(filepath.Join(fv.dir, filepath.FromSlash(cutoverRecordRel))); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the claim holds a cut-over record (stat %v)", statErr)
	}
}

// TestTownStateSeedRehearsalWritesNoCutoverRecord: a rehearsal seed leaves no
// cut-over record in the claim.
func TestTownStateSeedRehearsalWritesNoCutoverRecord(t *testing.T) {
	t.Parallel()
	town := cutoverTown(t)
	env, fv, _ := flagPathSession(t, town, true)
	var out bytes.Buffer
	env.out = &out
	if err := runTownStateSeedIn(env); err != nil {
		t.Fatalf("rehearsal seed: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(fv.dir, filepath.FromSlash(cutoverRecordRel))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the rehearsal left a cut-over record in the claim (stat %v)", err)
	}
}

// TestTownStateSeedRehearsalSettingsStepUnchanged: a rehearsal still copies and
// profiles its one settings file: one settings stream, one profile, and no rig file
// in the claim.
func TestTownStateSeedRehearsalSettingsStepUnchanged(t *testing.T) {
	t.Parallel()
	town := cutoverTown(t)
	env, fv, kube := flagPathSession(t, town, true)
	var out bytes.Buffer
	env.out = &out
	if err := runTownStateSeedIn(env); err != nil {
		t.Fatalf("rehearsal seed: %v\n%s", err, out.String())
	}
	settings := 0
	profiles := 0
	for _, call := range kube.calls {
		switch {
		case podArgvIs(call, withTown(seedSettingsArgv, town)):
			settings++
		case podArgvIs(call, withTown(seedProfileArgv, town)):
			profiles++
		}
	}
	if settings != 1 || profiles != 1 {
		t.Errorf("pod ran the settings step %d times and the profile %d times, want 1 and 1", settings, profiles)
	}
	if _, err := os.Stat(filepath.Join(fv.dir, "alpha", "settings", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the rehearsal copied a rig settings file: %v", err)
	}
	if strings.Contains(out.String(), "is absent on the host") {
		t.Errorf("the rehearsal printed the live copy's absent-file line:\n%s", out.String())
	}
}

// TestTownStateSeedCutoverRefusesAnUnreadableTownFile: a town settings file that
// is there but cannot be read is refused before the stream, and the claim is not
// touched.
func TestTownStateSeedCutoverRefusesAnUnreadableTownFile(t *testing.T) {
	t.Parallel()
	town := cutoverTown(t)
	env, fv, kube, out := cutoverSession(t, town)
	env.readHost = func(string) ([]byte, error) { return nil, errors.New("read refused") }
	err := runTownStateSeedIn(env)
	if err == nil || !strings.Contains(err.Error(), "reading the host's settings/config.json") {
		t.Fatalf("live seed = %v, want a refusal naming the unreadable file\n%s", err, out.String())
	}
	if len(kube.calls) != 0 {
		t.Errorf("the pod was called %d times, want none before a refusal", len(kube.calls))
	}
	entries, err := os.ReadDir(fv.dir)
	if err != nil {
		t.Fatalf("reading the claim: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the claim was written by a refused run: %d entries", len(entries))
	}
}

// cutoverBudgetWriter accepts budget bytes and then fails, so a write fails at the
// point the budget runs out.
type cutoverBudgetWriter struct{ budget int }

func (w *cutoverBudgetWriter) Write(p []byte) (int, error) {
	if len(p) <= w.budget {
		w.budget -= len(p)
		return len(p), nil
	}
	n := w.budget
	w.budget = 0
	return n, errors.New("write refused")
}

// TestSeedCutoverTarListsParentsBeforeEntries: each file's parent directories are
// in the stream before the file, shallowest first, and a directory two files share
// is written once.
func TestSeedCutoverTarListsParentsBeforeEntries(t *testing.T) {
	t.Parallel()
	files := []seedCutoverFile{
		newSeedCutoverFile("a/b/c/f.json", []byte("{}\n")),
		newSeedCutoverFile("a/b/g.json", []byte("{}\n")),
		newSeedCutoverFile("top.json", []byte("{}\n")),
	}
	var stream bytes.Buffer
	if err := writeCutoverTar(&stream, files); err != nil {
		t.Fatalf("writeCutoverTar: %v", err)
	}
	var names []string
	tr := tar.NewReader(&stream)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		names = append(names, hdr.Name)
	}
	want := []string{"a/", "a/b/", "a/b/c/", "a/b/c/f.json", "a/b/g.json", "top.json"}
	if !slices.Equal(names, want) {
		t.Errorf("stream entries = %q, want %q", names, want)
	}
}

// TestSeedCutoverTarRefusesAFailingWriter: a write that fails anywhere in the
// stream is returned, whichever of the directory header, the file header, the file
// bytes or the closing blocks it fails in.
func TestSeedCutoverTarRefusesAFailingWriter(t *testing.T) {
	t.Parallel()
	files := []seedCutoverFile{newSeedCutoverFile("settings/config.json", []byte(`{"daemon":{}}`+"\n"))}
	var whole bytes.Buffer
	if err := writeCutoverTar(&whole, files); err != nil {
		t.Fatalf("writeCutoverTar to a whole buffer: %v", err)
	}
	// The stream is a directory header (512 bytes), a file header (512), the file
	// bytes, and two closing blocks. Each budget runs out in one of those writes.
	for _, budget := range []int{0, 512, 1024, whole.Len() - 1} {
		if err := writeCutoverTar(&cutoverBudgetWriter{budget: budget}, files); err == nil {
			t.Errorf("budget %d: writeCutoverTar = nil, want the write error", budget)
		}
	}
}
