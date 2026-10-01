package steward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// Defaults for a town that configures nothing.
const (
	// DefaultMaxJobs is the concurrency cap: jobs act on submitted work, so
	// the daemon runs few of them (gt-9bioi).
	DefaultMaxJobs = 2
	// DefaultJobTimeout bounds one job, agent session included.
	DefaultJobTimeout = 45 * time.Minute
	// DefaultRoutineAgent is the preset routine jobs run on first; a town
	// overrides it with patrols.steward config.
	DefaultRoutineAgent = "deepseek-flash"
	// DefaultHardAgent is the preset a job retries on after a routine
	// failure, and the one a conflict job starts on.
	DefaultHardAgent = "deepseek-pro"
)

// ResultFile is the file a job writes to report its verdict: a JSON object
// with "outcome" (one of Outcomes) and "summary". The runner records the
// file's outcome; a job that writes none records OutcomeError, whatever its
// exit code, because nothing vouches for what it did (gt-9bioi.2 owns the
// prompt that asks for it).
const ResultFile = "steward-result.json"

// Result is the job's own verdict, as it writes it.
type Result struct {
	Outcome Outcome `json:"outcome"`
	Summary string  `json:"summary,omitempty"`
}

// SpawnRequest is one headless job to run. Dir is the job's worktree, which
// the runner has created empty; the spawner fills it (a git worktree at the
// branch head) and runs the agent there.
type SpawnRequest struct {
	ID      string
	Dir     string
	Prompt  string
	Model   string
	Timeout time.Duration
	Event   Event
	// Started is called once the job's process exists, with its process
	// group. The runner records the group so a daemon that dies can kill it;
	// an error means it could not, and the spawner must not leave the job
	// running unrecorded. Nil when no one is listening.
	Started func(pgid int) error
}

// SpawnResult is what running a job's process produced.
type SpawnResult struct {
	// ExitCode is the process's, -1 when it never started.
	ExitCode int
	// TimedOut is true when the job's timeout killed it.
	TimedOut bool
	// Err is a failure to run the job at all (worktree, fetch, exec).
	Err error
	// Transcript is the agent's conversation log, when one was found.
	Transcript string
	// Verdict is the job's own report, read from ResultFile before the job's
	// worktree was removed; VerdictErr says why there is none.
	Verdict    *Result
	VerdictErr error
}

// Spawner runs one job. The production one is *AgentSpawner.
type Spawner interface {
	Spawn(ctx context.Context, req SpawnRequest) SpawnResult
}

// Runner starts jobs within a concurrency cap, one per bead, and records
// each in the ledger. It is the daemon's side of gt-9bioi.1.
type Runner struct {
	Ledger *Ledger
	// Spawn runs a job that SpawnFor does not answer for.
	Spawn Spawner
	// SpawnFor picks the spawner for an event, because a job runs in its
	// rig's own repository; nil uses Spawn for every event.
	SpawnFor func(ev Event) Spawner
	WorkDir  string
	MaxJobs  int
	Timeout  time.Duration
	Logf     func(format string, args ...any)
	Now      func() time.Time
	NewID    func() string
	// Kill ends an orphaned job's process group; nil is
	// util.KillProcessGroupID.
	Kill KillGroup

	mu      sync.Mutex
	running map[string]Job
	beads   map[string]bool
	wg      sync.WaitGroup
}

// Start runs one job on its own goroutine and reports whether it started. It
// starts nothing when the cap is full or the bead already has a job in
// flight; both answers are the daemon's to log, not this call's to retry.
func (r *Runner) Start(ctx context.Context, ev Event, model, prompt string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.running) >= r.maxJobs() {
		return false
	}
	if r.beads == nil {
		r.beads = map[string]bool{}
	}
	if r.busyLocked(ev.Bead) {
		return false
	}
	id := "steward-" + r.newID()
	dir := filepath.Join(r.WorkDir, id)
	// The parent only: the spawner runs `git worktree add` at dir, which
	// refuses a path that already exists.
	if err := os.MkdirAll(r.WorkDir, 0o700); err != nil {
		r.logf("steward: creating %s: %v", r.WorkDir, err)
		return false
	}
	started := r.now()
	j := Job{ID: id, Event: ev.Kind, Bead: ev.Bead, Rig: ev.Rig, Branch: ev.Branch, Head: ev.Head, Model: model, Mode: ev.Mode, Started: started}
	if err := r.Ledger.Append(j); err != nil {
		r.logf("steward: %s: recording the start failed: %v", id, err)
		return false
	}
	if r.running == nil {
		r.running = map[string]Job{}
	}
	r.running[id] = j
	r.beads[ev.Bead] = true
	r.logf("steward: start id=%s event=%s bead=%s rig=%s head=%s model=%s dir=%s", id, ev.Kind, ev.Bead, ev.Rig, ShortHead(ev.Head), model, dir)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.run(ctx, j, SpawnRequest{ID: id, Dir: dir, Prompt: prompt, Model: model, Timeout: r.timeout(), Event: ev, Started: r.recordGroup(j)})
	}()
	return true
}

