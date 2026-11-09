package cmd

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townstate"
)

// secretsYAML is a secrets manifest with one secret file the copy must leave out.
const secretsYAML = `files:
  - name: forgejo-landing.env
    consumer: "test: the landing bot's token"
    mode: "0600"
    required: true
`

// secretContent is written into the secret file so a test can prove no output
// line carries file contents.
const secretContent = "SECRET-CONTENT-7f3a\n"

// fakeVolume is a townstate.Engine over a directory standing in for the volume.
// The import runs the real townstate.Import into the directory and the listing
// tars the directory, so the copy and the verify are the production code on
// both sides. fault makes the import stop early, with or without an error.
type fakeVolume struct {
	dir    string
	exists bool
	free   int64
	bdFail bool
	fault  *importFault
	calls  []string
	// hasErr, listErr and freeErr make the volume's existence check, listing and
	// df fail, so each command's read of the volume has a failure to refuse.
	hasErr  error
	listErr error
	freeErr error
	// created names the volumes a copy created, and createErr makes the create fail.
	created   []string
	createErr error
	// afterMerge runs on the volume's directory after a merge import, so a test
	// can change the volume between the cut-over send and its second verify.
	afterMerge func(dir string)
	// onRun runs before every helper run, with its argv, so a test can change the
	// host between the steps of a copy. onList runs at each volume listing with
	// the listing's number, counted from the fake's start. failListFrom makes the
	// listing from that number on fail (zero: never).
	onRun        func(argv []string)
	onList       func(n int)
	failListFrom int
	lists        int
}

type importFault struct {
	after int64
	fail  bool
}

func (f *fakeVolume) Run(volume string, argv []string, stdin io.Reader, stdout io.Writer) error {
	f.calls = append(f.calls, strings.Join(argv, " "))
	if f.onRun != nil {
		f.onRun(argv)
	}
	if strings.Join(argv, " ") == strings.Join(townstate.ListArgv, " ") {
		f.lists++
		if f.onList != nil {
			f.onList(f.lists)
		}
		if f.failListFrom > 0 && f.lists >= f.failListFrom {
			return errors.New("the volume listing failed")
		}
	}
	switch strings.Join(argv, " ") {
	case strings.Join(townstate.ImportArgv, " "):
		if f.fault != nil {
			if _, err := io.CopyN(io.Discard, stdin, f.fault.after); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if f.fault.fail {
				return errors.New("import helper exited 1")
			}
			return nil
		}
		s, err := townstate.Import(stdin, f.dir, townstate.Default(), townstate.ImportOptions{})
		fmt.Fprintln(stdout, s.String())
		return err
	case strings.Join(townstate.ImportMergeArgv, " "):
		if f.fault != nil {
			if _, err := io.CopyN(io.Discard, stdin, f.fault.after); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if f.fault.fail {
				return errors.New("import helper exited 1")
			}
			return nil
		}
		s, err := townstate.Import(stdin, f.dir, townstate.Default(), townstate.ImportOptions{MergeNewer: true})
		fmt.Fprintln(stdout, s.String())
		if f.afterMerge != nil {
			f.afterMerge(f.dir)
		}
		return err
	case strings.Join(townstate.ListArgv, " "):
		if f.listErr != nil {
			return f.listErr
		}
		return tarTree(f.dir, stdout)
	case strings.Join(townstate.FreeArgv, " "):
		if f.freeErr != nil {
			return f.freeErr
		}
		fmt.Fprintf(stdout, "Filesystem 1024-blocks Used Available Capacity Mounted on\nvolume 1000 0 %d 0%% %s\n", f.free/1024, townstate.VolumeMount)
		return nil
	case strings.Join(townstate.BeadsReadArgv, " "):
		if f.bdFail {
			return errors.New("bd exited 1")
		}
		fmt.Fprintln(stdout, "gt-1 open task")
		return nil
	}
	return fmt.Errorf("fakeVolume: no answer for %q", argv)
}

func (f *fakeVolume) HasVolume(string) (bool, error) {
	if f.hasErr != nil {
		return false, f.hasErr
	}
	return f.exists, nil
}

func (f *fakeVolume) CreateVolume(volume string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, volume)
	f.exists = true
	return nil
}

