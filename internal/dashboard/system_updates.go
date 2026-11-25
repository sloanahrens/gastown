package dashboard

import (
	"slices"
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

	// systemUpdatesSweepCap is how many of the gastown rig's newest sweeps the
	// section draws.
	systemUpdatesSweepCap = 5

	// systemUpdatesRig is the rig the section's sweeps are read for. Its pipeline
	// and rolls are the gastown repository's, and the feed names the same rig
	// when it reads them (dashboard.go, rigForgejoRemote). Other rigs' sweeps are
	// left out of Sweeps and LastGreen.
	systemUpdatesRig = "gastown"
)

// SweepGreen is the newest sweep of the gastown rig that was all GREEN: the sha
// as the sweep wrote it, eight characters, and when the sweep ran.
type SweepGreen struct {
	SHA string    `json:"sha"`
	At  time.Time `json:"at,omitzero"`
}

// SystemUpdateRoll is one finished gt update run as the System updates section
// draws it: the build it moved from and to, its outcome and step, and its wall
// time. internal/cmd maps each update.ReadHistory line into one, the way it maps
// update.State into UpdateStatus, because this package cannot import
// internal/update: the beads test that imports this package cycles through
// notify (gt-yu15gd).
type SystemUpdateRoll struct {
	From    string `json:"from,omitempty"`
	To      string `json:"to"`
	Outcome string `json:"outcome"`
	Current string `json:"current,omitempty"`
	// Step names the step a partial roll did not pass, so the pane can say why
	// the roll is not ok; empty when the record names none (gt-c8yqh9).
	Step      string    `json:"step,omitempty"`
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
// its sha cut to twelve characters, when it last changed, whichever of the three
// stages have run, and the state of its image pair.
type SystemUpdateRow struct {
	SHA string `json:"sha"`
	// At is the newest time any stage of the commit reported, the same stamp the
	// section orders rows by, so the drawn order can be read against it. Zero
	// when no stage reported a time.
	At    time.Time         `json:"at,omitzero"`
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
	rows, _ := SystemUpdateRowsWithMore(sweeps, pipeline, rolls, limit)
	return rows
}

// SystemUpdateRowsWithMore is SystemUpdateRows and how many joined commits the
// limit left out, so the section can say how many older commits it does not show
// (gt-71xpmh). A limit of zero or less draws no row, so every joined commit is
// reported left out.
//
// Each keep is a sha the section draws even when the limit cut its commit: the
// commit under a sweep in flight, and the last green sweep's (gt-hueeyb). A keep
// that no joined commit names gets a row with the sha and no stages, so the
// panel shows it while the join has nothing for it. Kept rows follow the newest
// rows, newest first among themselves; a keep the drawn rows or an earlier keep
// already name is not repeated. More counts only joined commits, so a row with
// no join is not counted as left out. A limit of zero or less adds no keep.
func SystemUpdateRowsWithMore(sweeps []TierSweepRow, pipeline *GastownPipeline, rolls []SystemUpdateRoll, limit int, keeps ...string) ([]SystemUpdateRow, int) {
	commits := joinSystemUpdates(sweeps, pipeline, rolls)
	sort.SliceStable(commits, func(i, j int) bool { return commits[i].at.After(commits[j].at) })
	shown := len(commits)
	if limit <= 0 {
		shown = 0
	} else if shown > limit {
		shown = limit
	}
	rows := make([]SystemUpdateRow, 0, shown)
	for _, c := range commits[:shown] {
		rows = append(rows, systemUpdateRow(c))
	}
	if limit <= 0 {
		return rows, len(commits)
	}
	kept, keptJoined := keptSystemUpdateRows(commits[:shown], commits[shown:], keeps)
	return append(rows, kept...), len(commits) - shown - keptJoined
}

// systemUpdateRow is the row a commit draws as; a commit no stage joined has no stages.
func systemUpdateRow(c *systemCommit) SystemUpdateRow {
	return SystemUpdateRow{SHA: pipelineSHA(c.sha), At: c.at, Sweep: c.sweep, Runs: c.runs, Roll: c.roll}
}

// keptSystemUpdateRows is the row each keep adds beyond the drawn commits. A keep
// names a cut commit when one matches it, and otherwise a commit with no stages.
// It returns the rows newest first, and how many of them name a joined commit.
func keptSystemUpdateRows(drawn, cut []*systemCommit, keeps []string) ([]SystemUpdateRow, int) {
	var picked []*systemCommit
	joined := 0
	for _, keep := range keeps {
		sha := strings.TrimSpace(keep)
		if len(sha) < systemUpdateMinSHA || commitNamed(drawn, sha) != nil || commitNamed(picked, sha) != nil {
			continue
		}
		c := commitNamed(cut, sha)
		if c != nil {
			joined++
		} else {
			c = &systemCommit{sha: sha}
		}
		picked = append(picked, c)
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].at.After(picked[j].at) })
	rows := make([]SystemUpdateRow, 0, len(picked))
	for _, c := range picked {
		rows = append(rows, systemUpdateRow(c))
	}
	return rows, joined
}

