package doltsquash

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CatchupQuietTip is how long main's tip must hold still before an empty tail
// ends the replay: the design's 300 ms, about a hundredth of the town's 25 s
// commit interval.
const CatchupQuietTip = 300 * time.Millisecond

// catchupRetries bounds how many times one pick's "uncommitted changes" is
// retried before the run returns it. The waits below total under four seconds,
// so a working set a writer holds longer fails the pass instead of stalling the
// run.
const catchupRetries = 8

// catchupBackoff is the wait before the first retry of a pick; each later wait
// doubles, up to catchupBackoffCap.
const (
	catchupBackoff    = 25 * time.Millisecond
	catchupBackoffCap = time.Second
)

// The two texts Dolt answers a cherry-pick with that the replay reads
// differently: one is success, the other is a wait.
const (
	pickNothingToCommit    = "nothing to commit"
	pickUncommittedChanges = "uncommitted changes"
)

// Clock is the time source the quiet-tip rule reads. A clockwork.Clock
// satisfies it.
type Clock interface {
	Now() time.Time
}

// Sleeper is the package's wait seam: the retry backoff and the quiet-tip
// window wait through it, so a test drives the waits without sleeping.
type Sleeper interface {
	Sleep(d time.Duration)
}

// CatchupResult is what design step 5's replay did.
type CatchupResult struct {
	// Tip is the tip of main the replay stopped at: the quiet tip.
	Tip string
	// Passes is how many times the replay read main's tip; the last read saw
	// the tail empty at that tip for the second time.
	Passes int
	// Picks is how many commits the replay attempted, in order.
	Picks int
	// Empty is how many of those Dolt answered "nothing to commit": the
	// change was already on compact-work, which is benign.
	Empty int
	// Retries is how many waits the replay spent on picks that reported
	// uncommitted changes.
	Retries int
}

// Catchup runs design step 5: it replays the commits after from that main has
// and compact-work does not, one cherry-pick at a time, until main's tip has
// held still for CatchupQuietTip with nothing left to pick.
//
// from is the tip the rebase branched compact-work at. It is the replay's
// lower bound, and each pass's upper bound is a tip read before that pass, so
// no range reaches past the tip it recorded.
func Catchup(ctx context.Context, ex Exec, from string, clk Clock, sleep Sleeper) (CatchupResult, error) {
	if err := checkCommitHash("the commit the replay starts after", from); err != nil {
		return CatchupResult{}, err
	}
	s := &designStep{ex: ex}
	branch, err := s.scalar(ctx, "SELECT active_branch()")
	if err != nil {
		return CatchupResult{}, err
	}
	if branch != "compact-work" {
		return CatchupResult{}, fmt.Errorf(
			"doltsquash: statement %d (SELECT active_branch()) answered %q, want compact-work: a pick must land on the squashed branch, and a pick on another branch would rewrite it",
			s.n, branch)
	}
	var result CatchupResult
	picked := from
	var seenTip string
	var seenAt time.Time
	for {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("doltsquash: the replay stopped between passes: %w", err)
		}
		tip, err := s.scalar(ctx, "SELECT dolt_hashof('main')")
		if err != nil {
			return result, err
		}
		if err := checkCommitHash("main's tip", tip); err != nil {
			return result, err
		}
		result.Tip = tip
		result.Passes++
		commits, err := s.column(ctx, tailStatement(picked, tip))
		if err != nil {
			return result, err
		}
		for _, commit := range commits {
			if err := checkCommitHash("a commit in main's tail", commit); err != nil {
				return result, err
			}
		}
		if len(commits) == 0 {
			if tip != picked {
				return result, fmt.Errorf(
					"doltsquash: statement %d (%s) listed no commit though main's tip %s is not %s, the commit the replay reached: a commit would be left unpicked",
					s.n, shortStatement(tailStatement(picked, tip)), tip, picked)
			}
			if quietTip(seenTip, tip, seenAt, clk.Now()) {
				return result, nil
			}
			seenTip, seenAt = tip, clk.Now()
			sleep.Sleep(CatchupQuietTip)
			continue
		}
		for _, commit := range commits {
			applied, retries, err := s.pick(ctx, commit, sleep)
			if err != nil {
				return result, err
			}
			result.Picks++
			result.Retries += retries
			if !applied {
				result.Empty++
			}
		}
		picked = tip
	}
}