// tarTree writes the directory's tree as a tar archive named the way tar -C
// writes it: "./" for the root, then each entry below it.
func tarTree(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := "./" + filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			return tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o755, Typeflag: tar.TypeDir})
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// migrateTown builds a town with two daemon-owned files and one secret file.
func migrateTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	writeTownStateFile(t, town, "daemon/maintenance_state.json")
	writeTownStateFile(t, town, "daemon/nested/notes.json")
	if err := os.WriteFile(filepath.Join(town, "daemon", "forgejo-landing.env"), []byte(secretContent), 0o600); err != nil {
		t.Fatal(err)
	}
	return town
}

// migrateSecretExclusion is the exclusion the production command builds from the
// secrets manifest.
func migrateSecretExclusion(t *testing.T) func(string) bool {
	t.Helper()
	exclude, err := secretExclusion([]byte(secretsYAML))
	if err != nil {
		t.Fatalf("secretExclusion: %v", err)
	}
	return exclude
}

// readyProbes are the cut-over prerequisites all met.
func readyProbes() migrateProbes {
	return migrateProbes{
		holdPresent:       func(string) (bool, error) { return true, nil },
		daemonContainer:   func(string) (string, error) { return "gt-town-daemon-1", nil },
		containerRunning:  func(string) (bool, error) { return false, nil },
		landingQueue:      func(string) (int, error) { return 0, nil },
		landingCandidates: func(string) ([]string, error) { return nil, nil },
		postLandPending:   func(string) ([]string, error) { return nil, nil },
	}
}

// newMigrateEnv is a session over town and a fake volume whose directory is
// empty. The clock advances two seconds per reading, so a measured copy is 2s.
func newMigrateEnv(t *testing.T, town string) (townStateMigrateEnv, *fakeVolume, *bytes.Buffer) {
	t.Helper()
	fv := &fakeVolume{dir: t.TempDir(), exists: true, free: 1 << 30}
	var out bytes.Buffer
	clock := time.Unix(0, 0)
	return townStateMigrateEnv{
		town:            town,
		volume:          "gt-state",
		image:           "gt-daemon:test",
		composePath:     filepath.Join(town, "daemon", "container", "compose.yml"),
		savedCompose:    filepath.Join(town, "daemon", "container", "compose.yml.pre-cutover"),
		allowNoRigs:     true,
		exclude:         migrateSecretExclusion(t),
		beadsHost:       "gt-dolt",
		beadsHostSource: "test",
		engine:          fv,
		probes:          readyProbes(),
		now: func() time.Time {
			clock = clock.Add(2 * time.Second)
			return clock
		},
		out:     &out,
		summary: io.Discard,
	}, fv, &out
}