// commitNamed is the commit in commits that sha names, or nil when none does.
func commitNamed(commits []*systemCommit, sha string) *systemCommit {
	for _, c := range commits {
		if sameCommit(c.sha, sha) {
			return c
		}
	}
	return nil
}

// systemCommit is one commit while the inputs are folded together: the longest
// sha seen, the newest and the earliest timestamp any stage reported, and the
// stages themselves.
type systemCommit struct {
	sha   string
	at    time.Time
	first time.Time
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
		c.first = oldest(c.first, s.At)
	}

	if pipeline != nil {
		for _, run := range pipeline.Runs {
			c := find(run.SHA)
			if c == nil {
				continue
			}
			c.runs = append(c.runs, run)
			c.at = newest(c.at, newest(run.Started, run.Ended))
			c.first = oldest(c.first, oldest(run.Started, run.Ended))
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
		c.first = oldest(c.first, oldest(r.StartedAt, r.EndedAt))
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

// rigSweeps is the systemUpdatesRig's sweeps, newest first. It is a copy, so the
// caller's slice keeps its order.
func rigSweeps(sweeps []TierSweepRow) []TierSweepRow {
	var rig []TierSweepRow
	for _, s := range sweeps {
		if s.Rig == systemUpdatesRig {
			rig = append(rig, s)
		}
	}
	sort.SliceStable(rig, func(i, j int) bool { return rig[i].At.After(rig[j].At) })
	return rig
}

// lastGreenSweep is the newest sweep that was all GREEN. A commit is judged by its
// newest sweep: an older green sweep of a commit that a newer sweep reran red is
// not green. The row's stages are the ones its swept line named, so a tier the
// cycle should have run but the line omitted cannot be seen here. Nil when no
// sweep is green. rig must be newest first.
func lastGreenSweep(rig []TierSweepRow) *SweepGreen {
	var judged []string
	for _, s := range rig {
		if slices.ContainsFunc(judged, func(sha string) bool { return sameCommit(sha, s.SHA) }) {
			continue
		}
		judged = append(judged, s.SHA)
		if sweepGreen(s) {
			return &SweepGreen{SHA: s.SHA, At: s.At}
		}
	}
	return nil
}

// systemUpdatesAhead is how many commits the join reads, other than the green
// commit, first reported after the last green sweep: the commits that landed
// since it. rig is the gastown rig's sweeps, so another rig's sweep does not add
// to the count. The join is taken before the row cap, so a commit the cap cuts
// still counts. A commit counts by its earliest stage time, so the green commit's
// own later roll, and an older commit whose roll finished after the green sweep,
// do not. -1 when no sweep is green.
func systemUpdatesAhead(rig []TierSweepRow, pipeline *GastownPipeline, rolls []SystemUpdateRoll, green *SweepGreen) int {
	if green == nil {
		return -1
	}
	ahead := 0
	for _, c := range joinSystemUpdates(rig, pipeline, rolls) {
		if !sameCommit(c.sha, green.SHA) && c.first.After(green.At) {
			ahead++
		}
	}
	return ahead
}

// sweepGreen is whether every stage of the sweep reported GREEN exactly. A sweep
// with no stage is not green, and neither is a stage whose verdict is blank or
// anything but GREEN.
func sweepGreen(s TierSweepRow) bool {
	if len(s.Stages) == 0 {
		return false
	}
	for _, st := range s.Stages {
		if st.Verdict != "GREEN" {
			return false
		}
	}
	return true
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

// oldest is the earlier of two stage times. A zero time is no time, so it never
// wins: a commit's first stamp is the earliest any of its stages reported.
func oldest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
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
