package logtriage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev/battery"
)

// reask reads every line of the repeat log beside the pass's own log.
func (fx *fixture) reask() []battery.ReaskLine {
	fx.t.Helper()
	var lines []battery.ReaskLine
	if err := battery.ReadRecent(battery.ReaskPath(fx.pass.LogPath), math.MaxInt, &lines); err != nil {
		fx.t.Fatal(err)
	}
	return lines
}

// reset clears the triage log, its repeat log and the cursor, and empties the
// daemon log in place, so the next pass reads the same lines from the same
// inode and writes the same cursor.
func (fx *fixture) reset() {
	fx.t.Helper()
	if err := os.Truncate(fx.pass.DaemonLog, 0); err != nil {
		fx.t.Fatal(err)
	}
	for _, path := range []string{fx.pass.LogPath, fx.pass.CursorPath, battery.ReaskPath(fx.pass.LogPath)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			fx.t.Fatal(err)
		}
	}
}

// states returns each recorded request's state as it was sent.
func (fx *fixture) states(t *testing.T) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, body := range fx.jev.requests {
		var req struct {
			State json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("request %s: %v", body, err)
		}
		out = append(out, req.State)
	}
	return out
}

// TestLogtriagePassRepeatsSampledGroups: a sampled group is asked again five
// times with the state the original ask sent, each answer going to the repeat
// log beside the triage log; an unsampled group is asked once and leaves no
// repeat line, and the triage log and cursor do not depend on the repeats.
func TestLogtriagePassRepeatsSampledGroups(t *testing.T) {
	t.Parallel()

	t.Run("repeat lines", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.pass.Sampled = func(key string, version int) bool { return key == keyWarn }
		fx.logLines(lineTick1, lineTick2, lineWarn)

		sum := fx.run()
		require.Equal(t, 2, sum.Asked)
		assert.Equal(t, battery.ReaskRepeats, sum.Repeated)
		assert.Zero(t, sum.RepeatsSkipped)
		assert.Len(t, fx.triage(), 2, "one line per group, repeats never scored")

		repeats := fx.reask()
		require.Len(t, repeats, battery.ReaskRepeats)
		for i, l := range repeats {
			assert.Equal(t, keyWarn, l.Key, "only the sampled group repeats")
			assert.Equal(t, i+1, l.Repeat)
			assert.Equal(t, Version, l.QuestionVersion)
			assert.Equal(t, "jev-1.13.0", l.Model)
			assertFiveAnswers(t, keyWarn, answersOf(t, l.Answers))
		}
		info, err := os.Stat(battery.ReaskPath(fx.pass.LogPath))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the repeat log is written at the log's mode")
		assert.Equal(t, filepath.Dir(fx.pass.LogPath), filepath.Dir(battery.ReaskPath(fx.pass.LogPath)))

		// Two original asks and five repeats, and every request for the
		// sampled group carries the state the original one sent.
		require.Len(t, fx.jev.requests, 2+battery.ReaskRepeats)
		warnStates := 0
		var first json.RawMessage
		for _, state := range fx.states(t) {
			var got groupState
			require.NoError(t, json.Unmarshal(state, &got))
			if got.Key != keyWarn {
				continue
			}
			if warnStates == 0 {
				first = state
			}
			assert.JSONEq(t, string(first), string(state), "a repeat must resend the original ask's state")
			warnStates++
		}
		assert.Equal(t, 1+battery.ReaskRepeats, warnStates)
	})

	t.Run("every ask comes before the repeats", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.pass.Sampled = func(string, int) bool { return true }
		fx.logLines(lineMerge1, lineMerge2, lineWarn)

		sum := fx.run()
		require.Equal(t, 2, sum.Asked)
		require.Equal(t, 2*battery.ReaskRepeats, sum.Repeated)

		var asked []string
		for _, state := range fx.states(t) {
			var got groupState
			require.NoError(t, json.Unmarshal(state, &got))
			asked = append(asked, got.Key)
		}
		want := []string{keyMerge, keyWarn}
		for range battery.ReaskRepeats {
			want = append(want, keyMerge)
		}
		for range battery.ReaskRepeats {
			want = append(want, keyWarn)
		}
		assert.Equal(t, want, asked, "the repeats wait until every group is asked, so they cannot spend the pass's deadline first")
	})

	t.Run("triage log and cursor unchanged", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.pass.Sampled = func(string, int) bool { return false }
		fx.logLines(lineTick1, lineWarn)
		fx.run()
		wantLog := readFile(t, fx.pass.LogPath)
		wantCursor := readFile(t, fx.pass.CursorPath)

		fx.reset()
		fx.logLines(lineTick1, lineWarn)
		fx.pass.Sampled = func(string, int) bool { return true }
		sum := fx.run()
		require.Equal(t, 2*battery.ReaskRepeats, sum.Repeated)
		assert.Equal(t, string(wantLog), readFile(t, fx.pass.LogPath), "sampling must leave the triage log byte-identical")
		assert.Equal(t, string(wantCursor), readFile(t, fx.pass.CursorPath), "sampling must leave the cursor byte-identical")
	})
}

