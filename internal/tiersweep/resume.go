package tiersweep

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Resume is the record of the packages a sweep already passed at one commit,
// so a sweep a restart cut picks up where it stopped instead of starting over
// (gt-loqew). It is keyed by the commit, the skip list and the environment as
// well as the package, so a record never carries a pass across a different
// code, a different list of what runs or a different build environment.
type Resume struct {
	// Path is TIER_SWEEP_RESUME, the file the records live in.
	Path string
	// Key is what a record must name to count for this run.
	Key ResumeKey
	// ReadFile and AppendFile are the filesystem seams; OpenResume fills them
	// with the process's own when the Host leaves them nil.
	ReadFile   func(name string) ([]byte, error)
	AppendFile func(name string, data []byte) error
	// stderr names a failure once; no tier's verdict changes.
	stderr io.Writer
	// mu orders the appends with the failed flag, so a write that failed and the
	// appends behind it are never seen out of order.
	mu sync.Mutex
	// failed drops every later append after the first one fails, so a full disk
	// is one stderr line rather than one per package.
	failed bool
}

// ResumeKey is what a record must name to count as a pass: the commit under
// test, the skip list the run worked from, and the environment its children
// inherit.
type ResumeKey struct {
	// Tested is TIER_SWEEP_TESTED, the commit every record read must name.
	Tested string
	// SkipHash is the hash of the skip list the record must have been written
	// under.
	SkipHash string
	// EnvHash is the hash of the environment the sweep's children inherit that
	// can change what they run.
	EnvHash string
}

// resumeRecord is one line of the record: a package whose final verdict was a
// pass, at Tested under the skip list and environment hashed beside it. Passed
// is written as true and read as a requirement: a line that does not say the
// package passed is no pass, whatever else it names.
type resumeRecord struct {
	Tested   string `json:"tested"`
	Tier     string `json:"tier"`
	Package  string `json:"package"`
	SkipHash string `json:"skip_hash"`
	EnvHash  string `json:"env_hash"`
	Passed   bool   `json:"passed"`
}

// OpenResume is the run's resume record, and nil when TIER_SWEEP_RESUME names
// no file. A run that names a file but no commit gets none either, named once
// on stderr: without a commit no record can be shown to belong to this run.
func OpenResume(path, tested, skipList string, host Host, stderr io.Writer) *Resume {
	if path == "" {
		return nil
	}
	if tested == "" {
		fmt.Fprintf(stderr, "%s: resume: %s names no commit, so %s is not read (%s)\n",
			Name, EnvTested, path, sweepGoesOn)
		return nil
	}
	read := host.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	appendFile := host.AppendFile
	if appendFile == nil {
		appendFile = appendToFile
	}
	key := ResumeKey{Tested: tested, SkipHash: skipHash(skipList), EnvHash: envHash(goflags(host))}
	return &Resume{Path: path, Key: key, ReadFile: read, AppendFile: appendFile, stderr: stderr}
}

// Passed is the set of packages the record holds as passed at this run's key,
// keyed by TimingsKey. A file that is not there is the first run; an unreadable
// one is named once. Either way every package runs, which costs a sweep and
// still reports the truth.
func (r *Resume) Passed() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.ReadFile(r.Path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.report("read", err)
		}
		return nil
	}
	return ParseResume(data, r.Key)
}

// Record appends one package whose final verdict is a pass. Only a pass reaches
// it, so a failure is never resumed over, and the append is one line so a
// restart tears at most the line being written. The write is under the same
// lock as the failed flag: a package is in the record or it is not, and the
// reader never sees a line the writer had already given up on.
func (r *Resume) Record(tier, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed {
		return
	}
	line, err := json.Marshal(resumeRecord{
		Tested: r.Key.Tested, Tier: tier, Package: name,
		SkipHash: r.Key.SkipHash, EnvHash: r.Key.EnvHash, Passed: true,
	})
	if err != nil {
		r.fail(err)
		return
	}
	if err := r.AppendFile(r.Path, append(line, '\n')); err != nil {
		r.fail(err)
	}
}

