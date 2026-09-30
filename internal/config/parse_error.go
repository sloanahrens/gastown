package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrUnparseable marks a town config file that exists but does not decode
// into its Go type. gt fails closed on it: the town does not start, no
// session spawns, and no writer replaces the file (gt-fcxe9.10, G3-03, G3-04).
// A missing file is a different answer and is not this error.
var ErrUnparseable = errors.New("config file does not parse")

// ParseError names the file and where decoding failed.
type ParseError struct {
	Path   string
	Offset int64 // byte offset reported by the decoder, 0 when unknown
	Line   int   // 1-based, 0 when unknown
	Column int   // 1-based, 0 when unknown
	// Keys lists every undeclared key by dotted path, in file order, when
	// the file was rejected for unknown keys.
	Keys []string
	Err  error
}

func (e *ParseError) Error() string {
	where := "does not parse"
	if e.Offset > 0 {
		where = fmt.Sprintf("does not parse at offset %d (line %d, column %d)", e.Offset, e.Line, e.Column)
	} else if e.Line > 0 {
		where = fmt.Sprintf("does not parse at line %d", e.Line)
	}
	msg := strings.ReplaceAll(e.Err.Error(), "\n", " ")
	return fmt.Sprintf("%s: %s: %s; gt will not start the town from it and will never rewrite it: fix the file by hand", e.Path, where, msg)
}

// Is makes errors.Is(err, ErrUnparseable) true for every ParseError.
func (e *ParseError) Is(target error) bool { return target == ErrUnparseable }

func (e *ParseError) Unwrap() error { return e.Err }

// DecodeJSONFile is the one parser for town config files (gt-y3pgh.1). It
// decodes data (read from path) into v strictly: one JSON value, and every
// object key declared by v's type. Any failure is a *ParseError naming the
// file, the offset turned into a line and column, and for unknown keys every
// undeclared key path.
func DecodeJSONFile(path string, data []byte, v any) error {
	return decodeJSONStrict(path, data, v)
}

// lineColumn converts a decoder offset (bytes consumed, so the failing byte is
// at offset-1) into a 1-based line and column.
func lineColumn(data []byte, offset int64) (line, col int) {
	at := int(offset) - 1
	if at > len(data) {
		at = len(data)
	}
	if at < 0 {
		at = 0
	}
	before := data[:at]
	line = bytes.Count(before, []byte("\n")) + 1
	col = at - (bytes.LastIndexByte(before, '\n') + 1) + 1
	return line, col
}

// CheckJSONFileParses reports whether the file at path decodes into v. An
// absent file is nil; a present one that does not decode is a *ParseError;
// any other read failure is returned as is.
func CheckJSONFileParses(path string, v any) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return DecodeJSONFile(path, data, v)
}

// RegisterDaemonPatrolConfigCheck is a no-op kept until internal/daemon
// stops calling it: the daemon.json schema now lives in this package and the
// one writer decodes it strictly before every write.
//
// Deprecated: nothing needs to register a check.
func RegisterDaemonPatrolConfigCheck(func(path string) error) {}
