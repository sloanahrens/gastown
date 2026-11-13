package bdgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultCallLogSize is the size in bytes at which the live call log rotates,
	// the design's 50 MB (docs/design/bd-container.md, "The call log").
	DefaultCallLogSize int64 = 50 << 20
	// DefaultCallLogFiles is how many call log files are kept, the live file and
	// its backups together: the design's seven files.
	DefaultCallLogFiles = 7
	// DefaultCallLogBuffer is how many lines may wait for the writer before a
	// line is dropped rather than block the call.
	DefaultCallLogBuffer = 1024
	// DefaultCallLogName is the live file's name inside the log directory.
	DefaultCallLogName = "call.log"
)

// The per-verb counter map's bound. bd's allowlist is far smaller than
// maxVerbKeys, so the cap leaves room for a longer list while keeping a
// caller's argv from growing Stats without bound.
const (
	// maxVerbKeys is the most distinct verb keys Stats keeps.
	maxVerbKeys = 128
	// otherVerb is the key every verb past maxVerbKeys is counted under.
	otherVerb = "(other)"
	// maxVerbLen bounds a verb: bd's longest command word is shorter, and the
	// bound keeps a long token from being stored as a verb at all.
	maxVerbLen = 32
	// unparsedVerb is the verb an argv the policy scanner cannot place is
	// counted under, so an unreadable call is still visible in Stats.
	unparsedVerb = "unparsed"
	// maxFieldBytes bounds the class and caller strings: the caller writes
	// X-Gt-Caller, so a long one must not grow a line without bound.
	maxFieldBytes = 64
)

// CallLogOptions configures a CallLog.
type CallLogOptions struct {
	// Dir is the mounted log volume's directory; it is created if missing.
	Dir string
	// Policy is the loaded verbs file whose scanner reads argv for the log, so
	// the log and the policy can never disagree about which token is a flag, a
	// flag's value or an operand. It is required.
	Policy *Policy
	// MaxBytes is the size the live file may not exceed when the next line would
	// cross it; zero means DefaultCallLogSize.
	MaxBytes int64
	// MaxFiles counts the live file and its rotated backups; zero means
	// DefaultCallLogFiles.
	MaxFiles int
	// Buffer is how many lines wait for the writer; zero means
	// DefaultCallLogBuffer.
	Buffer int
	// BeadPrefixes names every bead id prefix the town uses, without the hyphen
	// ("gt", "hq"). A token is logged as a bead id only when its prefix is named
	// here, so a title that merely looks like an id is not (gt-itjuwu).
	BeadPrefixes []string
	// Now stamps each line; nil means time.Now.
	Now func() time.Time
}

// CallRecord is one bd call as the gateway observed it.
type CallRecord struct {
	// Class is the caller's token class (auth).
	Class string
	// Caller is the X-Gt-Caller name.
	Caller string
	// Argv is bd's arguments. Its verb, flag names and bead ids are logged; a
	// flag's value and every other operand are data and stay out.
	Argv []string
	// ExitCode is bd's exit status; non-zero counts the call as an error.
	ExitCode int
	// Duration is how long the call took.
	Duration time.Duration
}

// Stats is a snapshot of the log's counters, which GET /v1/stats serves.
type Stats struct {
	// Calls is how many calls Log was given.
	Calls uint64 `json:"calls"`
	// Errors is how many of them exited non-zero.
	Errors uint64 `json:"errors"`
	// Dropped is how many log lines were not written: the buffer was full, the
	// log was closed, the line could not be marshaled, or the writer could not
	// store it.
	Dropped uint64 `json:"dropped"`
	// Verbs counts calls by their verb, the first command word of argv. An argv
	// the scanner cannot place is counted under unparsedVerb. The key set is
	// bounded: past maxVerbKeys distinct verbs, calls fall under otherVerb.
	Verbs map[string]uint64 `json:"verbs"`
}

