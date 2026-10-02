package cmd

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
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
}

// newTailBeads wraps show with the per-run title cache.
func newTailBeads(show func(rig, id string) (*beads.Issue, error)) *tailBeads {
	return &tailBeads{show: show, max: tailTitleCacheSize, titles: map[string]string{}}
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
// per line.
func (b *tailBeads) title(rig, id string) string {
	if b == nil || b.show == nil || id == "" {
		return ""
	}
	b.mu.Lock()
	if title, ok := b.titles[id]; ok {
		b.mu.Unlock()
		return title
	}
	if len(b.titles) >= b.max {
		b.mu.Unlock()
		return ""
	}
	b.mu.Unlock()

	title := ""
	if issue := b.issue(rig, id); issue != nil {
		title = issue.Title
	}

	b.mu.Lock()
	b.titles[id] = title
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
}

// line is the one dim line the summary prints, "" when no field could be read.
func (f tailSummaryFields) line() string {
	var parts []string
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

// tailSummaryIcon leads the summary line, and tailSummarySep joins its fields.
const (
	tailSummaryIcon = "📊"
	tailSummarySep  = " · "
)

// readTailSummary reads the town's live state for the summary line. Every part
// is optional and every failure is silence: the line reports what it could
// read. It only reads — no bd write, no fetch, no reservation cleanup.
func readTailSummary(townRoot string) tailSummaryFields {
	var f tailSummaryFields
	if seats, cap, pairs, err := tailSeatPicture(townRoot); err == nil {
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
	return f
}

// tailSeatPicture counts the town's polecat seats in use and its cap, and names
// the work each one holds. A seat is in use when the polecat's agent bead
// carries a hooked bead: that is the write every dispatch makes and every
// completion clears. The cap is the scheduler's, and 0 when the town runs
// uncapped.
func tailSeatPicture(townRoot string) (used, cap int, pairs []string, err error) {
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, rigName := range rigs {
		rigPath := filepath.Join(townRoot, rigName)
		names, err := listPolecatDirectoryNames(rigPath)
		if err != nil || len(names) == 0 {
			continue
		}
		agents, err := beads.New(rigPath).ListAgentBeads()
		if err != nil {
			continue
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
	issues, err := beads.New(beads.ResolveBeadsDir(townRoot)).ListEscalationsAcrossRigs()
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
