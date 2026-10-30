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

// rigRecord is the record a rig run derives from its log directory, and the
// directory it lives in, made for real so the run can append to it.
func rigRecord(t *testing.T, r *rig) string {
	t.Helper()
	path := ResumeRecordPath(filepath.Dir(r.logDir))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make the resume directory: %v", err)
	}
	return path
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

// fakeFS is the filesystem a resume test sees: the Lstat answer per path, the
// bytes ReadFile returns per path, and every call the record makes on it. No
// test that uses it touches the machine's filesystem.
type fakeFS struct {
	infos     map[string]fs.FileInfo
	data      map[string][]byte
	appends   [][]byte
	removes   []string
	removeErr error
}

// host is the Host the fake answers for: every seam a resume uses is wired.
func (f *fakeFS) host() Host {
	return Host{
		Env:      func(string) string { return "" },
		MkdirAll: func(string, fs.FileMode) error { return nil },
		ReadFile: func(name string) ([]byte, error) {
			if b, ok := f.data[name]; ok {
				return b, nil
			}
			return nil, fs.ErrNotExist
		},
		AppendFile: func(_ string, data []byte) error {
			f.appends = append(f.appends, data)
			return nil
		},
		Remove: func(name string) error {
			f.removes = append(f.removes, name)
			return f.removeErr
		},
		Lstat: func(name string) (fs.FileInfo, error) {
			if info, ok := f.infos[name]; ok {
				return info, nil
			}
			return nil, fs.ErrNotExist
		},
	}
}

// TestResumeSkipsPackagesPassedAtThisSha: a sweep that passed two of four
// packages records those two, and the next run for the same sha executes only
// the other two while counting all four passed.
func TestResumeSkipsPackagesPassedAtThisSha(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	fourTagged(r.repo)
	r.tested, r.resumeFile = "shaA", rigRecord(t, r)
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
			r.tested, r.resumeFile = tc.tested, rigRecord(t, r)
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
	r.tested, r.resumeFile = "shaA", rigRecord(t, r)
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
	r.tested, r.resumeFile = "shaA", rigRecord(t, r)
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
			r.tested, r.resumeFile = "shaA", rigRecord(t, r)
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

// TestResumeTornTailIsNotGluedToTheNextRecord: a record whose last line was torn
// by a restart gets a newline before the next line is appended, so the new pass
// is not swallowed by the torn one. A record that already ends in a newline gets
// no extra one (om 9aa0b4).
func TestResumeTornTailIsNotGluedToTheNextRecord(t *testing.T) {
	t.Parallel()
	whole := recordLine(t, TierIntegration, "./r1", true) + "\n"
	cases := []struct {
		name       string
		data       string
		wantPrefix string
	}{
		{name: "a torn tail is closed off before the append", data: whole + whole[:len(whole)/2], wantPrefix: "\n"},
		{name: "a record ending on a newline is appended to as it is", data: whole, wantPrefix: "{"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := "/x/logs/resume/resume.jsonl"
			f := &fakeFS{data: map[string][]byte{path: []byte(tc.data)}}
			res := OpenResume(Options{ResumeFile: path, Tested: "shaA", LogDir: "/x/logs/run"}, f.host(), &strings.Builder{})
			if res == nil {
				t.Fatalf("OpenResume returned nil for the derived record")
			}
			res.Passed()
			res.Record(TierIntegration, "./r2")
			if len(f.appends) != 1 {
				t.Fatalf("appends = %d, want 1", len(f.appends))
			}
			got := string(f.appends[0])
			if !strings.HasPrefix(got, tc.wantPrefix) || !strings.HasSuffix(got, "\n") {
				t.Errorf("append = %q, want a line starting %q and ending in a newline", got, tc.wantPrefix)
			}
			if strings.Count(strings.TrimPrefix(got, "\n"), "\n") != 1 {
				t.Errorf("append = %q, want exactly one new line", got)
			}
		})
	}
}

// TestResumeRecordOnlyCountsAPass: the reader takes a package as passed only
// when the line says its final verdict was a pass, so a line that says
// otherwise is rerun (om aad931).
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
			r.tested, r.resumeFile = "shaA", rigRecord(t, r)
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
// so it deletes the record it derived, while a RED one keeps it for the next run.
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
			r.tested = "shaA"
			r.resumeFile = rigRecord(t, r)
			// The rig's Lstat answers for the record as a regular file, so the
			// run may remove it.
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
	r.resumeFile = rigRecord(t, r)
	// A directory where the record should be is not a regular file, so the rig's
	// Lstat finds nothing to refuse and the read fails instead.
	if err := os.MkdirAll(r.resumeFile, 0o755); err != nil {
		t.Fatalf("make the directory: %v", err)
	}
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
	r.tested, r.resumeFile = "shaA", rigRecord(t, r)
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
	r.noRemove = true
	r.tested, r.resumeFile = "shaA", rigRecord(t, r)
	r.infos = map[string]fs.FileInfo{
		r.resumeFile: fakeInfo{name: "resume.jsonl", size: 2, mode: 0o644},
	}
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

