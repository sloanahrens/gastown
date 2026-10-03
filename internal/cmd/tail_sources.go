package cmd

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/version"
)

// tailOnce remembers the last failure a source printed, so a source that
// keeps failing the same way prints one line, not one per poll. A success
// clears it, so the next failure prints again.
type tailOnce struct{ last string }

// tailTitleCacheSize bounds the titles one run remembers. A run that outlives
// this many distinct beads stops taking new titles rather than growing: the
// line prints without one, which is what a failed read does too.
const tailTitleCacheSize = 512

// tailBeads resolves the two bead facts gt tail prints beside an events line:
// the title of a create, close, status change or dependency line, and the
// verdict a comment or a note carries. It is the run's one door to bd — a
// title costs one bd show per bead, and the verdict read is the caller's to
// bound.
//
// Every read is read-only, and every failure is silence: a title that cannot
// be read prints no title, never an error line, because a stream about the
// town's health must not become a stream about bd's.
type tailBeads struct {
	show func(rig, id string) (*beads.Issue, error)
	max  int

	mu     sync.Mutex
	titles map[string]string
	// fetching marks the ids a read is already in flight for. The sources
	// poll concurrently and share this cache, so two of them can ask about
	// the same bead in the same instant; the marker keeps that from becoming
	// two bd shows, and the second line prints without a title, which is what
	// a failed read does too.
	fetching map[string]bool
}

// newTailBeads wraps show with the per-run title cache.
func newTailBeads(show func(rig, id string) (*beads.Issue, error)) *tailBeads {
	return &tailBeads{show: show, max: tailTitleCacheSize, titles: map[string]string{}, fetching: map[string]bool{}}
}

// issue reads one bead now, with no caching: the caller wants what the bead
// says at this moment (its notes, its comments).
func (b *tailBeads) issue(rig, id string) *beads.Issue {
	if b == nil || b.show == nil || id == "" {
		return nil
	}
	issue, err := b.show(rig, id)
	if err != nil {
		return nil
	}
	return issue
}

// title reads one bead's title, at most once per run. A read that failed is
// remembered as "no title", so a store that is down costs one attempt, not one
// per line. A read already in flight answers "" rather than starting a second
// one: the sources poll concurrently, and the town scan must not fan one
// bead's read out into one per source.
func (b *tailBeads) title(rig, id string) string {
	if b == nil || b.show == nil || id == "" {
		return ""
	}
	b.mu.Lock()
	if title, ok := b.titles[id]; ok {
		b.mu.Unlock()
		return title
	}
	if b.fetching[id] || len(b.titles) >= b.max {
		b.mu.Unlock()
		return ""
	}
	b.fetching[id] = true
	b.mu.Unlock()

	title := ""
	if issue := b.issue(rig, id); issue != nil {
		title = issue.Title
	}

	b.mu.Lock()
	delete(b.fetching, id)
	if len(b.titles) < b.max {
		b.titles[id] = title
	}
	b.mu.Unlock()
	return title
}

// lineTitle is the title an events line carries, "" for a line that names no
// bead the reader needs to recognize.
func (b *tailBeads) lineTitle(l tailLine) string {
	id, ok := tailTitleBeadID(l)
	if !ok {
		return ""
	}
	return b.title(l.Rig, id)
}

// tailTitleBeadID reads the bead a title belongs to off an events line, and
// whether the line is one a title is appended to: a create, a close, a
// dependency, or an update that moved the bead's status. The line's text is
// "<op> <id> [status=<s>] [actor=<a>] seq=<n>".
func tailTitleBeadID(l tailLine) (string, bool) {
	if l.Kind != tailKindEvents || l.Verdict != "" {
		return "", false
	}
	f := strings.Fields(l.Text)
	if len(f) < 2 {
		return "", false
	}
	switch f[0] {
	case "create", "close", "dep_add":
		return f[1], true
	case "update":
		for _, field := range f[2:] {
			if strings.HasPrefix(field, "status=") {
				return f[1], true
			}
		}
	}
	return "", false
}

// openTailBeads is the production tailBeads.show: the store that owns a rig's
// beads, opened once per rig. The store is read with bd show, which routes by
// issue id, so one store answers for any bead the monitor asks about.
func openTailBeads(townRoot string) *tailBeads {
	var mu sync.Mutex
	stores := map[string]*beads.Beads{}
	show := func(rig, id string) (*beads.Issue, error) {
		mu.Lock()
		store, ok := stores[rig]
		if !ok {
			dir := doltserver.FindRigBeadsDir(townRoot, rig)
			if dir == "" {
				mu.Unlock()
				return nil, fmt.Errorf("no beads directory for %s", rig)
			}
			store = beads.NewWithBeadsDir(townRoot, dir)
			stores[rig] = store
		}
		mu.Unlock()
		return store.Show(id)
	}
	return newTailBeads(show)
}

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

