// Package daemonlog reads ~/gt/daemon/daemon.log and the rotated backups
// lumberjack keeps beside it. gt tail follows the log by byte offset and the
// hour report reads a window of it; both parse a line and find its backups
// here, so they cannot disagree on either.
//
// The live log holds three clock regions (gt-gg9hv.44): the Mac daemon's local
// stamps, the container's UTC stamps from an image with no zone database, and
// local stamps again once the image carried tzdata. Parser tracks which region
// it is in, so a reader that sees the region changes dates each line by its
// true instant.
package daemonlog

import (
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Stamp is log.LstdFlags as the daemon's logger writes it: a wall clock with no
// zone.
const Stamp = "2006/01/02 15:04:05"

// FileName is the live log inside the daemon directory.
const FileName = "daemon.log"

// FutureSkew is how far ahead of the reader's clock a stamp may read before the
// parser takes it for UTC: the tail readers' backstop, since they begin after
// the log's zone markers.
const FutureSkew = 2 * time.Minute

// backupRe matches lumberjack's rotated names; the stamp is the rotation time
// in UTC, so a backup holds only lines older than it.
var backupRe = regexp.MustCompile(`^daemon-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}\.\d{3})\.log(\.gz)?$`)

// Line is one log line split into the instant it carries and its text.
type Line struct {
	// At is the line's instant. A line with no stamp of its own carries the
	// instant of the entry above it.
	At time.Time
	// Text is the message after the stamp, or the whole line when there is no
	// stamp.
	Text string
	// Stamped is whether the line carried its own stamp, so a continuation of
	// the entry above it is Stamped false and a reader that needs a line's own
	// time can tell it apart.
	Stamped bool
}

// Parser splits raw lines into time and text. A line without the stamp keeps
// its whole text and the previous line's time, so it belongs to the entry
// above it. One Parser reads one log in order, backups first.
//
// The zone a stamp is read in starts as Loc and follows the log: a
// "log timezone: <zone> (UTC<offset>)" line switches it for the lines after
// it. That marker is recognized only at the daemon's own line shape — a stamp
// followed by the fixed text at the start of the message — so a bead title or
// an error message that merely contains the text never switches the zone. A
// marker naming a zone this host cannot load is reported once for each
// distinct marker text (one line through Logf, naming the marker and not the
// file) and the zone already in force is kept.
//
// A start sequence ending in "Daemon owner: container" switches the zone to
// UTC for that sequence and the lines after it. The owner line comes after the
// sequence's own lines, so Feed and Read hold a start sequence back until its
// owner is known; Parse cannot, and a tail reader's backstop is the future
// guard instead: a stamp that reads later than Now plus FutureSkew is re-read
// as UTC, the only way a stamp here can be in the future. The guard cannot
// correct a UTC-region line older than the offset (it reads as the past in
// local time), which is why a whole-file reader uses the sequence.
type Parser struct {
	// Loc is the zone a stamp is read in until the log names another; nil is UTC.
	Loc *time.Location
	// Now is the clock the future guard reads; nil is time.Now.
	Now func() time.Time
	// LoadZone loads a zone a timezone marker names; nil is time.LoadLocation.
	LoadZone func(string) (*time.Location, error)
	// Logf reports a timezone marker whose zone cannot be loaded; nil is
	// log.Printf. Each distinct marker text is reported once.
	Logf func(format string, args ...any)

	last time.Time
	zone *time.Location
	// reported holds the marker texts already reported unloadable, so a log
	// that repeats one does not repeat the report.
	reported map[string]bool

	// group holds the raw lines of a start sequence whose owner is not known
	// yet. await says the last of them named its owner, and awaitZone is the
	// zone to resolve the group in unless the line after it is a timezone
	// marker.
	group     []string
	await     bool
	awaitZone *time.Location
}

// Parse returns the line's time and text and moves the parser to the zone the
// line names; a start sequence is read in place, so a read that must date its
// own lines uses Feed, or Read.
func (p *Parser) Parse(raw string) (time.Time, string) {
	if at, text, ok := p.split(raw, p.current()); ok {
		p.observe(text)
		p.last = at
		return at, text
	}
	return p.last, raw
}

// Feed parses one line of a stream and returns the lines ready to emit: one,
// none while a start sequence's owner is still unknown, or several when an
// owner line resolves the sequence held so far.
func (p *Parser) Feed(raw string) []Line {
	var out []Line
	if p.await {
		zone := p.awaitZone
		if _, text, ok := p.split(raw, p.current()); ok {
			if name, isMarker := zoneMarker(text); isMarker {
				if z, ok := p.zoneFor(name, text); ok {
					zone = z
				}
			}
		}
		out = append(out, p.resolve(zone)...)
	}

	at, text, ok := p.split(raw, p.current())
	if !ok {
		if len(p.group) > 0 {
			p.group = append(p.group, raw)
			return out
		}
		return append(out, Line{At: p.last, Text: raw})
	}
	if len(p.group) > 0 || startLine(text) {
		if startPhase(text) {
			p.group = append(p.group, raw)
			p.last = at
			if kind, isOwner := ownerKind(text); isOwner {
				p.await = true
				p.awaitZone = p.current()
				if kind == containerOwner {
					p.awaitZone = time.UTC
				}
			}
			return out
		}
		if len(p.group) > 0 {
			out = append(out, p.resolve(p.current())...)
		}
	}
	p.observe(text)
	p.last = at
	return append(out, Line{At: at, Text: text, Stamped: true})
}

// Flush returns the lines of a start sequence whose owner never came, so end a
// read with it.
func (p *Parser) Flush() []Line {
	if len(p.group) == 0 {
		return nil
	}
	zone := p.current()
	if p.await {
		zone = p.awaitZone
	}
	return p.resolve(zone)
}

// resolve emits a held start sequence in zone and makes it the current zone.
func (p *Parser) resolve(zone *time.Location) []Line {
	group := p.group
	p.group = nil
	p.await = false
	out := make([]Line, 0, len(group))
	for _, raw := range group {
		at, text, ok := p.split(raw, zone)
		if !ok {
			out = append(out, Line{At: p.last, Text: raw})
			continue
		}
		p.last = at
		out = append(out, Line{At: at, Text: text, Stamped: true})
	}
	p.zone = zone
	return out
}

// split parses raw's stamp in zone and applies the future guard. ok is false
// for a line without a stamp.
func (p *Parser) split(raw string, zone *time.Location) (time.Time, string, bool) {
	if len(raw) < len(Stamp) {
		return time.Time{}, raw, false
	}
	at, err := time.ParseInLocation(Stamp, raw[:len(Stamp)], zone)
	if err != nil {
		return time.Time{}, raw, false
	}
	return p.guard(at, raw[:len(Stamp)]), strings.TrimPrefix(raw[len(Stamp):], " "), true
}

// guard re-reads a stamp as UTC when it would otherwise be later than now plus
// FutureSkew. It takes the UTC reading only when that moves the instant
// earlier, so a zone east of UTC — where the same wall clock as UTC is later —
// never pushes a line further into the future.
func (p *Parser) guard(at time.Time, stamp string) time.Time {
	if !at.After(p.now().Add(FutureSkew)) {
		return at
	}
	utc, err := time.ParseInLocation(Stamp, stamp, time.UTC)
	if err != nil || !utc.Before(at) {
		return at
	}
	return utc
}

func (p *Parser) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Parser) current() *time.Location {
	if p.zone != nil {
		return p.zone
	}
	return p.Loc
}

