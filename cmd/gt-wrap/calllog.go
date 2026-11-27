package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// callLogMaxBytes is the size past which the live file rotates to calls.log.1.
const callLogMaxBytes int64 = 5 << 20

// callLogWords is how many leading arguments a line may name.
const callLogWords = 2

// callLogDirMode is the mode record gives the log's directory.
const callLogDirMode fs.FileMode = 0o755

// commandWord is the shape a recorded argument must have: a lowercase word.
var commandWord = regexp.MustCompile(`^[a-z][a-z-]*$`)

// callLogAppender is the open file a callLog writes one line to.
type callLogAppender interface {
	Write([]byte) (int, error)
	Close() error
}

// callLog appends one line per wrapped call under path. The function fields are
// seams, so a test fixes the clock and fails any file operation.
type callLog struct {
	path   string
	now    func() time.Time
	stat   func(string) (fs.FileInfo, error)
	mkdir  func(string, fs.FileMode) error
	rename func(string, string) error
	open   func(string) (callLogAppender, error)
	lock   func(string) (func(), error)
}

// osCallLog is the callLog under path on the operating system.
func osCallLog(path string) *callLog {
	return &callLog{path: path, now: time.Now, stat: os.Stat, mkdir: os.MkdirAll, rename: os.Rename, open: openAppend, lock: lock.FlockAcquire}
}

// record makes the log's directory, then appends the call's line: the lock does
// not make the directory, so a new town would fail every append. A failed step
// is one note on stderr, and nothing is appended under a directory not made.
func (l *callLog) record(stderr io.Writer, tool string, args []string, status int, took time.Duration, dir string) {
	logDir := filepath.Dir(l.path)
	if err := l.mkdir(logDir, callLogDirMode); err != nil {
		fmt.Fprintf(stderr, "gt-wrap: the call was not logged: make call log directory %s: %v\n", logDir, err)
		return
	}
	if err := l.append(tool, args, status, took, dir); err != nil {
		fmt.Fprintf(stderr, "gt-wrap: the call was not logged: %v\n", err)
	}
}

// openAppend opens path for one appended line.
func openAppend(path string) (callLogAppender, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // G304: path is the call log's configured path
}

// append writes one tab-separated line: the UTC time, tool, up to two leading
// command words, the exit status, the duration in milliseconds and dir. A file
// over callLogMaxBytes is renamed to path+".1" first, replacing the older
// rotation. The lock spans stat, rotate and append so a concurrent wrapper
// cannot lose the rotation. A failed lock, stat, rename, open, write or close
// is returned.
func (l *callLog) append(tool string, args []string, status int, d time.Duration, dir string) error {
	unlock, err := l.lock(l.path + ".lock")
	if err != nil {
		return fmt.Errorf("gt-wrap: lock call log %s: %w", l.path, err)
	}
	defer unlock()
	line := fmt.Sprintf("%s\t%s\t%s\t%d\t%d\t%s\n", l.now().UTC().Format(time.RFC3339),
		safeField(tool), strings.Join(commandWords(args), " "), status, d.Milliseconds(), safeField(dir))
	info, err := l.stat(l.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("gt-wrap: stat call log %s: %w", l.path, err)
	}
	if err == nil && info.Size() > callLogMaxBytes {
		if err := l.rename(l.path, l.path+".1"); err != nil {
			return fmt.Errorf("gt-wrap: rotate call log %s: %w", l.path, err)
		}
	}
	f, err := l.open(l.path)
	if err != nil {
		return fmt.Errorf("gt-wrap: append call log %s: %w", l.path, err)
	}
	_, werr := f.Write([]byte(line))
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("gt-wrap: append call log %s: %w", l.path, werr)
	}
	if cerr != nil {
		return fmt.Errorf("gt-wrap: close call log %s: %w", l.path, cerr)
	}
	return nil
}

// safeField replaces each control byte in a tool name or a directory with '?'
// so a newline or tab in either cannot forge a field or a whole log line.
func safeField(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// commandWords returns the arguments a line may name: each among the first
// callLogWords that matches commandWord, stopping at the first that does not.
// A word among those two is recorded even when it is a positional value; a
// later argument is not read, since it can hold a secret.
func commandWords(args []string) []string {
	var words []string
	for i := 0; i < len(args) && i < callLogWords; i++ {
		if !commandWord.MatchString(args[i]) {
			break
		}
		words = append(words, args[i])
	}
	return words
}
