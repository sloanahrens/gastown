package land

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tornTail writes raw onto the end of f's file with no trailing newline: the
// tail an append leaves when it stops mid-record.
func tornTail(t *testing.T, f *LandingsFile, raw string) {
	t.Helper()
	fh, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte(raw)); err != nil {
		_ = fh.Close()
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLandingsFileAppendsJSONLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(town, ".runtime", "landings", "gastown.jsonl"); f.Path != want {
		t.Fatalf("path = %s, want %s", f.Path, want)
	}
	for _, c := range []string{"aaa", "bbb"} {
		if err := f.Append(LandingRecord{BeadID: "gt-" + c, LandedCommit: c}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil || rec.LandedCommit != "bbb" {
		t.Fatalf("second line = %q (%v)", lines[1], err)
	}
	if info, _ := os.Stat(f.Path); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Dir(f.Path)); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", info.Mode().Perm())
	}
}

func TestLandingsFileRefusesUnsafeDirAndNames(t *testing.T) {
	t.Parallel()
	if _, err := RigLandingsFile(t.TempDir(), "../evil"); err == nil {
		t.Error("rig name with a path separator accepted")
	}
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is a directory, not a file: the test needs a world-writable landings dir
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(LandingRecord{}); err == nil {
		t.Error("world-writable landings dir accepted")
	}
	town2 := t.TempDir()
	dir2 := filepath.Join(town2, ".runtime", "landings")
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(town2, "elsewhere"), filepath.Join(dir2, "gastown.jsonl")); err != nil {
		t.Fatal(err)
	}
	f2, _ := RigLandingsFile(town2, "gastown")
	if err := f2.Append(LandingRecord{}); err == nil {
		t.Error("symlinked landings file accepted")
	}
}

