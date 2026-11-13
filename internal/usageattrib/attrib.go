package usageattrib

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// The scan attributes each polecat session to the bead it worked. A seat's copied transcripts
// are matched to the runs of that seat, and one work record per bead is rebuilt from them.
//
// A run is one dispatch of a bead, from its sling to the event that ended it. Runs come from the
// town's events and landing records:
//   - a sling starts a run on the seat its target names; a sling while a run is open ends that run,
//     without counting as an end event;
//   - a done, bead_closed or overrun_stop event ends the open run, and the seat is then the event's
//     actor when that names a polecat;
//   - a landing ends the open run at its landed_at, when one is still open;
//   - a run with no sling has no start, so no session can be placed in it: its sessions are unmatched.
//
// A session belongs to the latest-starting run of its seat whose window holds the transcript's last
// write. A record is final once its run's done event is more than FinalAfterDone old, or, for a bead
// with no end event, once its newest transcript is more than FinalAfterIdle old. A final record is
// left alone unless its session set changed.

// FinalAfterDone is how long after its done event a bead's record counts as final.
const FinalAfterDone = 10 * time.Minute

// FinalAfterIdle is how long after its newest transcript a bead with no end event is final.
const FinalAfterIdle = 24 * time.Hour

// UnmatchedFile and StateFile are the files a scan keeps beside the bead records.
const (
	UnmatchedFile = "unmatched.jsonl"
	StateFile     = "state.json"
)

// Landing is one landing record as the scan reads it.
type Landing struct {
	Bead     string    `json:"bead"`
	Rig      string    `json:"rig"`
	LandedAt time.Time `json:"landed_at"`
}

// Run is one dispatch of a bead to a polecat seat. End is zero while the run is open; Ended tells
// an end event from a re-dispatch that only closed the window.
type Run struct {
	Bead  string
	Rig   string
	Seat  string
	Start time.Time
	End   time.Time
	Ended bool
}

type stepKind int

const (
	stepSling stepKind = iota
	stepEnd
	stepLanded
)

// step is one event or landing on the timeline the runs are read from.
type step struct {
	kind stepKind
	bead string
	rig  string
	seat string
	at   time.Time
}

// Runs reads the runs of every bead from the events and the landings. The events are read in time
// order. An event or landing with a timestamp that does not parse is an error: a run cannot be
// bounded without it.
func Runs(evs []events.Event, landings []Landing) ([]Run, error) {
	var steps []step
	for _, ev := range evs {
		at, err := time.Parse(time.RFC3339, ev.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("usageattrib: event %q has timestamp %q: %w", ev.Type, ev.Timestamp, err)
		}
		bead := payloadString(ev.Payload, "bead")
		switch ev.Type {
		case events.TypeSling:
			rig, seat := seatOf(payloadString(ev.Payload, "target"))
			steps = append(steps, step{kind: stepSling, bead: bead, rig: rig, seat: seat, at: at})
		case events.TypeDone, events.TypeBeadClosed, events.TypeOverrunStop:
			rig, seat := seatOf(ev.Actor)
			steps = append(steps, step{kind: stepEnd, bead: bead, rig: rig, seat: seat, at: at})
		}
	}
	for _, l := range landings {
		if l.LandedAt.IsZero() {
			continue
		}
		steps = append(steps, step{kind: stepLanded, bead: l.Bead, rig: l.Rig, at: l.LandedAt})
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].at.Before(steps[j].at) })

	var runs []Run
	open := map[string]int{}
	for _, s := range steps {
		if !validBead(s.bead) {
			continue
		}
		i, isOpen := open[s.bead]
		switch s.kind {
		case stepSling:
			if isOpen {
				runs[i].End = s.at
				delete(open, s.bead)
			}
			runs = append(runs, Run{Bead: s.bead, Rig: s.rig, Seat: s.seat, Start: s.at})
			open[s.bead] = len(runs) - 1
		case stepEnd, stepLanded:
			if !isOpen {
				continue
			}
			// The seat that ended the run is the one whose transcripts it wrote, so an actor
			// that names a polecat replaces the seat the sling named.
			if s.seat != "" {
				runs[i].Rig, runs[i].Seat = s.rig, s.seat
			}
			if runs[i].Rig == "" {
				runs[i].Rig = s.rig
			}
			runs[i].End, runs[i].Ended = s.at, true
			delete(open, s.bead)
		}
	}
	return runs, nil
}

