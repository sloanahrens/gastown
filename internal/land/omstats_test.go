package land

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// omStatsRecordLine renders rec the way the landing worker writes it.
func omStatsRecordLine(t *testing.T, rec ReviewRecord) string {
	t.Helper()
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal %+v: %v", rec, err)
	}
	return string(line)
}

func omStatsRecords(t *testing.T, recs ...ReviewRecord) string {
	t.Helper()
	lines := make([]string, 0, len(recs))
	for _, rec := range recs {
		lines = append(lines, omStatsRecordLine(t, rec))
	}
	return strings.Join(lines, "\n") + "\n"
}

// omStatsRecordWithoutFigures renders rec with the named JSON fields removed,
// for a line written before that figure was always present.
func omStatsRecordWithoutFigures(t *testing.T, rec ReviewRecord, fields ...string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(omStatsRecordLine(t, rec)), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range fields {
		delete(m, field)
	}
	line, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(line)
}

func omStatsAt(day int, hour, min, sec int) time.Time {
	return time.Date(2026, 10, day, hour, min, sec, 0, time.Local)
}

func wantOmStatsDay(t *testing.T, got OmStats, want OmStatsDay) {
	t.Helper()
	for _, d := range got.Days {
		if d.Day != want.Day {
			continue
		}
		if d.Approvals != want.Approvals || d.Rejections != want.Rejections || d.Fallback != want.Fallback {
			t.Errorf("day %s = %+v, want %+v", want.Day, d, want)
		}
		if math.Abs(d.Rate-want.Rate) > 1e-9 {
			t.Errorf("day %s rate = %v, want %v", want.Day, d.Rate, want.Rate)
		}
		if math.Abs(d.MeanFindings-want.MeanFindings) > 1e-9 {
			t.Errorf("day %s mean findings = %v, want %v", want.Day, d.MeanFindings, want.MeanFindings)
		}
		if math.Abs(d.MeanScore-want.MeanScore) > 1e-9 {
			t.Errorf("day %s mean score = %v, want %v", want.Day, d.MeanScore, want.MeanScore)
		}
		return
	}
	t.Fatalf("no day %s in %+v", want.Day, got.Days)
}