// tailBDJournal is the production tailJournal: the journal through
// beads.EventsTail, and the events-journal key read with any inherited
// BD_EVENTS_JOURNAL cleared, so the answer is the store's config (as the
// daemon's startup check reads it), not the caller's environment.
type tailBDJournal struct {
	*beads.Beads
	dir string
	run deps.BDRunner
}

func (j *tailBDJournal) ConfigGet(key string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), beads.ResolveSubprocessTimeout())
	defer cancel()
	stdout, stderr, err := j.run(ctx, []string{"BEADS_DIR=" + j.dir, "BD_EVENTS_JOURNAL=", "BD_MACHINE=1"}, "config", "get", key, "--json")
	if err != nil {
		return "", fmt.Errorf("bd config get %s: %w (%s)", key, err, strings.TrimSpace(string(stderr)))
	}
	return beads.ParseConfigGetJSON(stdout)
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
	beads    *tailBeads // the run's bead reads; nil prints no title and no verdict

	// pollIssues and shownVerdicts are the verdict read's two bounds. One bd
	// show per bead per poll answers every record that bead wrote, and a
	// verdict already shown is not shown again however often the bead is
	// rewritten (a rejection is re-recorded as the rework is retried).
	pollIssues    map[string]*beads.Issue
	shownVerdicts map[string]string

	configChecked bool
	// backlogDone is set by the first read that reaches the journal's head;
	// until then every record is cut at the cutoff, so a first read that
	// fails is retried as a backlog read, never as a dump from seq 0.
	backlogDone bool
	since       int64
	failed      tailOnce
}

func (s *eventsSource) line(at time.Time, format string, args ...any) tailLine {
	return tailLine{At: at, Rig: s.rig, Kind: tailKindEvents, Text: fmt.Sprintf(format, args...)}
}

// tailVerdictOp reports whether a journal record can carry a verdict: a
// comment the review loop wrote, or a note a rejection appended to.
func tailVerdictOp(op string) bool {
	return op == "comment" || op == "update"
}

// verdictText is the verdict the record's bead carries, "" when there is none
// or when this run already showed it. One bd show per bead per poll answers
// every record that bead wrote, and a bead whose line the default view hides
// (a wisp, a worker's own agent bead) is not read at all: nothing would print
// it.
func (s *eventsSource) verdictText(op, id string) string {
	if s.beads == nil || !tailVerdictOp(op) || isWispID(id) || isPolecatAgentBead(id) {
		return ""
	}
	if s.pollIssues == nil {
		s.pollIssues = map[string]*beads.Issue{}
	}
	issue, ok := s.pollIssues[id]
	if !ok {
		issue = s.beads.issue(s.rig, id)
		s.pollIssues[id] = issue
	}
	if issue == nil {
		return ""
	}
	text := tailVerdictText(issue, op)
	if text == "" || s.shownVerdicts[id] == text {
		return ""
	}
	if s.shownVerdicts == nil {
		s.shownVerdicts = map[string]string{}
	}
	s.shownVerdicts[id] = text
	return text
}

