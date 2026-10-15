// Package daemonlog reads ~/gt/daemon/daemon.log and the rotated backups
// lumberjack keeps beside it. gt tail follows the log by byte offset and the
// hour report reads a window of it; both parse a line and find its backups
// here, so they cannot disagree on either.
package daemonlog

import (
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Stamp is log.LstdFlags as the daemon's logger writes it: local time, no
// zone.
const Stamp = "2006/01/02 15:04:05"

// FileName is the live log inside the daemon directory.
const FileName = "daemon.log"

// backupRe matches lumberjack's rotated names; the stamp is the rotation time
// in UTC, so a backup holds only lines older than it.
var backupRe = regexp.MustCompile(`^daemon-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}\.\d{3})\.log(\.gz)?$`)

// Line is one log line split into the instant it carries and its text.
type Line struct {
	At   time.Time
	Text string
}

// Parser splits raw lines into time and text. A line without the stamp keeps
// its whole text and the previous line's time, so it belongs to the entry
// above it. One Parser reads one log in order, backups first.
type Parser struct {
	Loc  *time.Location
	last time.Time
}

// Parse returns the line's time and text.
func (p *Parser) Parse(raw string) (time.Time, string) {
	if len(raw) >= len(Stamp) {
		if t, err := time.ParseInLocation(Stamp, raw[:len(Stamp)], p.Loc); err == nil {
			p.last = t
			return t, strings.TrimPrefix(raw[len(Stamp):], " ")
		}
	}
	return p.last, raw
}

// Backups returns the rotated logs in dir whose rotation time is at or after
// cutoff, oldest first.
func Backups(dir string, cutoff time.Time) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type backup struct {
		path string
		at   time.Time
	}
	var bs []backup
	for _, e := range entries {
		m := backupRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02T15-04-05.000", m[1], time.UTC)
		if err != nil || at.Before(cutoff) {
			continue
		}
		bs = append(bs, backup{filepath.Join(dir, e.Name()), at})
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].at.Before(bs[j].at) })
	paths := make([]string, len(bs))
	for i, b := range bs {
		paths[i] = b.path
	}
	return paths
}

// ReadBackup calls emit with each line of a rotated log, gunzipping a .gz
// one.
func ReadBackup(path string, emit func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}
	return scan(r, false, emit)
}

// scan calls emit with each line of r. A final line with no newline is
// emitted only when completeOnly is false: in the live file it is still being
// written.
func scan(r io.Reader, completeOnly bool, emit func(string)) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err == io.EOF {
			if line != "" && !completeOnly {
				emit(strings.TrimRight(line, "\r\n"))
			}
			return nil
		}
		if err != nil {
			return err
		}
		emit(strings.TrimRight(line, "\r\n"))
	}
}

// Read returns every line of dir's logs stamped at or after cutoff: the
// backups that can hold one, then the live file, in order. A line without a
// stamp keeps its entry's time; one before any stamp is dropped. The live
// file's last line is read only once its newline is written. A missing
// daemon.log is no lines, not an error; any other read failure is returned
// with the lines read before it.
func Read(dir string, cutoff time.Time, loc *time.Location) ([]Line, error) {
	var out []Line
	p := &Parser{Loc: loc}
	emit := func(raw string) {
		at, text := p.Parse(raw)
		if at.IsZero() || at.Before(cutoff) {
			return
		}
		out = append(out, Line{At: at, Text: text})
	}
	for _, b := range Backups(dir, cutoff) {
		if err := ReadBackup(b, emit); err != nil {
			return out, err
		}
	}
	f, err := os.Open(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()
	return out, scan(f, true, emit)
}
