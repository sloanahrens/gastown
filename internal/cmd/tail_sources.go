package cmd

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/landings"
)

// tailOnce remembers the last failure a source printed, so a source that
// keeps failing the same way prints one line, not one per poll. A success
// clears it, so the next failure prints again.
type tailOnce struct{ last string }

func (o *tailOnce) first(msg string) bool {
	if msg == o.last {
		return false
	}
	o.last = msg
	return true
}

func (o *tailOnce) clear() { o.last = "" }

// tailJournal is the part of beads.Admin gt tail reads: the events journal
// and its config key. It never writes.
type tailJournal interface {
	EventsTail(since int64, limit int) (*beads.EventsPage, error)
	ConfigGet(key string) (string, error)
}

// tailEventsPageSize bounds one bd events tail read.
const tailEventsPageSize = 500

// eventsSource tails one store's bd events journal by seq cursor. The
// cursor is a sequence, not a time, so the backlog is read from seq 0 and
// cut at the cutoff by each record's ts; later polls read from the cursor.
type eventsSource struct {
	rig      string
	journal  tailJournal
	openErr  error // the journal could not be reached at all
	cutoff   time.Time
	now      func() time.Time
	pageSize int

	started bool
	since   int64
	failed  tailOnce
}

func (s *eventsSource) line(at time.Time, format string, args ...any) tailLine {
	return tailLine{At: at, Rig: s.rig, Kind: tailKindEvents, Text: fmt.Sprintf(format, args...)}
}

func (s *eventsSource) Poll() []tailLine {
	now := s.now()
	if s.openErr != nil {
		if s.failed.first(s.openErr.Error()) {
			return []tailLine{s.line(now, "cannot read the journal: %v", s.openErr)}
		}
		return nil
	}
	var out []tailLine
	backlog := !s.started
	if backlog {
		s.started = true
		// The journal is off in production until the paired install turns it
		// on. gt's own bd calls journal regardless (BD_EVENTS_JOURNAL=1), so
		// the journal is read anyway; the operator is told it is partial.
		v, err := s.journal.ConfigGet("events-journal")
		switch {
		case err != nil:
			out = append(out, s.line(now, "cannot read events-journal config (%v): the journal may hold only mutations made through gt", err))
		case strings.TrimSpace(v) != "true":
			out = append(out, s.line(now, "journal off in config (events-journal=%s): only mutations made through gt are journaled", strings.TrimSpace(v)))
		}
	}
	size := s.pageSize
	if size <= 0 {
		size = tailEventsPageSize
	}
	for {
		page, err := s.journal.EventsTail(s.since, size)
		if err != nil {
			var trunc *beads.EventsTruncatedError
			if errors.As(err, &trunc) {
				resume := trunc.Floor - 1
				if resume <= s.since {
					resume = trunc.Head
				}
				if resume > s.since {
					out = append(out, s.line(now, "journal pruned past seq %d (oldest retained %d, head %d): records before %d are gone", s.since, trunc.Floor, trunc.Head, trunc.Floor))
					s.since = resume
					continue
				}
			}
			if s.failed.first(err.Error()) {
				out = append(out, s.line(now, "read failed: %v", err))
			}
			return out
		}
		s.failed.clear()
		for _, r := range page.Records {
			at, ok := parseJournalTS(r.TS)
			if backlog && ok && at.Before(s.cutoff) {
				continue
			}
			if !ok {
				at = now
			}
			text := r.Op + " " + r.IssueID
			if r.Status != "" {
				text += " status=" + r.Status
			}
			if r.Actor != "" {
				text += " actor=" + r.Actor
			}
			text += fmt.Sprintf(" seq=%d", r.Seq)
			if !ok {
				text += " ts=" + r.TS
			}
			out = append(out, s.line(at, "%s", text))
		}
		advanced := page.NextSince > s.since
		if advanced {
			s.since = page.NextSince
		}
		if !page.More || !advanced {
			return out
		}
	}
}