func (s *eventsSource) Poll() []tailLine {
	now := s.now()
	s.pollIssues = nil
	if s.openErr != nil {
		if s.failed.first(s.openErr.Error()) {
			return []tailLine{s.line(now, "cannot read the journal: %v", s.openErr)}
		}
		return nil
	}
	var out []tailLine
	backlog := !s.backlogDone
	if !s.configChecked {
		s.configChecked = true
		// A store whose config leaves the journal off journals nothing
		// (gt-7iwy0.7); the journal is read anyway and the operator told.
		v, err := s.journal.ConfigGet(beads.EventsJournalKey)
		switch {
		case err != nil:
			out = append(out, s.line(now, "cannot read events-journal config (%v): the journal may be off", err))
		case !beads.EventsJournalOn(v):
			out = append(out, s.line(now, "journal off in config (events-journal=%s): nothing is journaled", strings.TrimSpace(v)))
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
			line := s.line(at, "%s", text)
			line.Verdict = s.verdictText(r.Op, r.IssueID)
			out = append(out, line)
		}
		advanced := page.NextSince > s.since
		if advanced {
			s.since = page.NextSince
		}
		if !page.More || !advanced {
			s.backlogDone = true
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
	recs, bad, err := s.reader.ReadNew()
	if err != nil {
		// started stays false: the retry is still a backlog read, cut at the
		// cutoff.
		if s.failed.first(err.Error()) {
			return []tailLine{{At: now, Rig: s.rig, Kind: tailKindLandings, Text: "read failed: " + err.Error()}}
		}
		return nil
	}
	s.started = true
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

// tailWatchAlert is one line of a watch feed file: the alerts.jsonl schema
// (gt-z2pdg) that the operator's monitor scripts write and the daemon's
// attention transitions (internal/attention) reuse. Unknown keys are ignored,
// so an attention line's key and state fields need no handling here.
type tailWatchAlert struct {
	TS       time.Time `json:"ts"`
	Class    string    `json:"class"`
	Severity string    `json:"severity"`
	Text     string    `json:"text"`
}

// tailWatchSeverityClass is the class a watch line is drawn in: the schema's
// low is yellow, high is red. false is a severity the schema does not name,
// which makes the line unreadable rather than drawable.
func tailWatchSeverityClass(severity string) (tailClass, bool) {
	switch strings.TrimSpace(severity) {
	case string(attention.SeverityLow):
		return tailClassWarning, true
	case string(attention.SeverityHigh):
		return tailClassFailure, true
	}
	return tailClassPlain, false
}

// tailWatchText is an alert's line: the class names what kind of condition it
// is, the text says it. A line missing either prints the other alone.
func tailWatchText(a tailWatchAlert) string {
	text, class := strings.TrimSpace(a.Text), strings.TrimSpace(a.Class)
	switch {
	case class == "":
		return text
	case text == "":
		return class
	}
	return class + ": " + text
}

// parseTailWatchLine reads one line as an alert. A line is unreadable when it
// is not JSON, carries no timestamp, or names a severity the schema does not:
// none of those can be drawn, and guessing a color would misreport them.
func parseTailWatchLine(line []byte) (tailWatchAlert, bool) {
	var a tailWatchAlert
	if err := json.Unmarshal(line, &a); err != nil {
		return a, false
	}
	if a.TS.IsZero() {
		return a, false
	}
	if _, ok := tailWatchSeverityClass(a.Severity); !ok {
		return a, false
	}
	return a, true
}

// tailWatchFile tails one watch feed file by byte offset. Each read returns the
// complete lines appended since the previous one; a trailing line with no
// newline yet is left for the next read, so a writer's half-written line is
// never reported as a bad one. A missing file has no alerts and is not an
// error. A file that shrank or was replaced — its writer rotates it at 1 MB —
// is read again from its start.
type tailWatchFile struct {
	path string
	name string // the base name, which is how a note names the file
	// offset and info are the previous read's end and the file it ended in,
	// so a replaced file is recognized rather than read as a longer one.
	offset int64
	info   os.FileInfo
}

func newTailWatchFile(path string) *tailWatchFile {
	return &tailWatchFile{path: path, name: filepath.Base(path)}
}

// tailWatchItem is one complete line of a watch file, in file order: an alert
// the stream prints, or the bytes of a line that is not one. The two travel
// together so a skipped-line note prints where its line was.
type tailWatchItem struct {
	Alert tailWatchAlert
	Bad   string // the line itself, when it is not an alert
}

// readNew returns the complete lines appended since the last read.
func (f *tailWatchFile) readNew() (items []tailWatchItem, err error) {
	file, err := os.Open(f.path)
	if errors.Is(err, os.ErrNotExist) {
		f.offset, f.info = 0, nil
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", f.name, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("checking %s: %w", f.name, err)
	}
	if f.info != nil && (!os.SameFile(f.info, info) || info.Size() < f.offset) {
		f.offset = 0
	}
	f.info = info
	if info.Size() == f.offset {
		return nil, nil
	}
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking %s: %w", f.name, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, info.Size()-f.offset))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", f.name, err)
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, nil
	}
	f.offset += int64(end + 1)
	for _, line := range bytes.Split(data[:end], []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		alert, ok := parseTailWatchLine(line)
		if !ok {
			items = append(items, tailWatchItem{Bad: string(line)})
			continue
		}
		items = append(items, tailWatchItem{Alert: alert})
	}
	return items, nil
}

// tailWatchSource reads the town's watch feed: the operator monitors' alerts
// and the daemon's attention transitions. Both are one alert per line in one
// schema and both print under the tag "watch", so the operator reads them
// where they happened rather than in a second command.
type tailWatchSource struct {
	files  []*tailWatchFile
	cutoff time.Time
	now    func() time.Time

	// started is set by the first poll that read every file, so a first poll
	// that failed is retried as a backlog read cut at the cutoff.
	started bool
	failed  tailOnce
}

// newTailWatchSource is the production watch source: the two files, in the
// order their lines print when their times tie.
func newTailWatchSource(townRoot string, cutoff time.Time, now func() time.Time) *tailWatchSource {
	return &tailWatchSource{
		files: []*tailWatchFile{
			newTailWatchFile(filepath.Join(constants.TownRuntimePath(townRoot), "watch", "alerts.jsonl")),
			newTailWatchFile(attention.EventsPath(townRoot)),
		},
		cutoff: cutoff,
		now:    now,
	}
}

func (s *tailWatchSource) line(at time.Time, format string, args ...any) tailLine {
	return tailLine{At: at, Rig: tailKindWatch, Kind: tailKindWatch, Text: fmt.Sprintf(format, args...)}
}

func (s *tailWatchSource) Poll() []tailLine {
	now := s.now()
	backlog := !s.started
	var out []tailLine
	read := true
	for _, f := range s.files {
		items, err := f.readNew()
		if err != nil {
			read = false
			if s.failed.first(err.Error()) {
				out = append(out, s.line(now, "cannot read %s: %v", f.name, err))
			}
			continue
		}
		for _, item := range items {
			if item.Bad != "" {
				// A line that is not an alert is counted, never echoed: the
				// stream must not republish whatever text ended up in the file.
				out = append(out, s.line(now, "skipped unreadable watch line in %s (%d bytes)", f.name, len(item.Bad)))
				continue
			}
			if backlog && item.Alert.TS.Before(s.cutoff) {
				continue
			}
			class, _ := tailWatchSeverityClass(item.Alert.Severity)
			out = append(out, tailLine{At: item.Alert.TS, Rig: tailKindWatch, Kind: tailKindWatch, Class: class, Text: tailWatchText(item.Alert)})
		}
	}
	if read {
		s.started = true
		s.failed.clear()
	}
	return out
}

// tailSummaryMaxSeats bounds the polecat:bead pairs the summary line lists;
// the rest are counted, not named.
const tailSummaryMaxSeats = 6

// tailSummarySpendFresh is how old the DeepSeek spend reading may be and still
// be reported. An older one describes a rate that has stopped being measured,
// so the field drops out rather than reporting it as current.
const tailSummarySpendFresh = 15 * time.Minute

// tailSummaryFresh is how often gt tail -f may reprint the summary line, even
// when one of its fields moved. The line is a state line, not an event: what
// the operator reads it for is the change, and a line that reprints every poll
// is the noise this bead exists to remove.
const tailSummaryFresh = 5 * time.Minute

// tailSummaryFields is the town state the summary line reports. Each part is
// optional: a part that could not be read is left out of the line rather than
// reported as zero, so a store that is down does not read as an idle town.
type tailSummaryFields struct {
	Seats        []string // "<rig>/<polecat>:<bead>", sorted
	SeatsUsed    int
	SeatsCap     int
	HasSeats     bool
	ReadyToLand  int
	HasReady     bool
	MainTip      string // origin/main's short sha
	InstalledGT  string // the running binary's short sha
	Behind       int
	HasMain      bool
	Escalations  int
	HasEscalate  bool
	SpendPerHour float64
	HasSpend     bool
	// HasDeploy and Deploy are the speed the town is shipping at, read off
	// the dispatch-to-deploy tracker. They are absent until a landing is
	// tracked: a town with no landings has no median to report.
	HasDeploy bool
	Deploy    tailDeploySummary
}

// line is the one dim line the summary prints, "" when no field could be
// read. The deploy fields lead: the line is truncated to the terminal's
// width, and the median is the town's headline number, so a narrow terminal
// must not cut it off the end.
func (f tailSummaryFields) line() string {
	parts := tailDeploySummaryParts(f)
	if f.HasSeats {
		seats := fmt.Sprintf("seats %d", f.SeatsUsed)
		if f.SeatsCap > 0 {
			seats = fmt.Sprintf("seats %d/%d", f.SeatsUsed, f.SeatsCap)
		}
		if len(f.Seats) > 0 {
			shown := f.Seats
			more := ""
			if len(shown) > tailSummaryMaxSeats {
				more = fmt.Sprintf(", +%d more", len(shown)-tailSummaryMaxSeats)
				shown = shown[:tailSummaryMaxSeats]
			}
			seats += " [" + strings.Join(shown, ", ") + more + "]"
		}
		parts = append(parts, seats)
	}
	if f.HasReady {
		parts = append(parts, fmt.Sprintf("ready %d", f.ReadyToLand))
	}
	if f.HasMain {
		main := fmt.Sprintf("main %s vs gt %s", f.MainTip, f.InstalledGT)
		if f.Behind > 0 {
			main += fmt.Sprintf(" (+%d behind)", f.Behind)
		}
		parts = append(parts, main)
	}
	if f.HasEscalate {
		parts = append(parts, fmt.Sprintf("escalations %d", f.Escalations))
	}
	if f.HasSpend {
		parts = append(parts, fmt.Sprintf("DeepSeek $%.2f/h", f.SpendPerHour))
	}
	if len(parts) == 0 {
		return ""
	}
	return tailSummaryIcon + " " + strings.Join(parts, tailSummarySep)
}

// tailDeploySummaryParts is the shipping-speed half of the summary line: the
// median dispatched-to-deployed minutes over the beads that landed in the
// last hour, and the landed beads still waiting for a deploy.
func tailDeploySummaryParts(f tailSummaryFields) []string {
	if !f.HasDeploy {
		return nil
	}
	var parts []string
	if f.Deploy.HasMedian {
		parts = append(parts, fmt.Sprintf("dispatch→deploy %s median (%d)",
			tailDeployMinutesOf(f.Deploy.MedianMin), f.Deploy.MedianBeads))
	}
	waiting := fmt.Sprintf("waiting %d", f.Deploy.Waiting)
	if f.Deploy.Waiting > 0 && f.Deploy.HasOldest {
		waiting += " (oldest " + reportAge(f.Deploy.OldestWaiting) + ")"
	}
	return append(parts, waiting)
}

// tailSummaryIcon leads the summary line, and tailSummarySep joins its fields.
const (
	tailSummaryIcon = "📊"
	tailSummarySep  = " · "
)

// readTailSummary reads the town's live state for the summary line. Every part
// is optional and every failure is silence: the line reports what it could
// read. It only reads — no bd write, no fetch, no reservation cleanup.
//
// listAgent lists one rig's agent beads (tailRigAgentBeads in production), so a
// test can drive a rig's read failure without a store.
func readTailSummary(townRoot string, listAgent func(rigPath string) (map[string]*beads.Issue, error), deploys *tailDeploys) tailSummaryFields {
	var f tailSummaryFields
	if seats, cap, pairs, err := tailSeatPicture(townRoot, listAgent); err == nil {
		f.HasSeats, f.SeatsUsed, f.SeatsCap, f.Seats = true, seats, cap, pairs
	}
	if n, err := tailReadyToLand(townRoot); err == nil {
		f.HasReady, f.ReadyToLand = true, n
	}
	if tip, installed, behind, err := tailMainPicture(townRoot); err == nil {
		f.HasMain, f.MainTip, f.InstalledGT, f.Behind = true, tip, installed, behind
	}
	if n, err := tailOpenEscalations(townRoot); err == nil {
		f.HasEscalate, f.Escalations = true, n
	}
	if perHour, at, err := tailSpend(townRoot); err == nil && !at.IsZero() {
		f.HasSpend, f.SpendPerHour = true, perHour
	}
	// The tracker is in-memory, so the deploy reading costs nothing here; it
	// is absent only until the first landing reaches it.
	if s := deploys.snapshot(); s.Landed > 0 {
		f.HasDeploy, f.Deploy = true, s
	}
	return f
}

// tailSeatPicture counts the town's polecat seats in use and its cap, and names
// the work each one holds. A seat is in use when the polecat's agent bead
// carries a hooked bead: that is the write every dispatch makes and every
// completion clears. The cap is the scheduler's, and 0 when the town runs
// uncapped.
//
// A rig whose polecat directory or agent beads cannot be read is an error, not
// a skip: counting it as zero would make a store that is down read as an idle
// town, which is the one report the field exists to prevent.
func tailSeatPicture(townRoot string, listAgent func(rigPath string) (map[string]*beads.Issue, error)) (used, cap int, pairs []string, err error) {
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, rigName := range rigs {
		rigPath := filepath.Join(townRoot, rigName)
		names, err := listPolecatDirectoryNames(rigPath)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("polecat directory of %s: %w", rigName, err)
		}
		if len(names) == 0 {
			continue
		}
		agents, err := listAgent(rigPath)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("agent beads of %s: %w", rigName, err)
		}
		prefix := beads.GetPrefixForRig(townRoot, rigName)
		agentBeadID := func(name string) string { return beads.PolecatBeadIDWithPrefix(prefix, rigName, name) }
		for name, bead := range polecatHookBeads(names, agentBeadID, agents) {
			used++
			pairs = append(pairs, fmt.Sprintf("%s/%s:%s", rigName, name, bead))
		}
	}
	sort.Strings(pairs)
	max, err := configuredSchedulerMaxPolecats(townRoot)
	if err != nil {
		return 0, 0, nil, err
	}
	if max < 0 {
		// The scheduler answers -1 for "no cap configured"; a town that runs
		// uncapped is not a town with a cap of minus one.
		max = 0
	}
	return used, max, pairs, nil
}

