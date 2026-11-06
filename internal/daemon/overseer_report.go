package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/crewfresh"
	"github.com/steveyegge/gastown/internal/crewstale"
	"github.com/steveyegge/gastown/internal/git"
	townreport "github.com/steveyegge/gastown/internal/report"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// The overseer_report patrol (gt-a6fg7): every interval the daemon writes the
// status the overseer used to write by hand — done, in progress, left, open
// questions and the health line, plus gt report --hour's sections on the
// hour — to <town>/.runtime/overseer/reports/, where the dashboard's Report
// pane reads the newest timestamp-named file. The text is the digest of
// internal/report; this file is the cadence, the slot rule and the sources.
// Each run also writes daily.json and daily.html there, the sidecar and page
// the dashboard's Birds card reads (gt-ag7u0c). Off unless
// patrols.overseer_report.enabled is true. It reads and writes that one
// directory and restarts or kills nothing.

const (
	defaultOverseerReportInterval = 15 * time.Minute

	// overseerReportBeadTimeout bounds one bd call the report makes.
	overseerReportBeadTimeout = 30 * time.Second

	// overseerReportLookback bounds how far back DONE reaches when the
	// previous report is older than this or absent, so a daemon that was down
	// for days does not ask bd for every bead closed since.
	overseerReportLookback = 24 * time.Hour

	// overseerReportMaxBytes keeps a report under the dashboard reader's cap
	// (internal/dashboard/reports.go), which refuses a larger file.
	overseerReportMaxBytes = 60 << 10

	// overseerReportLayout is a report file's name, which the dashboard reader
	// parses for the report's time: the UTC instant of its slot, then .md.
	overseerReportLayout = "20060102T150405Z"
	overseerReportExt    = ".md"

	// overseerLatestName is the fixed name of the copy of the newest report, so
	// a reader can take the newest one without sorting the directory by time
	// (gt-y0xcl). It is not a report of its own: overseerReportNameRe rejects
	// it, so the lister and the previous-report lookup never read it as a
	// digest.
	overseerLatestName = "latest.md"

	// overseerSidecarName and overseerPageName are the two files the dashboard's
	// Birds card reads beside the reports (gt-ag7u0c), which look for one fixed
	// name each rather than the newest timestamp-named digest.
	overseerSidecarName = "daily.json"
	overseerPageName    = "daily.html"

	// overseerSidecarMaxBytes matches the dashboard reader's cap: a sidecar
	// over it is refused there, so writing one would only publish a row in
	// error.
	overseerSidecarMaxBytes = 16 << 10

	// overseerSidecarDecisionItems, overseerSidecarDecisionRunes and
	// overseerSidecarSummaryRunes bound what the sidecar carries to what the
	// dashboard shows: birdDecisionItemsMax items of birdDecisionItemRunes
	// each and a birdSummaryRunes summary. A longer list is cut there anyway,
	// and an unbounded one pushed the whole sidecar past the cap, which left
	// the Birds card stale instead of current (gt-ag7u0c rework).
	overseerSidecarDecisionItems = 8
	overseerSidecarDecisionRunes = 240
	overseerSidecarSummaryRunes  = 1200

	// overseerStaleCrewAge is how long a crew clone may go unfetched before the
	// stale crew section lists it. It matches gt crew stale's day (gt-f1qng).
	overseerStaleCrewAge = 24 * time.Hour

	// overseerStaleCrewRows caps the stale crew section's rows, with the count
	// it left out, so a town with many clones keeps the report short.
	overseerStaleCrewRows = 10
)