// volumeFiles lists the regular files in the fake volume, by slash path.
func volumeFiles(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// TestTownStateMigratePlanNamesThePrerequisites: the plan prints the counts, the
// bytes and the volume's free space, and reports each cut-over prerequisite met
// or unmet by name.
func TestTownStateMigratePlanNamesThePrerequisites(t *testing.T) {
	t.Parallel()
	town := migrateTown(t)
	env, _, out := newMigrateEnv(t, town)
	env.probes = migrateProbes{
		holdPresent:       func(string) (bool, error) { return false, nil },
		daemonContainer:   func(string) (string, error) { return "gt-town-daemon-1", nil },
		containerRunning:  func(string) (bool, error) { return true, nil },
		landingQueue:      func(string) (int, error) { return 2, nil },
		landingCandidates: func(string) ([]string, error) { return []string{"alpha"}, nil },
		postLandPending:   func(string) ([]string, error) { return []string{"beta (newest verdict at 0123456789ab)"}, nil },
	}
	if err := runTownStateMigratePlanIn(env); err != nil {
		t.Fatalf("plan: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"migrate plan: ", "entries: ", "daemon", "copy: 2 files", "skipped:", "1 secret files (the secrets list)",
		"free space on volume gt-state: ", "bytes",
		"gt daemon compose write --town-state-volume gt-state",
		"unmet: dispatch hold missing",
		"unmet: daemon container running (gt-town-daemon-1)",
		"unmet: landing queue: run in flight (2 ready-to-land bead(s) waiting)",
		"unmet: landing candidate: run in flight on alpha",
		"unmet: post-land status: run in flight, verdict pending on beta (newest verdict at 0123456789ab)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, secretContent) || strings.Contains(got, "forgejo-landing.env") {
		t.Errorf("plan output names or shows the secret:\n%s", got)
	}

	ready, _, readyOut := newMigrateEnv(t, town)
	if err := runTownStateMigratePlanIn(ready); err != nil {
		t.Fatalf("plan with prerequisites met: %v", err)
	}
	for _, want := range []string{
		"met: dispatch hold present", "met: daemon container stopped",
		"met: landing queue: no ready-to-land bead",
		"met: landing candidate: no landing, post-landing or revert run in flight",
		"met: post-land status: no verdict pending",
	} {
		if !strings.Contains(readyOut.String(), want) {
			t.Errorf("plan with every prerequisite met lacks %q:\n%s", want, readyOut.String())
		}
	}
}

// TestTownStateMigrateCopyThenVerify: the copy lands the daemon-owned set, minus
// the secret, and verify passes; a changed, missing or extra file each fail verify
// and are named by path.
func TestTownStateMigrateCopyThenVerify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
		want   string
	}{
		{name: "clean", mutate: func(*testing.T, string) {}, want: ""},
		{name: "changed", mutate: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "daemon", "maintenance_state.json"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "changed daemon/maintenance_state.json"},
		{name: "missing", mutate: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "daemon", "nested", "notes.json")); err != nil {
				t.Fatal(err)
			}
		}, want: "missing daemon/nested/notes.json"},
		{name: "extra", mutate: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "daemon", "stray.json"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "extra daemon/stray.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := migrateTown(t)
			env, fv, out := newMigrateEnv(t, town)
			if err := runTownStateMigrateCopyIn(env); err != nil {
				t.Fatalf("copy: %v\n%s", err, out.String())
			}
			files := volumeFiles(t, fv.dir)
			if !containsName(files, "daemon/maintenance_state.json") || !containsName(files, "daemon/nested/notes.json") {
				t.Fatalf("the copy lacks daemon-owned files: %v", files)
			}
			if containsName(files, "daemon/forgejo-landing.env") {
				t.Fatalf("the copy carried the secret: %v", files)
			}

			tc.mutate(t, fv.dir)
			out.Reset()
			err := runTownStateMigrateVerifyIn(env)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("verify of a clean copy: %v\n%s", err, out.String())
				}
				if !strings.Contains(out.String(), "matches the host copy set") {
					t.Errorf("verify did not say the copy matches:\n%s", out.String())
				}
				return
			}
			var exit *ExitCodeError
			if !errors.As(err, &exit) || exit.Code != 1 {
				t.Fatalf("verify = %v, want exit 1", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("verify output lacks %q:\n%s", tc.want, out.String())
			}
		})
	}
}

// TestTownStateMigrateAbsentDaemonContainer: a container docker cannot find is
// stopped only when the operator says it was removed (--daemon-removed); a
// misnamed container reads the same as a removed one.
func TestTownStateMigrateAbsentDaemonContainer(t *testing.T) {
	t.Parallel()
	absent := func(string) (bool, error) { return false, errDaemonContainerAbsent }
	for _, tc := range []struct {
		name    string
		removed bool
		wantMet bool
		want    string
	}{
		{name: "without the flag it is unmet", removed: false, wantMet: false, want: "re-run with --daemon-removed"},
		{name: "with the flag it is met and says why", removed: true, wantMet: true, want: "confirmed removed by --daemon-removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, _, _ := newMigrateEnv(t, migrateTown(t))
			env.probes = readyProbes()
			env.probes.containerRunning = absent
			env.daemonRemoved = tc.removed
			var found bool
			for _, c := range cutoverChecks(env) {
				if strings.Contains(c.Text, "daemon container") {
					found = true
					if c.Met != tc.wantMet || !strings.Contains(c.Text, tc.want) {
						t.Errorf("check = %+v, want met=%v naming %q", c, tc.wantMet, tc.want)
					}
				}
			}
			if !found {
				t.Fatal("no daemon container check was produced")
			}
		})
	}
}