// parseJournalTS reads a journal record's ts. bd writes RFC3339 in UTC; the
// SQL datetime forms are accepted as UTC too.
func parseJournalTS(ts string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.ParseInLocation(layout, ts, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// landingsSource tails one rig's landings file.
type landingsSource struct {
	rig    string
	reader *landings.Reader
	cutoff time.Time
	now    func() time.Time

	started bool
	failed  tailOnce
}

func (s *landingsSource) Poll() []tailLine {
	now := s.now()
	backlog := !s.started
	s.started = true
	recs, bad, err := s.reader.ReadNew()
	if err != nil {
		if s.failed.first(err.Error()) {
			return []tailLine{{At: now, Rig: s.rig, Kind: tailKindLandings, Text: "read failed: " + err.Error()}}
		}
		return nil
	}
	s.failed.clear()
	var out []tailLine
	// A line that is not a record is counted, never echoed: the stream must
	// not republish whatever text ended up in the file.
	for _, b := range bad {
		out = append(out, tailLine{At: now, Rig: s.rig, Kind: tailKindLandings, Text: fmt.Sprintf("unreadable landings line (%d bytes)", len(b))})
	}
	for _, r := range recs {
		at := r.LandedAt
		if backlog && !at.IsZero() && at.Before(s.cutoff) {
			continue
		}
		if at.IsZero() {
			at = now
		}
		text := fmt.Sprintf("landed %s %s -> %s commit=%s patch=%s gate=%s om=%s/%.2f route=%s",
			orDash(r.Bead), orDash(r.Branch), orDash(r.Target), orDash(shortHash(r.LandedCommit)), orDash(shortHash(r.PatchID)),
			orDash(r.GateResult), orDash(r.OMVerdict), r.OMScore, orDash(r.Route))
		out = append(out, tailLine{At: at, Rig: s.rig, Kind: tailKindLandings, Text: text})
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// daemonLogStamp is log.LstdFlags as the daemon's logger writes it: local
// time, no zone.
const daemonLogStamp = "2006/01/02 15:04:05"

// daemonBackupRe matches lumberjack's rotated daemon.log names; the stamp is
// the rotation time in UTC, so a backup holds only lines older than it.
var daemonBackupRe = regexp.MustCompile(`^daemon-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}\.\d{3})\.log(\.gz)?$`)

// daemonSource tails ~/gt/daemon/daemon.log by byte offset. The first poll
// also reads the rotated backups that can hold lines after the cutoff. A
// line with no timestamp belongs to the previous timestamped line.
type daemonSource struct {
	dir       string
	cutoff    time.Time
	loc       *time.Location
	now       func() time.Time
	rigFilter *regexp.Regexp // nil = every line

	started bool
	offset  int64
	info    os.FileInfo
	lastAt  time.Time
	failed  tailOnce
}

// tailRigFilter matches a line naming rig as a whole word: "gastown" in
// "(from gastown)" and "gastown/jade", not in "gastownish" or "gastown-x".
func tailRigFilter(rig string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(rig) + `($|[^A-Za-z0-9_.-])`)
}

func (s *daemonSource) line(at time.Time, text string) tailLine {
	return tailLine{At: at, Rig: "town", Kind: tailKindDaemon, Text: text}
}

func (s *daemonSource) Poll() []tailLine {
	now := s.now()
	var out []tailLine
	backlog := !s.started
	s.started = true
	emit := func(raw string) {
		at, text := s.parse(raw)
		if backlog && (at.IsZero() || at.Before(s.cutoff)) {
			return
		}
		if at.IsZero() {
			at = now
		}
		if s.rigFilter != nil && !s.rigFilter.MatchString(text) {
			return
		}
		out = append(out, s.line(at, text))
	}
	if backlog {
		for _, b := range s.backups() {
			if err := readDaemonBackup(b, emit); err != nil {
				out = append(out, s.line(now, "cannot read "+filepath.Base(b)+": "+err.Error()))
			}
		}
	}
	if note, err := s.readCurrent(emit); err != nil {
		if s.failed.first(err.Error()) {
			out = append(out, s.line(now, "cannot read daemon.log: "+err.Error()))
		}
	} else {
		s.failed.clear()
		if note != "" {
			// The notice goes before the new file's lines.
			out = append([]tailLine{s.line(now, note)}, out...)
		}
	}
	return out
}

// parse splits a daemon.log line into its time and text. A line without the
// stamp keeps its whole text and the previous line's time.
func (s *daemonSource) parse(raw string) (time.Time, string) {
	if len(raw) >= len(daemonLogStamp) {
		if t, err := time.ParseInLocation(daemonLogStamp, raw[:len(daemonLogStamp)], s.loc); err == nil {
			s.lastAt = t
			return t, strings.TrimPrefix(raw[len(daemonLogStamp):], " ")
		}
	}
	return s.lastAt, raw
}

// backups returns the rotated logs whose rotation time is at or after the
// cutoff, oldest first.
func (s *daemonSource) backups() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	type backup struct {
		path string
		at   time.Time
	}
	var bs []backup
	for _, e := range entries {
		m := daemonBackupRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02T15-04-05.000", m[1], time.UTC)
		if err != nil || at.Before(s.cutoff) {
			continue
		}
		bs = append(bs, backup{filepath.Join(s.dir, e.Name()), at})
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].at.Before(bs[j].at) })
	paths := make([]string, len(bs))
	for i, b := range bs {
		paths[i] = b.path
	}
	return paths
}

func readDaemonBackup(path string, emit func(string)) error {
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
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			emit(strings.TrimRight(line, "\r\n"))
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readCurrent reads daemon.log's complete lines past the offset. A file that
// was replaced (lumberjack renamed it to a backup) or shrank is read from
// its start, and note says so.
func (s *daemonSource) readCurrent(emit func(string)) (note string, err error) {
	f, err := os.Open(filepath.Join(s.dir, "daemon.log"))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if s.info != nil && (!os.SameFile(s.info, info) || info.Size() < s.offset) {
		s.offset = 0
		note = "daemon.log was rotated; reading the new file from its start"
	}
	s.info = info
	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return note, err
	}
	br := bufio.NewReader(io.LimitReader(f, info.Size()-s.offset))
	for {
		line, err := br.ReadString('\n')
		if err == io.EOF {
			// A line with no newline yet is still being written: leave it.
			return note, nil
		}
		if err != nil {
			return note, err
		}
		s.offset += int64(len(line))
		emit(strings.TrimRight(line, "\r\n"))
	}
}