// tailRigAgentBeads lists one rig's agent beads by polecat name. A list reads
// the wrapper's own database and never per-ID rerouting, so the rig's own
// store is the whole of what it needs — the agent-scoping that gt-a6g requires
// of the per-ID helpers has nothing to route here.
func tailRigAgentBeads(rigPath string) (map[string]*beads.Issue, error) {
	return beads.ListAgentBeads(beads.New(rigPath))
}

// tailReadyToLand counts the beads waiting to land. The label lives on the
// work bead in the rig that owns it, so every store is asked.
func tailReadyToLand(townRoot string) (int, error) {
	stores := []string{"hq"}
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return 0, err
	}
	stores = append(stores, rigs...)
	total := 0
	for _, store := range stores {
		dir := doltserver.FindRigBeadsDir(townRoot, store)
		if dir == "" {
			continue
		}
		issues, err := beads.NewWithBeadsDir(townRoot, dir).List(beads.ListOptions{
			Label: land.LabelReadyToLand, Priority: -1,
		})
		if err != nil {
			return 0, err
		}
		for _, issue := range issues {
			if beads.HasLabel(issue, land.LabelReadyToLand) && beads.IssueStatus(issue.Status).IsActionable() {
				total++
			}
		}
	}
	return total, nil
}

// tailMainPicture is origin/main against the binary running this stream: the
// two short shas and how many commits the binary is behind. It reads the local
// refs only — the summary line must not put a fetch in front of the operator
// every five minutes.
func tailMainPicture(townRoot string) (tip, installed string, behind int, err error) {
	repo, err := version.GetRepoRootForTown(townRoot)
	if err != nil {
		return "", "", 0, err
	}
	info := version.CheckStaleBinary(repo)
	if info == nil || info.Error != nil || info.Skipped {
		return "", "", 0, fmt.Errorf("staleness of the installed gt is unknown")
	}
	if info.BinaryCommit == "" || info.RepoCommit == "" {
		return "", "", 0, fmt.Errorf("commit of the installed gt or of %s is unknown", info.CompareRef)
	}
	return version.ShortCommit(info.RepoCommit), version.ShortCommit(info.BinaryCommit), info.CommitsBehind, nil
}