// overseerReportNameRe is the whole name of a report file.
var overseerReportNameRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.md$`)

// overseerReportInterval returns the configured interval, or 15m.
func overseerReportInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.OverseerReport != nil {
		if s := config.Patrols.OverseerReport.IntervalStr; s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultOverseerReportInterval
}

// overseerReportTickEvery is how often the patrol ticker fires for a report
// interval: four times a slot, so every slot sees a tick whatever the
// ticker's phase.
func overseerReportTickEvery(interval time.Duration) time.Duration {
	if tick := interval / 4; tick >= time.Second {
		return tick
	}
	return interval
}

// overseerReportSeams are the report's reads, its clock, and where it says what
// it could not do. A test sets all of them; production fills them in
// (overseerReportProdSeams).
type overseerReportSeams struct {
	now    func() time.Time
	digest func(slot, since time.Time) townreport.Digest
	// hour is the run behind the hour sections, ending at slot.
	hour func(slot time.Time) townreport.HourRun
	// logf is where the report says what it will not fail on: a latest.md copy
	// it could not refresh. Production takes the daemon's logger; a seam may
	// leave it nil, and the report then drops the message.
	logf func(format string, args ...any)
	// staleCrewOn adds the stale crew clones section; off unless the patrol
	// config says stale_crew (gt-t3mky).
	staleCrewOn bool
	// staleCrew lists the crew clones the section judges. It only reads.
	staleCrew func() ([]crewfresh.Clone, error)
}

// overseerReportStaleCrewOn reads patrols.overseer_report.stale_crew, false
// when the config does not set it.
func overseerReportStaleCrewOn(config *DaemonPatrolConfig) bool {
	return config != nil && config.Patrols != nil && config.Patrols.OverseerReport != nil &&
		config.Patrols.OverseerReport.StaleCrew
}

// triggerOverseerReport starts one report on its own goroutine unless one is
// already running.
func (d *Daemon) triggerOverseerReport() {
	if !d.overseerReportRunning.CompareAndSwap(false, true) {
		d.logger.Printf("overseer_report: previous run still going, skipping")
		return
	}
	d.overseerReportCycles.Add(1)
	go func() {
		defer d.overseerReportCycles.Done()
		defer d.overseerReportRunning.Store(false)
		d.runOverseerReport()
	}()
}

// runOverseerReport writes the current slot's report and logs the outcome.
func (d *Daemon) runOverseerReport() {
	if !d.isPatrolActive("overseer_report") || d.config == nil {
		return
	}
	seams := d.overseerReportSeams
	if seams == nil {
		seams = d.overseerReportProdSeams()
	}
	path, err := writeOverseerReport(d.config.TownRoot, overseerReportInterval(d.patrolConfig), seams)
	switch {
	case err != nil:
		d.logger.Printf("overseer_report: %v", err)
	case path != "":
		d.logger.Printf("overseer_report: wrote %s", filepath.Base(path))
	}
}

// writeOverseerReport writes the report for the slot the clock is in, and
// returns its path, or "" when the slot already has one. The slot is the
// interval-aligned UTC instant at or before now, and names the file; a report
// at or after the slot ends the run, so a second run in a slot, or a clock
// that stepped back, writes nothing. The report before the slot is "the
// previous report": DONE counts what closed since it, and the first report
// of a clock hour adds the hour sections.
func writeOverseerReport(townRoot string, interval time.Duration, s *overseerReportSeams) (string, error) {
	now := s.now()
	slot := now.UTC().Truncate(interval)
	dir := townreport.ReportsDir(townRoot)

	prev, newest, err := overseerReportPrevious(dir, slot)
	if err != nil {
		return "", err
	}
	if !newest.IsZero() && !newest.Before(slot) {
		if newest.After(now) {
			// Not a covered slot: a report named for an instant the clock has
			// not reached, which the run waits behind. Say so, or the patrol
			// looks alive while writing nothing.
			return "", fmt.Errorf("newest report %s is dated after now (%s), so no report is written until the clock reaches it",
				newest.UTC().Format(overseerReportLayout), now.UTC().Format(overseerReportLayout))
		}
		// The slot already has a report, so this run writes none. Its daily
		// files may still be missing: the run that published the report can
		// have failed after the link and before the sidecar. Repair them from
		// that report's own text, or the Birds card stays on the last sidecar
		// it got until the next slot (gt-ag7u0c rework).
		if err := refreshOverseerDaily(dir, newest); err != nil {
			return "", err
		}
		return "", nil
	}
	since := prev
	if since.IsZero() {
		since = slot.Add(-interval)
	}
	if floor := slot.Add(-overseerReportLookback); since.Before(floor) {
		since = floor
	}

	text := s.digest(slot, since).Render()
	if s.staleCrewOn {
		text += overseerStaleCrewSection(s.staleCrew, now)
	}
	if prev.IsZero() || !prev.Truncate(time.Hour).Equal(slot.Truncate(time.Hour)) {
		var hour bytes.Buffer
		r := s.hour(slot)
		r.Out = &hour
		r.Render(r.Collect())
		text += "\nHOUR\n" + hour.String()
	}
	text = overseerReportClamp(text)

	return writeOverseerReportFile(townRoot, dir, slot, text, s.logf)
}

// overseerReportPrevious reads the report files in dir: prev is the newest
// one before slot, the previous report, and newest the newest of all. A
// missing directory holds no reports.
func overseerReportPrevious(dir string, slot time.Time) (prev, newest time.Time, err error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return time.Time{}, time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !overseerReportNameRe.MatchString(e.Name()) {
			continue
		}
		at, perr := time.Parse(overseerReportLayout, strings.TrimSuffix(e.Name(), overseerReportExt))
		if perr != nil {
			continue
		}
		if at.After(newest) {
			newest = at
		}
		if at.Before(slot) && at.After(prev) {
			prev = at
		}
	}
	return prev, newest, nil
}

// overseerReportClamp keeps text under overseerReportMaxBytes, cutting at a
// line break and saying so.
func overseerReportClamp(text string) string {
	if len(text) <= overseerReportMaxBytes {
		return text
	}
	cut := strings.LastIndexByte(text[:overseerReportMaxBytes-64], '\n')
	if cut < 0 {
		cut = overseerReportMaxBytes - 64
	}
	return text[:cut+1] + "(report cut at the size the dashboard reads)\n"
}

// writeOverseerReportFile writes text as the slot's report, refreshes
// latest.md from the same text, and writes the daily.json and daily.html the
// dashboard's Birds card reads from it. Every name is published by a link or a
// rename and never by a write in place, so a reader gets a whole file or none
// of it: the report is linked into place, which fails if the name is taken, so
// a report the patrol did not write is never replaced, where the copy and the
// two daily files are replaced by every run that writes a report. A latest.md
// refresh that fails is logged and leaves the report write alone: the copy is a
// convenience, the slot's report is the record. base is the town root, the
// directory the run may create levels below and the one above which symlinked
// ancestors are the operator's own (macOS /var is one). logf may be nil, and
// the message is then dropped.
func writeOverseerReportFile(base, dir string, slot time.Time, text string, logf func(string, ...any)) (string, error) {
	// The dashboard reads the bird directory and its reports directory only
	// when both are real directories, so a page published through a symlink
	// here would be one no row shows. The levels are created one at a time
	// below base, each checked before anything is written under it.
	if err := ensureOverseerDirectories(base, dir); err != nil {
		return "", err
	}
	name := slot.UTC().Format(overseerReportLayout) + overseerReportExt
	final := filepath.Join(dir, name)
	tmp, err := writeOverseerTemp(dir, name, text)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, final); err != nil {
		if os.IsExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("writing %s: %w", name, err)
	}
	if err := refreshOverseerLatest(dir, text); err != nil {
		// logf is a seam, and a seam may leave it unset: the report is already
		// in place, so a missing logger costs the message about the copy and
		// never a panic in the patrol's goroutine.
		if logf != nil {
			logf("overseer_report: %v", err)
		}
	}
	if err := writeOverseerDaily(dir, slot, text); err != nil {
		return "", err
	}
	return final, nil
}

// ensureOverseerDirectories creates dir and the levels below base that are
// missing, one at a time, refusing any level that exists but is not a real
// directory. os.MkdirAll is not used because it follows a symlinked level: a
// symlinked bird directory would get its reports directory created inside the
// link's target before any check could refuse the write.
func ensureOverseerDirectories(base, dir string) error {
	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return fmt.Errorf("locating %s under %s: %w", dir, base, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%s is not below the town root %s, so no report is written there", dir, base)
	}
	path := base
	for _, level := range strings.Split(rel, string(filepath.Separator)) {
		if level == "" || level == "." {
			continue
		}
		path = filepath.Join(path, level)
		// The parent was created or checked in the pass before, so a level is
		// only ever made under a directory this loop has seen as real.
		if err := ensureOverseerDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

// ensureOverseerDirectory creates path when it is missing and leaves it alone
// when it is a real directory. A name that exists as anything else is refused:
// the dashboard reads the bird directory and its reports directory only when
// both are real, so a file or a symlink there would hold a page no row shows.
func ensureOverseerDirectory(path string) error {
	err := os.Mkdir(path, 0o755)
	if err == nil {
		return nil
	}
	if !os.IsExist(err) {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	fi, lerr := os.Lstat(path)
	if lerr != nil {
		return fmt.Errorf("reading %s: %w", path, lerr)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a real directory, and the dashboard reads no symlink", path)
	}
	return nil
}

// refreshOverseerDaily writes the daily files for the report at the instant at
// when the sidecar beside them is not for it, so a run in a slot whose daily
// write failed repairs the pair rather than leaving the Birds card at the last
// sidecar it got. The text is the report file's own, never the text a later
// run would render, so the row and the page never describe different reads. A
// file without any section the digest renders is refused: summarizing it would
// publish a clean-looking row built from text no run read.
func refreshOverseerDaily(dir string, at time.Time) error {
	if overseerDailyIsCurrent(dir, at) {
		return nil
	}
	name := at.UTC().Format(overseerReportLayout) + overseerReportExt
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	if !overseerIsDigest(string(raw)) {
		return fmt.Errorf("%s carries none of the sections the digest writes, so no daily files are built from it", name)
	}
	return writeOverseerDaily(dir, at, string(raw))
}

// overseerDailyIsCurrent reports whether dir's daily.json is the sidecar for
// the report at at: one whose written_at is that instant. A sidecar that is
// missing, unreadable, not a valid instant, or written for another instant —
// older, or from a clock that ran ahead — is not current: the caller writes one
// from the report, which either repairs it or fails with the write's own error.
func overseerDailyIsCurrent(dir string, at time.Time) bool {
	raw, err := os.ReadFile(filepath.Join(dir, overseerSidecarName))
	if err != nil {
		return false
	}
	var side struct {
		WrittenAt string `json:"written_at"`
	}
	if err := json.Unmarshal(raw, &side); err != nil {
		return false
	}
	written, err := time.Parse(time.RFC3339, side.WrittenAt)
	if err != nil {
		return false
	}
	return written.Equal(at)
}

// overseerIsDigest reports whether text carries a section the digest renders,
// so a note at a slot's name that this patrol did not write is told apart from
// a report whose daily files need rebuilding. One section is enough: a report
// the size clamp cut short keeps the DONE it starts with.
func overseerIsDigest(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		switch line {
		case "DONE", "IN PROGRESS", "LEFT", "OPEN QUESTIONS":
			return true
		}
	}
	return false
}

// writeOverseerDaily writes the two files the dashboard's Birds card reads:
// daily.json, the sidecar, and daily.html, the page. They are built from the
// text of the report the same run published, so the row and the page never
// describe different runs. A failed write is returned, never logged only: the
// dashboard would otherwise keep showing the previous sidecar as current.
func writeOverseerDaily(dir string, slot time.Time, text string) error {
	// The sidecar's bird is the directory it sits in, the name the dashboard
	// checks it against when it draws the row.
	sidecar, page, err := overseerDailyFiles(filepath.Base(filepath.Dir(dir)), slot, text)
	if err != nil {
		return err
	}
	// The page goes first and the sidecar last: the sidecar's written_at is
	// the row's claim that the report is current, so it is published only
	// once the page it describes is in place.
	for _, f := range []struct {
		name string
		data []byte
	}{
		{overseerPageName, page},
		{overseerSidecarName, sidecar},
	} {
		if err := writeOverseerFileAtomic(dir, f.name, f.data); err != nil {
			return err
		}
	}
	return nil
}

// writeOverseerFileAtomic puts data at dir/name by renaming a temp file onto
// it, so a dashboard reader opens the whole old file or the whole new one. A
// target that is a symlink is refused rather than replaced: the dashboard
// reads no symlink, so a row built over one would never be shown.
func writeOverseerFileAtomic(dir, name string, data []byte) error {
	target := filepath.Join(dir, name)
	// An Lstat that fails for any reason but a missing target is left to the
	// rename below, which takes the same directory permissions and reports its
	// own error, so a path the check cannot read is refused there and never
	// written through.
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink, and the dashboard reads no symlink", target)
	}
	tmp, err := writeOverseerTemp(dir, name, string(data))
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("replacing %s: %w", name, err)
	}
	return nil
}

// overseerDailySidecar is the daily.json the dashboard's Birds card reads. The
// fields and their meanings are the reader's birdSidecar
// (internal/dashboard/birdreports.go); the page is daily.html beside it.
type overseerDailySidecar struct {
	Bird          string                  `json:"bird"`
	WrittenAt     string                  `json:"written_at"`
	Title         string                  `json:"title"`
	Summary       string                  `json:"summary"`
	NeedsOperator bool                    `json:"needs_operator"`
	Decisions     int                     `json:"decisions"`
	DecisionItems []overseerDailyDecision `json:"decision_items,omitempty"`
	// Truncated says part of what this file holds was cut to fit: the list is
	// shorter than Decisions, a question was shortened, or the summary was.
	// The count and the page beside it carry the rest.
	Truncated bool `json:"truncated,omitempty"`
}

// overseerDailyDecision is one open question as the sidecar carries it: the row
// the digest's OPEN QUESTIONS section draws, which names the bead and the work
// it holds. The digest carries no recommended answer, so none is written.
type overseerDailyDecision struct {
	Question       string `json:"question"`
	Recommendation string `json:"recommendation,omitempty"`
}

// overseerDailyFiles builds daily.json and daily.html from the report text.
// written_at is the slot rather than the clock: the slot is at or before now,
// which is the order the dashboard requires, and it keeps the instant the
// report's own name carries.
func overseerDailyFiles(bird string, slot time.Time, text string) (sidecar, page []byte, err error) {
	counts := overseerDigestSectionCounts(text)
	written := slot.UTC().Format(time.RFC3339)
	title := "Overseer report " + written
	summary, summaryCut := overseerCutRunes(overseerDailySummary(counts), overseerSidecarSummaryRunes)
	items, itemsCut := overseerDailyDecisionItems(counts.questions)
	side := overseerDailySidecar{
		Bird:      bird,
		WrittenAt: written,
		Title:     title,
		Summary:   summary,
		// An open question is a decision waiting on the operator, and a
		// question section the digest could not read is not an empty queue.
		NeedsOperator: len(counts.questions) > 0 || counts.questionsUnavailable != "",
		Decisions:     len(counts.questions),
		DecisionItems: items,
		Truncated:     summaryCut || itemsCut,
	}
	// The sidecar is strings, booleans, an int and string slices, whose JSON
	// encoding cannot fail, so the error is dropped.
	raw, _ := json.Marshal(side)
	// The caps above bound runes, and JSON escaping can make the bytes longer
	// than the runes. When the file is still over, the list is what gives: a
	// row with fewer decisions is a smaller loss than a sidecar the dashboard
	// refuses.
	for len(raw) > overseerSidecarMaxBytes && len(side.DecisionItems) > 0 {
		side.DecisionItems = side.DecisionItems[:len(side.DecisionItems)-1]
		side.Truncated = true
		raw, _ = json.Marshal(side)
	}
	if len(raw) > overseerSidecarMaxBytes {
		return nil, nil, fmt.Errorf("%s is over the %d KiB the dashboard reads", overseerSidecarName, overseerSidecarMaxBytes>>10)
	}
	return raw, overseerDailyPage(title, text), nil
}

// overseerDailyDecisionItems cuts the digest's open questions to what the
// dashboard's row shows and says whether anything was left out: at most
// overseerSidecarDecisionItems questions of overseerSidecarDecisionRunes runes
// each. The whole list stays in the report text and on the page beside the
// sidecar.
func overseerDailyDecisionItems(questions []string) (items []overseerDailyDecision, cut bool) {
	for i, q := range questions {
		if i == overseerSidecarDecisionItems {
			return items, true
		}
		question, wasCut := overseerCutRunes(q, overseerSidecarDecisionRunes)
		cut = cut || wasCut
		items = append(items, overseerDailyDecision{Question: question})
	}
	return items, cut
}

// overseerCutRunes returns s cut to at most n runes and whether it was cut.
func overseerCutRunes(s string, n int) (string, bool) {
	if r := []rune(s); len(r) > n {
		return string(r[:n]), true
	}
	return s, false
}

// overseerDigestCounts is what the daily sidecar says about a report: the rows
// of each section the digest renders, the OPEN QUESTIONS rows themselves, the
// reason that section could not be read when it could not, and the health line.
type overseerDigestCounts struct {
	done, working, left  int
	questions            []string
	questionsUnavailable string
	// unavailable names the other sections one of whose sources could not be
	// read, so the summary never counts a partial section as a whole one.
	unavailable []string
	health      string
}

// overseerDigestSectionCounts reads a digest's rows off its rendered text. The
// render is the one read of the sources the run makes, so the sidecar and the
// page describe the same read.
func overseerDigestSectionCounts(text string) overseerDigestCounts {
	var c overseerDigestCounts
	section := ""
	for _, line := range strings.Split(text, "\n") {
		switch line {
		case "DONE", "IN PROGRESS", "LEFT", "OPEN QUESTIONS":
			section = line
			continue
		}
		if line == "" {
			section = ""
			continue
		}
		if rest, ok := strings.CutPrefix(line, "health: "); ok {
			c.health = strings.TrimSpace(rest)
			section = ""
			continue
		}
		if section == "" || !strings.HasPrefix(line, "  ") {
			continue
		}
		row := strings.Join(strings.Fields(line), " ")
		if row == "(none)" || strings.HasPrefix(row, "+") {
			continue
		}
		if strings.HasPrefix(row, "unavailable (") {
			if section == "OPEN QUESTIONS" {
				c.questionsUnavailable = row
			} else if n := len(c.unavailable); n == 0 || c.unavailable[n-1] != section {
				// Rows of a section are contiguous, so the last name is the
				// only repeat to guard against (DONE reads one row per store).
				c.unavailable = append(c.unavailable, section)
			}
			continue
		}
		switch section {
		case "DONE":
			c.done++
		case "IN PROGRESS":
			c.working++
		case "LEFT":
			c.left++
		case "OPEN QUESTIONS":
			c.questions = append(c.questions, row)
		}
	}
	return c
}

// overseerDailySummary is the sidecar's summary: the section counts and the
// health line, with a section the digest could not read said in the same
// sentence rather than counted as a whole or empty one.
func overseerDailySummary(c overseerDigestCounts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d done, %d in progress, %d queued or held", c.done, c.working, c.left)
	if c.questionsUnavailable != "" {
		fmt.Fprintf(&b, "; open questions %s", c.questionsUnavailable)
	} else {
		fmt.Fprintf(&b, ", %d open questions", len(c.questions))
	}
	if len(c.unavailable) > 0 {
		fmt.Fprintf(&b, "; partly unreadable: %s", strings.Join(c.unavailable, ", "))
	}
	if c.health != "" {
		b.WriteString(". Health: " + c.health + ".")
	} else {
		b.WriteString(".")
	}
	return b.String()
}

// overseerDailyPage is daily.html: the report text escaped under the title the
// sidecar carries.
func overseerDailyPage(title, text string) []byte {
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>")
	b.WriteString(html.EscapeString(title))
	b.WriteString("</title>\n</head>\n<body>\n<h1>")
	b.WriteString(html.EscapeString(title))
	b.WriteString("</h1>\n<pre>")
	b.WriteString(html.EscapeString(text))
	b.WriteString("</pre>\n</body>\n</html>\n")
	return []byte(b.String())
}

// refreshOverseerLatest puts text at dir/latest.md by renaming a temp file onto
// it, so the name holds a whole report: a reader opens the old copy or the new
// one, never a half-written file. The copy is its own file and not a second
// link to the report, since it is the one a human may annotate by hand and the
// report is the record.
func refreshOverseerLatest(dir, text string) error {
	tmp, err := writeOverseerTemp(dir, overseerLatestName, text)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Rename(tmp, filepath.Join(dir, overseerLatestName)); err != nil {
		return fmt.Errorf("replacing %s: %w", overseerLatestName, err)
	}
	return nil
}

// writeOverseerTemp writes text to a fresh file in dir, named after hint, and
// returns its path: a whole file the caller puts in place under the name a
// reader uses. The caller owns it and removes it.
func writeOverseerTemp(dir, hint, text string) (string, error) {
	tmp, err := os.CreateTemp(dir, ".tmp-"+hint+"-*")
	if err != nil {
		return "", fmt.Errorf("creating a file in %s: %w", dir, err)
	}
	fail := func(err error) (string, error) {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("writing %s: %w", hint, err)
	}
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	// CreateTemp makes the file 0600, and whoever runs the dashboard reads the
	// reports.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fail(err)
	}
	return tmp.Name(), nil
}

// overseerReportProdSeams reads the town: bd pinned read-only for every
// store, the working seats the overrun guard reads, the spec dispatcher's dry
// run and the town health file.
func (d *Daemon) overseerReportProdSeams() *overseerReportSeams {
	townRoot := d.config.TownRoot
	rigs := func() ([]string, error) { return d.getKnownRigs(), nil } //nolint:unparam // the seam's signature carries the error gt report's registry read can return
	store := func(name string) (townreport.BeadStore, error) {
		dir, err := townreport.StoreDir(townRoot, name)
		if err != nil {
			return nil, err
		}
		b := beads.NewPlain(townRoot, bdReadOnlyPinnedEnv(beads.ResolveBeadsDir(dir)),
			beads.WithBin(d.bdPathOrDefault())).WithTimeout(overseerReportBeadTimeout)
		return townreport.WrapStore(b), nil
	}
	return &overseerReportSeams{
		now:         time.Now,
		logf:        d.logger.Printf,
		staleCrewOn: overseerReportStaleCrewOn(d.patrolConfig),
		// staleCrew reads the crew clones under the town over the real disk
		// and git. It only reads: no clone is fetched or moved.
		staleCrew: func() ([]crewfresh.Clone, error) {
			return crewstale.Clones(townRoot, crewstale.Disk{}, func(root string) crewstale.Git { return git.NewGit(root) }, d.clk().Now())
		},
		digest: func(slot, since time.Time) townreport.Digest {
			return townreport.Digest{
				Now:    d.clk().Now(),
				Slot:   slot,
				Since:  since,
				Rigs:   rigs,
				Store:  store,
				Seats:  d.overseerReportSeats,
				Left:   d.overseerReportLeft,
				Health: d.overseerReportHealth,
			}
		},
		hour: func(slot time.Time) townreport.HourRun {
			// The daemon cannot name the installed gt's version (that is a
			// cmd ldflag), so the mains row names the running build.
			r := townreport.NewHourRun(townRoot, slot, "daemon", d.buildCommit(), rigs)
			r.Store = store
			return r
		},
	}
}

// overseerReportSeats lists the working seats and dates each from its bead's
// dispatch clock, the one the overrun guard judges.
func (d *Daemon) overseerReportSeats() ([]townreport.SeatWork, error) {
	seats, err := d.overrunWorkingSeats()
	if err != nil {
		return nil, err
	}
	return overseerSeatWork(seats, (&overrunSlungReader{d: d}).slungAt), nil
}

// overseerSeatWork dates each seat that holds a bead from that bead's dispatch
// clock (events.DispatchStart, else the seat's agent bead): the earliest sling
// of the bead this run, not the sling that reached this seat, so a row whose
// bead changed hands reads the time the work has been in progress. It is the
// same clock the overrun guard judges, submission end included (gt-chd44).
func overseerSeatWork(seats []overrunSeat, slungAt func(seat overrunSeat) (time.Time, bool)) []townreport.SeatWork {
	out := make([]townreport.SeatWork, 0, len(seats))
	for _, s := range seats {
		if s.Bead == "" {
			continue
		}
		since, _ := slungAt(s)
		out = append(out, townreport.SeatWork{Rig: s.Rig, Polecat: s.Polecat, Bead: s.Bead, Since: since})
	}
	return out
}

// overseerReportLeft asks the spec dispatcher what it would do now, without
// slinging, labeling or commenting. It runs the dry run with the flags the
// next real tick runs with, so a bead with a sling in flight is neither next in
// the queue nor counted as a free seat (gt-nosod).
func (d *Daemon) overseerReportLeft() (townreport.DispatchLeft, error) {
	args, err := d.specTickArgs()
	if err != nil {
		return townreport.DispatchLeft{}, err
	}
	out, err := d.runSpecDispatchCommand(d.specDispatchTickBound(), append(args, "--dry-run")...)
	if err != nil {
		return townreport.DispatchLeft{}, err
	}
	r, err := parseSpecDispatchReport(out)
	if err != nil {
		return townreport.DispatchLeft{}, fmt.Errorf("unparseable dispatch output: %w", err)
	}
	return overseerReportLeftFrom(r)
}

// overseerReportLeftFrom sorts a dry run's decisions. A bead the dry run would
// sling, or finds clean but without a seat or past the per-tick limit, is
// next in the queue, in the order the dispatcher named them; every other bead
// it declined is held, with its reason. The beads the dispatcher holds out
// with the spec-dispatch-failed label have no entry, only a count, which LEFT
// carries so a bead stuck behind the label is not read as an empty queue.
//
// A dry run that exits 0 can still carry Errors (an unreadable roster, a
// failed board read), and then its empty queue means "could not read", not "no
// ready work". Such a report with no decided bead is an error, so LEFT renders
// as unavailable the way a failed command does: an unreadable roster stops the
// run before it decides anything. Once any bead was decided, an error is one
// bead's failed read, and LEFT keeps the decided rows and carries the count.
func overseerReportLeftFrom(r specDispatchTickReport) (townreport.DispatchLeft, error) {
	decided := len(r.Dispatched)+len(r.Skipped)+len(r.Refused)+len(r.Planning)+len(r.Failed) > 0 || r.LabeledFailed > 0
	if len(r.Errors) > 0 && !decided {
		err := fmt.Errorf("dispatch dry run reported errors: %s", r.Errors[0])
		if more := len(r.Errors) - 1; more > 0 {
			err = fmt.Errorf("%w (+%d more)", err, more)
		}
		return townreport.DispatchLeft{}, err
	}
	left := townreport.DispatchLeft{Hold: r.Hold, LabeledFailed: r.LabeledFailed, Errors: len(r.Errors)}
	row := func(e specDispatchTickEntry, prefix string) townreport.DispatchRow {
		return townreport.DispatchRow{Bead: e.Bead, Rig: e.Rig, Text: prefix + specSkipReason(e)}
	}
	for _, e := range r.Dispatched {
		left.Next = append(left.Next, row(e, ""))
	}
	for _, e := range r.Skipped {
		reason := specSkipReason(e)
		// A pin to an agent the pool lacks waits on no seat that will free up,
		// so it is held, not next.
		waitsForSeat := strings.HasPrefix(reason, "no seat:") && !strings.HasPrefix(reason, "no seat: pinned to ")
		if strings.HasPrefix(reason, "clean, per-tick limit") || waitsForSeat {
			left.Next = append(left.Next, row(e, ""))
			continue
		}
		left.Held = append(left.Held, row(e, ""))
	}
	for _, g := range []struct {
		name    string
		entries []specDispatchTickEntry
	}{{"refused", r.Refused}, {"planning", r.Planning}, {"failed", r.Failed}} {
		for _, e := range g.entries {
			left.Held = append(left.Held, row(e, g.name+": "))
		}
	}
	return left, nil
}

// overseerReportHealth is the town health line from the file the heartbeat
// writes, judged stale by the same threshold the health computation uses.
func (d *Daemon) overseerReportHealth() (string, error) {
	r, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		return "", err
	}
	_, stale, err := d.loadOperationalConfig().GetHealthSettings().Resolve()
	if err != nil {
		stale = townhealth.DefaultStaleAfter
	}
	return townhealth.Line(r, d.clk().Now(), stale), nil
}

// overseerStaleCrewSection is the stale crew clones section: the clones not
// fetched within overseerStaleCrewAge before now, oldest fetch first, or the
// reason the list could not be read. A failed list is one line, not an empty
// section, since an empty section reads as every clone being fresh (gt-t3mky).
func overseerStaleCrewSection(list func() ([]crewfresh.Clone, error), now time.Time) string {
	var b strings.Builder
	b.WriteString("\nSTALE CREW CLONES\n")
	var clones []crewfresh.Clone
	err := errors.New("no crew clone source is wired")
	if list != nil {
		clones, err = list()
	}
	if err != nil {
		b.WriteString("  unavailable (" + townreport.Scrub(err.Error()) + ")\n")
		return b.String()
	}
	stale := crewfresh.Stale(clones, now, overseerStaleCrewAge)
	if len(stale) == 0 {
		b.WriteString("  (none)\n")
	}
	for i, c := range stale {
		if i == overseerStaleCrewRows {
			fmt.Fprintf(&b, "  +%d more\n", len(stale)-i)
			break
		}
		b.WriteString("  " + overseerStaleCrewLine(c, now) + "\n")
	}
	return b.String()
}

// overseerStaleCrewLine is one clone's row: rig/dir, branch, hours since its
// last fetch and the commits it is behind origin/main. A clone that could not
// be read shows the reason instead, since its zero fields would read as fresh.
func overseerStaleCrewLine(c crewfresh.Clone, now time.Time) string {
	if c.Err != "" {
		return fmt.Sprintf("%s/%s  unreadable (%s)", c.Rig, c.Dir, c.Err)
	}
	branch := c.Branch
	if branch == "" {
		branch = "detached"
	}
	age := "never"
	if !c.LastFetch.IsZero() {
		age = fmt.Sprintf("fetched %dh ago", int(now.Sub(c.LastFetch).Hours()))
	}
	return fmt.Sprintf("%s/%s  %s  %s  %d behind origin/main", c.Rig, c.Dir, branch, age, c.Behind)
}