// TestTownStateMigrateCutoverRefusesAnUnsafeState: --cutover is refused, before
// any byte moves, with the hold absent, the container running or a landing in
// flight; a rehearsal copy does not need the hold or the stopped container; a
// cut-over that passes prints its measured copy time.
func TestTownStateMigrateCutoverRefusesAnUnsafeState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		probes migrateProbes
		want   string
	}{
		{name: "hold absent", probes: func() migrateProbes {
			p := readyProbes()
			p.holdPresent = func(string) (bool, error) { return false, nil }
			return p
		}(), want: "dispatch hold missing"},
		{name: "container running", probes: func() migrateProbes {
			p := readyProbes()
			p.containerRunning = func(string) (bool, error) { return true, nil }
			return p
		}(), want: "daemon container running"},
		{name: "container absent without the operator's word", probes: func() migrateProbes {
			p := readyProbes()
			p.containerRunning = func(string) (bool, error) { return false, errDaemonContainerAbsent }
			return p
		}(), want: "not found: a removed container and a misnamed one read the same"},
		{name: "container state unknown", probes: func() migrateProbes {
			p := readyProbes()
			p.containerRunning = func(string) (bool, error) { return false, errors.New("docker inspect failed") }
			return p
		}(), want: "daemon container state unreadable"},
		{name: "landing in flight", probes: func() migrateProbes {
			p := readyProbes()
			p.landingQueue = func(string) (int, error) { return 1, nil }
			return p
		}(), want: "landing queue: run in flight"},
		{name: "landing queue unreadable", probes: func() migrateProbes {
			p := readyProbes()
			p.landingQueue = func(string) (int, error) { return 0, errors.New("bd list failed") }
			return p
		}(), want: "landing queue unreadable"},
		{name: "landing candidate in flight", probes: func() migrateProbes {
			p := readyProbes()
			p.landingCandidates = func(string) ([]string, error) { return []string{"alpha"}, nil }
			return p
		}(), want: "landing candidate: run in flight on alpha"},
		{name: "landing candidate unreadable", probes: func() migrateProbes {
			p := readyProbes()
			p.landingCandidates = func(string) ([]string, error) { return nil, errors.New("work root unreadable") }
			return p
		}(), want: "landing candidate unreadable"},
		{name: "post-land verdict pending", probes: func() migrateProbes {
			p := readyProbes()
			p.postLandPending = func(string) ([]string, error) { return []string{"alpha (newest verdict at abc)"}, nil }
			return p
		}(), want: "post-land status: run in flight"},
		{name: "post-land status unreadable", probes: func() migrateProbes {
			p := readyProbes()
			p.postLandPending = func(string) ([]string, error) { return nil, errors.New("red-main state is not JSON") }
			return p
		}(), want: "post-land status unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, fv, _ := newMigrateEnv(t, migrateTown(t))
			env.probes = tc.probes
			env.cutover = true
			err := runTownStateMigrateCopyIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("cut-over copy = %v, want a refusal naming %q", err, tc.want)
			}
			if files := volumeFiles(t, fv.dir); len(files) != 0 {
				t.Errorf("a refused cut-over wrote %v", files)
			}
		})
	}

	t.Run("rehearsal ignores the cut-over prerequisites", func(t *testing.T) {
		t.Parallel()
		env, _, _ := newMigrateEnv(t, migrateTown(t))
		env.probes = migrateProbes{
			holdPresent:      func(string) (bool, error) { return false, nil },
			daemonContainer:  func(string) (string, error) { return "gt-town-daemon-1", nil },
			containerRunning: func(string) (bool, error) { return true, nil },
			landingQueue:     func(string) (int, error) { return 3, nil },
		}
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("rehearsal copy with the daemon up = %v, want it to pass", err)
		}
	})

	t.Run("cut-over that passes prints the measured time and writes the cut-over record", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, _, out := newMigrateEnv(t, town)
		env.cutover = true
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("cut-over copy: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "cut-over copy took ") {
			t.Errorf("the cut-over did not print its measured copy time:\n%s", out.String())
		}
		rec := readCutoverRecord(t, town)
		if _, err := time.Parse(time.RFC3339, rec.CutoverAt); err != nil || !strings.HasSuffix(rec.CutoverAt, "Z") {
			t.Errorf("cutover_at = %q, want an RFC 3339 UTC time", rec.CutoverAt)
		}
		if rec.Volume != "gt-state" || rec.CopySeconds <= 0 {
			t.Errorf("cut-over record = %+v, want the volume and a positive copy time", rec)
		}
	})
}

