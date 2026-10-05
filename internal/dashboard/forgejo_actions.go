package dashboard

import (
	"context"
	"regexp"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

const (
	// forgejoRunPage bounds one workflow-run query. Current is read once per
	// status and completed once for every finished status, so a repo with a
	// deep history still costs a fixed, small number of calls rather than a
	// walk back through it.
	forgejoRunPage = 20
	// forgejoRecentKept is how many completed runs the section lists.
	forgejoRecentKept = 5
	// forgejoStatsKept is the window the ok, failed and median stats are taken
	// over: the newest completed runs across every repo.
	forgejoStatsKept = 20
	// forgejoTitleKept is how much of a run title stands in for the bead id
	// when the title carries none.
	forgejoTitleKept = 24
	// forgejoHashKept is how much of a run's commit sha a row shows: the eight
	// characters the Actions page's own rows carry.
	forgejoHashKept = 8
)

// forgejoCurrentStatuses are the statuses of a run the section calls current:
// one that is queued (waiting or blocked) or running.
var forgejoCurrentStatuses = []string{"waiting", "blocked", "running"}

// forgejoRunCancelled is the run status Forgejo reports for a canceled run.
const forgejoRunCancelled = "cancelled" //nolint:misspell // the status the API reports

// forgejoCompletedStatuses are the statuses of a finished run: the set the
// recent list and the stats window are drawn from.
var forgejoCompletedStatuses = []string{"success", "failure", forgejoRunCancelled, "skipped"}

// ForgejoAction is one workflow-run line of the panel's Actions section.
type ForgejoAction struct {
	// Repo is the owner/name, the same value the activity feed's rows carry
	// (forgejoRepoName), so one pane names a repo one way.
	Repo string `json:"repo"`
	// Bead is the bead id the run's title carries, or the title's head when it
	// carries none.
	Bead string `json:"bead"`
	// Status and Event are the API's own words for the run, which the page
	// colors by.
	Status string `json:"status"`
	Event  string `json:"event"`
	// Started and Stopped are RFC3339, absent until the run has started or
	// ended: a queued run has neither.
	Started string `json:"started,omitempty"`
	Stopped string `json:"stopped,omitempty"`
	// DurationSecs is the run's elapsed time in seconds.
	DurationSecs float64 `json:"duration_secs"`
	URL          string  `json:"url"`
	// Hash is the commit the run tested, cut to the eight characters the
	// Actions page shows, and SHA is the whole one for the row's tooltip; both
	// are absent when the API sent no commit, which the page reads as no link.
	Hash string `json:"hash,omitempty"`
	SHA  string `json:"sha,omitempty"`
	// Number is the run's index in its repo, the "#20" the Actions page shows
	// beside the hash so a row can be named without opening it.
	Number int64 `json:"number,omitempty"`
}

// ForgejoActionStats is the section's one-line summary. OK, Failed and
// MedianSecs are taken over the newest forgejoStatsKept completed runs across
// every repo, so a run that has not ended yet cannot weight them.
type ForgejoActionStats struct {
	Running    int     `json:"running"`
	Queued     int     `json:"queued"`
	OK         int     `json:"ok"`
	Failed     int     `json:"failed"`
	MedianSecs float64 `json:"median_secs"`
}

// ForgejoActions is the Actions section's data. Errors names the repos whose
// run lists could not be read; their runs are simply missing from Current,
// Recent and Stats rather than costing the section the repos that answered.
type ForgejoActions struct {
	Current []ForgejoAction    `json:"current"`
	Recent  []ForgejoAction    `json:"recent"`
	Stats   ForgejoActionStats `json:"stats"`
	Errors  []string           `json:"errors,omitempty"`
}

// repoRuns is one repo's workflow runs, or the error that kept the panel from
// reading them.
type repoRuns struct {
	repo      string
	current   []forgejo.ActionRun
	completed []forgejo.ActionRun
	err       error
}

// readRepoRuns reads one repo's current and completed runs under the caller's
// deadline: one query per current status (each its own page, so one busy
// status cannot hide the others) and one for every finished status together.
func (r *ForgejoReader) readRepoRuns(ctx context.Context, name, owner, repo string) repoRuns {
	out := repoRuns{repo: name}
	for _, status := range forgejoCurrentStatuses {
		list, err := r.api.ListRuns(ctx, owner, repo, forgejo.RunFilter{Status: []string{status}, Limit: forgejoRunPage})
		if err != nil {
			out.err = err
			return out
		}
		out.current = append(out.current, list.Runs...)
	}
	list, err := r.api.ListRuns(ctx, owner, repo, forgejo.RunFilter{Status: forgejoCompletedStatuses, Limit: forgejoRunPage})
	if err != nil {
		out.err = err
		return out
	}
	out.completed = list.Runs
	return out
}

// mergeForgejoRuns folds every repo's runs into the Actions section: the
// current runs oldest first, the newest forgejoRecentKept completed, and the
// stats over the newest forgejoStatsKept completed. A repo whose list failed is
// named in errors and otherwise skipped. A nil return means no repo answered,
// which the reader turns into the section's last good value (gt-fn9e6.47).
func mergeForgejoRuns(repos []repoRuns) *ForgejoActions {
	var (
		errs      []string
		current   []runRow
		completed []runRow
	)
	read := 0
	for _, rr := range repos {
		if rr.err != nil {
			errs = append(errs, rr.repo)
			continue
		}
		read++
		for _, run := range rr.current {
			current = append(current, newRunRow(rr.repo, run))
		}
		for _, run := range rr.completed {
			completed = append(completed, newRunRow(rr.repo, run))
		}
	}
	if read == 0 {
		return nil
	}

	sort.SliceStable(current, func(i, j int) bool { return current[i].queuedAt().Before(current[j].queuedAt()) })
	sort.SliceStable(completed, func(i, j int) bool { return completed[i].finishedAt().After(completed[j].finishedAt()) })

	stats := ForgejoActionStats{}
	for _, row := range current {
		switch row.run.Status {
		case "running":
			stats.Running++
		case "waiting", "blocked":
			stats.Queued++
		}
	}
	window := completed
	if len(window) > forgejoStatsKept {
		window = window[:forgejoStatsKept]
	}
	durations := make([]float64, 0, len(window))
	for _, row := range window {
		switch row.run.Status {
		case "success":
			stats.OK++
		case "failure":
			stats.Failed++
		}
		durations = append(durations, row.seconds())
	}
	stats.MedianSecs = medianSecs(durations)

	recent := completed
	if len(recent) > forgejoRecentKept {
		recent = recent[:forgejoRecentKept]
	}
	return &ForgejoActions{
		Current: rowsToActions(current),
		Recent:  rowsToActions(recent),
		Stats:   stats,
		Errors:  errs,
	}
}

// runRow is one run with the repo it belongs to and its stamps parsed once, so
// ordering, the stats window and the row the page draws all agree on what the
// API sent.
type runRow struct {
	repo    string
	run     forgejo.ActionRun
	created time.Time
	started time.Time
	stopped time.Time
}

func newRunRow(repo string, run forgejo.ActionRun) runRow {
	return runRow{
		repo:    repo,
		run:     run,
		created: parseRunStamp(run.Created),
		started: parseRunStamp(run.Started),
		stopped: parseRunStamp(run.Stopped),
	}
}

// queuedAt orders the current list oldest first. A waiting run has no start
// yet, so its creation is the earliest time it has.
func (r runRow) queuedAt() time.Time {
	if r.created.IsZero() {
		return r.started
	}
	return r.created
}

// finishedAt orders completed runs newest first; a run that never recorded a
// stop falls back to its creation.
func (r runRow) finishedAt() time.Time {
	if r.stopped.IsZero() {
		return r.created
	}
	return r.stopped
}

// seconds is the run's duration in seconds; the API counts nanoseconds.
func (r runRow) seconds() float64 { return float64(r.run.Duration) / float64(time.Second) }

func (r runRow) action() ForgejoAction {
	return ForgejoAction{
		Repo:         r.repo,
		Bead:         forgejoRunBead(r.run.Title),
		Status:       r.run.Status,
		Event:        r.run.Event,
		Started:      runStamp(r.started),
		Stopped:      runStamp(r.stopped),
		DurationSecs: r.seconds(),
		URL:          r.run.HTMLURL,
		Hash:         shortSHA(r.run.CommitSHA),
		SHA:          r.run.CommitSHA,
		Number:       r.run.Index,
	}
}

// shortSHA is the commit as the row shows it: its first forgejoHashKept
// characters, so a row names which commit ran without spending the cell on
// forty. A sha the API never sent stays empty (gt-qes2r).
func shortSHA(sha string) string {
	if len(sha) <= forgejoHashKept {
		return sha
	}
	return sha[:forgejoHashKept]
}

// rowsToActions renders rows for the page. An empty list is an empty array,
// never null: the page would draw nothing for null rather than its idle line.
func rowsToActions(rows []runRow) []ForgejoAction {
	out := make([]ForgejoAction, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.action())
	}
	return out
}