// matchRun is the index of the run a transcript last written at mtime belongs to, on seat rig/seat:
// the latest-starting run whose window holds mtime. An open run holds every mtime after its start.
func matchRun(runs []Run, rig, seat string, mtime time.Time) (int, bool) {
	best, found := -1, false
	for i, r := range runs {
		if r.Rig != rig || r.Seat != seat || mtime.Before(r.Start) {
			continue
		}
		if !r.End.IsZero() && mtime.After(r.End) {
			continue
		}
		if !found || r.Start.After(runs[best].Start) {
			best, found = i, true
		}
	}
	return best, found
}

// seatOf reads "rig/polecats/name", the form a polecat's actor and dispatch target take. Anything
// else, or a name with a path separator, names no seat.
func seatOf(s string) (rig, seat string) {
	rig, seat, ok := strings.Cut(s, "/polecats/")
	if !ok || !plainName(rig) || !plainName(seat) {
		return "", ""
	}
	return rig, seat
}

func plainName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

// validBead is the bead ids the record files can be named by: a letter or digit first, then letters,
// digits, dot, hyphen and underscore, with no ".." run in it.
func validBead(id string) bool {
	if id == "" || !isAlnum(id[0]) || strings.Contains(id, "..") {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !isAlnum(id[i]) && id[i] != '.' && id[i] != '-' && id[i] != '_' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

func payloadString(p map[string]interface{}, key string) string {
	s, _ := p[key].(string)
	return s
}

// ReadLandings reads every rig's landing records under town, from <town>/.runtime/landings/<rig>.jsonl.
// A missing directory holds none. A line that does not parse is an error, since a skipped landing could
// leave a run open.
func ReadLandings(town string) ([]Landing, error) {
	dir := filepath.Join(constants.TownRuntimePath(town), "landings")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("usageattrib: reading landings: %w", err)
	}
	var out []Landing
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		ls, err := readLandingFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, ls...)
	}
	return out, nil
}

// readLandingFile reads one rig's landing file, one record per line.
func readLandingFile(path string) ([]Landing, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("usageattrib: reading landings: %w", err)
	}
	var out []Landing
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var parseErr error
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var l Landing
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			parseErr = fmt.Errorf("usageattrib: %s line %d does not parse: %w", path, n, err)
			break
		}
		out = append(out, l)
	}
	if err := errors.Join(parseErr, sc.Err(), f.Close()); err != nil {
		return nil, fmt.Errorf("usageattrib: reading %s: %w", path, err)
	}
	return out, nil
}

// ScanOptions are the flags of gt usage attrib scan.
type ScanOptions struct {
	Town    string
	Rebuild bool
	// Since is the first instant a run may have ended at; zero keeps every run.
	Since time.Time
}

// ScanDeps are the reads a scan makes, so a test names its own town and clock.
type ScanDeps struct {
	Now      func() time.Time
	Events   func(town string) ([]events.Event, error)
	Landings func(town string) ([]Landing, error)
	// ListSeat names the transcripts in a seat's copy, and how many the copy's cap left out.
	ListSeat func(town, rig, seat string) ([]string, int, error)
	// Read reads one transcript; nil is ReadSession.
	Read func(path string) (SessionUsage, time.Time, error)
	// Replace writes one bead's record; nil is Replace.
	Replace func(usageDir string, rec dashboard.UsageRecord, sessionIDs []string, reason string) error
}