// TestTownStateMigrateRollbackPlan: the rollback lists the stop, the export of the
// rollback rows, the --merge-newer import and the compose restore, in that order.
func TestTownStateMigrateRollbackPlan(t *testing.T) {
	t.Parallel()
	env, _, out := newMigrateEnv(t, migrateTown(t))
	if err := runTownStateMigrateRollbackPlanIn(env); err != nil {
		t.Fatalf("rollback plan: %v", err)
	}
	got := out.String()
	order := []string{"stop the daemon container", "export the rollback rows", "--merge-newer", "restore the saved compose file"}
	last := -1
	for _, step := range order {
		i := strings.Index(got, step)
		if i < 0 {
			t.Fatalf("rollback plan lacks %q:\n%s", step, got)
		}
		if i <= last {
			t.Fatalf("rollback step %q is out of order:\n%s", step, got)
		}
		last = i
	}
	if !strings.Contains(got, "--rollback") || !strings.Contains(got, "gt-daemon:test") {
		t.Errorf("the export step lacks --rollback or the helper image:\n%s", got)
	}

	t.Run("refuses without a volume", func(t *testing.T) {
		t.Parallel()
		env, _, _ := newMigrateEnv(t, migrateTown(t))
		env.volume = ""
		if err := runTownStateMigrateRollbackPlanIn(env); err == nil {
			t.Fatal("rollback plan with no volume = nil, want a refusal")
		}
	})
	t.Run("refuses when the daemon container is unnamed", func(t *testing.T) {
		t.Parallel()
		env, _, _ := newMigrateEnv(t, migrateTown(t))
		env.probes.daemonContainer = func(string) (string, error) { return "", errors.New("the town names no daemon.container_name") }
		if err := runTownStateMigrateRollbackPlanIn(env); err == nil {
			t.Fatal("rollback plan with no container name = nil, want a refusal")
		}
	})
}

// TestTownStateMigratePartialCopyIsReported: a helper that fails, or that ends
// its stream early, makes copy exit non-zero and say the volume is partial; verify
// then fails on the volume and names a path.
func TestTownStateMigratePartialCopyIsReported(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		fault importFault
		want  string
	}{
		{name: "helper fails", fault: importFault{after: 10, fail: true}, want: "the import helper failed"},
		{name: "stream ends early", fault: importFault{after: 10, fail: false}, want: "the import helper ended the stream early"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := migrateTown(t)
			env, fv, out := newMigrateEnv(t, town)
			env.cutover = true
			fault := tc.fault
			fv.fault = &fault
			err := runTownStateMigrateCopyIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "partial copy") {
				t.Fatalf("copy = %v, want a partial-copy refusal naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "is not to be used") {
				t.Errorf("the partial-copy refusal does not say the volume must not be used: %v", err)
			}
			if _, err := os.Stat(filepath.Join(town, "daemon", "town-state-cutover.json")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a partial cut-over wrote a cut-over record (stat err %v)", err)
			}
			if _, ok, _ := readSeedRecord(town); ok {
				t.Errorf("a partial copy left its seed record, so a later copy would treat the partial volume as seeded")
			}

			fv.fault = nil
			out.Reset()
			verifyErr := runTownStateMigrateVerifyIn(env)
			if verifyErr == nil {
				t.Fatalf("verify of a partial volume passed:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "missing daemon/") {
				t.Errorf("verify did not name a path of the partial volume:\n%s", out.String())
			}
		})
	}
}