// TestAggregateOmStatsCountsTheRecords is the record half of gt-r05qz's
// acceptance: per-day, per-rig and per-seat counts, the rounds buckets, the
// means and the beads rejected most, over a fixture with an approval, a
// request_changes, a bounced approval, an unparseable line and a day boundary.
func TestAggregateOmStatsCountsTheRecords(t *testing.T) {
	t.Parallel()
	records := omStatsRecords(t,
		ReviewRecord{BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.40, Verdict: VerdictRequestChanges, Agent: "opus", Findings: 2, At: omStatsAt(16, 10, 0, 0)},
		ReviewRecord{BeadID: "gt-a", Rig: "gastown", Attempt: 2, Score: 0.90, Verdict: VerdictApprove, Agent: "opus", At: omStatsAt(16, 11, 0, 0)},
		ReviewRecord{BeadID: "gt-b", Rig: "gastown", Attempt: 1, Score: 0.95, Verdict: VerdictApprove, Rejected: true, Agent: "sonnet", Findings: 1, At: omStatsAt(16, 12, 0, 0)},
		// The last second of the 16th and the first of the 17th split the days.
		ReviewRecord{BeadID: "gt-e", Rig: "gastown", Attempt: 1, Score: 0.88, Verdict: VerdictApprove, Agent: "sonnet", At: omStatsAt(16, 23, 59, 59)},
		ReviewRecord{BeadID: "gt-c", Rig: "longeye", Attempt: 1, Score: 0.70, Verdict: VerdictApprove, Agent: "haiku", At: omStatsAt(17, 0, 0, 0)},
		ReviewRecord{BeadID: "gt-d", Rig: "longeye", Attempt: 1, Score: 0.50, Verdict: VerdictRequestChanges, Findings: 1, At: omStatsAt(17, 9, 0, 0)},
		ReviewRecord{BeadID: "gt-d", Rig: "longeye", Attempt: 2, Score: 0.80, Verdict: VerdictApprove, At: omStatsAt(17, 10, 0, 0)},
	) + "this is not a review record\n" + `{"bead":"gt-torn"` + "\n"

	stats, err := AggregateOmStats(strings.NewReader(records), nil, time.Time{})
	if err != nil {
		t.Fatalf("AggregateOmStats: %v", err)
	}
	if stats.Skipped != 2 {
		t.Errorf("skipped = %d, want 2", stats.Skipped)
	}
	if len(stats.Days) != 2 {
		t.Fatalf("days = %+v, want 2", stats.Days)
	}
	wantOmStatsDay(t, stats, OmStatsDay{Day: "2026-10-16", Approvals: 2, Rejections: 2, Rate: 0.5, MeanFindings: 1.5, MeanScore: 0.675})
	wantOmStatsDay(t, stats, OmStatsDay{Day: "2026-10-17", Approvals: 2, Rejections: 1, Rate: 1.0 / 3.0, MeanFindings: 1, MeanScore: 0.5})

	wantRigs := []OmStatsGroup{
		{Name: "gastown", Approvals: 2, Rejections: 2, Rate: 0.5},
		{Name: "longeye", Approvals: 2, Rejections: 1, Rate: 1.0 / 3.0},
	}
	if len(stats.Rigs) != len(wantRigs) {
		t.Fatalf("rigs = %+v, want %+v", stats.Rigs, wantRigs)
	}
	for i, want := range wantRigs {
		if stats.Rigs[i].Name != want.Name || stats.Rigs[i].Approvals != want.Approvals ||
			stats.Rigs[i].Rejections != want.Rejections || math.Abs(stats.Rigs[i].Rate-want.Rate) > 1e-9 {
			t.Errorf("rig %d = %+v, want %+v", i, stats.Rigs[i], want)
		}
	}

	wantAgents := []OmStatsGroup{
		{Name: OmStatsUnnamed, Approvals: 1, Rejections: 1, Rate: 0.5},
		{Name: "opus", Approvals: 1, Rejections: 1, Rate: 0.5},
		{Name: "sonnet", Approvals: 1, Rejections: 1, Rate: 0.5},
		{Name: "haiku", Approvals: 1, Rejections: 0, Rate: 0},
	}
	if len(stats.Agents) != len(wantAgents) {
		t.Fatalf("agents = %+v, want %+v", stats.Agents, wantAgents)
	}
	for i, want := range wantAgents {
		if stats.Agents[i] != want {
			t.Errorf("agent %d = %+v, want %+v", i, stats.Agents[i], want)
		}
	}

	wantRounds := OmStatsRounds{One: 3, Two: 2}
	if stats.Rounds != wantRounds {
		t.Errorf("rounds = %+v, want %+v", stats.Rounds, wantRounds)
	}

	wantBeads := []OmStatsBead{
		{Bead: "gt-a", Rig: "gastown", Rejections: 1},
		{Bead: "gt-b", Rig: "gastown", Rejections: 1},
		{Bead: "gt-d", Rig: "longeye", Rejections: 1},
	}
	if len(stats.TopBeads) != len(wantBeads) {
		t.Fatalf("top beads = %+v, want %+v", stats.TopBeads, wantBeads)
	}
	for i, want := range wantBeads {
		if stats.TopBeads[i] != want {
			t.Errorf("top bead %d = %+v, want %+v", i, stats.TopBeads[i], want)
		}
	}
}

