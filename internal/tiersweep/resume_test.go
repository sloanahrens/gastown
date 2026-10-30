package tiersweep

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fourTagged makes the root module hold four integration-tagged packages, ./r1
// to ./r4, so a resume test can name the ones that ran and the ones it took as
// already passed.
func fourTagged(r *fakeRepo) {
	for _, pkg := range []string{"r1", "r2", "r3", "r4"} {
		r.lists[". *_test.go"] = append(r.lists[". *_test.go"], pkg+"/"+pkg+"_test.go")
		r.files[pkg+"/"+pkg+"_test.go"] = taggedTest
	}
}

// primeKey is the key a record must name for the rig's default run: shaA under
// an empty skip list and an unset GOFLAGS.
func primeKey(tested string) ResumeKey {
	return ResumeKey{Tested: tested, SkipHash: skipHash(""), EnvHash: envHash("")}
}

// recordLine is one line a cut run appends for a package, as the record writes
// it, with the verdict the caller gives rather than the pass Record always says.
func recordLine(t *testing.T, tier, pkg string, passed bool) string {
	t.Helper()
	line, err := json.Marshal(resumeRecord{
		Tested: "shaA", Tier: tier, Package: pkg,
		SkipHash: skipHash(""), EnvHash: envHash(""), Passed: passed,
	})
	if err != nil {
		t.Fatalf("build the record line: %v", err)
	}
	return string(line)
}

// resumePath is a resume file in a directory of the test's own.
func resumePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "resume.jsonl")
}

// seedResume writes a record naming pkgs at tested under skipList, through the
// writer a cut run uses, so a test seeds exactly what such a run would leave.
func seedResume(t *testing.T, path, tested, skipList, tier string, pkgs ...string) {
	t.Helper()
	seed := &Resume{Path: path, Key: ResumeKey{Tested: tested, SkipHash: skipHash(skipList), EnvHash: envHash("")},
		AppendFile: appendToFile}
	for _, pkg := range pkgs {
		seed.Record(tier, pkg)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// seedLines writes record lines, exactly as given, to path: the torn tails and
// mismatched verdicts a test seeds by hand.
func seedLines(t *testing.T, path, lines string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// resumedLines is the RESUMED lines stderr holds.
func resumedLines(stderr string) []string {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "RESUMED") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestResumeSkipsPackagesPassedAtThisSha: a sweep that passed two of four
// packages records those two, and the next run for the same sha executes only
// the other two while counting all four passed.
//
// Mutation recorded in gt-loqew: make ParseResume ignore the sha and
// TestResumeIgnoresAnotherShaOrSkipList's "another commit" case goes red.
func TestResumeSkipsPackagesPassedAtThisSha(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	fourTagged(r.repo)
	r.tested, r.resumeFile = "shaA", resumePath(t)
	r.tier.codes = map[string]int{". ./r3": 1, ". ./r4": 1}
	code, _, stderr := r.run("integration")
	if code != 1 {
		t.Fatalf("cut run exit %d, stderr %q, want 1: two packages fail", code, stderr)
	}
	if strings.Contains(stderr, "resume: read") {
		t.Errorf("stderr = %q, want the first run's missing record read as no record", stderr)
	}

	data, err := os.ReadFile(r.resumeFile)
	if err != nil {
		t.Fatalf("the cut run left no record: %v", err)
	}
	want := map[string]bool{
		TimingsKey(TierIntegration, "./r1"): true,
		TimingsKey(TierIntegration, "./r2"): true,
	}
	if got := ParseResume(data, primeKey("shaA")); !reflect.DeepEqual(got, want) {
		t.Fatalf("record = %v, want %v", got, want)
	}

	r.tier.codes, r.tier.calls = nil, nil
	code, stdout, stderr := r.run("integration")
	if code != 0 || !strings.Contains(stdout, "integration GREEN passed=4 failed=0 skipped=0") {
		t.Errorf("resumed run exit %d, stdout %q, stderr %q, want GREEN passed=4", code, stdout, stderr)
	}
	if got, want := r.tier.testRuns(), []string{". ./r3", ". ./r4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("go test runs = %v, want only the two the record does not name %v", got, want)
	}
	wantResumed := []string{
		"tier-sweep: integration ./r1 RESUMED (TIER_SWEEP_RESUME)",
		"tier-sweep: integration ./r2 RESUMED (TIER_SWEEP_RESUME)",
	}
	if got := resumedLines(stderr); !reflect.DeepEqual(got, wantResumed) {
		t.Errorf("RESUMED lines = %q, want %q", got, wantResumed)
	}
}

// TestResumeIgnoresAnotherShaOrSkipList: a record belongs to one commit under
// one skip list, so a record for another of either leaves every package to run.
func TestResumeIgnoresAnotherShaOrSkipList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tested  string
		skip    string
		wantErr string
	}{
		{name: "another commit runs every package", tested: "shaB"},
		{name: "another skip list runs every package", tested: "shaA", skip: "integration:./unrelated"},
		{
			name:    "no commit named resumes nothing",
			tested:  "",
			wantErr: "TIER_SWEEP_TESTED names no commit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			fourTagged(r.repo)
			r.tested, r.resumeFile = tc.tested, resumePath(t)
			r.skip = tc.skip
			seedResume(t, r.resumeFile, "shaA", "", TierIntegration, "./r1", "./r2")
			code, stdout, stderr := r.run("integration")
			if code != 0 || !strings.Contains(stdout, "integration GREEN passed=4 failed=0 skipped=0") {
				t.Errorf("exit %d, stdout %q, stderr %q, want GREEN passed=4", code, stdout, stderr)
			}
			if got := r.tier.testRuns(); len(got) != 4 {
				t.Errorf("go test runs = %v, want all four", got)
			}
			if got := resumedLines(stderr); len(got) != 0 {
				t.Errorf("RESUMED lines = %q, want none: the record is for another run", got)
			}
			if tc.wantErr != "" && !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want it to name the disabled resume %q", stderr, tc.wantErr)
			}
		})
	}
}

