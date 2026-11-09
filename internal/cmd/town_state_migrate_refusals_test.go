package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townstate"
)

// mkdirs creates the directory path, failing the test if it cannot.
func mkdirs(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// seededEnv is a session over a town already seeded onto a fresh volume, so a
// later copy takes the delta path. Its output is cleared for the copy under test.
func seededEnv(t *testing.T, town string) (townStateMigrateEnv, *fakeVolume, *bytes.Buffer) {
	t.Helper()
	env, fv, out := newMigrateEnv(t, town)
	if err := runTownStateMigrateCopyIn(env); err != nil {
		t.Fatalf("seed: %v\n%s", err, out.String())
	}
	out.Reset()
	return env, fv, out
}

// TestTownStateMigrateSeedAndDeltaStepsRefuse: each step of a seed or a delta that
// cannot read its record, list the volume, price the delta or write its record
// stops the copy and names the step. A refused step copies nothing it refused.
func TestTownStateMigrateSeedAndDeltaStepsRefuse(t *testing.T) {
	t.Parallel()
	t.Run("the seed record cannot be written", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		// A directory where the record's temporary file goes: its write fails.
		mkdirs(t, filepath.Join(town, "daemon", "town-state-seed.json.tmp"))
		env, fv, _ := newMigrateEnv(t, town)
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "writing daemon/town-state-seed.json") {
			t.Fatalf("seed = %v, want the record write named", err)
		}
		if files := volumeFiles(t, fv.dir); len(files) != 0 {
			t.Errorf("a seed whose record could not be written copied %v", files)
		}
	})
	t.Run("the seed record is a directory on a populated volume", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		mkdirs(t, filepath.Join(town, "daemon", "town-state-seed.json"))
		env, fv, _ := newMigrateEnv(t, town)
		writeHostFile(t, filepath.Join(fv.dir, "daemon", "old.json"), "x\n", time.Now())
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "reading the seed record") {
			t.Fatalf("delta = %v, want the unreadable seed record named", err)
		}
	})
	t.Run("the seed record is not JSON", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		writeTownStateFileContent(t, town, "daemon/town-state-seed.json", "{")
		env, fv, _ := newMigrateEnv(t, town)
		writeHostFile(t, filepath.Join(fv.dir, "daemon", "old.json"), "x\n", time.Now())
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "reading the seed record") {
			t.Fatalf("delta = %v, want the unparsable seed record named", err)
		}
	})
	t.Run("the delta's verify cannot list the volume", func(t *testing.T) {
		t.Parallel()
		env, fv, _ := seededEnv(t, migrateTown(t))
		// The copy's own empty check is the next listing; the delta's verify is the one after.
		fv.failListFrom = fv.lists + 2
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "listing volume") {
			t.Fatalf("delta = %v, want the listing named", err)
		}
	})
	t.Run("the delta cannot read the free space", func(t *testing.T) {
		t.Parallel()
		env, fv, _ := seededEnv(t, migrateTown(t))
		fv.freeErr = errors.New("df exited 1")
		err := runTownStateMigrateCopyIn(env)
		if err == nil {
			t.Fatal("delta with an unreadable free space = nil, want a refusal")
		}
		for _, call := range fv.calls {
			if call == strings.Join(townstate.ImportMergeArgv, " ") {
				t.Errorf("a delta that could not price its free space still ran the import")
			}
		}
	})
	t.Run("the delta is larger than the free space", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, _ := seededEnv(t, town)
		writeHostFile(t, filepath.Join(town, "daemon", "maintenance_state.json"), "changed\n", time.Now().Add(time.Hour))
		fv.free = 0
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "the delta needs") {
			t.Fatalf("delta onto a full volume = %v, want the shortfall named", err)
		}
	})
	t.Run("a host file vanishes while the delta is priced", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, _ := seededEnv(t, town)
		changed := filepath.Join(town, "daemon", "maintenance_state.json")
		writeHostFile(t, changed, "changed\n", time.Now().Add(time.Hour))
		// The delta's verify is the listing after the copy's empty check; the
		// host file goes between that verify and the pricing.
		at := fv.lists + 2
		fv.onList = func(n int) {
			if n == at {
				if err := os.Remove(changed); err != nil {
					panic(err)
				}
			}
		}
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "reading the size of daemon/maintenance_state.json") {
			t.Fatalf("delta = %v, want the vanished file named", err)
		}
	})
	t.Run("the delta fails part way and the seed record cannot be removed", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, _ := seededEnv(t, town)
		writeHostFile(t, filepath.Join(town, "daemon", "maintenance_state.json"), "changed\n", time.Now().Add(time.Hour))
		seedPath := filepath.Join(town, "daemon", "town-state-seed.json")
		// The merge import is the step that fails. Before it, the seed record becomes
		// a directory holding a file, so the failed copy cannot remove it.
		fv.onRun = func(argv []string) {
			if strings.Join(argv, " ") != strings.Join(townstate.ImportMergeArgv, " ") {
				return
			}
			if err := os.Remove(seedPath); err != nil {
				panic(err)
			}
			writeHostFile(t, filepath.Join(seedPath, "keep"), "x\n", time.Now())
		}
		fv.fault = &importFault{fail: true}
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "removing daemon/town-state-seed.json") {
			t.Fatalf("failed delta = %v, want the seed record removal named", err)
		}
	})
	t.Run("the cut-over record cannot be written", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		// A directory where the cut-over record's temporary file goes: the copy
		// verifies, then the record's write fails.
		mkdirs(t, filepath.Join(town, "daemon", "town-state-cutover.json.tmp"))
		env, _, _ := newMigrateEnv(t, town)
		env.cutover = true
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "writing daemon/town-state-cutover.json") {
			t.Fatalf("cut-over = %v, want the record write named", err)
		}
		if _, statErr := os.Stat(filepath.Join(town, filepath.FromSlash(cutoverRecordRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a cut-over whose record failed left a record (stat %v)", statErr)
		}
	})
	t.Run("the copy's verify cannot list the fresh volume", func(t *testing.T) {
		t.Parallel()
		env, fv, _ := newMigrateEnv(t, migrateTown(t))
		// The copy's empty check is listing 1; the verify after the seed is listing 2.
		fv.failListFrom = 2
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "listing volume") {
			t.Fatalf("seed = %v, want the verify's listing named", err)
		}
	})
}

