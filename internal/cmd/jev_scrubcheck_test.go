package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
	"github.com/steveyegge/gastown/internal/jev/noise"
	"github.com/steveyegge/gastown/internal/jev/scrubcheck"
)

// writeScrubChecks writes the lines to the check log of town.
func writeScrubChecks(t *testing.T, town string, lines ...scrubcheck.Line) {
	t.Helper()
	path := jev.ScrubCheckPath(town)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	var buf bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(l)
		require.NoError(t, err)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
}

// checkedPairs is n checked lines of caller whose scrubbed call answered
// needs_action the same as the original, so none crosses the cut.
func checkedPairs(caller string, n int) []scrubcheck.Line {
	out := make([]scrubcheck.Line, 0, n)
	for range n {
		out = append(out, scrubcheck.Line{
			TS:          time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
			Caller:      caller,
			QuestionIDs: []string{logtriage.QNeedsAction},
			Outcome:     "checked",
			Original:    &scrubcheck.Answers{Noul: map[string]float64{logtriage.QNeedsAction: 0.3}},
			Scrubbed:    &scrubcheck.Answers{Noul: map[string]float64{logtriage.QNeedsAction: 0.3}},
		})
	}
	return out
}

// TestJevScrubcheckCommand runs the subcommand over a fixture town: the text table
// names the caller, question, verdict and baseline, --json prints the same row,
// and a town with no check log gets a clear message instead of an error.
func TestJevScrubcheckCommand(t *testing.T) {
	t.Parallel()
	if found, _, err := jevCmd.Find([]string{"scrubcheck"}); err != nil || found.Name() != "scrubcheck" {
		t.Fatalf("gt jev does not offer scrubcheck: %v", err)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	t.Run("text table", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		// The repeats of the one item straddle the cut, so the baseline is 100%.
		noiseFixtureOriginal(t, town, "k", 0.43, 0.53, 0.43, 0.53)
		writeScrubChecks(t, town, checkedPairs(logtriage.Caller, scrubcheck.MinPairs)...)

		var out bytes.Buffer
		require.NoError(t, jevScrubcheckWithFlags(&out, town, "", "", noise.DefaultCut, false, now))

		text := out.String()
		assert.Contains(t, text, "scrub check, cut 0.50")
		assert.Contains(t, text, logtriage.Caller)
		assert.Contains(t, text, logtriage.QNeedsAction)
		assert.Contains(t, text, "100.0%", "the baseline column reads the repeat log")
		assert.Contains(t, text, scrubcheck.VerdictFree)
	})

	t.Run("json rows", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		noiseFixtureOriginal(t, town, "k", 0.43, 0.53, 0.43, 0.53)
		writeScrubChecks(t, town, checkedPairs(logtriage.Caller, scrubcheck.MinPairs)...)

		var out bytes.Buffer
		require.NoError(t, jevScrubcheckWithFlags(&out, town, "", "", noise.DefaultCut, true, now))

		var rep scrubcheck.Report
		require.NoError(t, json.Unmarshal(out.Bytes(), &rep), out.String())
		require.Len(t, rep.Rows, 1)
		row := rep.Rows[0]
		assert.Equal(t, logtriage.Caller, row.Caller)
		assert.Equal(t, logtriage.QNeedsAction, row.Question)
		assert.Equal(t, scrubcheck.MinPairs, row.Pairs)
		require.NotNil(t, row.Crossing)
		assert.Equal(t, 0.0, *row.Crossing)
		require.NotNil(t, row.Baseline)
		assert.InDelta(t, 1.0, *row.Baseline, 1e-9)
		assert.Equal(t, scrubcheck.VerdictFree, row.Verdict)
	})

	t.Run("no repeat log has no baseline", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeScrubChecks(t, town, checkedPairs(logtriage.Caller, scrubcheck.MinPairs)...)

		var out bytes.Buffer
		require.NoError(t, jevScrubcheckWithFlags(&out, town, "", "", noise.DefaultCut, false, now))
		assert.Contains(t, out.String(), scrubcheck.VerdictNoBaseline)
	})

	t.Run("caller filter", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeScrubChecks(t, town, checkedPairs("jev-other", 1)...)

		var out bytes.Buffer
		require.NoError(t, jevScrubcheckWithFlags(&out, town, logtriage.Caller, "", noise.DefaultCut, false, now))
		assert.Equal(t, "scrub check, cut 0.50, margins: free within 2 points, costly above 5 points, at least 30 pairs\n"+
			"no checked request has both answer sets to count\n", out.String())
	})

	t.Run("no check log", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		var out bytes.Buffer
		require.NoError(t, jevScrubcheckWithFlags(&out, town, "", "", noise.DefaultCut, false, now))
		assert.Equal(t, "no scrub check log at "+jev.ScrubCheckPath(town)+": no sampled request has been checked yet\n", out.String())
	})

	t.Run("no check log as json has no rows", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		require.NoError(t, jevScrubcheckWithFlags(&out, t.TempDir(), "", "", noise.DefaultCut, true, now))
		var rep scrubcheck.Report
		require.NoError(t, json.Unmarshal(out.Bytes(), &rep), out.String())
		assert.Empty(t, rep.Rows)
	})
}