// runStamp renders a parsed stamp the way the API sent it. A zero stamp is
// left out rather than rendered as year 1: the page reads an absent start as
// "not started".
func runStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseRunStamp reads one of the API's RFC3339 stamps. An empty or unparseable
// one is the zero time, which orders last and renders as absent.
func parseRunStamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// medianSecs is the median of the durations: the middle value, or the mean of
// the two middle ones, so an even window has a well-defined answer.
func medianSecs(d []float64) float64 {
	if len(d) == 0 {
		return 0
	}
	sort.Float64s(d)
	mid := len(d) / 2
	if len(d)%2 == 1 {
		return d[mid]
	}
	return (d[mid-1] + d[mid]) / 2
}

var (
	// runBeadRe matches a bead id as a run title writes one: a lowercase prefix,
	// a dash and the id body with its sub-id dots (gt-fn9e6.39). The "+" that
	// joins a landing branch's molecule does not match, so the id stops at the
	// bead and leaves the molecule off.
	runBeadRe = regexp.MustCompile(`[a-z][a-z0-9]*-[a-z0-9][a-z0-9]*(?:\.[a-z0-9]+)*`)
	// runParenRe is one parenthesized span of a title.
	runParenRe = regexp.MustCompile(`\(([^()]*)\)`)
)

// forgejoRunBead names the bead a run belongs to. A landing title carries the
// id in the branch the run tested ("land: polecat/obsidian/gt-fn9e6.39+muvj72nn
// (c7210be8) onto main (dd809ca6)" -> "gt-fn9e6.39"), and a push run's title is
// the commit subject, which ends with the id in parentheses. The parenthesized
// spans are read first so that a dashed word in the subject does not win
// ("feat: forgejo-cutover ... (gt-fn9e6.40)"). A title carrying no id at all
// falls back to its first forgejoTitleKept characters, so a row always says
// what ran.
func forgejoRunBead(title string) string {
	for _, span := range runParenRe.FindAllStringSubmatch(title, -1) {
		if id := runBeadRe.FindString(span[1]); id != "" {
			return id
		}
	}
	if id := runBeadRe.FindString(title); id != "" {
		return id
	}
	return firstRunes(title, forgejoTitleKept)
}

// firstRunes is the first n runes of s, so a title with multibyte characters is
// not cut in the middle of one.
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
