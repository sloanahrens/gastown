package scrubcheck

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
)

const qNeeds = "needs_action"

// noulPairAt is a checked line whose scrubbed call answered needs_action with
// scrubbed where the original answered orig.
func noulPairAt(ts, caller string, orig, scrubbed float64) string {
	return fmt.Sprintf(`{"ts":%q,"caller":%q,"question_ids":[%q],"outcome":"checked","original_answers":{"noul":{%q:%v}},"scrubbed_answers":{"noul":{%q:%v}}}`,
		ts, caller, qNeeds, qNeeds, orig, qNeeds, scrubbed)
}

func noulPair(caller string, orig, scrubbed float64) string {
	return noulPairAt("2026-10-30T12:00:00Z", caller, orig, scrubbed)
}

// readLines writes the lines to a log in a temporary directory and reads it back.
func readLines(t *testing.T, lines ...string) []Line {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scrubcheck.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	got, found, err := Read(path)
	require.NoError(t, err)
	require.True(t, found)
	return got
}

// onlyRow is the report's single row, failing the test when there is another.
func onlyRow(t *testing.T, rep Report) Row {
	t.Helper()
	require.Len(t, rep.Rows, 1)
	return rep.Rows[0]
}

// TestScrubcheckStatistics is a table over fixture lines: each case reads the
// lines from a log and checks the row its statistics give.
func TestScrubcheckStatistics(t *testing.T) {
	t.Parallel()
	const caller = "jev-logtriage"
	tests := []struct {
		name  string
		lines []string
		opts  Options
		check func(t *testing.T, rep Report)
	}{
		{
			name:  "identical answer pairs give mean change 0 and crossing share 0",
			lines: []string{noulPair(caller, 0.3, 0.3), noulPair(caller, 0.8, 0.8)},
			check: func(t *testing.T, rep Report) {
				r := onlyRow(t, rep)
				assert.Equal(t, 2, r.Pairs)
				require.NotNil(t, r.MeanChange)
				assert.Equal(t, 0.0, *r.MeanChange)
				assert.Equal(t, 0.0, *r.MaxChange)
				require.NotNil(t, r.Crossing)
				assert.Equal(t, 0.0, *r.Crossing)
			},
		},
		{
			name:  "a pair crossing 0.5 counts once",
			lines: []string{noulPair(caller, 0.4, 0.6), noulPair(caller, 0.3, 0.3)},
			check: func(t *testing.T, rep Report) {
				r := onlyRow(t, rep)
				assert.Equal(t, 2, r.Pairs)
				require.NotNil(t, r.Crossing)
				assert.InDelta(t, 0.5, *r.Crossing, 1e-9, "one of two pairs crosses")
				assert.InDelta(t, 0.1, *r.MeanChange, 1e-9)
				assert.InDelta(t, 0.2, *r.MaxChange, 1e-9)
			},
		},
		{
			name: "a choice pick change counts",
			lines: []string{
				`{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["triage"],"outcome":"checked",` +
					`"original_answers":{"choice":{"triage":{"choice":"yes","probabilities":{"yes":0.8,"no":0.2}}}},` +
					`"scrubbed_answers":{"choice":{"triage":{"choice":"no","probabilities":{"yes":0.4,"no":0.6}}}}}`,
				`{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["triage"],"outcome":"checked",` +
					`"original_answers":{"choice":{"triage":{"choice":"no","probabilities":{"yes":0.1,"no":0.9}}}},` +
					`"scrubbed_answers":{"choice":{"triage":{"choice":"no","probabilities":{"yes":0.1,"no":0.9}}}}}`,
			},
			check: func(t *testing.T, rep Report) {
				r := onlyRow(t, rep)
				assert.Equal(t, "choice", r.Kind)
				assert.Equal(t, 2, r.Pairs)
				require.NotNil(t, r.Crossing)
				assert.InDelta(t, 0.5, *r.Crossing, 1e-9, "one of two picks changed")
				assert.InDelta(t, 0.2, *r.MeanChange, 1e-9, "largest option probability moved 0.4 then 0")
			},
		},
		{
			name: "an unchanged line counts only in the unchanged column",
			lines: []string{
				`{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["needs_action"],"outcome":"checked",` +
					`"unchanged":true,"original_answers":{"noul":{"needs_action":0.3}}}`,
			},
			check: func(t *testing.T, rep Report) {
				r := onlyRow(t, rep)
				assert.Equal(t, 0, r.Pairs)
				assert.Equal(t, 1, r.Unchanged)
				assert.Nil(t, r.MeanChange)
				assert.Nil(t, r.Crossing)
				assert.Equal(t, VerdictTooFew, r.Verdict)
			},
		},
		{
			name: "failed, capped and unsampled lines are not measured",
			lines: []string{
				`{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["needs_action"],"outcome":"failed","error_class":"timeout","original_answers":{"noul":{"needs_action":0.3}}}`,
				`{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["needs_action"],"outcome":"capped","original_answers":{"noul":{"needs_action":0.3}}}`,
				`{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["needs_action"],"outcome":"unsampled"}`,
			},
			check: func(t *testing.T, rep Report) {
				assert.Empty(t, rep.Rows)
			},
		},
		{
			name: "a question the scrubbed call did not answer is missing, not a pair",
			lines: []string{
				`{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["needs_action"],"outcome":"checked",` +
					`"original_answers":{"noul":{"needs_action":0.3}},"scrubbed_answers":{"noul":{}}}`,
			},
			check: func(t *testing.T, rep Report) {
				r := onlyRow(t, rep)
				assert.Equal(t, 0, r.Pairs)
				assert.Equal(t, 1, r.Missing)
			},
		},
		{
			name: "rule counts sum over the measured pairs",
			lines: []string{
				`{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["needs_action"],"outcome":"checked",` +
					`"original_answers":{"noul":{"needs_action":0.3}},"scrubbed_answers":{"noul":{"needs_action":0.3}},` +
					`"replacements":{"pass":1,"cookie":2}}`,
				`{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["needs_action"],"outcome":"checked",` +
					`"original_answers":{"noul":{"needs_action":0.3}},"scrubbed_answers":{"noul":{"needs_action":0.3}},` +
					`"replacements":{"pass":3}}`,
			},
			check: func(t *testing.T, rep Report) {
				r := onlyRow(t, rep)
				assert.Equal(t, map[string]int{"pass": 4, "cookie": 2}, r.Rules)
			},
		},
		{
			name: "the caller filter keeps one caller's lines",
			lines: []string{
				noulPair(caller, 0.3, 0.3),
				noulPair("jev-other", 0.3, 0.7),
			},
			opts: Options{Caller: "jev-other"},
			check: func(t *testing.T, rep Report) {
				r := onlyRow(t, rep)
				assert.Equal(t, "jev-other", r.Caller)
				assert.Equal(t, 1, r.Pairs)
			},
		},
		{
			name: "the since window keeps lines at or after it",
			lines: []string{
				noulPairAt("2026-10-20T12:00:00Z", caller, 0.3, 0.7),
				noulPairAt("2026-10-30T12:00:00Z", caller, 0.3, 0.3),
			},
			opts: Options{Since: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)},
			check: func(t *testing.T, rep Report) {
				r := onlyRow(t, rep)
				assert.Equal(t, 1, r.Pairs)
				require.NotNil(t, r.Crossing)
				assert.Equal(t, 0.0, *r.Crossing)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.opts
			if opts.Cut == 0 {
				opts.Cut = 0.5
			}
			rep, err := Compute(readLines(t, tc.lines...), opts, nil)
			require.NoError(t, err)
			tc.check(t, rep)
		})
	}
}