// TestTownStateMigrateRecordFiles: a record is refused when it cannot be encoded,
// written or renamed into place, and a failed rename leaves no temporary file; a
// record that cannot be removed is an error, and a missing one is not.
func TestTownStateMigrateRecordFiles(t *testing.T) {
	t.Parallel()
	t.Run("a value that cannot be encoded is refused and writes nothing", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		err := writeRecordFile(town, "daemon/x.json", make(chan int))
		if err == nil || !strings.Contains(err.Error(), "encoding daemon/x.json") {
			t.Fatalf("writeRecordFile of a channel = %v, want the encoding refused", err)
		}
		if _, statErr := os.Stat(filepath.Join(town, "daemon")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a refused encoding created the directory (stat %v)", statErr)
		}
	})
	t.Run("a temporary file that cannot be written is refused", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		mkdirs(t, filepath.Join(town, "daemon", "x.json.tmp"))
		err := writeRecordFile(town, "daemon/x.json", seedRecord{Volume: "gt-state"})
		if err == nil || !strings.Contains(err.Error(), "writing daemon/x.json") {
			t.Fatalf("writeRecordFile over a directory = %v, want the write named", err)
		}
	})
	t.Run("a rename that fails leaves no temporary file", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		// The record's path is a directory, which a file cannot be renamed over.
		mkdirs(t, filepath.Join(town, "daemon", "x.json"))
		err := writeRecordFile(town, "daemon/x.json", seedRecord{Volume: "gt-state"})
		if err == nil || !strings.Contains(err.Error(), "writing daemon/x.json") {
			t.Fatalf("writeRecordFile over a directory = %v, want the rename named", err)
		}
		if _, statErr := os.Stat(filepath.Join(town, "daemon", "x.json.tmp")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a failed rename left its temporary file (stat %v)", statErr)
		}
	})
	t.Run("a record that cannot be removed is an error, and a missing one is not", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		if err := removeRecordFile(town, "daemon/absent.json"); err != nil {
			t.Fatalf("removing a missing record = %v, want no error", err)
		}
		mkdirs(t, filepath.Join(town, "daemon", "x.json"))
		writeHostFile(t, filepath.Join(town, "daemon", "x.json", "keep"), "x\n", time.Now())
		err := removeRecordFile(town, "daemon/x.json")
		if err == nil || !strings.Contains(err.Error(), "removing daemon/x.json") {
			t.Fatalf("removing a non-empty directory = %v, want the removal named", err)
		}
	})
	t.Run("a seed record that is a directory is an error, and an absent one is not a seed", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		if _, ok, err := readSeedRecord(town); err != nil || ok {
			t.Fatalf("absent seed record = %v, %v; want no seed and no error", ok, err)
		}
		mkdirs(t, filepath.Join(town, "daemon", "town-state-seed.json"))
		_, _, err := readSeedRecord(town)
		if err == nil || !strings.Contains(err.Error(), "reading the seed record") {
			t.Fatalf("seed record as a directory = %v, want the read named", err)
		}
	})
}