// TestResumeIgnoresAnotherEnvironment: the record also belongs to one build
// environment, so a run whose children inherit another GOFLAGS starts over
// (om cad9f0).
func TestResumeIgnoresAnotherEnvironment(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	fourTagged(r.repo)
	r.tested, r.resumeFile = "shaA", resumePath(t)
	r.goflags = "-tags=changed"
	seedResume(t, r.resumeFile, "shaA", "", TierIntegration, "./r1", "./r2")
	code, stdout, stderr := r.run("integration")
	if code != 0 || !strings.Contains(stdout, "integration GREEN passed=4 failed=0 skipped=0") {
		t.Fatalf("exit %d, stdout %q, stderr %q, want GREEN passed=4", code, stdout, stderr)
	}
	if got := r.tier.testRuns(); len(got) != 4 {
		t.Errorf("go test runs = %v, want all four: the record names another GOFLAGS", got)
	}
	if got := resumedLines(stderr); len(got) != 0 {
		t.Errorf("RESUMED lines = %q, want none", got)
	}
}

// TestResumeNeverRecordsAFailure: only a package that passed reaches the
// record, and a line a restart tore is dropped rather than read as a pass, so
// the package it would have named runs again.
func TestResumeNeverRecordsAFailure(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	fourTagged(r.repo)
	r.tested, r.resumeFile = "shaA", resumePath(t)
	r.tier.codes = map[string]int{". ./r2": 1}
	if code, _, stderr := r.run("integration"); code != 1 {
		t.Fatalf("exit %d, stderr %q, want 1: ./r2 fails", code, stderr)
	}

	data, err := os.ReadFile(r.resumeFile)
	if err != nil {
		t.Fatalf("the run left no record: %v", err)
	}
	want := map[string]bool{
		TimingsKey(TierIntegration, "./r1"): true,
		TimingsKey(TierIntegration, "./r3"): true,
		TimingsKey(TierIntegration, "./r4"): true,
	}
	if got := ParseResume(data, primeKey("shaA")); !reflect.DeepEqual(got, want) {
		t.Errorf("record = %v, want %v, with no line for ./r2", got, want)
	}
	if lines := strings.Count(string(data), "\n"); lines != 3 {
		t.Errorf("record holds %d lines, want 3:\n%s", lines, data)
	}
}