// pairLines builds pairs measured states of one question, crossing of which
// cross the cut.
func pairLines(caller string, pairs, crossing int) []Line {
	out := make([]Line, 0, pairs)
	for i := range pairs {
		scrubbed := 0.3
		if i < crossing {
			scrubbed = 0.7
		}
		out = append(out, Line{
			Caller:   caller,
			Outcome:  outcomeChecked,
			Original: &Answers{Noul: map[string]float64{qNeeds: 0.3}},
			Scrubbed: &Answers{Noul: map[string]float64{qNeeds: scrubbed}},
		})
	}
	return out
}

func baselineOf(v float64) *float64 { return &v }

// TestScrubcheckVerdicts runs each verdict and its boundaries through Compute:
// the margins are exact, so a share two points above the baseline is free and
// one five points above is borderline.
func TestScrubcheckVerdicts(t *testing.T) {
	t.Parallel()
	const caller = "jev-logtriage"
	tests := []struct {
		name     string
		pairs    int
		crossing int
		baseline *float64
		want     string
	}{
		{"exactly two points above the baseline is free", 100, 52, baselineOf(0.5), VerdictFree},
		{"below the baseline is free", 100, 40, baselineOf(0.5), VerdictFree},
		{"just over two points is borderline", 100, 53, baselineOf(0.5), VerdictBorderline},
		{"exactly five points above the baseline is borderline", 100, 55, baselineOf(0.5), VerdictBorderline},
		{"just over five points is costly", 100, 56, baselineOf(0.5), VerdictCostly},
		{"exactly the minimum pairs counts", MinPairs, MinPairs / 2, baselineOf(0.5), VerdictFree},
		{"one pair under the minimum is too few", MinPairs - 1, 0, baselineOf(0.1), VerdictTooFew},
		{"no baseline when the repeat data is missing", 100, 52, nil, VerdictNoBaseline},
		{"too few beats no baseline", 10, 5, nil, VerdictTooFew},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var base map[string]map[string]float64
			if tc.baseline != nil {
				base = map[string]map[string]float64{caller: {qNeeds: *tc.baseline}}
			}
			rep, err := Compute(pairLines(caller, tc.pairs, tc.crossing), Options{Cut: 0.5}, base)
			require.NoError(t, err)
			r := onlyRow(t, rep)
			assert.Equal(t, tc.want, r.Verdict)
			assert.Equal(t, tc.baseline != nil, r.Baseline != nil)
		})
	}
}