// answersOf reads a repeat line's answers back into the shape the triage log
// holds, which is what a reader of the repeat log sees.
func answersOf(t *testing.T, v any) map[string]Answer {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]Answer
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

// readFile reads path as text, failing the test when it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// TestLogtriageRepeatsStopAtTheCapAndSkipFailures: the cap ends the repeats for
// the pass while the pass still finishes and writes its line and cursor, and a
// repeat whose call fails is skipped and counted, never retried.
func TestLogtriageRepeatsStopAtTheCapAndSkipFailures(t *testing.T) {
	t.Parallel()

	t.Run("the cap ends the repeats", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.pass.Sampled = func(string, int) bool { return true }
		fx.logLines(lineWarn)
		// The pass's own check and the group's check allow the ask, the first
		// two repeats fit, and the third finds the day's spend used up.
		allowed := []bool{true, true, true, true, false}
		fx.pass.Allows = func() (bool, error) {
			ok := allowed[0]
			if len(allowed) > 1 {
				allowed = allowed[1:]
			}
			return ok, nil
		}

		sum := fx.run()
		assert.True(t, sum.CapReached)
		assert.Equal(t, 2, sum.Repeated)
		assert.Equal(t, battery.ReaskRepeats-2, sum.RepeatsSkipped)
		require.Len(t, fx.reask(), 2, "no repeat is asked after the cap closes")
		var numbers []int
		for _, l := range fx.reask() {
			numbers = append(numbers, l.Repeat)
		}
		assert.Equal(t, []int{1, 2}, numbers)

		// The pass still finishes: the group's own line and the cursor land.
		lines := fx.triage()
		require.Len(t, lines, 1)
		assert.Empty(t, lines[0].Reason)
		assertFiveAnswers(t, lineWarn, lines[0].Answers)
		_, err := os.Stat(fx.pass.CursorPath)
		assert.NoError(t, err, "a pass whose repeats the cap stopped still writes its cursor")
	})

	t.Run("the cap stops the repeats for the rest of the pass", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.pass.Sampled = func(string, int) bool { return true }
		// The merge group is the larger one, so it is asked first, and both groups
		// are asked before any repeat runs. The cap closes during the merge
		// group's repeats and opens again (a pass crossing local midnight) before
		// the warning's repeats: once the cap has stopped the repeats, no group
		// repeats.
		fx.logLines(lineMerge1, lineMerge2, lineWarn)
		allowed := []bool{true, true, true, true, false}
		fx.pass.Allows = func() (bool, error) {
			ok := allowed[0]
			if len(allowed) > 1 {
				allowed = allowed[1:]
			}
			return ok, nil
		}

		sum := fx.run()
		assert.True(t, sum.CapReached)
		assert.Equal(t, 2, sum.Asked, "both groups are still asked")
		assert.Equal(t, 1, sum.Repeated, "the merge group repeats once before the cap closes")
		assert.Equal(t, 2*battery.ReaskRepeats-1, sum.RepeatsSkipped)
		repeats := fx.reask()
		require.Len(t, repeats, 1)
		for _, l := range repeats {
			assert.Equal(t, keyMerge, l.Key, "the warning's repeats were stopped by the cap")
		}
	})

	t.Run("a failed original ask is not repeated", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.pass.Sampled = func(string, int) bool { return true }
		fx.jev.status, fx.jev.body = http.StatusInternalServerError, `{"error":"down"}`
		fx.logLines(lineWarn)

		sum := fx.run()
		assert.Equal(t, 1, sum.Unanswered)
		assert.Zero(t, sum.Repeated, "a group with no answers has nothing to measure")
		assert.Zero(t, sum.RepeatsSkipped)
		assert.Len(t, fx.jev.requests, 1)
		assert.Empty(t, fx.reask())
	})

	t.Run("a failed repeat is skipped and not retried", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.pass.Sampled = func(string, int) bool { return true }
		fx.logLines(lineWarn)
		// Call three is the second repeat: it answers a server error.
		fx.jev.failCall = 3

		sum := fx.run()
		assert.False(t, sum.CapReached)
		assert.Equal(t, battery.ReaskRepeats-1, sum.Repeated)
		assert.Equal(t, 1, sum.RepeatsSkipped)
		var numbers []int
		for _, l := range fx.reask() {
			numbers = append(numbers, l.Repeat)
		}
		assert.Equal(t, []int{1, 3, 4, 5}, numbers, "the failed repeat leaves no line and is not retried")
		assert.Len(t, fx.jev.requests, 1+battery.ReaskRepeats, "one original ask and five repeat calls, no retry")

		lines := fx.triage()
		require.Len(t, lines, 1)
		assert.Empty(t, lines[0].Reason, "a failed repeat never touches the group's own line")
		assertFiveAnswers(t, lineWarn, lines[0].Answers)
		_, err := os.Stat(fx.pass.CursorPath)
		assert.NoError(t, err)
	})
}

