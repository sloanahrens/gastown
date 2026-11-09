package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townstate"
)

// readCutoverRecord reads the cut-over time file the town wrote.
func readCutoverRecord(t *testing.T, town string) cutoverRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(town, filepath.FromSlash(cutoverRecordRel)))
	if err != nil {
		t.Fatalf("reading the cut-over record: %v", err)
	}
	var rec cutoverRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parsing the cut-over record: %v", err)
	}
	return rec
}

// writeHostFile writes content to a host file and stamps it at mod, so a test
// controls whether the copy sees it as newer than the volume's.
func writeHostFile(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// TestTownStateMigrateCutoverRecordReachesTheVolume: the cut-over record is written
// after the copy's verify, so the volume gets it by a send of its own, and a verify
// after that passes with it in the set. A send that fails, and a volume that stops
// verifying after the send, each remove the host record, so no cut-over time stands
// for a volume that lacks it.
func TestTownStateMigrateCutoverRecordReachesTheVolume(t *testing.T) {
	t.Parallel()
	seedCutover := func(t *testing.T, town string) (townStateMigrateEnv, *fakeVolume, *bytes.Buffer) {
		t.Helper()
		env, fv, out := newMigrateEnv(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("seed: %v\n%s", err, out.String())
		}
		env.cutover = true
		out.Reset()
		return env, fv, out
	}
	t.Run("the volume holds the record and verifies with it", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, out := seedCutover(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("cut-over copy: %v\n%s", err, out.String())
		}
		want, err := os.ReadFile(filepath.Join(town, filepath.FromSlash(cutoverRecordRel)))
		if err != nil {
			t.Fatalf("reading the host record: %v", err)
		}
		assertVolumeFile(t, fv.dir, cutoverRecordRel, string(want))
		out.Reset()
		if err := runTownStateMigrateVerifyIn(env); err != nil {
			t.Fatalf("verify after the cut-over = %v\n%s", err, out.String())
		}
	})
	t.Run("a send that fails removes the host record", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, _ := seedCutover(t, town)
		fv.fault = &importFault{fail: true}
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "sending the cut-over record") {
			t.Fatalf("cut-over copy = %v, want the send named", err)
		}
		if _, statErr := os.Stat(filepath.Join(town, filepath.FromSlash(cutoverRecordRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a failed send left the cut-over record on the host (stat %v)", statErr)
		}
	})
	t.Run("a volume that does not verify with the record removes it", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, out := seedCutover(t, town)
		fv.afterMerge = func(dir string) {
			writeHostFile(t, filepath.Join(dir, "daemon", "late.json"), "x\n", time.Now())
		}
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(out.String(), "extra daemon/late.json") {
			t.Fatalf("cut-over copy = %v, want the extra file named\n%s", err, out.String())
		}
		if _, statErr := os.Stat(filepath.Join(town, filepath.FromSlash(cutoverRecordRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a volume that does not verify kept the cut-over record on the host (stat %v)", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(fv.dir, filepath.FromSlash(cutoverRecordRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a volume that does not verify kept the cut-over record on the volume (stat %v)", statErr)
		}
	})
	t.Run("a volume that cannot drop the record names the removal", func(t *testing.T) {
		t.Parallel()
		env, fv, out := seedCutover(t, migrateTown(t))
		fv.afterMerge = func(dir string) {
			writeHostFile(t, filepath.Join(dir, "daemon", "late.json"), "x\n", time.Now())
		}
		fv.rmErr = errors.New("rm exited 1")
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "removing the cut-over record from volume gt-state") {
			t.Fatalf("cut-over copy = %v, want the failed removal named\n%s", err, out.String())
		}
	})
}

// TestTownStateMigrateDeltaLinks: a seeded volume keeps its links. An unchanged
// link is not sent again, since a merge never replaces a link; a changed link is
// refused before any byte moves, with no import run.
func TestTownStateMigrateDeltaLinks(t *testing.T) {
	t.Parallel()
	seedWithLink := func(t *testing.T, town string) (townStateMigrateEnv, *fakeVolume, *bytes.Buffer) {
		t.Helper()
		if err := os.Symlink("maintenance_state.json", filepath.Join(town, "daemon", "current.json")); err != nil {
			t.Fatal(err)
		}
		env, fv, out := newMigrateEnv(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("seed: %v\n%s", err, out.String())
		}
		out.Reset()
		return env, fv, out
	}
	t.Run("an unchanged link is not sent again", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, out := seedWithLink(t, town)
		writeHostFile(t, filepath.Join(town, "daemon", "new.json"), "new\n", time.Now())
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("delta: %v\n%s", err, out.String())
		}
		got, err := os.Readlink(filepath.Join(fv.dir, "daemon", "current.json"))
		if err != nil || got != "maintenance_state.json" {
			t.Fatalf("volume link = %q, %v; want the seeded link kept", got, err)
		}
		assertVolumeFile(t, fv.dir, "daemon/new.json", "new\n")
	})
	t.Run("a changed link is refused before any byte moves", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, _ := seedWithLink(t, town)
		link := filepath.Join(town, "daemon", "current.json")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("nested/notes.json", link); err != nil {
			t.Fatal(err)
		}
		fv.calls = nil
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "links that differ") {
			t.Fatalf("delta = %v, want the changed link refused", err)
		}
		for _, call := range fv.calls {
			if call == strings.Join(townstate.ImportMergeArgv, " ") {
				t.Errorf("a delta with a changed link still ran the import")
			}
		}
	})
}