// TestAggregateOmStatsFillsDaysBeforeTheFirstRecord covers the fallback half of
// gt-r05qz's acceptance: the daemon log fills the days before the first record,
// those days are marked, they count in the day table alone, and a rejected
// gate landing, a bare merge and the alternate rejection line are not om
// verdicts.
func TestAggregateOmStatsFillsDaysBeforeTheFirstRecord(t *testing.T) {
	t.Parallel()
	records := omStatsRecords(t,
		ReviewRecord{BeadID: "gt-new", Rig: "gastown", Attempt: 1, Score: 0.90, Verdict: VerdictApprove, Agent: "opus", At: omStatsAt(16, 12, 0, 0)},
	)
	log := strings.Join([]string{
		"2026/10/14 23:10:00 landing_worker: [land] gt-old: rejected (review): om requested changes (score 0.55, 3 finding(s))",
		"2026/10/14 23:40:00 landing_worker: [land] gt-old2: landed abc1234 on origin/main (patch-id def5678)",
		"2026/10/15 10:00:00 landing_worker: [land] gt-old3: already landed as abc1234; finishing its record",
		"2026/10/15 10:05:00 landing_worker: [land] gt-old3: landed abc1234 on origin/main (patch-id def5678)",
		// A gate rejection never reached om, and a merge is made before the
		// review, so neither is a verdict.
		"2026/10/15 11:00:00 landing_worker: [land] gt-gate: rejected (gate): make gate failed",
		"2026/10/15 12:00:00 landing_worker: [land] gt-merged: merged abc1234 onto origin/main (def5678) as aaaabbbb",
		"2026/10/15 13:00:00 landing_worker: gastown: gt-alt: landing rejected (review): om requested changes (score 0.10)",
		"2026/10/15 14:00:00 some other daemon line",
		// Same day as the first record: the record is the source, not the log.
		"2026/10/16 09:00:00 landing_worker: [land] gt-new: rejected (review): om requested changes (score 0.30, 1 finding(s))",
	}, "\n") + "\n"

	stats, err := AggregateOmStats(strings.NewReader(records), strings.NewReader(log), time.Time{})
	if err != nil {
		t.Fatalf("AggregateOmStats: %v", err)
	}
	want := []OmStatsDay{
		{Day: "2026-10-14", Approvals: 1, Rejections: 1, Rate: 0.5, MeanFindings: 3, MeanScore: 0.55, Fallback: true},
		{Day: "2026-10-15", Approvals: 1, Rejections: 0, Rate: 0, Fallback: true},
		{Day: "2026-10-16", Approvals: 1, Rejections: 0, Rate: 0},
	}
	if len(stats.Days) != len(want) {
		t.Fatalf("days = %+v, want %+v", stats.Days, want)
	}
	for i, day := range want {
		if stats.Days[i].Day != day.Day || stats.Days[i].Approvals != day.Approvals ||
			stats.Days[i].Rejections != day.Rejections || stats.Days[i].Fallback != day.Fallback {
			t.Errorf("day %d = %+v, want %+v", i, stats.Days[i], day)
		}
		if math.Abs(stats.Days[i].Rate-day.Rate) > 1e-9 || math.Abs(stats.Days[i].MeanFindings-day.MeanFindings) > 1e-9 ||
			math.Abs(stats.Days[i].MeanScore-day.MeanScore) > 1e-9 {
			t.Errorf("day %d = %+v, want %+v", i, stats.Days[i], day)
		}
	}
	// The log names no rig and no seat, so fallback days fill the day table
	// alone.
	if len(stats.Rigs) != 1 || stats.Rigs[0] != (OmStatsGroup{Name: "gastown", Approvals: 1, Rate: 0}) {
		t.Errorf("rigs = %+v, want only the record's gastown approval", stats.Rigs)
	}
	if len(stats.Agents) != 1 || stats.Agents[0].Name != "opus" {
		t.Errorf("agents = %+v, want only the record's opus approval", stats.Agents)
	}
	if len(stats.TopBeads) != 0 {
		t.Errorf("top beads = %+v, want none: no record rejected", stats.TopBeads)
	}
	if stats.Rounds != (OmStatsRounds{One: 1}) {
		t.Errorf("rounds = %+v, want one bead at one round", stats.Rounds)
	}
}

// TestAggregateOmStatsSinceFiltersEveryFigure pins --since to the calendar day:
// a record and a fallback day before it are out of every count.
func TestAggregateOmStatsSinceFiltersEveryFigure(t *testing.T) {
	t.Parallel()
	records := omStatsRecords(t,
		ReviewRecord{BeadID: "gt-old", Rig: "gastown", Attempt: 1, Score: 0.40, Verdict: VerdictRequestChanges, Findings: 2, At: omStatsAt(16, 10, 0, 0)},
		ReviewRecord{BeadID: "gt-new", Rig: "longeye", Attempt: 1, Score: 0.90, Verdict: VerdictApprove, Agent: "opus", At: omStatsAt(17, 10, 0, 0)},
	)
	log := "2026/10/15 10:00:00 landing_worker: [land] gt-older: rejected (review): om requested changes (score 0.20, 1 finding(s))\n"

	stats, err := AggregateOmStats(strings.NewReader(records), strings.NewReader(log), omStatsAt(17, 0, 0, 0))
	if err != nil {
		t.Fatalf("AggregateOmStats: %v", err)
	}
	if len(stats.Days) != 1 || stats.Days[0].Day != "2026-10-17" {
		t.Fatalf("days = %+v, want only 2026-10-17", stats.Days)
	}
	if stats.Skipped != 0 {
		t.Errorf("skipped = %d, want 0", stats.Skipped)
	}
	if len(stats.Rigs) != 1 || stats.Rigs[0].Name != "longeye" {
		t.Errorf("rigs = %+v, want only longeye", stats.Rigs)
	}
	if stats.Rounds != (OmStatsRounds{One: 1}) {
		t.Errorf("rounds = %+v, want the 17th's one bead", stats.Rounds)
	}
	if len(stats.TopBeads) != 0 {
		t.Errorf("top beads = %+v, want none after the filter", stats.TopBeads)
	}
}