// TestLogtriageRepeatFailuresKeepThePassOutput: a repeat the pass cannot finish
// ends the repeats and is reported as the pass's error, and the day's lines and
// cursor are already written, so the error costs nothing the pass paid for.
func TestLogtriageRepeatFailuresKeepThePassOutput(t *testing.T) {
	t.Parallel()

	t.Run("a failed spend read", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.pass.Sampled = func(string, int) bool { return true }
		fx.logLines(lineWarn)
		// The pass's own check, the group's, and the first repeat's allow; the
		// second repeat's read of the ledger fails.
		calls := 0
		fx.pass.Allows = func() (bool, error) {
			calls++
			if calls == 4 {
				return false, errors.New("the ledger is unreadable")
			}
			return true, nil
		}

		sum, err := fx.pass.Run(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading today's spend")
		assert.Equal(t, 1, sum.Repeated)
		assert.Equal(t, battery.ReaskRepeats, sum.Repeated+sum.RepeatsSkipped, "every repeat is answered or counted")
		assert.Len(t, fx.reask(), 1, "the repeat that answered before the failed read is kept")

		lines := fx.triage()
		require.Len(t, lines, 1, "the failed read never costs the group its line")
		assert.Empty(t, lines[0].Reason)
		assertFiveAnswers(t, lineWarn, lines[0].Answers)
		_, statErr := os.Stat(fx.pass.CursorPath)
		assert.NoError(t, statErr, "the failed read never costs the pass its cursor")
	})

	t.Run("an unwritable repeat log", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.pass.Sampled = func(string, int) bool { return true }
		fx.logLines(lineWarn)
		// A directory where the repeat log belongs makes every append fail.
		require.NoError(t, os.MkdirAll(battery.ReaskPath(fx.pass.LogPath), 0o700))

		sum, err := fx.pass.Run(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "writing the repeat log")
		assert.Zero(t, sum.Repeated)
		assert.Equal(t, battery.ReaskRepeats, sum.RepeatsSkipped, "an unwritable repeat log stops and counts the repeats")

		lines := fx.triage()
		require.Len(t, lines, 1, "the unwritable repeat log never costs the group its line")
		assertFiveAnswers(t, lineWarn, lines[0].Answers)
		_, statErr := os.Stat(fx.pass.CursorPath)
		assert.NoError(t, statErr, "the unwritable repeat log never costs the pass its cursor")
	})
}
