package battery

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/steveyegge/gastown/internal/lock"
)

// Append adds v to the log at path as one JSON line, creating the directory
// 0700 and the file 0600. When the file does not end in a newline, a short
// write or a crash left a partial line there: the new line starts on its own
// line, so the partial one stays unusable and the new one still parses.
//
// The tail check and the write run under an exclusive lock on a sibling
// .flock file, so no other Append can land between them: a concurrent writer
// mid-line would otherwise have its line split by ours.
func Append(path string, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding the log line: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the log directory: %w", err)
	}
	unlock, err := lock.FlockAcquire(path + ".flock")
	if err != nil {
		return fmt.Errorf("locking the log: %w", err)
	}
	defer unlock()
	partial, err := endsMidLine(path)
	if err != nil {
		return fmt.Errorf("reading the log's tail: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening the log: %w", err)
	}
	buf := append(line, '\n')
	if partial {
		buf = append([]byte{'\n'}, buf...)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the log: %w", err)
	}
	return f.Close()
}

// endsMidLine reports whether the log at path is non-empty and its last byte
// is not a newline, which is what a short write leaves behind. A missing log
// is not mid-line.
func endsMidLine(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if fi.Size() == 0 {
		return false, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}

// ReadRecent reads the last n lines of the log at path into into, which must
// be a pointer to a slice, oldest first. A missing log is empty, not an error,
// and a line that does not parse is skipped before the last n are taken.
func ReadRecent(path string, n int, into any) error {
	ptr := reflect.ValueOf(into)
	if ptr.Kind() != reflect.Pointer || ptr.IsNil() || ptr.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("reading the log: into must be a pointer to a slice, not %T", into)
	}
	dst := ptr.Elem()
	elem := dst.Type().Elem()
	dst.Set(reflect.Zero(dst.Type()))

	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening the log: %w", err)
	}
	defer func() { _ = f.Close() }()

	lines := reflect.MakeSlice(dst.Type(), 0, 0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		v := reflect.New(elem)
		if err := json.Unmarshal([]byte(text), v.Interface()); err != nil {
			continue
		}
		lines = reflect.Append(lines, v.Elem())
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading the log: %w", err)
	}
	if n < lines.Len() {
		lines = lines.Slice(lines.Len()-n, lines.Len())
	}
	if lines.Len() > 0 {
		dst.Set(lines)
	}
	return nil
}