// TestJevScrubcheckRefusals: a command that cannot find its town, read its
// window, read its check log or read the repeat baseline reports the failure
// and prints nothing.
func TestJevScrubcheckRefusals(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	t.Run("no town root", func(t *testing.T) {
		t.Parallel()
		deps := jevScrubcheckDeps{findTownRoot: func() (string, error) { return "", errors.New("no town here") }}
		var out bytes.Buffer
		err := runJevScrubcheck(&out, deps, jevScrubcheckFlags{cut: noise.DefaultCut}, now)
		assert.ErrorContains(t, err, "finding the town root")
		assert.Empty(t, out.String())
	})

	t.Run("bad since window", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		err := jevScrubcheckWithFlags(&out, t.TempDir(), "", "soon", noise.DefaultCut, false, now)
		assert.ErrorContains(t, err, `--since "soon"`)
		assert.Empty(t, out.String())
	})

	t.Run("malformed check log", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		path := jev.ScrubCheckPath(town)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("not json\n"), 0o600))
		var out bytes.Buffer
		err := jevScrubcheckWithFlags(&out, town, "", "", noise.DefaultCut, false, now)
		assert.ErrorContains(t, err, "line 1 does not decode as a check line")
		assert.Empty(t, out.String())
	})

	t.Run("check log answers a question as two kinds", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		score := checkedPairs(logtriage.Caller, 1)[0]
		score.Original = &scrubcheck.Answers{Score: map[string]scrubcheck.Score{logtriage.QNeedsAction: {Score: 1}}}
		score.Scrubbed = &scrubcheck.Answers{Score: map[string]scrubcheck.Score{logtriage.QNeedsAction: {Score: 1}}}
		writeScrubChecks(t, town, checkedPairs(logtriage.Caller, 1)[0], score)
		var out bytes.Buffer
		err := jevScrubcheckWithFlags(&out, town, "", "", noise.DefaultCut, false, now)
		assert.ErrorContains(t, err, "was answered as noul and as score")
		assert.Empty(t, out.String())
	})

	t.Run("unreadable repeat log", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeScrubChecks(t, town, checkedPairs(logtriage.Caller, scrubcheck.MinPairs)...)
		// A directory where the repeat log should be cannot be read as lines.
		require.NoError(t, os.MkdirAll(battery.ReaskPath(logtriage.LogPath(town)), 0o700))
		var out bytes.Buffer
		err := jevScrubcheckWithFlags(&out, town, "", "", noise.DefaultCut, false, now)
		assert.ErrorContains(t, err, "reading the repeat baseline")
		assert.Empty(t, out.String())
	})
}