// observe moves the parser to the zone a line names.
func (p *Parser) observe(text string) {
	if name, ok := zoneMarker(text); ok {
		if z, ok := p.zoneFor(name, text); ok {
			p.zone = z
		}
		return
	}
	if kind, ok := ownerKind(text); ok && kind == containerOwner {
		p.zone = time.UTC
	}
}

// zoneFor loads the zone a marker names. A zone this host cannot load is
// reported once for its marker text and ok is false, so the caller keeps the
// zone it already had.
func (p *Parser) zoneFor(name, marker string) (*time.Location, bool) {
	z, err := p.loadZone(name)
	if err != nil {
		p.reportUnloadable(marker)
		return nil, false
	}
	return z, true
}

// reportUnloadable says once, for each distinct marker text, that the zone it
// names cannot be loaded and that the previous zone stays in force. The report
// names the log's own marker text — never the file — so an operator can find
// the line.
func (p *Parser) reportUnloadable(marker string) {
	if p.reported == nil {
		p.reported = make(map[string]bool)
	}
	if p.reported[marker] {
		return
	}
	p.reported[marker] = true
	logf := p.Logf
	if logf == nil {
		logf = log.Printf
	}
	logf("daemonlog: timezone marker %q names a zone that cannot be loaded; keeping the previous zone", marker)
}