// TestScrubcheckRefusals: a log or repeat data that cannot be read, or a question
// answered as two kinds, fails the report instead of reading as clean.
func TestScrubcheckRefusals(t *testing.T) {
	t.Parallel()
	const caller = "jev-logtriage"

	t.Run("malformed line refuses the read", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "scrubcheck.jsonl")
		body := noulPair(caller, 0.3, 0.3) + "\nnot json\n"
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		lines, found, err := Read(path)
		assert.ErrorContains(t, err, "line 2 does not decode as a check line")
		assert.NotContains(t, err.Error(), "not json", "the error names the line, never its text")
		assert.True(t, found)
		assert.Nil(t, lines)
	})

	t.Run("unreadable log refuses the read", func(t *testing.T) {
		t.Parallel()
		lines, found, err := Read(t.TempDir())
		assert.ErrorContains(t, err, "reading the scrub check log")
		assert.True(t, found)
		assert.Nil(t, lines)
	})

	t.Run("log path under a file refuses the read", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "not-a-directory")
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		lines, found, err := Read(filepath.Join(file, "scrubcheck.jsonl"))
		assert.ErrorContains(t, err, "opening the scrub check log")
		assert.False(t, found)
		assert.Nil(t, lines)
	})

	t.Run("absent log is not an error", func(t *testing.T) {
		t.Parallel()
		lines, found, err := Read(filepath.Join(t.TempDir(), "scrubcheck.jsonl"))
		require.NoError(t, err)
		assert.False(t, found)
		assert.Nil(t, lines)
	})

	t.Run("question answered as two kinds", func(t *testing.T) {
		t.Parallel()
		score := `{"ts":"2026-10-30T12:00:00Z","caller":"jev-logtriage","question_ids":["needs_action"],"outcome":"checked",` +
			`"original_answers":{"score":{"needs_action":{"score":1}}},"scrubbed_answers":{"score":{"needs_action":{"score":1}}}}`
		_, err := Compute(readLines(t, noulPair(caller, 0.3, 0.3), score), Options{Cut: 0.5}, nil)
		assert.ErrorContains(t, err, "question needs_action was answered as noul and as score")
	})

	t.Run("unreadable repeat log refuses the baseline", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		// A directory where the repeat log should be reads as no lines but a failing scan.
		require.NoError(t, os.MkdirAll(battery.ReaskPath(logtriage.LogPath(town)), 0o700))
		base, err := Baseline(town, 0.5, time.Time{})
		assert.ErrorContains(t, err, "reading the repeat baseline")
		assert.Nil(t, base)
	})
}

