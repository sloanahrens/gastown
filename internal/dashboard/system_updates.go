package dashboard

import (
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// The System updates section joins the three places a landed commit shows up —
// its tier sweep, its post-land and images pipeline runs, and its roll — into
// one row per commit, newest first. The join is by commit, not by time: a sweep
// logs an eight-character sha and a roll records the full one, so a row's parts
// are found by prefix (gt-yu15gd).

const (
	// systemUpdatesRowCap is how many commit rows the System updates section draws.
	systemUpdatesRowCap = 6

	// systemUpdateMinSHA is the shortest sha a join trusts, the floor
	// update.verify.go uses, so a blank or stubbed value never joins a commit.
	systemUpdateMinSHA = 7
)

// SystemUpdateRoll is one finished gt update run as the System updates section
// draws it: the build it moved from and to, its outcome and step, and its wall
// time. internal/cmd maps each update.ReadHistory line into one, the way it maps
// update.State into UpdateStatus, because this package cannot import
// internal/update: the beads test that imports this package cycles through
// notify (gt-yu15gd).
type SystemUpdateRoll struct {
	From      string    `json:"from,omitempty"`
	To        string    `json:"to"`
	Outcome   string    `json:"outcome"`
	Current   string    `json:"current,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
}

// The states a commit row's Pair names.
const (
	pairAbsent   = "absent"
	pairBuilding = "building"
	pairReady    = "ready"
	pairFailed   = "failed"
	pairRefused  = "refused"
	pairStuck    = "stuck"
	pairUnknown  = "unknown"
)

// SystemUpdateRow is one landed commit as the System updates section draws it:
// its sha cut to twelve characters, whichever of the three stages have run, and
// the state of its image pair.
type SystemUpdateRow struct {
	SHA   string            `json:"sha"`
	Sweep *TierSweepRow     `json:"sweep,omitempty"`
	Runs  []PipelineRun     `json:"runs,omitempty"`
	Roll  *SystemUpdateRoll `json:"roll,omitempty"`
	// Pair is the commit's images pair as the hub classified it, one of the
	// pair constants; it is empty only before the hub fills it.
	Pair string `json:"pair"`
}

// SystemUpdateRows joins sweeps, pipeline runs and rolls into at most limit rows,
// newest first; limit six draws the section.
func SystemUpdateRows(sweeps []TierSweepRow, pipeline *GastownPipeline, rolls []SystemUpdateRoll, limit int) []SystemUpdateRow {
	if limit <= 0 {
		return []SystemUpdateRow{}
	}
	commits := joinSystemUpdates(sweeps, pipeline, rolls)
	sort.SliceStable(commits, func(i, j int) bool { return commits[i].at.After(commits[j].at) })
	if len(commits) > limit {
		commits = commits[:limit]
	}
	rows := make([]SystemUpdateRow, 0, len(commits))
	for _, c := range commits {
		rows = append(rows, SystemUpdateRow{SHA: pipelineSHA(c.sha), Sweep: c.sweep, Runs: c.runs, Roll: c.roll})
	}
	return rows
}

// systemCommit is one commit while the inputs are folded together: the longest
// sha seen, the newest timestamp any stage reported, and the stages themselves.
type systemCommit struct {
	sha   string
	at    time.Time
	sweep *TierSweepRow
	runs  []PipelineRun
	roll  *SystemUpdateRoll
}

func joinSystemUpdates(sweeps []TierSweepRow, pipeline *GastownPipeline, rolls []SystemUpdateRoll) []*systemCommit {
	var commits []*systemCommit
	// find returns the commit a sha names, adding it, and keeps the longest sha
	// seen so the row is named by twelve characters when any stage recorded
	// that many. A sha under systemUpdateMinSHA names no commit.
	find := func(sha string) *systemCommit {
		sha = strings.TrimSpace(sha)
		if len(sha) < systemUpdateMinSHA {
			return nil
		}
		for _, c := range commits {
			if sameCommit(c.sha, sha) {
				if len(sha) > len(c.sha) {
					c.sha = sha
				}
				return c
			}
		}
		c := &systemCommit{sha: sha}
		commits = append(commits, c)
		return c
	}

	for i := range sweeps {
		s := sweeps[i]
		c := find(s.SHA)
		if c == nil {
			continue
		}
		// A commit can be swept once per rig; the newest sweep is the row's.
		if c.sweep == nil || s.At.After(c.sweep.At) {
			sweep := s
			c.sweep = &sweep
		}
		c.at = newest(c.at, s.At)
	}

	if pipeline != nil {
		for _, run := range pipeline.Runs {
			c := find(run.SHA)
			if c == nil {
				continue
			}
			c.runs = append(c.runs, run)
			c.at = newest(c.at, newest(run.Started, run.Ended))
		}
	}

	for i := range rolls {
		r := rolls[i]
		c := find(r.To)
		if c == nil {
			continue
		}
		// A retried roll targets the same commit; the newest roll is the row's.
		if c.roll == nil || rollTime(r).After(rollTime(*c.roll)) {
			roll := r
			c.roll = &roll
		}
		c.at = newest(c.at, rollTime(r))
	}

	return commits
}

// sameCommit reports whether two shas name one commit: one is a prefix of the
// other and the shorter has at least systemUpdateMinSHA characters.
func sameCommit(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if len(a) < len(b) {
		a, b = b, a
	}
	return len(b) >= systemUpdateMinSHA && strings.HasPrefix(a, b)
}

// rollTime is when a roll last showed progress: its end once it ended, else its
// start.
func rollTime(r SystemUpdateRoll) time.Time {
	return newest(r.StartedAt, r.EndedAt)
}

func newest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// classifySystemUpdatePairs sets each row's Pair from the hub's attention items
// and the images run the row carries. A queue that was not read leaves every row
// unknown, never absent, so an attention read that failed, has not run yet, or
// has no items reader does not read as a commit with no pair.
//
// Several readings can name one commit, so the state is taken in this order:
// refused, failed, stuck, ready, building, absent. The green ref's own
// publication is not read: a commit whose images have not started is absent, and
// a ref that failed to publish is the green-ref attention item, which is not one
// of the kinds mapped here.
func classifySystemUpdatePairs(rows []SystemUpdateRow, items []attention.Item, queueKnown bool) {
	for i := range rows {
		rows[i].Pair = systemUpdatePair(rows[i], items, queueKnown)
	}
}

// systemUpdatePair is one row's state: the attention items that name its commit
// by the join's own prefix rule, and the status of its newest images run. An
// unread or failed queue known nothing, so the state is unknown.
func systemUpdatePair(row SystemUpdateRow, items []attention.Item, queueKnown bool) string {
	if !queueKnown {
		return pairUnknown
	}
	var ready, failed, refused, stuck, building bool
	for _, it := range items {
		if !sameCommit(it.SHA, row.SHA) {
			continue
		}
		switch it.Kind {
		case attention.KindDeployPairRefused:
			refused = true
		case attention.KindImagesFailed:
			failed = true
		case attention.KindDeployPairStuck:
			stuck = true
		case attention.KindDeployPairReady:
			ready = true
		}
	}
	switch newestImagesStatus(row.Runs) {
	case pipelineRunning, pipelineQueued:
		building = true
	case pipelineFailure:
		failed = true
	}
	switch {
	case refused:
		return pairRefused
	case failed:
		return pairFailed
	case stuck:
		return pairStuck
	case ready:
		return pairReady
	case building:
		return pairBuilding
	}
	return pairAbsent
}

// newestImagesStatus is the status of the newest images run the row carries. The
// pipeline lists runs newest first and the join keeps that order, so the first
// images run is the newest; a run of the post-land workflow is skipped.
func newestImagesStatus(runs []PipelineRun) string {
	for _, run := range runs {
		if run.Workflow == imagesWorkflow {
			return run.Status
		}
	}
	return ""
}