// TestAggregateOmStatsWithoutRecordsFallsBackEntirely: a town whose review log
// does not exist yet still reports the log's days, all marked fallback.
func TestAggregateOmStatsWithoutRecordsFallsBackEntirely(t *testing.T) {
	t.Parallel()
	log := "2026/10/15 10:00:00 landing_worker: [land] gt-old: rejected (review): om requested changes (score 0.20)\n" +
		"2026/10/15 11:00:00 landing_worker: [land] gt-old2: landed abc1234 on origin/main (patch-id def5678)\n"

	stats, err := AggregateOmStats(nil, strings.NewReader(log), time.Time{})
	if err != nil {
		t.Fatalf("AggregateOmStats: %v", err)
	}
	if len(stats.Days) != 1 || !stats.Days[0].Fallback || stats.Days[0].Approvals != 1 || stats.Days[0].Rejections != 1 {
		t.Fatalf("days = %+v, want one fallback day with one approval and one rejection", stats.Days)
	}
	if stats.Days[0].MeanScore != 0.20 || stats.Days[0].MeanFindings != 0 {
		t.Errorf("means = %+v, want a scored rejection and no findings", stats.Days[0])
	}
}

// TestAggregateOmStatsLogMeansIgnoreAbsentFigures: a daemon-log rejection that
// names no score or no findings length enters neither mean, so a partial line
// does not pull the average toward zero.
func TestAggregateOmStatsLogMeansIgnoreAbsentFigures(t *testing.T) {
	t.Parallel()
	log := strings.Join([]string{
		"2026/10/15 10:00:00 landing_worker: [land] gt-a: rejected (review): om requested changes",
		"2026/10/15 11:00:00 landing_worker: [land] gt-b: rejected (review): om requested changes (score 0.20)",
		"2026/10/15 12:00:00 landing_worker: [land] gt-c: rejected (review): om requested changes (2 finding(s))",
		"2026/10/16 10:00:00 landing_worker: [land] gt-d: rejected (review): om requested changes (score 0.00, 0 finding(s))",
	}, "\n") + "\n"

	stats, err := AggregateOmStats(nil, strings.NewReader(log), time.Time{})
	if err != nil {
		t.Fatalf("AggregateOmStats: %v", err)
	}
	wantOmStatsDay(t, stats, OmStatsDay{Day: "2026-10-15", Rejections: 3, Rate: 1, MeanFindings: 2, MeanScore: 0.20, Fallback: true})
	// A named zero is a sample, unlike an absent figure.
	wantOmStatsDay(t, stats, OmStatsDay{Day: "2026-10-16", Rejections: 1, Rate: 1, MeanScore: 0, Fallback: true})
}

// TestAggregateOmStatsOrdersTopBeads: the ten-bead table keeps the highest
// counts, then the lowest bead id, so the same fixture always prints the same
// rows.
func TestAggregateOmStatsOrdersTopBeads(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := 0; i < OmStatsTopBeads+1; i++ {
		bead := fmt.Sprintf("gt-%02d", i)
		lines = append(lines, omStatsRecordLine(t, ReviewRecord{
			BeadID: bead, Rig: "gastown", Attempt: 1, Score: 0.4,
			Verdict: VerdictRequestChanges, At: omStatsAt(16, 10, 0, 0),
		}))
	}
	// The bead rejected twice leads a table capped at ten even though its id
	// sorts last, so the cap cannot pass by insertion or id order.
	lines = append(lines,
		omStatsRecordLine(t, ReviewRecord{
			BeadID: "gt-zzz", Rig: "gastown", Attempt: 1, Score: 0.4,
			Verdict: VerdictRequestChanges, At: omStatsAt(16, 10, 0, 0),
		}),
		omStatsRecordLine(t, ReviewRecord{
			BeadID: "gt-zzz", Rig: "gastown", Attempt: 3, Score: 0.4,
			Verdict: VerdictRequestChanges, At: omStatsAt(16, 10, 0, 0),
		}),
	)

	stats, err := AggregateOmStats(strings.NewReader(strings.Join(lines, "\n")), nil, time.Time{})
	if err != nil {
		t.Fatalf("AggregateOmStats: %v", err)
	}
	if len(stats.TopBeads) != OmStatsTopBeads {
		t.Fatalf("top beads = %d rows, want %d", len(stats.TopBeads), OmStatsTopBeads)
	}
	if stats.TopBeads[0].Bead != "gt-zzz" || stats.TopBeads[0].Rejections != 2 {
		t.Errorf("first row = %+v, want gt-zzz with two rejections", stats.TopBeads[0])
	}
	if stats.Rounds.ThreePlus != 1 || stats.Rounds.One != OmStatsTopBeads+1 {
		t.Errorf("rounds = %+v, want one bead at three rounds", stats.Rounds)
	}
}