// TestResumeTornLastLineIsRerun: a record a restart cut can end in a half
// written line, and that line is no pass while every whole line before it
// still counts (om 05f655).
//
// Mutation recorded in gt-loqew: accept a line that ends without its newline
// and the "a torn last line" row goes red; drop the parse-error skip and the
// same row goes red again.
func TestResumeTornLastLineIsRerun(t *testing.T) {
	t.Parallel()
	whole := func(t *testing.T) []string {
		t.Helper()
		return []string{
			recordLine(t, TierIntegration, "./r1", true),
			recordLine(t, TierIntegration, "./r2", true),
			recordLine(t, TierIntegration, "./r3", true),
		}
	}
	cases := []struct {
		name     string
		record   func(t *testing.T) string
		wantRuns []string
	}{
		{
			name: "a torn last line is dropped and the whole lines before it count",
			record: func(t *testing.T) string {
				lines := whole(t)
				torn := lines[2][:len(lines[2])/2]
				return lines[0] + "\n" + lines[1] + "\n" + torn
			},
			wantRuns: []string{". ./r3", ". ./r4"},
		},
		{
			name: "a record ending exactly on a newline counts every line",
			record: func(t *testing.T) string {
				return strings.Join(whole(t), "\n") + "\n"
			},
			wantRuns: []string{". ./r4"},
		},
		{
			name: "a record that is only a torn line resumes nothing",
			record: func(t *testing.T) string {
				line := recordLine(t, TierIntegration, "./r1", true)
				return line[:len(line)/2]
			},
			wantRuns: []string{". ./r1", ". ./r2", ". ./r3", ". ./r4"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			fourTagged(r.repo)
			r.tested, r.resumeFile = "shaA", resumePath(t)
			seedLines(t, r.resumeFile, tc.record(t))
			code, stdout, stderr := r.run("integration")
			if code != 0 || !strings.Contains(stdout, "integration GREEN passed=4 failed=0 skipped=0") {
				t.Fatalf("exit %d, stdout %q, stderr %q, want GREEN passed=4", code, stdout, stderr)
			}
			if got := r.tier.testRuns(); !reflect.DeepEqual(got, tc.wantRuns) {
				t.Errorf("go test runs = %v, want %v", got, tc.wantRuns)
			}
			if strings.Contains(stderr, "resume: read") {
				t.Errorf("stderr = %q, want a torn line ignored without a read failure", stderr)
			}
		})
	}
}

// TestResumeRecordOnlyCountsAPass: the reader takes a package as passed only
// when the line says its final verdict was a pass, so a line that says
// otherwise is rerun (om aad931).
//
// Mutation recorded in gt-loqew: read a line without checking Passed and the
// "verdict not a pass" row goes red.
func TestResumeRecordOnlyCountsAPass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		passed   bool
		wantRuns []string
	}{
		{name: "a line whose verdict is a pass is resumed", passed: true, wantRuns: []string{". ./r2", ". ./r3", ". ./r4"}},
		{name: "a line whose verdict is not a pass is rerun", passed: false, wantRuns: []string{". ./r1", ". ./r2", ". ./r3", ". ./r4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			fourTagged(r.repo)
			r.tested, r.resumeFile = "shaA", resumePath(t)
			seedLines(t, r.resumeFile, recordLine(t, TierIntegration, "./r1", tc.passed)+"\n")
			code, stdout, stderr := r.run("integration")
			if code != 0 || !strings.Contains(stdout, "integration GREEN passed=4 failed=0 skipped=0") {
				t.Fatalf("exit %d, stdout %q, stderr %q, want GREEN passed=4", code, stdout, stderr)
			}
			if got := r.tier.testRuns(); !reflect.DeepEqual(got, tc.wantRuns) {
				t.Errorf("go test runs = %v, want %v", got, tc.wantRuns)
			}
		})
	}
}

// TestGreenSweepDeletesTheResumeFile: a GREEN sweep has nothing left to resume,
// so it deletes its own record, while a RED one keeps it for the next run.
func TestGreenSweepDeletesTheResumeFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		codes       map[string]int
		wantCode    int
		wantDeleted bool
	}{
		{name: "a green sweep deletes the record", wantCode: 0, wantDeleted: true},
		{
			name:     "a red sweep keeps the record for the next run",
			codes:    map[string]int{". ./r3": 1},
			wantCode: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			fourTagged(r.repo)
			// The record the sweep may delete is the one inside its own log
			// directory; the rig's Lstat answers for it as a regular file.
			r.tested = "shaA"
			r.resumeFile = filepath.Join(r.logDir, "resume.jsonl")
			r.infos = map[string]fs.FileInfo{
				r.resumeFile: fakeInfo{name: "resume.jsonl", size: 2, mode: 0o644},
			}
			seedResume(t, r.resumeFile, "shaA", "", TierIntegration, "./r1", "./r2")
			r.tier.codes = tc.codes
			code, stdout, stderr := r.run("integration")
			if code != tc.wantCode {
				t.Fatalf("exit %d, stdout %q, stderr %q, want %d", code, stdout, stderr, tc.wantCode)
			}
			deleted := reflect.DeepEqual(r.removes, []string{r.resumeFile})
			if deleted != tc.wantDeleted {
				t.Errorf("single-file removals = %v, want removed %v", r.removes, tc.wantDeleted)
			}
			if len(r.removals) != 0 {
				t.Errorf("RemoveAll calls = %v, want none: the record goes one file at a time", r.removals)
			}
			if strings.Contains(stderr, "resume: remove") {
				t.Errorf("stderr = %q, want no removal failure", stderr)
			}
		})
	}
}