// Remove deletes the record a GREEN run leaves nothing to resume from. Only the
// run's own record goes: TIER_SWEEP_RESUME is environment text naming whatever
// it names, so the path must be a regular file, not a link, directly inside the
// run's log directory, and it is removed with one os.Remove. Every other path
// is left alone and named once on stderr, because deleting an operator-supplied
// path is the worst thing this feature could do (om b49f3e).
func (r *Resume) Remove(logDir string, host Host) {
	path, err := removableRecord(r.Path, logDir, host)
	if errors.Is(err, errNoRecord) {
		// A run that never wrote a record has nothing to delete, and saying so
		// on every clean sweep would bury the lines that matter.
		return
	}
	if err != nil {
		r.report("remove", err)
		return
	}
	if err := host.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.report("remove", err)
	}
}

// errNoRecord is what a GREEN run finds when the record is already gone: there
// is nothing to delete, so the run says nothing.
var errNoRecord = errors.New("no record to remove")

// removableRecord is the path a GREEN run may delete, or why it may not: the
// file must sit directly in logDir after both are cleaned, and Lstat must find
// a regular file there, never a directory and never a symlink. The checks run
// before the removal, so a refusal reaches no filesystem call beyond Lstat.
func removableRecord(path, logDir string, host Host) (string, error) {
	if host.Remove == nil {
		return "", errors.New("skipped, host has no Remove")
	}
	if host.Lstat == nil {
		return "", errors.New("skipped, host has no Lstat")
	}
	clean := filepath.Clean(path)
	dir := filepath.Clean(logDir)
	if filepath.Dir(clean) != dir {
		return "", fmt.Errorf("%s is not a record in %s, so it is left alone", clean, dir)
	}
	info, err := host.Lstat(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errNoRecord
		}
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file, so it is left alone", clean)
	}
	return clean, nil
}

// fail names a write failure and stops the recording for this run; the package
// that earned the line still counts as passed, and the next run runs it again.
func (r *Resume) fail(err error) {
	r.failed = true
	r.report("write", err)
}

// report names a resume failure once, in the same shape as the timings lines.
func (r *Resume) report(verb string, err error) {
	if r.stderr == nil {
		return
	}
	fmt.Fprintf(r.stderr, "%s: resume: %s %s: %v (%s)\n", Name, verb, r.Path, err, sweepGoesOn)
}

// ParseResume reads resume lines into the packages they hold as passed at the
// key given, keyed by TimingsKey. A line that does not parse, such as one a
// restart tore, is dropped, and so is one whose key or verdict does not match:
// reading either as a pass would report a package that never finished
// (om 05f655, om aad931).
func ParseResume(data []byte, key ResumeKey) map[string]bool {
	passed := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec resumeRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if !rec.Passed || rec.Tested != key.Tested || rec.SkipHash != key.SkipHash || rec.EnvHash != key.EnvHash {
			continue
		}
		passed[TimingsKey(rec.Tier, rec.Package)] = true
	}
	return passed
}

// skipHash keys a record to the skip list it was written under: the list decides
// what runs, so a package passed under another list is not a pass under this one.
func skipHash(list string) string {
	return hashString("skip=" + list)
}

// envHash keys a record to the environment the sweep's children inherit: the go
// command reads GOFLAGS from it, and a changed value can change which tests
// build or run, so a record written under one GOFLAGS is not a pass under
// another (om cad9f0).
func envHash(goflags string) string {
	return hashString("GOFLAGS=" + goflags)
}

// goflags is the GOFLAGS a run's children inherit, empty when the host leaves
// the name unset.
func goflags(host Host) string {
	if host.Env == nil {
		return os.Getenv("GOFLAGS")
	}
	return host.Env("GOFLAGS")
}

// hashString is the short hex digest a record key compares, sized so a
// collision is not a thing two runs meet.
func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:8])
}

// appendToFile is the production Host.AppendFile: one write per line, opening
// the file in append mode so the lines before it are never rewritten. A failed
// write reaches the caller, which names it and reruns that package next time; a
// close that fails after a good write is not reported, since the line it may
// have dropped costs a rerun and never a wrong pass.
func appendToFile(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