// writeItem writes one original logtriage line for key, answering needs_action
// as orig, and a repeat line per answer in repeats, beside the battery log of town.
func writeItem(t *testing.T, town, key string, orig logtriage.Answer, repeats ...logtriage.Answer) {
	t.Helper()
	logPath := logtriage.LogPath(town)
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	original := logtriage.Line{
		TS: ts, Key: key, QuestionVersion: logtriage.Version, Model: "jev-test",
		Answers: map[string]logtriage.Answer{qNeeds: orig},
	}
	require.NoError(t, battery.Append(logPath, original))
	for i, a := range repeats {
		require.NoError(t, battery.AppendReask(logPath, battery.ReaskLine{
			TS: ts.Add(time.Duration(i+1) * time.Minute), Key: key, Repeat: i + 1,
			QuestionVersion: logtriage.Version, Model: "jev-test",
			Answers: map[string]logtriage.Answer{qNeeds: a},
		}))
	}
}

// writeRepeats writes a noul item for key at p, with a repeat per value in repeats.
func writeRepeats(t *testing.T, town, key string, p float64, repeats ...float64) {
	t.Helper()
	answers := make([]logtriage.Answer, 0, len(repeats))
	for _, v := range repeats {
		answers = append(answers, logtriage.Answer{Noul: &v})
	}
	writeItem(t, town, key, logtriage.Answer{Noul: &p}, answers...)
}

// TestBaselineRefusals: a battery log that cannot be read, or repeat data that
// answers a question as two kinds, fails the baseline instead of reading as none.
func TestBaselineRefusals(t *testing.T) {
	t.Parallel()

	t.Run("unreadable battery log", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		require.NoError(t, os.MkdirAll(logtriage.LogPath(town), 0o700))
		base, err := Baseline(town, 0.5, time.Time{})
		assert.ErrorContains(t, err, "reading the repeat baseline")
		assert.Nil(t, base)
	})

	t.Run("repeat data answers a question as two kinds", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		// Two items with three repeats each, one answering needs_action as a noul
		// and the other as a score.
		writeRepeats(t, town, "a", 0.3, 0.3, 0.3, 0.3)
		one := 1.0
		writeItem(t, town, "b", logtriage.Answer{Score: &one},
			logtriage.Answer{Score: &one}, logtriage.Answer{Score: &one}, logtriage.Answer{Score: &one})
		base, err := Baseline(town, 0.5, time.Time{})
		assert.ErrorContains(t, err, "reading the repeat baseline")
		assert.ErrorContains(t, err, "was answered as")
		assert.Nil(t, base)
	})
}

// failingWriter accepts n writes and then refuses every later one.
type failingWriter struct{ n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n == 0 {
		return 0, errors.New("disk full")
	}
	w.n--
	return len(p), nil
}

// TestScrubcheckWriteTextRefusesAFailingWriter: a report that cannot be written
// reports the write error rather than returning as printed.
func TestScrubcheckWriteTextRefusesAFailingWriter(t *testing.T) {
	t.Parallel()
	one := 0.5
	rep := Report{Cut: 0.5, Rows: []Row{{Caller: "jev-logtriage", Question: qNeeds, Kind: "noul", Pairs: 1, Crossing: &one, Verdict: VerdictTooFew}}}
	tests := []struct {
		name   string
		rep    Report
		writes int
	}{
		{"the header line", rep, 0},
		{"the no-rows line", Report{Cut: 0.5}, 1},
		{"the table on flush", rep, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.ErrorContains(t, WriteText(&failingWriter{n: tc.writes}, tc.rep), "disk full")
		})
	}
}

// TestBaselineReadsTheRepeatLog: the baseline is the repeats' crossing share under
// logtriage's caller name, and a town with no repeat log has no baseline.
func TestBaselineReadsTheRepeatLog(t *testing.T) {
	t.Parallel()
	t.Run("repeats that straddle the cut", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeRepeats(t, town, "k", 0.43, 0.53, 0.43, 0.53)
		base, err := Baseline(town, 0.5, time.Time{})
		require.NoError(t, err)
		require.Contains(t, base, logtriage.Caller)
		assert.InDelta(t, 1.0, base[logtriage.Caller][qNeeds], 1e-9)
	})
	t.Run("no repeat log", func(t *testing.T) {
		t.Parallel()
		base, err := Baseline(t.TempDir(), 0.5, time.Time{})
		require.NoError(t, err)
		assert.Empty(t, base)
	})
}