// run executes one job and records how it ended, exactly once.
func (r *Runner) run(ctx context.Context, j Job, req SpawnRequest) {
	defer func() {
		r.mu.Lock()
		delete(r.running, j.ID)
		delete(r.beads, j.Bead)
		r.mu.Unlock()
	}()
	// The timeout is the runner's, so it holds for every spawner: an agent
	// that outlives it is killed by whatever the spawner does with the
	// context, and the ledger records the kill either way.
	jctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	res := r.spawnerFor(req.Event).Spawn(jctx, req)
	// ctx is the daemon's: when it is done the daemon is going away, and a
	// job killed for that was not given its time, so it neither timed out nor
	// failed. A job that already has a verdict and ran clean finished first.
	shutdown := ctx.Err() != nil && !res.TimedOut && (res.Err != nil || res.Verdict == nil)
	if jctx.Err() != nil && !shutdown && res.Err == nil && res.Verdict == nil {
		res.TimedOut = true
	}
	if shutdown {
		j.Ended, j.Outcome, j.Summary, j.Transcript = r.now(), OutcomeInterrupted, "the daemon shut down while the job was running", res.Transcript
	} else {
		j.Ended, j.Outcome, j.Summary, j.Transcript = r.classify(res, r.now())
	}
	if err := r.Ledger.Append(j); err != nil {
		r.logf("steward: %s: recording the outcome failed: %v", j.ID, err)
	}
	r.logf("steward: end id=%s event=%s bead=%s outcome=%s dur=%s summary=%q transcript=%s",
		j.ID, j.Event, j.Bead, j.Outcome, j.Ended.Sub(j.Started).Round(time.Second), j.Summary, j.Transcript)
}

// recordGroup is the Started callback for job j: it appends j's row again
// with the process group, which supersedes the start row (Ledger.Active reads
// the latest row of each id).
func (r *Runner) recordGroup(j Job) func(pgid int) error {
	return func(pgid int) error {
		j.Pgid = pgid
		r.mu.Lock()
		if _, ok := r.running[j.ID]; ok {
			r.running[j.ID] = j
		}
		r.mu.Unlock()
		if err := r.Ledger.Append(j); err != nil {
			return fmt.Errorf("recording process group %d: %w", pgid, err)
		}
		return nil
	}
}

// classify is the job's outcome: what the job reported, else why there is
// none. The order is the truth of the run — a job whose process failed says
// nothing about its verdict, even if one is on disk.
func (r *Runner) classify(res SpawnResult, now time.Time) (time.Time, Outcome, string, string) {
	switch {
	case res.Err != nil:
		return now, OutcomeError, "the job could not run: " + oneLine(res.Err.Error()), res.Transcript
	case res.TimedOut:
		return now, OutcomeTimeout, fmt.Sprintf("killed after %s", r.timeout()), res.Transcript
	case res.VerdictErr != nil:
		return now, OutcomeError, oneLine(res.VerdictErr.Error()), res.Transcript
	case res.Verdict == nil:
		return now, OutcomeError, fmt.Sprintf("exit %d and no verdict: the job reported nothing", res.ExitCode), res.Transcript
	default:
		return now, res.Verdict.Outcome, oneLine(res.Verdict.Summary), res.Transcript
	}
}

// ReadVerdict reads a job's verdict file, refusing anything the ledger does
// not accept: an outcome outside the set is a job that cannot say what it
// did, and recording it verbatim would put a word no reader knows on the
// ledger (gt-9bioi.3). The spawner calls it before the worktree goes away;
// the fake spawner in tests answers directly.
func ReadVerdict(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("no %s: the job reported no verdict", ResultFile)
	}
	if err != nil {
		return Result{}, fmt.Errorf("reading %s: %w", ResultFile, err)
	}
	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return Result{}, fmt.Errorf("%s does not parse: %w", ResultFile, err)
	}
	if !res.Outcome.Valid() {
		return Result{}, fmt.Errorf("%s reports outcome %q, which is not one of %s", ResultFile, res.Outcome, outcomeList())
	}
	return res, nil
}

// Wait blocks until every job this runner started has ended.
func (r *Runner) Wait() { r.wg.Wait() }

// Running returns the jobs in flight, by id.
func (r *Runner) Running() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Job, 0, len(r.running))
	for _, j := range r.running {
		out = append(out, j)
	}
	return out
}