// TestTownStateMigrateHelperTimeoutIsNamed: a helper run that outlives its deadline
// is stopped and named by the timeout, not reported as a docker failure.
func TestTownStateMigrateHelperTimeoutIsNamed(t *testing.T) {
	t.Parallel()
	eng := dockerEngine{
		image:         "gt-daemon:test",
		helperTimeout: time.Millisecond,
		run: func(ctx context.Context, _, _ []string, _ io.Reader, _, _ io.Writer) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	err := eng.Run("gt-state", townstate.ListArgv, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "did not finish within 1ms and was stopped") {
		t.Fatalf("Run past its deadline = %v, want the timeout named", err)
	}
}

// TestTownStateMigrateVerifyRefusesAPathName: a verify given a path where a volume
// name belongs refuses it before docker sees it.
func TestTownStateMigrateVerifyRefusesAPathName(t *testing.T) {
	t.Parallel()
	env, fv, _ := newMigrateEnv(t, migrateTown(t))
	env.volume = "/host/dir"
	err := runTownStateMigrateVerifyIn(env)
	if err == nil || !strings.Contains(err.Error(), "not a docker volume name") {
		t.Fatalf("verify of a path = %v, want the name refused", err)
	}
	if len(fv.calls) != 0 {
		t.Errorf("verify of a path ran the engine: %v", fv.calls)
	}
}

// TestTownStateMigrateLandingCandidatesRefuseUnreadableState: a landing candidate or
// post-land probe whose settings or registry do not read fails with the cause,
// never as an empty answer.
func TestTownStateMigrateLandingCandidatesRefuseUnreadableState(t *testing.T) {
	t.Parallel()
	t.Run("the daemon settings do not parse", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/daemon.json", "{")
		_, err := landingCandidates(town)
		if err == nil || !strings.Contains(err.Error(), "reading the daemon settings") {
			t.Fatalf("landingCandidates = %v, want the settings named", err)
		}
	})
	t.Run("the rig registry does not parse", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", "{")
		if _, err := landingCandidates(town); err == nil {
			t.Fatal("landingCandidates with an unparsable registry = nil, want an error")
		}
		if _, err := postLandPending(town); err == nil {
			t.Fatal("postLandPending with an unparsable registry = nil, want an error")
		}
	})
	t.Run("a rig's work root is not an absolute path", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
		writeTownStateFileContent(t, town, "mayor/daemon.json", `{"patrols":{"landing_worker":{"work_root":"relative/root"}}}`)
		_, err := landingCandidates(town)
		if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
			t.Fatalf("landingCandidates with a relative work root = %v, want the root refused", err)
		}
	})
	t.Run("a rig's work root is a file, not a directory", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		base := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
		root, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		writeTownStateFileContent(t, town, "mayor/daemon.json", `{"patrols":{"landing_worker":{"work_root":`+string(root)+`}}}`)
		// The first rig, alpha, has a file where its work root directory belongs.
		writeTownStateFileContent(t, base, "alpha", "not a directory\n")
		_, err = landingCandidates(town)
		if err == nil || !strings.Contains(err.Error(), "rig alpha work root") {
			t.Fatalf("landingCandidates over a file = %v, want the work root named", err)
		}
	})
}

// TestTownStateMigrateCopyReadsTheTownSettingsForTheBeadsHost: a copy whose shell
// names no BEADS_DOLT_SERVER_HOST reads the town's dolt.container_host, and a
// settings file that does not parse refuses the copy rather than leaving the
// bd read without a host. The shell's value is passed in empty, so the town
// settings are the read under test.
func TestTownStateMigrateCopyReadsTheTownSettingsForTheBeadsHost(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownStateFileContent(t, town, "mayor/town.json", "{")
	compose := filepath.Join(t.TempDir(), "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  daemon:\n    image: gt-daemon:abcdef1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(secrets, []byte(secretsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume", "since", "secrets-manifest"}, []string{"cutover"})
	flags := map[string]string{
		"town":             town,
		"images":           filepath.Join(t.TempDir(), "absent.yaml"),
		"compose-file":     compose,
		"volume":           "gt-state",
		"secrets-manifest": secrets,
	}
	for name, value := range flags {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	_, err := migrateEnvFromFlags(cmd, true, func() (string, error) { return town, nil }, "")
	if err == nil || !strings.Contains(err.Error(), "reading the town settings for dolt.container_host") {
		t.Fatalf("copy with unparsable town settings = %v, want the settings named", err)
	}
}