// TestResumeUnreadableRecordRunsEveryPackage: a record the run cannot read is
// named once and every package runs, so a broken file costs a sweep and never a
// false pass.
func TestResumeUnreadableRecordRunsEveryPackage(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	fourTagged(r.repo)
	r.tested = "shaA"
	r.resumeFile = t.TempDir()
	code, stdout, stderr := r.run("integration")
	if code != 0 || !strings.Contains(stdout, "integration GREEN passed=4 failed=0 skipped=0") {
		t.Errorf("exit %d, stdout %q, stderr %q, want GREEN passed=4", code, stdout, stderr)
	}
	if got := r.tier.testRuns(); len(got) != 4 {
		t.Errorf("go test runs = %v, want all four", got)
	}
	if !strings.Contains(stderr, "tier-sweep: resume: read "+r.resumeFile) {
		t.Errorf("stderr = %q, want it to name the unreadable record", stderr)
	}
}

// TestResumeAppendFailureIsNamedOnce: a record the run cannot write is one
// stderr line however many packages pass, and no tier's verdict changes.
func TestResumeAppendFailureIsNamedOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	fourTagged(r.repo)
	r.tested, r.resumeFile = "shaA", resumePath(t)
	r.appendErr = errors.New("disk full")
	code, stdout, stderr := r.run("integration")
	if code != 0 || !strings.Contains(stdout, "integration GREEN passed=4 failed=0 skipped=0") {
		t.Errorf("exit %d, stdout %q, stderr %q, want GREEN passed=4", code, stdout, stderr)
	}
	if got := strings.Count(stderr, "tier-sweep: resume: write "+r.resumeFile); got != 1 {
		t.Errorf("stderr names the write failure %d times, want once:\n%s", got, stderr)
	}
}

// TestResumeWithoutRemoveKeepsTheRecord: a host that wired no single-file
// removal leaves the record for the next run and names why, rather than
// reaching past its seams to the machine's own filesystem (gt-xk9my).
func TestResumeWithoutRemoveKeepsTheRecord(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	fourTagged(r.repo)
	r.noCleanupSlots = true
	r.tested, r.resumeFile = "shaA", resumePath(t)
	seedResume(t, r.resumeFile, "shaA", "", TierIntegration, "./r1", "./r2")
	code, stdout, stderr := r.run("integration")
	if code != 0 || !strings.Contains(stdout, "integration GREEN passed=4 failed=0 skipped=0") {
		t.Fatalf("exit %d, stdout %q, stderr %q, want GREEN passed=4", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "host has no Remove") {
		t.Errorf("stderr = %q, want it to name the reason the record stays", stderr)
	}
	if _, err := os.Stat(r.resumeFile); err != nil {
		t.Errorf("the record is gone: %v", err)
	}
}

// TestResumeRemoveOnlyDeletesItsOwnRecord: a GREEN run removes the regular file
// TIER_SWEEP_RESUME names inside its log directory and nothing else — not a
// directory, not a symlink, not a path that climbs out of the directory and not
// the root. Each refusal reaches the removal seam zero times (om b49f3e).
//
// Mutation recorded in gt-loqew: drop the containment check and the climbing
// rows go red; drop the regular-file check and the directory and symlink rows
// go red; call host.RemoveAll and the recording seam sees no Remove call.
func TestResumeRemoveOnlyDeletesItsOwnRecord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		victim      func(t *testing.T, dir string) string
		wantRemoved bool
	}{
		{
			name: "the record inside the log directory is removed",
			victim: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "resume.jsonl")
				seedLines(t, path, recordLine(t, TierIntegration, "./r1", true)+"\n")
				return path
			},
			wantRemoved: true,
		},
		{
			name: "a directory of the same name is refused",
			victim: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "resume.jsonl")
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatalf("make the directory: %v", err)
				}
				seedLines(t, filepath.Join(path, "kept"), "keep me\n")
				return path
			},
		},
		{
			name: "a symlink is refused",
			victim: func(t *testing.T, dir string) string {
				target := filepath.Join(dir, "target.jsonl")
				seedLines(t, target, "keep me\n")
				path := filepath.Join(dir, "resume.jsonl")
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("link: %v", err)
				}
				return path
			},
		},
		{
			name: "a path climbing out of the log directory is refused",
			victim: func(t *testing.T, dir string) string {
				path := filepath.Join(filepath.Dir(dir), "outside.jsonl")
				seedLines(t, path, "keep me\n")
				return path
			},
		},
		{
			name: "the root is refused",
			victim: func(t *testing.T, dir string) string {
				return "/"
			},
		},
		{
			name: "a file in another directory is refused",
			victim: func(t *testing.T, dir string) string {
				path := resumePath(t)
				seedLines(t, path, "keep me\n")
				return path
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := tc.victim(t, dir)
			var removes []string
			host := Host{
				Lstat: os.Lstat,
				Remove: func(name string) error {
					removes = append(removes, name)
					return os.Remove(name)
				},
			}
			res := &Resume{Path: path, Key: primeKey("shaA")}
			res.Remove(dir, host)
			if got := len(removes) == 1; got != tc.wantRemoved {
				t.Errorf("removal calls = %v, want removed %v", removes, tc.wantRemoved)
			}
			if tc.wantRemoved {
				return
			}
			if _, err := os.Lstat(path); err != nil {
				t.Errorf("%s is gone, want it left alone: %v", path, err)
			}
		})
	}
}

