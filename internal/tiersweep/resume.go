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

// TIER_SWEEP_RESUME names the record file, as the bead's contract says: the
// run uses that file. What keeps a bad value from costing a file is what the run
// removes, not where it sits: the run deletes the record only when it is a
// regular file in a real directory whose every line is a resume record, so a
// variable that names anything else loses nothing (gt-loqew).

// Resume is the record of the packages a sweep already passed at one commit,
// so a sweep a restart cut picks up where it stopped instead of starting over
// (gt-loqew). It is keyed by the commit, the skip list and the environment as
// well as the package, so a record never carries a pass across a different
// code, a different list of what runs or a different build environment.
type Resume struct {
	// Path is the record this run derives, ResumeRecordPath of its log root.
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
	// torn is set when the record as last read ends without a newline: its last
	// line is a torn one, and the next append starts a new line so it is not
	// glued onto the torn one (gt-loqew).
	torn bool
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
// no file. A run that names a file but no commit gets none, and so does one
// whose file is a directory, a link or sits in a directory that is a link: each
// is named once on stderr and every package runs. The record's directory is
// made here, so the first append has somewhere to go.
func OpenResume(opts Options, host Host, stderr io.Writer) *Resume {
	if opts.ResumeFile == "" {
		return nil
	}
	if opts.Tested == "" {
		fmt.Fprintf(stderr, "%s: resume: %s names no commit, so %s is not read (%s)\n",
			Name, EnvTested, opts.ResumeFile, sweepGoesOn)
		return nil
	}
	path := filepath.Clean(opts.ResumeFile)
	if base := filepath.Base(path); base == "." || base == string(filepath.Separator) {
		fmt.Fprintf(stderr, "%s: resume: %s names %s, which is not a file, so resume is off (%s)\n",
			Name, EnvResume, opts.ResumeFile, sweepGoesOn)
		return nil
	}
	if err := checkRecord(path, host); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "%s: resume: %s: %v, so resume is off (%s)\n", Name, EnvResume, err, sweepGoesOn)
		return nil
	}
	if err := host.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "%s: resume: create %s: %v, so resume is off (%s)\n",
			Name, filepath.Dir(path), err, sweepGoesOn)
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
	key := ResumeKey{Tested: opts.Tested, SkipHash: skipHash(opts.SkipList), EnvHash: envHash(goflags(host))}
	return &Resume{Path: path, Key: key, ReadFile: read, AppendFile: appendFile, stderr: stderr}
}

// Passed is the set of packages the record holds as passed at this run's key,
// keyed by TimingsKey. A file that is not there is the first run; an unreadable
// one is named once. Either way every package runs, which costs a sweep and
// still reports the truth.
func (r *Resume) Passed() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.torn = false
	data, err := r.ReadFile(r.Path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.report("read", err)
		}
		return nil
	}
	r.torn = len(data) > 0 && data[len(data)-1] != '\n'
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
	data := append(line, '\n')
	if r.torn {
		data = append([]byte{'\n'}, data...)
	}
	if err := r.AppendFile(r.Path, data); err != nil {
		r.fail(err)
		return
	}
	r.torn = false
}

// Remove deletes the record a GREEN run leaves nothing to resume from. It must
// be a regular file on Lstat in a real directory, it must hold only resume
// records, and it goes with one os.Remove. A host that wired no Remove keeps the
// record and says so (gt-xk9my).
func (r *Resume) Remove(host Host) {
	if host.Remove == nil {
		r.report("remove", errors.New("skipped, host has no Remove"))
		return
	}
	err := checkRecord(r.Path, host)
	if errors.Is(err, fs.ErrNotExist) {
		// A run that never wrote a record has nothing to delete, and saying so
		// on every clean sweep would bury the lines that matter.
		return
	}
	if err != nil {
		r.report("remove", err)
		return
	}
	// Only a file made of resume records is the sweep's own: a variable that
	// names some other regular file loses nothing.
	read := host.ReadFile
	if read == nil {
		read = r.ReadFile
	}
	if read == nil {
		read = os.ReadFile
	}
	data, readErr := read(r.Path)
	if readErr != nil {
		if !errors.Is(readErr, fs.ErrNotExist) {
			r.report("remove", readErr)
		}
		return
	}
	if !onlyResumeRecords(data) {
		r.report("remove", errors.New("it holds something other than resume records, so it is left alone"))
		return
	}
	if err := host.Remove(r.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.report("remove", err)
	}
}

// onlyResumeRecords reports whether data is a resume record file: every
// non-empty line is a resume record naming a commit, a tier and a package, and
// the only line allowed to fail is a final one with no newline, which a restart
// may have torn.
func onlyResumeRecords(data []byte) bool {
	text := string(data)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec resumeRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Tested == "" || rec.Tier == "" || rec.Package == "" {
			if i == len(lines)-1 && !strings.HasSuffix(text, "\n") {
				continue // a torn last line
			}
			return false
		}
	}
	return true
}

// checkRecord is nil when path is a regular file, and fs.ErrNotExist (wrapped)
// when nothing is there. A link, a directory or any other file is refused, and so
// is a host with no Lstat, since the link check cannot run without one.
func checkRecord(path string, host Host) error {
	if host.Lstat == nil {
		return errors.New("skipped, host has no Lstat")
	}
	if parent, err := host.Lstat(filepath.Dir(path)); err == nil {
		if parent.Mode()&fs.ModeSymlink != 0 || !parent.IsDir() {
			return fmt.Errorf("%s is not a real directory, so %s is left alone", filepath.Dir(path), path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	info, err := host.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file, so it is left alone", path)
	}
	return nil
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
// (gt-loqew).
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
// another (gt-loqew).
func envHash(goflags string) string {
	return hashString("GOFLAGS=" + goflags)
}

// goflags is the GOFLAGS a run's children inherit, read only from the host's
// environment: a host that wires no Env has none to read, so it is empty and the
// process's own environment is never consulted (gt-loqew).
func goflags(host Host) string {
	if host.Env == nil {
		return ""
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
