package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
	"github.com/steveyegge/gastown/internal/jev/noise"
)

// noiseFixtureOriginal writes one original triage line for key with needs_action
// at p, then one repeat line per value in repeats, each answering needs_action
// with that value, beside the triage log.
func noiseFixtureOriginal(t *testing.T, town, key string, p float64, repeats ...float64) {
	t.Helper()
	logPath := logtriage.LogPath(town)
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	original := logtriage.Line{
		TS: ts, Key: key, QuestionVersion: logtriage.Version, Model: "jev-test",
		Answers: map[string]logtriage.Answer{logtriage.QNeedsAction: {Noul: &p}},
	}
	require.NoError(t, battery.Append(logPath, original))
	for i, v := range repeats {
		err := battery.AppendReask(logPath, battery.ReaskLine{
			TS: ts.Add(time.Duration(i+1) * time.Minute), Key: key, Repeat: i + 1,
			QuestionVersion: logtriage.Version, Model: "jev-test",
			Answers: map[string]logtriage.Answer{logtriage.QNeedsAction: {Noul: &v}},
		})
		require.NoError(t, err)
	}
}

// TestJevNoiseCommand runs the subcommand over a fixture town: the text table
// names the question and its numbers, --json prints the same row, and a town with
// no repeat log gets a clear message instead of an error.
func TestJevNoiseCommand(t *testing.T) {
	t.Parallel()
	if found, _, err := jevCmd.Find([]string{"noise"}); err != nil || found.Name() != "noise" {
		t.Fatalf("gt jev does not offer noise: %v", err)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	t.Run("text table", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		// One item: the original says 0.43, the repeats 0.53, 0.43, 0.53, so the
		// answers straddle the 0.5 cut.
		noiseFixtureOriginal(t, town, "k", 0.43, 0.53, 0.43, 0.53)

		var out bytes.Buffer
		require.NoError(t, jevNoiseWithFlags(&out, town, "logtriage", "", noise.DefaultCut, false, now))

		text := out.String()
		assert.Contains(t, text, "needs_action (noul)")
		assert.Contains(t, text, "  items            1\n")
		assert.Contains(t, text, "  cross share      100.0%\n")
		assert.Contains(t, text, "0 sampled item(s) skipped")
	})

	t.Run("json rows", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		noiseFixtureOriginal(t, town, "k", 0.43, 0.53, 0.43, 0.53)

		var out bytes.Buffer
		require.NoError(t, jevNoiseWithFlags(&out, town, "logtriage", "", noise.DefaultCut, true, now))

		var rep noise.Report
		require.NoError(t, json.Unmarshal(out.Bytes(), &rep), out.String())
		require.Len(t, rep.Rows, 1)
		row := rep.Rows[0]
		assert.Equal(t, "needs_action", row.Question)
		assert.Equal(t, noise.KindNoul, row.Kind)
		assert.Equal(t, 1, row.Items)
		require.NotNil(t, row.CrossShare)
		assert.InDelta(t, 1, *row.CrossShare, 1e-9)
		assert.Equal(t, "logtriage", rep.Battery)
	})

	t.Run("no repeat file", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		require.NoError(t, battery.Append(logtriage.LogPath(town), logtriage.Line{
			TS: now, Key: "k", QuestionVersion: logtriage.Version,
			Answers: map[string]logtriage.Answer{logtriage.QNeedsAction: {Noul: new(float64)}},
		}))

		var out bytes.Buffer
		require.NoError(t, jevNoiseWithFlags(&out, town, "logtriage", "", noise.DefaultCut, false, now))
		assert.True(t, strings.HasPrefix(out.String(), "no repeat log at "), out.String())
		assert.Contains(t, out.String(), "no sampled group has been asked again yet")
	})

	t.Run("unknown battery", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		err := jevNoiseWithFlags(&out, t.TempDir(), "dedupe", "", noise.DefaultCut, false, now)
		assert.ErrorContains(t, err, `battery "dedupe" writes no repeat log`)
	})
}

// TestJevNoiseRefusals: a command that cannot find its town, read its window, read
// its battery log or measure the repeats reports the failure and prints nothing.
func TestJevNoiseRefusals(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	t.Run("no town root", func(t *testing.T) {
		t.Parallel()
		deps := jevNoiseDeps{findTownRoot: func() (string, error) { return "", errors.New("no town here") }}
		var out bytes.Buffer
		err := runJevNoise(&out, deps, jevNoiseFlags{battery: "logtriage", cut: noise.DefaultCut}, now)
		assert.ErrorContains(t, err, "finding the town root")
		assert.Empty(t, out.String())
	})

	t.Run("bad since window", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		err := jevNoiseWithFlags(&out, t.TempDir(), "logtriage", "soon", noise.DefaultCut, false, now)
		assert.ErrorContains(t, err, `--since "soon"`)
		assert.Empty(t, out.String())
	})

	t.Run("unreadable battery log", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		// A directory where the battery log should be cannot be read as lines.
		require.NoError(t, os.MkdirAll(logtriage.LogPath(town), 0o700))
		var out bytes.Buffer
		err := jevNoiseWithFlags(&out, town, "logtriage", "", noise.DefaultCut, false, now)
		assert.ErrorContains(t, err, "reading the battery log")
		assert.Empty(t, out.String())
	})

	t.Run("one question answered as two kinds", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		logPath := logtriage.LogPath(town)
		ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
		p := 0.2
		// k1 answers the question as a noul and k2 as a choice, so it has two kinds.
		require.NoError(t, battery.Append(logPath, logtriage.Line{
			TS: ts, Key: "k1", QuestionVersion: logtriage.Version,
			Answers: map[string]logtriage.Answer{logtriage.QNeedsAction: {Noul: &p}},
		}))
		require.NoError(t, battery.Append(logPath, logtriage.Line{
			TS: ts, Key: "k2", QuestionVersion: logtriage.Version,
			Answers: map[string]logtriage.Answer{logtriage.QNeedsAction: {Choice: "a"}},
		}))
		// Each key's repeats answer the same kind as its original.
		for i := range 3 {
			require.NoError(t, battery.AppendReask(logPath, battery.ReaskLine{
				TS: ts.Add(time.Duration(i+1) * time.Minute), Key: "k1", Repeat: i + 1, QuestionVersion: logtriage.Version,
				Answers: map[string]logtriage.Answer{logtriage.QNeedsAction: {Noul: &p}},
			}))
			require.NoError(t, battery.AppendReask(logPath, battery.ReaskLine{
				TS: ts.Add(time.Duration(i+1) * time.Minute), Key: "k2", Repeat: i + 1, QuestionVersion: logtriage.Version,
				Answers: map[string]logtriage.Answer{logtriage.QNeedsAction: {Choice: "a"}},
			}))
		}
		var out bytes.Buffer
		err := jevNoiseWithFlags(&out, town, "logtriage", "", noise.DefaultCut, false, now)
		assert.ErrorContains(t, err, "was answered as")
		assert.Empty(t, out.String())
	})
}