// TestResumeWithoutLstatLeavesResumeOff: a host that cannot Lstat cannot show the
// record is a regular file, so the run does not read it and every package runs,
// and the record is left alone (om 70902e).
func TestResumeWithoutLstatLeavesResumeOff(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	fourTagged(r.repo)
	r.noCleanupSlots = true
	r.tested, r.resumeFile = "shaA", rigRecord(t, r)
	seedResume(t, r.resumeFile, "shaA", "", TierIntegration, "./r1", "./r2")
	code, stdout, stderr := r.run("integration")
	if code != 0 || !strings.Contains(stdout, "integration GREEN passed=4 failed=0 skipped=0") {
		t.Fatalf("exit %d, stdout %q, stderr %q, want GREEN passed=4", code, stdout, stderr)
	}
	if got := r.tier.testRuns(); len(got) != 4 {
		t.Errorf("go test runs = %v, want all four: resume is off without Lstat", got)
	}
	if !strings.Contains(stderr, "host has no Lstat, so resume is off") {
		t.Errorf("stderr = %q, want it to name why resume is off", stderr)
	}
	if _, err := os.Stat(r.resumeFile); err != nil {
		t.Errorf("the record is gone: %v", err)
	}
}

// TestResumeRecordIsOnlyTheDerivedPath: the record a run trusts and removes is
// the one ResumeRecordPath derives from its log root and nothing else. A path
// the variable names that is not that record, or a record that is not a regular
// file, is refused: the run says so once, reads nothing from it, and removes
// nothing. The filesystem is fake, so every refusal is a row here (om b49f3e,
// om 70902e).
func TestResumeRecordIsOnlyTheDerivedPath(t *testing.T) {
	t.Parallel()
	const (
		logDir = "/x/logs/run"
		root   = "/x/logs"
	)
	record := ResumeRecordPath(root)
	resumeDir := filepath.Dir(record)
	regular := fakeInfo{name: "resume.jsonl", size: 2, mode: 0o644}
	directory := fakeInfo{name: "resume.jsonl", mode: fs.ModeDir | 0o755}
	link := fakeInfo{name: "resume.jsonl", mode: fs.ModeSymlink | 0o777}
	cases := []struct {
		name        string
		own         bool
		resumeFile  string
		infos       map[string]fs.FileInfo
		wantTrusted bool
		wantRemoved []string
		wantNamed   string
	}{
		{
			name:        "the record this run derives is trusted and removed by a green run",
			resumeFile:  record,
			infos:       map[string]fs.FileInfo{record: regular},
			wantTrusted: true,
			wantRemoved: []string{record},
		},
		{
			name:        "a record not written yet is trusted and nothing is removed",
			resumeFile:  record,
			wantTrusted: true,
		},
		{
			name:        "the record derived from a run's own temporary directory is trusted",
			own:         true,
			resumeFile:  "/x/tmp/resume/resume.jsonl",
			infos:       map[string]fs.FileInfo{"/x/tmp/resume/resume.jsonl": regular},
			wantTrusted: true,
			wantRemoved: []string{"/x/tmp/resume/resume.jsonl"},
		},
		{
			name:       "a directory at the record is refused",
			resumeFile: record,
			infos:      map[string]fs.FileInfo{record: directory},
			wantNamed:  "is not a regular file",
		},
		{
			name:       "a symlink at the record is refused",
			resumeFile: record,
			infos:      map[string]fs.FileInfo{record: link},
			wantNamed:  "is not a regular file",
		},
		{
			name:       "another file in the resume directory is refused",
			resumeFile: filepath.Join(resumeDir, "other.jsonl"),
			infos:      map[string]fs.FileInfo{filepath.Join(resumeDir, "other.jsonl"): regular},
			wantNamed:  "not the record this run keeps",
		},
		{
			name:       "a path climbing out of the resume directory is refused",
			resumeFile: filepath.Join(resumeDir, "..", "outside.jsonl"),
			infos:      map[string]fs.FileInfo{filepath.Join(root, "outside.jsonl"): regular},
			wantNamed:  "not the record this run keeps",
		},
		{
			name:       "the file beside the resume directory in the log root is refused",
			resumeFile: filepath.Join(root, "resume.jsonl"),
			infos:      map[string]fs.FileInfo{filepath.Join(root, "resume.jsonl"): regular},
			wantNamed:  "not the record this run keeps",
		},
		{
			name:       "a file in a sibling directory is refused",
			resumeFile: "/x/other/resume/resume.jsonl",
			infos:      map[string]fs.FileInfo{"/x/other/resume/resume.jsonl": regular},
			wantNamed:  "not the record this run keeps",
		},
		{
			name:       "the root is refused",
			resumeFile: "/",
			wantNamed:  "not the record this run keeps",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{ResumeFile: tc.resumeFile, Tested: "shaA", LogDir: logDir}
			if tc.own {
				opts = Options{ResumeFile: tc.resumeFile, Tested: "shaA", LogDir: "/x/tmp/tier-sweep.abc",
					OwnsLogDir: true, LogDirParent: "/x/tmp"}
			}
			f := &fakeFS{infos: tc.infos}
			var stderr strings.Builder
			host := f.host()
			res := OpenResume(opts, host, &stderr)
			if got := res != nil; got != tc.wantTrusted {
				t.Errorf("resume trusted = %v, want %v (stderr %q)", got, tc.wantTrusted, stderr.String())
			}
			if res != nil {
				res.Remove(host)
			}
			if !reflect.DeepEqual(f.removes, tc.wantRemoved) {
				t.Errorf("removals = %v, want %v", f.removes, tc.wantRemoved)
			}
			if tc.wantNamed == "" {
				if stderr.String() != "" {
					t.Errorf("stderr = %q, want nothing", stderr.String())
				}
				return
			}
			if got := stderr.String(); !strings.Contains(got, tc.wantNamed) || strings.Count(got, "\n") != 1 {
				t.Errorf("stderr = %q, want one line naming %q", got, tc.wantNamed)
			}
		})
	}
}