// entry is one JSONL line. The field set is fixed — ts, class, caller, verb,
// flags, beads, exit code and duration — so argv data cannot ride along unseen.
type entry struct {
	TS         string   `json:"ts"`
	Class      string   `json:"class"`
	Caller     string   `json:"caller"`
	Verb       string   `json:"verb"`
	Flags      []string `json:"flags"`
	Beads      []string `json:"beads"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
}

// CallLog writes one JSONL line per bd call behind a buffered channel, rotates
// the file by size, and counts calls for GET /v1/stats.
type CallLog struct {
	// sendMu is held read while a line is queued and write while the channel is
	// closed, so a queued line can never follow a close.
	sendMu sync.RWMutex
	// mu guards the counters, err and closed; the writer takes it only to record
	// a write error.
	mu      sync.Mutex
	closed  bool
	err     error
	calls   uint64
	errors  uint64
	dropped uint64
	verbs   map[string]uint64

	ch   chan []byte
	done chan struct{}

	now      func() time.Time
	prefixes map[string]bool
	policy   *Policy
}

// NewCallLog opens the live log in opts.Dir and starts its writer.
func NewCallLog(opts CallLogOptions) (*CallLog, error) {
	return newCallLog(opts, nil)
}

// newCallLog is NewCallLog with a test seam: a non-nil hook writes each line
// instead of the file, so a test can park the writer without a disk.
func newCallLog(opts CallLogOptions, hook func([]byte) error) (*CallLog, error) {
	if opts.Dir == "" {
		return nil, errors.New("bdgateway: call log needs a directory")
	}
	if opts.Policy == nil {
		return nil, errors.New("bdgateway: call log needs the policy that scans argv")
	}
	prefixes, err := prefixSet(opts.BeadPrefixes)
	if err != nil {
		return nil, err
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultCallLogSize
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultCallLogFiles
	}
	if opts.Buffer <= 0 {
		opts.Buffer = DefaultCallLogBuffer
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	var w *logWriter
	if hook == nil {
		w = newLogWriter(filepath.Join(opts.Dir, DefaultCallLogName), opts.MaxBytes, opts.MaxFiles)
		if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
			return nil, fmt.Errorf("bdgateway: create call log directory: %w", err)
		}
		if err := w.open(); err != nil {
			return nil, err
		}
	} else {
		w = &logWriter{hook: hook}
	}
	l := &CallLog{
		ch:       make(chan []byte, opts.Buffer),
		done:     make(chan struct{}),
		now:      now,
		prefixes: prefixes,
		policy:   opts.Policy,
		verbs:    make(map[string]uint64),
	}
	go l.run(w)
	return l, nil
}

// prefixSet validates the configured bead id prefixes and returns them as a set.
// A missing list is refused rather than guessed from the token's shape: without
// prefixes a title like "refactor-sling" would read as a bead id and be logged.
func prefixSet(prefixes []string) (map[string]bool, error) {
	if len(prefixes) == 0 {
		return nil, errors.New("bdgateway: call log needs the town's bead id prefixes")
	}
	set := make(map[string]bool, len(prefixes))
	for _, p := range prefixes {
		if !prefixRe.MatchString(p) {
			return nil, fmt.Errorf("bdgateway: call log bead prefix %q is not a prefix", p)
		}
		set[p] = true
	}
	return set, nil
}

var (
	// prefixRe is a bead id prefix: lowercase letters and digits, starting with a
	// letter, without the hyphen it is joined to the id with.
	prefixRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
	// beadSuffixRe is what follows the prefix and hyphen: lowercase alphanumerics
	// with hyphen or dot segments (gt-s3rec.7, hq-cv-abc).
	beadSuffixRe = regexp.MustCompile(`^[a-z0-9]+(?:[-.][a-z0-9]+){0,3}$`)
)

// Log records one call. It never blocks: a full buffer or a closed log drops the
// line and counts the drop, and a line is discarded rather than delay the call
// (gt-itjuwu).
func (l *CallLog) Log(rec CallRecord) {
	parsed := l.parseArgv(rec.Argv)
	e := entry{
		TS:         l.now().UTC().Format(time.RFC3339Nano),
		Class:      boundText(rec.Class),
		Caller:     boundText(rec.Caller),
		Verb:       parsed.verb,
		Flags:      parsed.flags,
		Beads:      parsed.beads,
		ExitCode:   rec.ExitCode,
		DurationMS: rec.Duration.Milliseconds(),
	}
	line, marshalErr := json.Marshal(e)

	l.sendMu.RLock()
	l.mu.Lock()
	l.calls++
	if rec.ExitCode != 0 {
		l.errors++
	}
	if parsed.verb != "" {
		l.countVerb(parsed.verb)
	}
	if marshalErr != nil {
		l.dropped++
		if l.err == nil {
			l.err = fmt.Errorf("bdgateway: marshal call log line: %w", marshalErr)
		}
		l.mu.Unlock()
		l.sendMu.RUnlock()
		return
	}
	if l.closed {
		l.dropped++
		l.mu.Unlock()
		l.sendMu.RUnlock()
		return
	}
	l.mu.Unlock()
	select {
	case l.ch <- append(line, '\n'):
	default:
		l.mu.Lock()
		l.dropped++
		l.mu.Unlock()
	}
	l.sendMu.RUnlock()
}

// countVerb counts one call under verb, keeping the map bounded. The caller
// holds l.mu.
func (l *CallLog) countVerb(verb string) {
	if _, ok := l.verbs[verb]; !ok && len(l.verbs) >= maxVerbKeys {
		verb = otherVerb
	}
	l.verbs[verb]++
}

// Stats returns a copy of the counters.
func (l *CallLog) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	verbs := make(map[string]uint64, len(l.verbs))
	for verb, n := range l.verbs {
		verbs[verb] = n
	}
	return Stats{Calls: l.calls, Errors: l.errors, Dropped: l.dropped, Verbs: verbs}
}

// Close stops the writer after the lines already queued, and returns the first
// write error seen.
func (l *CallLog) Close() error {
	l.sendMu.Lock()
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		l.sendMu.Unlock()
		<-l.done
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.err
	}
	l.closed = true
	close(l.ch)
	l.mu.Unlock()
	l.sendMu.Unlock()
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// run writes every queued line in order and exits when Close closes the queue.
func (l *CallLog) run(w *logWriter) {
	defer close(l.done)
	defer func() {
		if err := w.close(); err != nil {
			l.setErr(err)
		}
	}()
	for line := range l.ch {
		if err := w.write(line); err != nil {
			l.setErr(err)
			l.drop()
		}
	}
}

// setErr keeps the first error: later ones are consequences of it.
func (l *CallLog) setErr(err error) {
	l.mu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.mu.Unlock()
}

// drop counts a line the writer could not store, so Stats reports a failing log
// as failing rather than clean (gt-itjuwu review).
func (l *CallLog) drop() {
	l.mu.Lock()
	l.dropped++
	l.mu.Unlock()
}

// logParts is the part of argv a line keeps.
type logParts struct {
	verb  string
	flags []string
	beads []string
}

// parseArgv reads argv with the policy's own scanner, so the log and the policy
// can never disagree about which token is a flag, a flag's value or an operand.
// It keeps the first command word as the verb, each flag's name (never a
// value), and the operands that are bead ids. Operands that are not bead ids —
// titles, descriptions, comment bodies — are never kept; a command flag's arity
// is not in bd's tree, so an id-shaped token after one is read as an operand and
// kept.
//
// An argv the scanner cannot place yields no flags and no ids and is counted
// under unparsedVerb, so an unreadable call is visible rather than blank
// (gt-itjuwu review).
func (l *CallLog) parseArgv(argv []string) logParts {
	parts := logParts{flags: []string{}, beads: []string{}}
	scan, err := l.policy.scanArgs(argv)
	if err != nil {
		parts.verb = unparsedVerb
		return parts
	}
	if len(scan.words) > 0 && isVerb(scan.words[0]) {
		parts.verb = scan.words[0]
	}
	for _, flag := range scan.flags {
		// A clustered shorthand may be part of the first one's value, so only
		// the first in a token is a name for certain.
		if flag.clustered || !bareFlag.MatchString(flag.name) {
			continue
		}
		parts.flags = append(parts.flags, flag.spelling)
	}
	for _, operand := range scan.operands {
		if isBeadID(operand, l.prefixes) {
			parts.beads = append(parts.beads, operand)
		}
	}
	return parts
}

// isVerb reports whether token may be stored and counted as a verb: a bd
// command word, bounded in length so a long argv token is not kept as one.
func isVerb(token string) bool {
	return len(token) <= maxVerbLen && pathWord.MatchString(token)
}

// isBeadID reports whether token is "<known prefix>-<id>" in bead id form.
func isBeadID(token string, prefixes map[string]bool) bool {
	prefix, rest, ok := strings.Cut(token, "-")
	if !ok || !prefixes[prefix] || len(rest) > 64 {
		return false
	}
	return beadSuffixRe.MatchString(rest)
}

// boundText strips control characters and truncates s to maxFieldBytes without
// splitting a rune.
func boundText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxFieldBytes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// logFile is the part of *os.File the writer uses; a test substitutes one whose
// methods fail to drive each error path.
type logFile interface {
	Write([]byte) (int, error)
	Stat() (os.FileInfo, error)
	Close() error
}

// logWriter owns the live file and its rotation. Only the writer goroutine
// touches it after construction.
type logWriter struct {
	path     string
	maxBytes int64
	maxFiles int
	file     logFile
	size     int64
	hook     func([]byte) error

	openFile func(string) (logFile, error)
	rename   func(oldpath, newpath string) error
	remove   func(name string) error
}

func newLogWriter(path string, maxBytes int64, maxFiles int) *logWriter {
	return &logWriter{
		path: path, maxBytes: maxBytes, maxFiles: maxFiles,
		openFile: func(name string) (logFile, error) {
			return os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		},
		rename: os.Rename,
		remove: os.Remove,
	}
}

func (w *logWriter) open() error {
	f, err := w.openFile(w.path)
	if err != nil {
		return fmt.Errorf("bdgateway: open call log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		// Both failures matter and both are reported: the close error would
		// otherwise vanish behind the stat error.
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return fmt.Errorf("bdgateway: stat call log: %w", err)
	}
	w.file = f
	w.size = info.Size()
	return nil
}

// write appends one line, rotating first when it would carry the live file over
// maxBytes. A single line larger than maxBytes is written to a file of its own;
// a line is never split. Rotation and write failures are returned, never
// swallowed.
func (w *logWriter) write(line []byte) error {
	if w.hook != nil {
		return w.hook(line)
	}
	if w.file == nil {
		if err := w.open(); err != nil {
			return err
		}
	}
	if w.size > 0 && w.size+int64(len(line)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return err
		}
		if err := w.open(); err != nil {
			return err
		}
	}
	n, err := w.file.Write(line)
	w.size += int64(n)
	if err != nil {
		return fmt.Errorf("bdgateway: write call log: %w", err)
	}
	return nil
}

// rotate closes the live file, shifts name.N to name.N+1 up to the kept files,
// and renames the live file to name.1. With one file kept the live file is
// removed instead.
func (w *logWriter) rotate() error {
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		w.size = 0
		if err != nil {
			return fmt.Errorf("bdgateway: close call log for rotation: %w", err)
		}
	}
	if w.maxFiles <= 1 {
		return w.removeIfExists(w.path)
	}
	if err := w.removeIfExists(backupPath(w.path, w.maxFiles-1)); err != nil {
		return err
	}
	for i := w.maxFiles - 2; i >= 1; i-- {
		if err := w.renameIfExists(backupPath(w.path, i), backupPath(w.path, i+1)); err != nil {
			return err
		}
	}
	return w.renameIfExists(w.path, backupPath(w.path, 1))
}

func (w *logWriter) close() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	if err != nil {
		return fmt.Errorf("bdgateway: close call log: %w", err)
	}
	return nil
}

func backupPath(path string, n int) string {
	return path + "." + strconv.Itoa(n)
}

func (w *logWriter) removeIfExists(path string) error {
	err := w.remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("bdgateway: remove call log %s: %w", path, err)
}

func (w *logWriter) renameIfExists(from, to string) error {
	err := w.rename(from, to)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("bdgateway: rotate call log %s to %s: %w", from, to, err)
}