// TestAggregateOmStatsSkipsAnOversizedLine: one enormous line is malformed
// data to skip and count, not a read error that discards every other record.
func TestAggregateOmStatsSkipsAnOversizedLine(t *testing.T) {
	t.Parallel()
	records := omStatsRecordLine(t, ReviewRecord{
		BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.90,
		Verdict: VerdictApprove, Agent: "opus", At: omStatsAt(16, 10, 0, 0),
	}) + "\n" + strings.Repeat("x", 2<<20) + "\n"

	stats, err := AggregateOmStats(strings.NewReader(records), nil, time.Time{})
	if err != nil {
		t.Fatalf("AggregateOmStats: %v", err)
	}
	if stats.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", stats.Skipped)
	}
	wantOmStatsDay(t, stats, OmStatsDay{Day: "2026-10-16", Approvals: 1, Rate: 0})
}

// TestAggregateOmStatsReturnsAReadError: a source that fails is an error, not a
// partial report that reads as the whole truth.
func TestAggregateOmStatsReturnsAReadError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	if _, err := AggregateOmStats(iotest.ErrReader(boom), nil, time.Time{}); err == nil || !strings.Contains(err.Error(), "review records") {
		t.Errorf("records read error = %v, want it to name the review records", err)
	}
	records := omStatsRecords(t, ReviewRecord{
		BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.90,
		Verdict: VerdictApprove, At: omStatsAt(16, 10, 0, 0),
	})
	if _, err := AggregateOmStats(strings.NewReader(records), iotest.ErrReader(boom), time.Time{}); err == nil || !strings.Contains(err.Error(), "daemon log") {
		t.Errorf("log read error = %v, want it to name the daemon log", err)
	}
}

// TestAggregateOmStatsRecordMeansIgnoreAbsentFigures: a record line that names
// no score or no findings — an older or hand-written line — stays out of that
// mean, while a named zero is a sample.
func TestAggregateOmStatsRecordMeansIgnoreAbsentFigures(t *testing.T) {
	t.Parallel()
	named := omStatsRecordLine(t, ReviewRecord{
		BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.60,
		Verdict: VerdictRequestChanges, Findings: 4, At: omStatsAt(16, 10, 0, 0),
	})
	absent := omStatsRecordWithoutFigures(t, ReviewRecord{
		BeadID: "gt-b", Rig: "gastown", Attempt: 1, Verdict: VerdictRequestChanges,
		At: omStatsAt(16, 11, 0, 0),
	}, "score", "findings")
	zero := omStatsRecordLine(t, ReviewRecord{
		BeadID: "gt-c", Rig: "gastown", Attempt: 1, Verdict: VerdictRequestChanges,
		At: omStatsAt(16, 12, 0, 0),
	})

	stats, err := AggregateOmStats(strings.NewReader(strings.Join([]string{named, absent, zero}, "\n")+"\n"), nil, time.Time{})
	if err != nil {
		t.Fatalf("AggregateOmStats: %v", err)
	}
	// The absent line is a rejection but no sample, so the named figures and
	// gt-c's named zeros are the two samples: findings 4/2, score 0.60/2.
	wantOmStatsDay(t, stats, OmStatsDay{Day: "2026-10-16", Rejections: 3, Rate: 1, MeanFindings: 2, MeanScore: 0.30})
}