func TestLandingsFileFind(t *testing.T) {
	t.Parallel()
	f, _ := RigLandingsFile(t.TempDir(), "gastown")
	if _, ok, err := f.Find("gt-a", "h1"); ok || err != nil {
		t.Fatalf("Find on a missing file = %v, %v", ok, err)
	}
	for _, r := range []LandingRecord{{BeadID: "gt-a", Head: "h0", LandedCommit: "c0"}, {BeadID: "gt-a", Head: "h1", LandedCommit: "c1"}, {BeadID: "gt-b", Head: "h1", LandedCommit: "c2"}} {
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := f.Find("gt-a", "h1")
	if err != nil || !ok || got.LandedCommit != "c1" {
		t.Fatalf("Find = %+v, %v, %v", got, ok, err)
	}
}

func TestLandingsLatestForBeadAndRecent(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.LatestForBead("gt-a"); ok || err != nil {
		t.Fatalf("absent file: ok=%v err=%v", ok, err)
	}
	for _, r := range []LandingRecord{
		{BeadID: "gt-a", Head: "h1", LandedCommit: "l1"},
		{BeadID: "gt-b", Head: "h2", LandedCommit: "l2"},
		{BeadID: "gt-a", Head: "h3", LandedCommit: "l3"},
	} {
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	rec, ok, err := f.LatestForBead("gt-a")
	if err != nil || !ok || rec.Head != "h3" {
		t.Fatalf("LatestForBead = %+v %v %v; want the h3 record", rec, ok, err)
	}
	recent, err := f.Recent(2)
	if err != nil || len(recent) != 2 || recent[0].BeadID != "gt-b" || recent[1].Head != "h3" {
		t.Fatalf("Recent(2) = %+v %v", recent, err)
	}
	// Find keeps its exact bead+head match.
	if rec, ok, _ := f.Find("gt-a", "h1"); !ok || rec.LandedCommit != "l1" {
		t.Fatalf("Find(gt-a, h1) = %+v %v", rec, ok)
	}
}

// TestLandingsFileReadsPastATornLine: an append that stopped mid-record left
// one unparsable line, and the reader failed on it — Find errored, Land turned
// that into an InfraError, and every landing on the rig backed off forever
// (gt-jr90x).
func TestLandingsFileReadsPastATornLine(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	// The torn line sits between two whole records, which is the shape the
	// repair in Append leaves behind.
	body := strings.Join([]string{
		`{"bead":"gt-a","rig":"gastown","head":"h1","landed_commit":"c1"}`,
		`{"bead":"gt-tor`,
		`{"bead":"gt-b","rig":"gastown","head":"h2","landed_commit":"c2"}`,
		"",
	}, "\n")
	if err := os.WriteFile(f.Path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := f.Find("gt-a", "h1")
	if err != nil || !ok || got.LandedCommit != "c1" {
		t.Fatalf("Find over a torn line = %+v, %v, %v; want the c1 record", got, ok, err)
	}
	latest, ok, err := f.LatestForBead("gt-b")
	if err != nil || !ok || latest.LandedCommit != "c2" {
		t.Fatalf("LatestForBead over a torn line = %+v, %v, %v; want the c2 record", latest, ok, err)
	}
}

// TestLandingsFileWarnsOnceAboutATornLine: the landing worker reads this file
// on every pass, so a line that stays torn is reported on the read that meets
// it and not again.
func TestLandingsFileWarnsOnceAboutATornLine(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(LandingRecord{BeadID: "gt-a", Head: "h1", LandedCommit: "c1"}); err != nil {
		t.Fatal(err)
	}
	tornTail(t, f, `{"bead":"gt-tor`)
	var warnings []string
	f.Logf = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	if _, ok, err := f.Find("gt-a", "h1"); err != nil || !ok {
		t.Fatalf("Find = %v, %v", ok, err)
	}
	if _, ok, err := f.LatestForBead("gt-a"); err != nil || !ok {
		t.Fatalf("LatestForBead = %v, %v", ok, err)
	}
	if len(warnings) != 1 {
		t.Fatalf("logged %d warning(s) %q; want one for the torn line", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "unparsable") || !strings.Contains(warnings[0], "gt-tor") {
		t.Fatalf("warning %q does not name the torn line", warnings[0])
	}
	// A second torn line is a different set of damage, so it is not swallowed
	// by the first warning.
	tornTail(t, f, "\n{\"bead\":\"gt-tor2")
	if _, ok, err := f.Find("gt-a", "h1"); err != nil || !ok {
		t.Fatalf("Find = %v, %v", ok, err)
	}
	if len(warnings) != 2 {
		t.Fatalf("logged %d warning(s) %q; want a second for the new torn line", len(warnings), warnings)
	}
}

// TestLandingsFileAppendStartsAFreshLineAfterATornTail: appending to a file
// whose tail is torn glued the new record onto the partial one, so a reader
// could parse neither (gt-jr90x).
func TestLandingsFileAppendStartsAFreshLineAfterATornTail(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(LandingRecord{BeadID: "gt-a", Head: "h1", LandedCommit: "c1"}); err != nil {
		t.Fatal(err)
	}
	tornTail(t, f, `{"bead":"gt-tor`)
	rec := LandingRecord{BeadID: "gt-b", Head: "h2", LandedCommit: "c2"}
	if err := f.Append(rec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n"+string(line)+"\n") {
		t.Fatalf("the record was glued to the torn tail:\n%q", data)
	}
	got, ok, err := f.LatestForBead("gt-b")
	if err != nil || !ok || got.LandedCommit != "c2" {
		t.Fatalf("LatestForBead after the repair = %+v, %v, %v", got, ok, err)
	}
}

// TestLandingsFileAppendRollsBackAShortWrite: a full disk part-wrote the
// record, and leaving the part behind is what tore the file in the first
// place, so the append puts the file back the way it found it (gt-jr90x).
func TestLandingsFileAppendRollsBackAShortWrite(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(LandingRecord{BeadID: "gt-a", Head: "h1", LandedCommit: "c1"}); err != nil {
		t.Fatal(err)
	}
	tornTail(t, f, `{"bead":"gt-tor`)
	before, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	f.write = func(fh *os.File, b []byte) (int, error) {
		n, _ := fh.Write(b[:len(b)/2])
		return n, errors.New("no space left on device")
	}
	err = f.Append(LandingRecord{BeadID: "gt-b", Head: "h2", LandedCommit: "c2"})
	if err == nil {
		t.Fatal("a short write was reported as a successful append")
	}
	after, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("the short write stayed in the file:\n got %q\nwant %q", after, before)
	}
	// The file takes the next record, so the rolled-back append left no
	// partial line to glue it onto.
	f.write = nil
	rec := LandingRecord{BeadID: "gt-b", Head: "h2", LandedCommit: "c2"}
	if err := f.Append(rec); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := f.LatestForBead("gt-b"); err != nil || !ok || got.LandedCommit != "c2" {
		t.Fatalf("LatestForBead after the rollback = %+v, %v, %v", got, ok, err)
	}
}

// TestRepairRecordSurvivesATornLanding: repairRecord could not look at
// anything while one line was torn — it returned an InfraError, which the
// landing worker backs off from, so the rig stopped landing (gt-jr90x).
func TestRepairRecordSurvivesATornLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	if err := l.Landings.Append(LandingRecord{BeadID: "gt-other", Head: "h9", LandedCommit: "c9"}); err != nil {
		t.Fatal(err)
	}
	tornTail(t, l.Landings, `{"bead":"gt-tor`)
	res, err := l.repairRecord(l.open(f.repo), f.work)
	if err != nil {
		t.Fatalf("repairRecord with a torn line in the file: %v", err)
	}
	if res != nil {
		t.Fatalf("repairRecord = %+v; want nothing to repair", res)
	}
}