// TestResumeRemoveNamesASeamItCannotUse: a host missing a seam the removal needs
// leaves the record alone, names the reason and never reaches the removal
// (gt-xk9my).
func TestResumeRemoveNamesASeamItCannotUse(t *testing.T) {
	t.Parallel()
	record := ResumeRecordPath("/x/logs")
	regular := fakeInfo{name: "resume.jsonl", mode: 0o644}
	cases := []struct {
		name string
		host func(f *fakeFS) Host
		want string
	}{
		{
			name: "a host with no Remove",
			host: func(f *fakeFS) Host {
				h := f.host()
				h.Remove = nil
				return h
			},
			want: "host has no Remove",
		},
		{
			name: "a host with no Lstat",
			host: func(f *fakeFS) Host {
				h := f.host()
				h.Lstat = nil
				return h
			},
			want: "host has no Lstat",
		},
		{
			name: "an Lstat that fails for another reason",
			host: func(f *fakeFS) Host {
				h := f.host()
				h.Lstat = func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
				return h
			},
			want: fs.ErrPermission.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeFS{infos: map[string]fs.FileInfo{record: regular}}
			var stderr strings.Builder
			res := &Resume{Path: record, Key: primeKey("shaA"), stderr: &stderr}
			res.Remove(tc.host(f))
			if len(f.removes) != 0 {
				t.Errorf("removals = %v, want none", f.removes)
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
	record := ResumeRecordPath("/x/logs")
	f := &fakeFS{}
	var stderr strings.Builder
	res := &Resume{Path: record, Key: primeKey("shaA"), stderr: &stderr}
	res.Remove(f.host())
	if len(f.removes) != 0 {
		t.Errorf("removals = %v, want none: there is no record", f.removes)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// TestResumeRemoveFailureIsNamed: a removal that fails is one stderr line and
// nothing else; the record stays for the next run.
func TestResumeRemoveFailureIsNamed(t *testing.T) {
	t.Parallel()
	record := ResumeRecordPath("/x/logs")
	f := &fakeFS{
		infos:     map[string]fs.FileInfo{record: fakeInfo{name: "resume.jsonl", mode: 0o644}},
		removeErr: errors.New("read-only file system"),
	}
	var stderr strings.Builder
	res := &Resume{Path: record, Key: primeKey("shaA"), stderr: &stderr}
	res.Remove(f.host())
	if got := strings.Count(stderr.String(), "tier-sweep: resume: remove "+record); got != 1 {
		t.Errorf("stderr names the removal failure %d times, want once:\n%s", got, stderr.String())
	}
}

// TestGoflagsReadsOnlyTheInjectedHost: the environment key is the GOFLAGS the
// host the run was given reports, and a host with no Env reports none, so a
// record key is never built from the process's own environment (om 97eb48).
func TestGoflagsReadsOnlyTheInjectedHost(t *testing.T) {
	t.Parallel()
	if got := goflags(Host{}); got != "" {
		t.Errorf("goflags(no Env) = %q, want empty", got)
	}
	host := Host{Env: func(name string) string {
		if name == "GOFLAGS" {
			return "-tags=host"
		}
		return ""
	}}
	if got := goflags(host); got != "-tags=host" {
		t.Errorf("goflags(host) = %q, want the host's value", got)
	}
}