// TestTownStateMigrateExcludesSecrets: the secret never enters the copy or the
// listing, a secret that reaches the volume fails verify without its name, and no
// output line carries file contents.
func TestTownStateMigrateExcludesSecrets(t *testing.T) {
	t.Parallel()
	town := migrateTown(t)
	env, fv, out := newMigrateEnv(t, town)
	var summary bytes.Buffer
	env.summary = &summary
	if err := runTownStateMigrateCopyIn(env); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if containsName(volumeFiles(t, fv.dir), "daemon/forgejo-landing.env") {
		t.Fatal("the copy carried the secret file")
	}

	out.Reset()
	if err := runTownStateMigrateVerifyIn(env); err != nil {
		t.Fatalf("verify of a clean copy: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "forgejo-landing.env") {
		t.Errorf("the verify listing names the secret:\n%s", out.String())
	}

	if err := os.WriteFile(filepath.Join(fv.dir, "daemon", "forgejo-landing.env"), []byte(secretContent), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := runTownStateMigrateVerifyIn(env)
	if err == nil {
		t.Fatal("verify passed with a secret on the volume")
	}
	if !strings.Contains(out.String(), "1 secret-named entries are on the volume") {
		t.Errorf("verify did not count the secret on the volume:\n%s", out.String())
	}
	for name, text := range map[string]string{"verify output": out.String(), "copy summary": summary.String(), "error": err.Error()} {
		if strings.Contains(text, "forgejo-landing.env") || strings.Contains(text, secretContent) {
			t.Errorf("%s names or shows the secret: %q", name, text)
		}
	}
}

// TestTownStateMigrateCopyRefusesABadVolume: a copy refuses a volume that is not
// empty without a seed record, is short of space, or is named like a path rather
// than a volume. An absent volume is created instead; see
// TestTownStateMigrateCopyCreatesAnAbsentVolume.
func TestTownStateMigrateCopyRefusesABadVolume(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		volume string
		setup  func(fv *fakeVolume)
		want   string
	}{
		{name: "a path, not a volume", volume: "/host/dir", setup: func(*fakeVolume) {}, want: "not a docker volume name"},
		{name: "not empty", volume: "gt-state", setup: func(fv *fakeVolume) {
			if err := os.MkdirAll(filepath.Join(fv.dir, "daemon"), 0o755); err != nil {
				panic(err)
			}
			if err := os.WriteFile(filepath.Join(fv.dir, "daemon", "old.json"), []byte("x\n"), 0o644); err != nil {
				panic(err)
			}
		}, want: "is not empty"},
		{name: "too little free space", volume: "gt-state", setup: func(fv *fakeVolume) { fv.free = 1 }, want: "bytes free"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, fv, _ := newMigrateEnv(t, migrateTown(t))
			env.volume = tc.volume
			tc.setup(fv)
			err := runTownStateMigrateCopyIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("copy = %v, want a refusal naming %q", err, tc.want)
			}
			for _, call := range fv.calls {
				if call == strings.Join(townstate.ImportArgv, " ") {
					t.Errorf("a refused copy still ran the import")
				}
			}
		})
	}
}

// TestTownStateMigrateCopyCreatesAnAbsentVolume: a copy onto a volume docker does
// not know creates it, then seeds it. A refused cut-over creates nothing, a failed
// create copies nothing, and verify still refuses an absent volume rather than
// creating one.
func TestTownStateMigrateCopyCreatesAnAbsentVolume(t *testing.T) {
	t.Parallel()
	t.Run("a rehearsal copy creates the volume and seeds it", func(t *testing.T) {
		t.Parallel()
		env, fv, out := newMigrateEnv(t, migrateTown(t))
		fv.exists = false
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("copy = %v\n%s", err, out.String())
		}
		if len(fv.created) != 1 || fv.created[0] != "gt-state" {
			t.Fatalf("created = %v, want gt-state once", fv.created)
		}
		if !strings.Contains(out.String(), "created volume gt-state (label gt/managed-by=town-state-migrate)") {
			t.Errorf("copy does not say it created the volume:\n%s", out.String())
		}
		if len(volumeFiles(t, fv.dir)) == 0 {
			t.Error("the created volume holds no copy")
		}
	})
	t.Run("a refused cut-over creates nothing", func(t *testing.T) {
		t.Parallel()
		env, fv, _ := newMigrateEnv(t, migrateTown(t))
		fv.exists = false
		env.cutover = true
		env.probes.holdPresent = func(string) (bool, error) { return false, nil }
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "dispatch hold missing") {
			t.Fatalf("cut-over copy = %v, want the hold named", err)
		}
		if len(fv.created) != 0 {
			t.Errorf("a refused cut-over created %v", fv.created)
		}
	})
	t.Run("a failed create copies nothing", func(t *testing.T) {
		t.Parallel()
		env, fv, _ := newMigrateEnv(t, migrateTown(t))
		fv.exists = false
		fv.createErr = errors.New("docker volume create failed")
		if err := runTownStateMigrateCopyIn(env); err == nil {
			t.Fatal("copy with a failed create = nil, want an error")
		}
		for _, call := range fv.calls {
			if call == strings.Join(townstate.ImportArgv, " ") {
				t.Errorf("a copy whose volume create failed still ran the import")
			}
		}
	})
	t.Run("verify refuses an absent volume and creates nothing", func(t *testing.T) {
		t.Parallel()
		env, fv, _ := newMigrateEnv(t, migrateTown(t))
		fv.exists = false
		err := runTownStateMigrateVerifyIn(env)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("verify = %v, want an absent volume refused", err)
		}
		if len(fv.created) != 0 {
			t.Errorf("verify created %v", fv.created)
		}
	})
}