// TestTownStateMigrateSeedThenDelta: the first copy into an empty volume seeds it
// and writes a seed record; a later copy onto that volume sends only the files that
// differ, deletes nothing, and writes the cut-over time when it verifies.
func TestTownStateMigrateSeedThenDelta(t *testing.T) {
	t.Parallel()
	t.Run("a seeded volume gets only the changed and missing files", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		notes := hostFileText(t, town, "daemon/nested/notes.json")
		env, fv, out := newMigrateEnv(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("seed: %v\n%s", err, out.String())
		}
		if rec, ok, err := readSeedRecord(town); err != nil || !ok || rec.Volume != "gt-state" {
			t.Fatalf("seed record = %+v, %v, %v; want the volume named", rec, ok, err)
		}

		later := time.Now().Add(time.Hour)
		writeHostFile(t, filepath.Join(town, "daemon", "maintenance_state.json"), "changed\n", later)
		writeHostFile(t, filepath.Join(town, "daemon", "new.json"), "new\n", time.Now())

		env.cutover = true
		var summary bytes.Buffer
		env.summary = &summary
		out.Reset()
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("delta: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "2 path(s) to send (1 missing, 1 changed)") {
			t.Errorf("the delta did not name what it sends:\n%s", out.String())
		}
		if !strings.Contains(summary.String(), "files the volume already holds unchanged") {
			t.Errorf("the delta re-sent files the volume already holds; its export summary:\n%s", summary.String())
		}
		assertVolumeFile(t, fv.dir, "daemon/maintenance_state.json", "changed\n")
		assertVolumeFile(t, fv.dir, "daemon/new.json", "new\n")
		assertVolumeFile(t, fv.dir, "daemon/nested/notes.json", notes)
		rec := readCutoverRecord(t, town)
		if rec.Volume != "gt-state" || rec.CutoverAt == "" || rec.CopySeconds <= 0 {
			t.Errorf("cut-over record = %+v, want the volume, the time and a positive copy time", rec)
		}
		info, err := os.Stat(filepath.Join(town, filepath.FromSlash(cutoverRecordRel)))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("cut-over record mode = %v, %v; want 0600", info, err)
		}
	})

	t.Run("--since limits the differing files a delta sends", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, out := newMigrateEnv(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("seed: %v\n%s", err, out.String())
		}
		writeHostFile(t, filepath.Join(town, "daemon", "maintenance_state.json"), "newer\n", time.Now().Add(time.Hour))
		env.since = time.Now().Add(30 * time.Minute)
		out.Reset()
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("delta with --since: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "1 path(s) to send (0 missing, 1 changed)") {
			t.Errorf("--since did not limit the delta to the newer file:\n%s", out.String())
		}
		assertVolumeFile(t, fv.dir, "daemon/maintenance_state.json", "newer\n")
	})

	t.Run("an older file that differs and is not sent fails the verify by name", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, _, out := newMigrateEnv(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("seed: %v\n%s", err, out.String())
		}
		writeHostFile(t, filepath.Join(town, "daemon", "nested", "notes.json"), "old change\n", time.Now().Add(-48*time.Hour))
		env.since = time.Now().Add(-time.Hour)
		env.cutover = true
		out.Reset()
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(out.String(), "changed daemon/nested/notes.json\n") {
			t.Fatalf("copy = %v, want the older change named by the verify\n%s", err, out.String())
		}
		if strings.Contains(out.String(), olderHostNote) {
			t.Errorf("a file --since left out was given the older-host note it was never sent for:\n%s", out.String())
		}
		if _, statErr := os.Stat(filepath.Join(town, filepath.FromSlash(cutoverRecordRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a copy that does not verify wrote a cut-over record (stat %v)", statErr)
		}
	})

	t.Run("a sent file the volume holds newer is refused with the way out named", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, out := newMigrateEnv(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("seed: %v\n%s", err, out.String())
		}
		// The volume's copy is an hour newer than the host's, so the delta sends the
		// host file and the merge keeps the volume's copy.
		writeHostFile(t, filepath.Join(fv.dir, "daemon", "maintenance_state.json"), "volume newer\n", time.Now().Add(time.Hour))
		writeHostFile(t, filepath.Join(town, "daemon", "maintenance_state.json"), "host older\n", time.Now())
		env.cutover = true
		out.Reset()
		err := runTownStateMigrateCopyIn(env)
		want := "changed daemon/maintenance_state.json: host copy is older than the volume's, so the delta does not overwrite it; touch the host file or use the full copy"
		if err == nil || !strings.Contains(out.String(), want) {
			t.Fatalf("delta over a newer volume copy = %v, want the refusal and its way out named\n%s", err, out.String())
		}
		assertVolumeFile(t, fv.dir, "daemon/maintenance_state.json", "volume newer\n")
		if _, statErr := os.Stat(filepath.Join(town, filepath.FromSlash(cutoverRecordRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a refused delta wrote a cut-over record (stat %v)", statErr)
		}
	})

	t.Run("an extra file on the volume is named and nothing is deleted", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, out := newMigrateEnv(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("seed: %v\n%s", err, out.String())
		}
		writeHostFile(t, filepath.Join(fv.dir, "daemon", "stray.json"), "x\n", time.Now())
		env.cutover = true
		out.Reset()
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(out.String(), "extra daemon/stray.json") {
			t.Fatalf("delta over an extra file = %v, want it named\n%s", err, out.String())
		}
		assertVolumeFile(t, fv.dir, "daemon/stray.json", "x\n")
		if _, statErr := os.Stat(filepath.Join(town, filepath.FromSlash(cutoverRecordRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a refused delta wrote a cut-over record (stat %v)", statErr)
		}
	})

	t.Run("a populated volume with no seed record is refused", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, fv, _ := newMigrateEnv(t, town)
		writeHostFile(t, filepath.Join(fv.dir, "daemon", "old.json"), "x\n", time.Now())
		err := runTownStateMigrateCopyIn(env)
		if err == nil || !strings.Contains(err.Error(), "is not empty") {
			t.Fatalf("copy onto an unseeded volume = %v, want it refused", err)
		}
		for _, call := range fv.calls {
			if call == strings.Join(townstate.ImportArgv, " ") || call == strings.Join(townstate.ImportMergeArgv, " ") {
				t.Errorf("a refused copy still ran an import: %v", fv.calls)
			}
		}
	})

	t.Run("a seed record that names another volume does not seed this one", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, _, _ := newMigrateEnv(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("seed: %v", err)
		}
		env.volume = "gt-other"
		if err := runTownStateMigrateCopyIn(env); err == nil || !strings.Contains(err.Error(), "is not empty") {
			t.Fatalf("copy onto a volume the seed record does not name = %v, want a refusal", err)
		}
	})

	t.Run("a delta with nothing to send still verifies and reports nothing sent", func(t *testing.T) {
		t.Parallel()
		town := migrateTown(t)
		env, _, out := newMigrateEnv(t, town)
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("seed: %v\n%s", err, out.String())
		}
		out.Reset()
		if err := runTownStateMigrateCopyIn(env); err != nil {
			t.Fatalf("second copy: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "0 path(s) to send") {
			t.Errorf("an unchanged volume reported a send:\n%s", out.String())
		}
	})
}

// TestTownStateMigrateSeedRecordFailureLeavesNoSeed: a copy that stops part way
// removes the seed record it wrote, so the partial volume names no seed.
func TestTownStateMigrateSeedRecordFailureLeavesNoSeed(t *testing.T) {
	t.Parallel()
	town := migrateTown(t)
	env, fv, _ := newMigrateEnv(t, town)
	fault := importFault{after: 10, fail: true}
	fv.fault = &fault
	if err := runTownStateMigrateCopyIn(env); err == nil {
		t.Fatal("a copy the helper failed = nil, want a refusal")
	}
	if _, ok, err := readSeedRecord(town); err != nil || ok {
		t.Fatalf("seed record after a failed seed = %v, %v; want none", ok, err)
	}
}

// assertVolumeFile checks that a file in the fake volume holds want.
func assertVolumeFile(t *testing.T, dir, rel, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s on the volume: %v", rel, err)
	}
	if string(got) != want {
		t.Errorf("%s on the volume = %q, want %q", rel, got, want)
	}
}

// hostFileText reads a host file under the town, for a test that compares the
// volume's copy with what the host held before the copy.
func hostFileText(t *testing.T, town, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(town, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s on the host: %v", rel, err)
	}
	return string(data)
}