// RunningBead reports whether a job for bead is in flight, this process's or
// a previous daemon's that is still alive (see ReapOrphans).
func (r *Runner) RunningBead(bead string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busyLocked(bead)
}

// busyLocked is RunningBead with r.mu held. A job the ledger shows with no end
// is either this process's own or one whose process group could not be killed
// (ReapOrphans closes the rest), so it may still be pushing to the bead's
// branch: no second job starts beside it. A ledger that cannot be read answers
// busy, the safe side.
func (r *Runner) busyLocked(bead string) bool {
	if r.beads[bead] {
		return true
	}
	active, err := r.Ledger.Active()
	if err != nil {
		r.logf("steward: reading the ledger for %s: %v; treating it as busy", bead, err)
		return true
	}
	for _, j := range active {
		if j.Bead == bead {
			return true
		}
	}
	return false
}

// ReapOrphans closes the ledger's running jobs this runner did not start,
// killing their process groups first: a daemon that died left them behind.
// It returns how many it closed. A group it cannot kill stays open and busy,
// so the scan calls this again each cycle until the group is gone.
func (r *Runner) ReapOrphans() (int, error) {
	// The snapshot comes before the ledger read: a job that ends in between
	// has its end row written before it leaves r.running, so it is either in
	// own or already closed, never an orphan.
	r.mu.Lock()
	own := make(map[string]bool, len(r.running))
	for id := range r.running {
		own[id] = true
	}
	r.mu.Unlock()
	kill := r.Kill
	if kill == nil {
		kill = util.KillProcessGroupID
	}
	return r.Ledger.CloseOrphans(kill, own, r.now())
}

// spawnerFor is the spawner one job runs on.
func (r *Runner) spawnerFor(ev Event) Spawner {
	if r.SpawnFor != nil {
		return r.SpawnFor(ev)
	}
	return r.Spawn
}

func (r *Runner) maxJobs() int {
	if r.MaxJobs > 0 {
		return r.MaxJobs
	}
	return DefaultMaxJobs
}

func (r *Runner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultJobTimeout
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) newID() string {
	if r.NewID != nil {
		return r.NewID()
	}
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// ChooseModel decides which preset a job for the event's key runs on, and
// whether to run one at all: no history earns a routine job, one failed
// routine job earns a single retry on the hard preset, and anything else
// stops (the epic's Q3: a second failed attempt goes to the overseer,
// gt-9bioi).
func ChooseModel(history []Job, routine, hard string) (string, bool) {
	if routine == "" {
		routine = DefaultRoutineAgent
	}
	if hard == "" {
		hard = DefaultHardAgent
	}
	jobs := attempts(history)
	switch len(jobs) {
	case 0:
		return routine, true
	case 1:
		job := jobs[0]
		if job.Outcome.Failed() && job.Model == routine {
			return hard, true
		}
	}
	return "", false
}

// latestRows collapses ledger rows to one per job, its latest, in the order
// the jobs started: the ledger holds a start row and an end row for each.
func latestRows(rows []Job) []Job {
	at := make(map[string]int, len(rows))
	var out []Job
	for _, r := range rows {
		if i, ok := at[r.ID]; ok {
			out[i] = r
			continue
		}
		at[r.ID] = len(out)
		out = append(out, r)
	}
	return out
}

// attempts is the jobs that count against an event's retries: every one but
// those a restart cut short. An interrupted job never got to try, so two
// restarts do not spend the routine run and the hard retry (gt-9bioi.5).
func attempts(history []Job) []Job {
	var out []Job
	for _, j := range latestRows(history) {
		if j.Outcome != OutcomeInterrupted {
			out = append(out, j)
		}
	}
	return out
}

// StartedModel is the preset a job runs on when no history exists yet, so a
// conflict — which the routine model is not trusted to resolve — starts hard
// (gt-9bioi).
func StartedModel(ev Event, history []Job, routine, hard string) (string, bool) {
	model, run := ChooseModel(history, routine, hard)
	if run && len(attempts(history)) == 0 && strings.HasPrefix(ev.RejectionDetail, "kind=conflict") {
		return hard, true
	}
	return model, run
}

func outcomeList() string {
	parts := make([]string, len(Outcomes))
	for i, o := range Outcomes {
		parts[i] = string(o)
	}
	return strings.Join(parts, ", ")
}

// oneLine collapses a summary onto one line: the ledger is JSONL, and a
// summary is agent-supplied text that must not carry a newline into a
// reader's output.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 500 {
		s = s[:500] + " …"
	}
	return s
}

// ShortHead is the first 8 characters of a commit id, for a log line or a
// message to the overseer.
func ShortHead(head string) string {
	if len(head) > 8 {
		return head[:8]
	}
	return head
}