// TestTownStateMigrateBeadsReadFailureIsReported: after a copy, one bd read runs
// through the volume; its failure is reported and the copy exits non-zero.
func TestTownStateMigrateBeadsReadFailureIsReported(t *testing.T) {
	t.Parallel()
	env, fv, out := newMigrateEnv(t, migrateTown(t))
	fv.bdFail = true
	err := runTownStateMigrateCopyIn(env)
	if err == nil {
		t.Fatal("copy with a failing bd read = nil, want a non-zero exit")
	}
	if !strings.Contains(out.String(), "bd read through the volume copy: FAILED") {
		t.Errorf("the failed bd read was not reported:\n%s", out.String())
	}
}

// TestTownStateMigrateHelperRunArgs: the helper clears the image's entrypoint (its
// own runs gt install --force), mounts only a validated volume name, and forwards
// BEADS_DOLT_SERVER_HOST by name.
func TestTownStateMigrateHelperRunArgs(t *testing.T) {
	t.Parallel()
	args, err := helperRunArgs("gt-state", "gt-daemon:abc1234", "gt-town-state-migrate-test", townstate.ImportArgv, true)
	if err != nil {
		t.Fatalf("helperRunArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--entrypoint  -w", "-v gt-state:/state", "-e BEADS_DOLT_SERVER_HOST", "gt-daemon:abc1234 gt town state import --town /state"} {
		if !strings.Contains(joined, want) {
			t.Errorf("helper argv lacks %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, ":ro") {
		t.Errorf("the import mounts the volume read-only, so it cannot write the copy: %s", joined)
	}
	if strings.Contains(joined, "BEADS_DOLT_SERVER_HOST=") {
		t.Errorf("the forwarded variable carries a value on the command line: %s", joined)
	}
	if _, err := helperRunArgs("/host/dir", "gt-daemon:abc1234", "gt-town-state-migrate-test", townstate.ImportArgv, false); err == nil {
		t.Error("a host path became a volume mount")
	}

	list, err := helperRunArgs("gt-state", "gt-daemon:abc1234", "gt-town-state-migrate-test", townstate.ListArgv, false)
	if err != nil {
		t.Fatalf("helperRunArgs listing: %v", err)
	}
	if !strings.Contains(strings.Join(list, " "), "-v gt-state:/state:ro") {
		t.Errorf("the listing mounts the volume writable: %s", strings.Join(list, " "))
	}
}

// TestTownStateMigrateHelperImage: the digest in the values file wins; a missing
// values file falls back to the compose tag; a values file that cannot be read is
// an error, never a fall back.
func TestTownStateMigrateHelperImage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  daemon:\n    image: gt-daemon:4f2a1c9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := "registry.example/gt-daemon@sha256:" + strings.Repeat("a", 64)
	values := filepath.Join(dir, "images.yaml")
	if err := os.WriteFile(values, []byte("daemon: "+digest+"\npolecat: registry.example/polecat@sha256:"+strings.Repeat("b", 64)+"\ncommit: "+strings.Repeat("c", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := helperImage(values, compose)
	if err != nil || got != digest {
		t.Errorf("helperImage with values = %q, %v; want the digest", got, err)
	}
	got, err = helperImage(filepath.Join(dir, "absent.yaml"), compose)
	if err != nil || got != "gt-daemon:4f2a1c9" {
		t.Errorf("helperImage without values = %q, %v; want the compose tag", got, err)
	}
	empty := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(empty, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := helperImage(filepath.Join(dir, "absent.yaml"), empty); err == nil {
		t.Error("a compose file with no daemon tag resolved an image")
	}
	if _, err := helperImage(dir, compose); err == nil {
		t.Error("an unreadable values file fell back to the compose tag")
	}
}