// ScanResult is what a scan did: the beads whose record it wrote, the final ones it left alone,
// those written incomplete, and how many sessions no run claimed.
type ScanResult struct {
	Written    []string
	Kept       []string
	Incomplete []string
	Unmatched  int
}

// unmatchedLine is one line of UnmatchedFile.
type unmatchedLine struct {
	Seat      string    `json:"seat"`
	Session   string    `json:"session"`
	LastWrite time.Time `json:"last_write"`
	Reason    string    `json:"reason"`
}

// beadPlan is the record one bead will get, decided before anything is written.
type beadPlan struct {
	bead   string
	rec    dashboard.UsageRecord
	ids    []string
	reason string
}

// Scan rebuilds the work records of the beads that ran in the town. Every read happens before the
// first write: a workspaces file that does not parse, or an events, landing or seat-copy read that
// fails, is an error and writes nothing.
func Scan(o ScanOptions, d ScanDeps) (ScanResult, error) {
	now := d.now()
	m, err := workspaceid.Load(o.Town)
	if err != nil {
		return ScanResult{}, fmt.Errorf("usageattrib: scan: %w", err)
	}
	evs, err := d.Events(o.Town)
	if err != nil {
		return ScanResult{}, fmt.Errorf("usageattrib: scan: reading events: %w", err)
	}
	landings, err := d.Landings(o.Town)
	if err != nil {
		return ScanResult{}, fmt.Errorf("usageattrib: scan: %w", err)
	}
	runs, err := Runs(evs, landings)
	if err != nil {
		return ScanResult{}, fmt.Errorf("usageattrib: scan: %w", err)
	}
	runs = keepSince(runs, o.Since)

	read := d.Read
	if read == nil {
		read = ReadSession
	}
	replace := d.Replace
	if replace == nil {
		replace = Replace
	}
	seats := seatKeys(runs)
	cut := map[string]int{}
	var all []SessionUsage
	var owners []int
	var mtimes []time.Time
	var unmatched []unmatchedLine
	runSessions := make([]int, len(runs))
	runFaults := make([][]string, len(runs))
	for _, k := range seats {
		paths, skipped, err := d.ListSeat(o.Town, k.rig, k.seat)
		if err != nil {
			return ScanResult{}, fmt.Errorf("usageattrib: scan: listing the transcripts of %s/%s: %w", k.rig, k.seat, err)
		}
		if skipped > 0 {
			cut[k.rig+"/"+k.seat] = skipped
		}
		for _, p := range paths {
			sess, mtime, rerr := read(p)
			if mtime.IsZero() {
				return ScanResult{}, fmt.Errorf("usageattrib: scan: %s cannot be placed in a run: %w", p, rerr)
			}
			idx, ok := matchRun(runs, k.rig, k.seat, mtime)
			if !ok {
				if o.Since.IsZero() || !mtime.Before(o.Since) {
					unmatched = append(unmatched, unmatchedLine{
						Seat: k.rig + "/" + k.seat, Session: sess.ID, LastWrite: mtime,
						Reason: "no run of its seat holds its last write",
					})
				}
				continue
			}
			all = append(all, sess)
			owners = append(owners, idx)
			mtimes = append(mtimes, mtime)
			runSessions[idx]++
			if rerr != nil {
				runFaults[idx] = append(runFaults[idx], rerr.Error())
			}
		}
	}
	dedupeAcross(all)

	usageDir := filepath.Join(constants.TownRuntimePath(o.Town), "usage")
	plans, err := planBeads(runs, all, owners, mtimes, runSessions, runFaults, cut, m, usageDir, now, o.Rebuild)
	if err != nil {
		return ScanResult{}, err
	}

	var res ScanResult
	res.Unmatched = len(unmatched)
	for _, p := range plans {
		if p.kept {
			res.Kept = append(res.Kept, p.bead)
			continue
		}
		if err := replace(usageDir, p.rec, p.ids, p.reason); err != nil {
			return res, fmt.Errorf("usageattrib: scan: %w", err)
		}
		res.Written = append(res.Written, p.bead)
		if p.reason != "" {
			res.Incomplete = append(res.Incomplete, p.bead)
		}
	}
	if err := os.MkdirAll(usageDir, 0o755); err != nil {
		return res, fmt.Errorf("usageattrib: scan: creating the usage directory: %w", err)
	}
	if err := writeUnmatched(usageDir, unmatched); err != nil {
		return res, err
	}
	if err := writeState(usageDir, now, o, res); err != nil {
		return res, err
	}
	return res, nil
}