// tailOpenEscalations counts the town's open escalations, both bead planes,
// minus the mail carriers that delivered them.
func tailOpenEscalations(townRoot string) (int, error) {
	issues, err := beads.ListEscalationsAcrossRigs(beads.New(beads.ResolveBeadsDir(townRoot)))
	if err != nil {
		return 0, err
	}
	return len(issues), nil
}

// tailSpend reads the health watch's DeepSeek rate: ~/.runtime/watch/spend.json
// is {"ts", "per_hour", "balance"}, rewritten as the watch measures it. A file
// older than tailSummarySpendFresh reports nothing.
func tailSpend(townRoot string) (perHour float64, at time.Time, err error) {
	path := filepath.Join(constants.TownRuntimePath(townRoot), "watch", "spend.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	var f struct {
		TS      time.Time `json:"ts"`
		PerHour float64   `json:"per_hour"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return 0, time.Time{}, err
	}
	if f.TS.IsZero() {
		return 0, time.Time{}, fmt.Errorf("spend.json carries no timestamp")
	}
	if time.Since(f.TS) > tailSummarySpendFresh {
		return 0, time.Time{}, fmt.Errorf("spend.json is %s old", time.Since(f.TS).Round(time.Second))
	}
	return f.PerHour, f.TS, nil
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

	// started is set by the first successful daemon.log read; until then
	// every line is cut at the cutoff. backupsRead keeps a retried first
	// read from printing the backups twice.
	started     bool
	backupsRead bool
	offset      int64
	info        os.FileInfo
	lastAt      time.Time
	failed      tailOnce
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
	if !s.backupsRead {
		s.backupsRead = true
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
		s.started = true
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
// its start, and note says so. Lines the old file gained between the last
// poll and the rename are not read: they are in the newest backup.
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

// --- dispatched to deployed ---

// tailDeployWindow is how far back the deploy tracker reads the daemon log:
// at least an hour, so the summary's rolling median sees the hour it names
// even when --since is shorter.
const tailDeployWindow = time.Hour

// tailDeployGitTimeout bounds one ancestry check. The check is a local git
// call, so a hang means the source checkout is on a stuck mount; the bead
// stays waiting rather than stalling the stream.
const tailDeployGitTimeout = 10 * time.Second

// The daemon log's dispatch-to-deploy facts, each read off lines the source
// already parses. The landed line comes in two wordings — the landing worker's
// own "landed <sha> on origin/main" and its repair of a bead already on main,
// "already landed as <sha>" — and both name the same landing.
var (
	tailDeployDispatchRe = regexp.MustCompile(`spec_dispatch: dispatched: (\S+): slung`)
	tailDeployMergeRe    = regexp.MustCompile(`\[land\] (\S+): merged `)
	tailDeployLandedRe   = regexp.MustCompile(`\[land\] (\S+): (?:already )?landed (?:as )?([0-9a-f]{7,40})`)
	tailDeployRestartRe  = regexp.MustCompile(`upgrade-restart: running (\S+) covers marker`)
)

// tailAncestry answers whether commit is an ancestor of of. ok is false when
// the question cannot be answered — a git error — and the caller then leaves
// the bead waiting rather than counting it deployed.
type tailAncestry func(commit, of string) (ancestor, ok bool)

// tailDeployRecord is one bead's trip, read off the daemon log. A zero time
// is a step the log has not shown.
type tailDeployRecord struct {
	bead       string
	dispatched time.Time
	merged     time.Time
	landed     time.Time
	sha        string
	deployed   time.Time
}

// dispatchTime is the dispatch the tracker counts: the first line, and only
// when it came before the landing. A redispatch after a landing is the rework
// loop, not this bead's start.
func (r *tailDeployRecord) dispatchTime() time.Time {
	if r.dispatched.IsZero() || r.dispatched.After(r.landed) {
		return time.Time{}
	}
	return r.dispatched
}

// tailDeploys tracks each bead from the spec dispatcher's sling to the daemon
// restart that installs its commit. It reads the daemon log's four facts and
// decides deployment by ancestry: the landed commit must be an ancestor of
// the installed one, because a restart can install a binary built before the
// bead landed. Every question it cannot answer leaves the bead waiting.
type tailDeploys struct {
	now      func() time.Time
	ancestor tailAncestry
	from     time.Time // the run's cutoff: no deploy line older than this

	mu      sync.Mutex
	records map[string]*tailDeployRecord
	order   []string // first-seen order, which for landed beads is landing order
}

// newTailDeploys returns a tracker that reads the daemon log back to the
// run's cutoff, from the run's clock, and answers deployment with ancestor.
func newTailDeploys(now func() time.Time, ancestor tailAncestry, from time.Time) *tailDeploys {
	return &tailDeploys{now: now, ancestor: ancestor, from: from, records: map[string]*tailDeployRecord{}}
}

// logCutoff is how far back the daemon log is read to feed the tracker: the
// run's cutoff, or an hour back when that is shorter.
func (t *tailDeploys) logCutoff(runCutoff time.Time) time.Time {
	if want := t.now().Add(-tailDeployWindow); want.Before(runCutoff) {
		return want
	}
	return runCutoff
}

func (t *tailDeploys) record(bead string) *tailDeployRecord {
	r, ok := t.records[bead]
	if !ok {
		r = &tailDeployRecord{bead: bead}
		t.records[bead] = r
		t.order = append(t.order, bead)
	}
	return r
}

// observe feeds a batch of daemon lines to the tracker and returns one line
// for each bead the batch deploys. Lines the run's window excludes still feed
// it: the summary's median and waiting count are state, not stream.
func (t *tailDeploys) observe(lines []tailLine) []tailLine {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []tailLine
	for _, ln := range lines {
		if ln.Kind != tailKindDaemon {
			continue
		}
		// One line carries one fact, so the first shape that matches wins and
		// the rest are not tried.
		if m := tailDeployDispatchRe.FindStringSubmatch(ln.Text); m != nil {
			if r := t.record(m[1]); r.dispatched.IsZero() {
				r.dispatched = ln.At
			}
		} else if m := tailDeployLandedRe.FindStringSubmatch(ln.Text); m != nil {
			if r := t.record(m[1]); r.landed.IsZero() {
				r.landed, r.sha = ln.At, m[2]
			}
		} else if m := tailDeployMergeRe.FindStringSubmatch(ln.Text); m != nil {
			if r := t.record(m[1]); r.merged.IsZero() {
				r.merged = ln.At
			}
		} else if m := tailDeployRestartRe.FindStringSubmatch(ln.Text); m != nil {
			out = append(out, t.deployAt(ln.At, m[1])...)
		}
	}
	return out
}

// deployAt marks every waiting landed bead whose commit the installed commit
// contains, and returns their lines. A bead that landed after the restart
// cannot be in a binary built before it, so it is not asked.
func (t *tailDeploys) deployAt(at time.Time, installed string) []tailLine {
	var out []tailLine
	for _, bead := range t.order {
		r := t.records[bead]
		if r.landed.IsZero() || !r.deployed.IsZero() || r.sha == "" || r.landed.After(at) {
			continue
		}
		ancestor, ok := t.ancestor(r.sha, installed)
		if !ok || !ancestor {
			continue
		}
		r.deployed = at
		if at.Before(t.from) {
			continue
		}
		out = append(out, t.deployedLine(r))
	}
	return out
}

// deployedLine is the one line a deployed bead prints: how long it took and
// the stages it broke into. A hand-slung bead has no dispatch line, so it
// prints its landed-to-deployed part alone.
func (t *tailDeploys) deployedLine(r *tailDeployRecord) tailLine {
	dispatched := r.dispatchTime()
	var parts []string
	total := r.deployed.Sub(r.landed)
	if !dispatched.IsZero() {
		total = r.deployed.Sub(dispatched)
		if !r.merged.IsZero() {
			parts = append(parts,
				"work "+tailDeployMinutes(r.merged.Sub(dispatched)),
				"land "+tailDeployMinutes(r.landed.Sub(r.merged)))
		}
	}
	parts = append(parts, "deploy "+tailDeployMinutes(r.deployed.Sub(r.landed)))
	return tailLine{
		At: r.deployed, Rig: "town", Kind: tailKindDaemon, Class: tailClassSuccess,
		Text: fmt.Sprintf("%s deployed in %s (%s)", r.bead, tailDeployMinutes(total), strings.Join(parts, ", ")),
	}
}

// tailDeploySummary is the tracker's state for the summary line: the rolling
// median the town is shipping at, and the landings still waiting for a
// restart.
type tailDeploySummary struct {
	Landed        int // landings tracked
	HasMedian     bool
	MedianMin     float64
	MedianBeads   int
	Waiting       int
	OldestWaiting time.Duration
	HasOldest     bool
}

// snapshot reads the tracker's state: the median dispatched-to-deployed
// minutes over the beads that landed in the last hour, and the landed beads
// still waiting for a deploy. A hand-slung bead prints its own time but is
// left out of the median: it has no dispatch to measure the town by.
func (t *tailDeploys) snapshot() tailDeploySummary {
	var s tailDeploySummary
	if t == nil {
		return s
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	since := now.Add(-tailDeployWindow)
	var mins []float64
	for _, bead := range t.order {
		r := t.records[bead]
		if r.landed.IsZero() {
			continue
		}
		s.Landed++
		if r.deployed.IsZero() {
			s.Waiting++
			if age := now.Sub(r.landed); !s.HasOldest || age > s.OldestWaiting {
				s.OldestWaiting, s.HasOldest = age, true
			}
			continue
		}
		// Only a bead with a dispatch, that landed in the window, has a
		// dispatched-to-deployed time to add; a hand-slung bead has none.
		dispatched := r.dispatchTime()
		if dispatched.IsZero() || r.landed.Before(since) {
			continue
		}
		mins = append(mins, r.deployed.Sub(dispatched).Minutes())
	}
	if len(mins) > 0 {
		sort.Float64s(mins)
		s.HasMedian, s.MedianBeads = true, len(mins)
		s.MedianMin = tailMedian(mins)
	}
	return s
}

// tailMedian is the middle value of a sorted slice, the mean of the two
// middle values when the count is even.
func tailMedian(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// tailDeployMinutes renders a stage's length in minutes, the unit the
// operator reads the town's speed in.
func tailDeployMinutes(d time.Duration) string {
	return tailDeployMinutesOf(d.Minutes())
}

// tailDeployMinutesOf renders a count of minutes, floored at zero so clock
// skew never prints a negative stage.
func tailDeployMinutesOf(m float64) string {
	if m < 0 {
		m = 0
	}
	return fmt.Sprintf("%.1fm", m)
}

// tailDeploySource is the daemon source with the tracker in front of it:
// every daemon.log line feeds the tracker, the lines the run's window and rig
// filter select pass through, and each bead a restart deploys adds its own
// line. The tracker reads wider than the stream — an hour, for the summary's
// median — so the passthrough, not the read, is what --since and --rig bound.
type tailDeploySource struct {
	inner     *daemonSource
	track     *tailDeploys
	from      time.Time
	rigFilter *regexp.Regexp
}

func (s *tailDeploySource) Poll() []tailLine {
	lines := s.inner.Poll()
	out := make([]tailLine, 0, len(lines))
	for _, ln := range lines {
		if ln.At.Before(s.from) {
			continue
		}
		if s.rigFilter != nil && !s.rigFilter.MatchString(ln.Text) {
			continue
		}
		out = append(out, ln)
	}
	return append(out, s.track.observe(lines)...)
}

// tailGitAncestry answers the tracker's ancestry question in the town's gt
// source checkout. A town with no checkout answers unknown to every question,
// so no bead is ever called deployed on a guess.
func tailGitAncestry(townRoot string) tailAncestry {
	repo, err := version.GetRepoRootForTown(townRoot)
	if err != nil {
		return func(string, string) (bool, bool) { return false, false }
	}
	return func(commit, of string) (bool, bool) { return tailGitIsAncestor(repo, commit, of) }
}

// tailGitIsAncestor runs git merge-base --is-ancestor in repo: exit 0 is an
// ancestor, exit 1 is not, and any other failure — a bad object, a timeout —
// is unknown.
func tailGitIsAncestor(repo, commit, of string) (ancestor, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), tailDeployGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", commit, of)
	cmd.Dir = repo
	// The same detached process group internal/version gives its git calls:
	// the context is the only thing that ends this one.
	util.SetDetachedProcessGroup(cmd)
	err := cmd.Run()
	if err == nil {
		return true, true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, true
	}
	return false, false
}