// quietTip reports whether tip may end the replay: the same tip an earlier
// empty pass observed, at least CatchupQuietTip earlier on the clock. A clock
// that reads before the observation is not quiet, so a clock that stepped back
// never ends the replay.
func quietTip(seenTip, tip string, seenAt, now time.Time) bool {
	if seenTip == "" || seenTip != tip || seenAt.IsZero() {
		return false
	}
	return now.Sub(seenAt) >= CatchupQuietTip
}

// tailStatement lists the commits after from up to tip, oldest first. Dolt's
// dolt_log takes one revision per argument, so the range is a single argument
// in git's from..tip form. Both ends are hashes the run recorded, because a
// list read from the live tip can hold a commit the next pass picks again (the
// lab race).
func tailStatement(from, tip string) string {
	return fmt.Sprintf("SELECT commit_hash FROM dolt_log(%s) ORDER BY commit_order ASC",
		quoteLiteral(from+".."+tip))
}

// pickStatement renders the pick of one commit.
func pickStatement(commit string) string {
	return "CALL DOLT_CHERRY_PICK(" + quoteLiteral(commit) + ")"
}

// pick cherry-picks one commit onto compact-work. It reports whether the
// commit changed the branch: false means Dolt answered "nothing to commit",
// because the change was already there. An uncommitted working set is retried
// on sleep up to catchupRetries; every other answer, including a pick left in
// conflict, stops the run.
func (s *designStep) pick(ctx context.Context, commit string, sleep Sleeper) (bool, int, error) {
	statement := pickStatement(commit)
	for retries := 0; ; retries++ {
		rows, err := s.exec(ctx, statement)
		if err == nil {
			if err := pickResult(commit, rows); err != nil {
				return false, retries, fmt.Errorf("doltsquash: statement %d (%s): %w", s.n, shortStatement(statement), err)
			}
			return true, retries, nil
		}
		switch {
		case strings.Contains(err.Error(), pickNothingToCommit):
			return false, retries, nil
		case strings.Contains(err.Error(), pickUncommittedChanges):
			if retries == catchupRetries {
				return false, retries, err
			}
			sleep.Sleep(catchupWait(retries))
		default:
			return false, retries, err
		}
	}
}

// pickResult refuses a pick that did not create a commit. Dolt answers a
// conflicting pick with a row of conflict counts and no hash, and the run must
// stop there: the branch holds a merge state that only the recovery table can
// resolve.
func pickResult(commit string, rows Rows) error {
	if len(rows.Data) != 1 {
		return fmt.Errorf("the pick of %s returned %d records, want one naming the new commit", commit, len(rows.Data))
	}
	record := rows.Data[0]
	if len(record) != 4 {
		return fmt.Errorf("the pick of %s returned a record of %d values, want the new hash and three conflict counts",
			commit, len(record))
	}
	if record[1] != "0" || record[2] != "0" || record[3] != "0" {
		return fmt.Errorf("the pick of %s left conflicts (data %s, schema %s, constraint %s), which the run cannot continue past",
			commit, record[1], record[2], record[3])
	}
	if err := checkCommitHash("the new commit", record[0]); err != nil {
		return fmt.Errorf("the pick of %s returned no commit hash: %w", commit, err)
	}
	return nil
}

// catchupWait is the wait before the retry after attempt, counting from zero:
// the backoff doubled per attempt, capped.
func catchupWait(attempt int) time.Duration {
	d := catchupBackoff
	for i := 0; i < attempt; i++ {
		if d >= catchupBackoffCap/2 {
			return catchupBackoffCap
		}
		d *= 2
	}
	return d
}