// TestResumeRemoveNamesASeamItCannotUse: a host missing the Lstat, or one whose
// Lstat fails for another reason, leaves the record alone, names the reason and
// never reaches the removal (gt-xk9my).
func TestResumeRemoveNamesASeamItCannotUse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		host func(removes *int) Host
		want string
	}{
		{
			name: "a host with no Lstat",
			host: func(removes *int) Host {
				return Host{Remove: func(string) error { *removes++; return nil }}
			},
			want: "host has no Lstat",
		},
		{
			name: "an Lstat that fails for another reason",
			host: func(removes *int) Host {
				return Host{
					Lstat:  func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission },
					Remove: func(string) error { *removes++; return nil },
				}
			},
			want: fs.ErrPermission.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var stderr strings.Builder
			var removals int
			res := &Resume{Path: filepath.Join(dir, "resume.jsonl"), Key: primeKey("shaA"), stderr: &stderr}
			res.Remove(dir, tc.host(&removals))
			if removals != 0 {
				t.Errorf("removal calls = %d, want none", removals)
			}
			if got := stderr.String(); !strings.Contains(got, tc.want) || strings.Count(got, "\n") != 1 {
				t.Errorf("stderr = %q, want one line naming %q", got, tc.want)
			}
		})
	}
}

// TestResumeRemoveWithoutARecordIsQuiet: a GREEN run that never wrote a record
// has nothing to delete, and says nothing about it.
func TestResumeRemoveWithoutARecordIsQuiet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stderr strings.Builder
	host := Host{
		Lstat: os.Lstat,
		Remove: func(name string) error {
			t.Errorf("removed %s, want no removal call: there is no record", name)
			return nil
		},
	}
	res := &Resume{Path: filepath.Join(dir, "resume.jsonl"), Key: primeKey("shaA"), stderr: &stderr}
	res.Remove(dir, host)
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// TestResumeRemoveFailureIsNamed: a removal that fails is one stderr line and
// nothing else; the record stays for the next run.
func TestResumeRemoveFailureIsNamed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "resume.jsonl")
	seedLines(t, path, recordLine(t, TierIntegration, "./r1", true)+"\n")
	var stderr strings.Builder
	host := Host{
		Lstat:  os.Lstat,
		Remove: func(string) error { return errors.New("read-only file system") },
	}
	res := &Resume{Path: path, Key: primeKey("shaA"), stderr: &stderr}
	res.Remove(dir, host)
	if got := strings.Count(stderr.String(), "tier-sweep: resume: remove "+path); got != 1 {
		t.Errorf("stderr names the removal failure %d times, want once:\n%s", got, stderr.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the record is gone: %v", err)
	}
}