func (d ScanDeps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

// keepSince drops the runs that ended before since. An open run is kept.
func keepSince(runs []Run, since time.Time) []Run {
	if since.IsZero() {
		return runs
	}
	var out []Run
	for _, r := range runs {
		if r.End.IsZero() || !r.End.Before(since) {
			out = append(out, r)
		}
	}
	return out
}

type seatKey struct{ rig, seat string }

// seatKeys is the distinct seats of the runs, sorted, so a scan reads them in a fixed order.
func seatKeys(runs []Run) []seatKey {
	seen := map[seatKey]bool{}
	var out []seatKey
	for _, r := range runs {
		if r.Rig == "" || r.Seat == "" {
			continue
		}
		k := seatKey{r.Rig, r.Seat}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b seatKey) int {
		if c := strings.Compare(a.rig, b.rig); c != 0 {
			return c
		}
		return strings.Compare(a.seat, b.seat)
	})
	return out
}

// plannedBead is a bead's record and whether the scan leaves its file alone.
type plannedBead struct {
	beadPlan
	kept bool
}

// planBeads builds the record of each bead with a run, and decides for each whether to write it.
// Nothing is written here: an unreadable existing record is an error, so no record is replaced
// that could not be read first.
func planBeads(runs []Run, all []SessionUsage, owners []int, mtimes []time.Time, runSessions []int,
	runFaults [][]string, cut map[string]int, m *workspaceid.Map, usageDir string, now time.Time, rebuild bool,
) ([]plannedBead, error) {
	type group struct {
		runs     []int
		sessions []SessionUsage
		mtimes   []time.Time
	}
	groups := map[string]*group{}
	var beads []string
	for i, r := range runs {
		g, ok := groups[r.Bead]
		if !ok {
			g = &group{}
			groups[r.Bead] = g
			beads = append(beads, r.Bead)
		}
		g.runs = append(g.runs, i)
	}
	sort.Strings(beads)
	for i, idx := range owners {
		g := groups[runs[idx].Bead]
		g.sessions = append(g.sessions, all[i])
		g.mtimes = append(g.mtimes, mtimes[i])
	}

	var plans []plannedBead
	for _, bead := range beads {
		g := groups[bead]
		ws, err := workspaceIn(m, bead)
		if err != nil {
			return nil, fmt.Errorf("usageattrib: scan: %w", err)
		}
		f := Facts{Bead: bead, Kind: dashboard.UsageKindWork, Class: dashboard.UsageClassPolecat, Workspace: ws}
		var reasons []string
		allEnded := true
		var newest time.Time
		for _, mt := range g.mtimes {
			newest = laterOf(newest, mt)
		}
		var latestEnd time.Time
		anyEnded := false
		for _, i := range g.runs {
			r := runs[i]
			f.Rig = r.Rig
			if f.Start.IsZero() || r.Start.Before(f.Start) {
				f.Start = r.Start
			}
			if r.Ended {
				anyEnded = true
				latestEnd = laterOf(latestEnd, r.End)
			} else {
				allEnded = false
			}
			f.End = laterOf(f.End, r.End)
			key := r.Rig + "/" + r.Seat
			switch {
			case r.Seat == "":
				reasons = append(reasons, "its run names no polecat seat, so no transcript can be placed in it")
			case runSessions[i] == 0:
				reasons = append(reasons, fmt.Sprintf("no transcript of its run on %s is in the seat's copy: the copy keeps one set per seat name, so a later run of that name replaces it", key))
			}
			if n := cut[key]; n > 0 && r.Seat != "" {
				reasons = append(reasons, fmt.Sprintf("the transcript copy of %s was cut by the cap: %d files skipped", key, n))
			}
			reasons = append(reasons, runFaults[i]...)
		}
		f.Complete = allEnded
		if sameSeatAll(runs, g.runs) {
			seat := runs[g.runs[0]].Seat
			f.Seat = &seat
		}
		ids := make([]string, 0, len(g.sessions))
		for _, s := range g.sessions {
			ids = append(ids, s.ID)
		}
		rec := Build(f, g.sessions)
		reason := strings.Join(uniq(reasons), "; ")
		p := plannedBead{beadPlan: beadPlan{bead: bead, rec: rec, ids: ids, reason: reason}}

		if !rebuild {
			existing, err := ReadWork(usageDir, bead)
			switch {
			case err == nil:
				if isFinal(allEnded, anyEnded, latestEnd, newest, now) && sameSet(existing.Sessions, ids) {
					p.kept = true
				}
			case !errors.Is(err, fs.ErrNotExist):
				return nil, fmt.Errorf("usageattrib: scan: %w", err)
			}
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// isFinal is whether a bead's record can no longer change on its own: every run ended and the last
// ended more than FinalAfterDone ago, or no run ended and the newest transcript is more than
// FinalAfterIdle old. A bead with some runs open and some ended is not final.
func isFinal(allEnded, anyEnded bool, latestEnd, newest, now time.Time) bool {
	switch {
	case allEnded:
		return now.Sub(latestEnd) > FinalAfterDone
	case !anyEnded:
		return !newest.IsZero() && now.Sub(newest) > FinalAfterIdle
	}
	return false
}

// sameSeatAll reports whether every run of one bead names the same seat.
func sameSeatAll(runs []Run, idx []int) bool {
	for _, i := range idx {
		if runs[i].Seat != runs[idx[0]].Seat || runs[i].Rig != runs[idx[0]].Rig {
			return false
		}
	}
	return true
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// uniq keeps the first of each repeated string.
func uniq(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func writeUnmatched(usageDir string, lines []unmatchedLine) error {
	var buf []byte
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			return fmt.Errorf("usageattrib: encoding an unmatched session: %w", err)
		}
		buf = append(append(buf, b...), '\n')
	}
	return writer{create: os.CreateTemp}.writeFileAtomic(filepath.Join(usageDir, UnmatchedFile), buf)
}

// scanState is what StateFile holds: when the last scan ran and what it did.
type scanState struct {
	ScannedAt  time.Time  `json:"scanned_at"`
	Since      *time.Time `json:"since,omitempty"`
	Rebuild    bool       `json:"rebuild"`
	Written    int        `json:"written"`
	Kept       int        `json:"kept"`
	Incomplete int        `json:"incomplete"`
	Unmatched  int        `json:"unmatched"`
}

func writeState(usageDir string, now time.Time, o ScanOptions, res ScanResult) error {
	st := scanState{
		ScannedAt:  now,
		Rebuild:    o.Rebuild,
		Written:    len(res.Written),
		Kept:       len(res.Kept),
		Incomplete: len(res.Incomplete),
		Unmatched:  res.Unmatched,
	}
	if !o.Since.IsZero() {
		since := o.Since
		st.Since = &since
	}
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return fmt.Errorf("usageattrib: encoding %s: %w", StateFile, err)
	}
	return writer{create: os.CreateTemp}.writeFileAtomic(filepath.Join(usageDir, StateFile), append(b, '\n'))
}
