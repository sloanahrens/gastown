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

// CallLogOptions configures a CallLog.
type CallLogOptions struct {
	// Dir is the mounted log volume's directory; it is created if missing.
	Dir string
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
	// log was closed, or the line could not be marshaled.
	Dropped uint64 `json:"dropped"`
	// Verbs counts calls by their verb, the first operand of argv.
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
		Class:      rec.Class,
		Caller:     rec.Caller,
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
		l.verbs[parsed.verb]++
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

// parsedArgv is the part of argv the log keeps.
type parsedArgv struct {
	verb  string
	flags []string
	beads []string
}

// parseArgv keeps the first operand as the verb, every flag's name, and the
// operands that are bead ids. A token after a flag is that flag's value and is
// never kept, whether or not it resembles an id: argv carries bead titles and
// comment bodies, and dropping a misplaced id is safer than logging a title.
func (l *CallLog) parseArgv(argv []string) parsedArgv {
	p := parsedArgv{flags: []string{}, beads: []string{}}
	pastSeparator, valueNext := false, false
	for _, arg := range argv {
		switch {
		case valueNext:
			valueNext = false
		case pastSeparator:
			p.addOperand(arg, l.prefixes)
		case arg == "--":
			pastSeparator = true
		case isFlagToken(arg):
			name, _, inline := strings.Cut(arg, "=")
			p.flags = append(p.flags, name)
			valueNext = !inline
		default:
			p.addOperand(arg, l.prefixes)
		}
	}
	return p
}

// addOperand names the verb from the first operand and records later operands
// that are bead ids.
func (p *parsedArgv) addOperand(arg string, prefixes map[string]bool) {
	if p.verb == "" {
		p.verb = arg
		return
	}
	if isBeadID(arg, prefixes) {
		p.beads = append(p.beads, arg)
	}
}

// isBeadID reports whether token is "<known prefix>-<id>" in bead id form.
func isBeadID(token string, prefixes map[string]bool) bool {
	prefix, rest, ok := strings.Cut(token, "-")
	if !ok || !prefixes[prefix] || len(rest) > 64 {
		return false
	}
	return beadSuffixRe.MatchString(rest)
}

// isFlagToken reports a flag rather than an operand; a lone "-" is an operand.
func isFlagToken(arg string) bool {
	return len(arg) > 1 && arg[0] == '-'
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
		f.Close()
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