func (p *Parser) loadZone(name string) (*time.Location, error) {
	if p.LoadZone != nil {
		return p.LoadZone(name)
	}
	return time.LoadLocation(name)
}

// containerOwner is the owner kind of a daemon running in a container.
const containerOwner = "container"

// zoneMarkerRe matches the daemon's own zone marker, which it writes as
// "log timezone: <zone> (UTC<offset>)". The whole message must be the marker,
// so a bead title or error text that merely contains it never matches.
var zoneMarkerRe = regexp.MustCompile(`^log timezone: (\S+) \(UTC[+-]\d\d:\d\d\)$`)

func zoneMarker(text string) (string, bool) {
	m := zoneMarkerRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ownerLineRe matches the daemon's own owner line, which it writes as
// "Daemon owner: <kind> <id>" with the id quoted by %q. A log line that merely
// mentions the owner text has another shape and never matches.
var ownerLineRe = regexp.MustCompile(`^Daemon owner: (\S+) ".*"$`)

// MayChangeZone reports whether a raw line can carry a zone change, so a
// reader that filters lines before parsing keeps the ones a Parser must see.
// It is a cheap substring test: true does not mean the line changes the zone,
// only that it cannot be skipped before the parser reads it.
func MayChangeZone(raw string) bool {
	return strings.Contains(raw, "log timezone: ") || strings.Contains(raw, "Daemon owner: ")
}

func ownerKind(text string) (string, bool) {
	m := ownerLineRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// startLine opens a start sequence: the line the daemon writes once it has a
// patrol config.
func startLine(text string) bool { return strings.HasPrefix(text, "Loaded patrol config from ") }

// startPhase reports whether text is a line a start writes between its
// "Loaded patrol config" line and its "Daemon owner" line, so the sequence can
// be held without knowing its owner yet.
func startPhase(text string) bool {
	for _, prefix := range []string{
		"Loaded patrol config from ",
		"Set env ",
		"Patrols disabled via town settings:",
		"Dolt server management ",
		"Warning: ",
		"Daemon starting (PID ",
		"Daemon startup failed (PID ",
		"Daemon owner: ",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
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
		bs = append(bs, backup{path: filepath.Join(dir, e.Name()), at: at})
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
// backups that can hold one, then the live file, in order. Each line is dated
// in the zone the log was writing in at that point. A line without a stamp
// keeps its entry's time; one before any stamp is dropped. The live file's
// last line is read only once its newline is written. A missing daemon.log is
// no lines, not an error; any other read failure is returned with the lines
// read before it.
func Read(dir string, cutoff time.Time, loc *time.Location) ([]Line, error) {
	var out []Line
	p := &Parser{Loc: loc}
	emit := func(lines []Line) {
		for _, l := range lines {
			if l.At.IsZero() || l.At.Before(cutoff) {
				continue
			}
			out = append(out, l)
		}
	}
	feed := func(raw string) { emit(p.Feed(raw)) }
	for _, b := range Backups(dir, cutoff) {
		if err := ReadBackup(b, feed); err != nil {
			emit(p.Flush())
			return out, err
		}
	}
	f, err := os.Open(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		emit(p.Flush())
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()
	scanErr := scan(f, true, feed)
	emit(p.Flush())
	return out, scanErr
}
