package metrics

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// preSandboxStart and podStart are the first landings of a run of test landings,
// each a day inside the era the run is meant to hold.
var (
	preSandboxStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	podStart        = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
)

// landedRows is n landings, one a minute from start, so the rows are already in
// landed_at order and the split's order is the order they were built in.
func landedRows(start time.Time, n int) []Row {
	rows := make([]Row, 0, n)
	for i := range n {
		rows = append(rows, Row{
			"landed_at":               start.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			"rejected_before_landing": "0",
		})
	}
	return rows
}

// TestRuntimeEraOnEachSideOfTheMoves: a landing reads as the era its landed_at
// falls in, with the sandbox starting at 2026-10-29 and the pod at 2026-11-10,
// and a landed_at that names no instant is unknown rather than a guessed era.
func TestRuntimeEraOnEachSideOfTheMoves(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		landedAt string
		want     string
	}{
		{"the last second before the sandbox move", "2026-10-28T23:59:59Z", eraPreSandbox},
		{"the sandbox move's first instant", "2026-10-29T00:00:00Z", eraSandbox},
		{"the last second before the pod move", "2026-11-09T23:59:59Z", eraSandbox},
		{"the pod move's first instant", "2026-11-10T00:00:00Z", eraPod},
		{"a landed_at with no instant", "not-a-time", eraUnknown},
		{"an empty landed_at", "", eraUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runtimeEra(tc.landedAt); got != tc.want {
				t.Errorf("runtimeEra(%q) = %q, want %q", tc.landedAt, got, tc.want)
			}
		})
	}
}

// eraLedgerLine is one ledger line for a landing at landedAt.
func eraLedgerLine(bead, landedAt string) string {
	return `{"bead":"` + bead + `","rig":"gastown","landed_commit":"c-` + bead +
		`","landed_at":"` + landedAt + `","route":"daemon","om_verdict":"approve","gate_result":"build"}`
}

// TestLandingsCsvCarriesEachLandingsEra: the runtime_era cell a landing row
// writes, read through the same BuildLandings join the extract runs, is the era
// of its landed_at on each side of both moves.
func TestLandingsCsvCarriesEachLandingsEra(t *testing.T) {
	t.Parallel()
	landings := map[string]string{
		"gt-e1": "2026-10-28T23:59:59Z",
		"gt-e2": "2026-10-29T00:00:00Z",
		"gt-e3": "2026-11-09T23:59:59Z",
		"gt-e4": "2026-11-10T00:00:00Z",
	}
	lines := make([]string, 0, len(landings))
	for bead, at := range landings {
		lines = append(lines, eraLedgerLine(bead, at))
	}
	ledger := LoadLedger(map[string][]string{"gastown": lines}, &Dropped{})
	rows := buildLedgerLandings(t, ledger, nil, nil, StageStamps{})
	if len(rows) != len(landings) {
		t.Fatalf("rows = %d, want %d landings", len(rows), len(landings))
	}
	want := map[string]string{
		"gt-e1": eraPreSandbox,
		"gt-e2": eraSandbox,
		"gt-e3": eraSandbox,
		"gt-e4": eraPod,
	}
	for _, row := range rows {
		if got := row["runtime_era"]; got != want[row["bead"]] {
			t.Errorf("%s runtime_era = %q, want %q (landed_at %s)", row["bead"], got, want[row["bead"]], row["landed_at"])
		}
	}
}

// TestSplitByEraCutsTheLastEraByTheRule: the holdout is the whole last era when
// that era holds 300 or more landings and earlier landings exist; under 300, or
// with no earlier landing, the holdout is the later 30% of the last era and
// every earlier landing trains.
func TestSplitByEraCutsTheLastEraByTheRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		rows        []Row
		train, hold map[string]int
	}{
		{
			name:  "a last era of 299 is cut inside the era",
			rows:  append(landedRows(preSandboxStart, 10), landedRows(podStart, 299)...),
			train: map[string]int{eraPreSandbox: 10, eraPod: 209},
			hold:  map[string]int{eraPod: 90},
		},
		{
			name:  "a last era of 300 is held out whole",
			rows:  append(landedRows(preSandboxStart, 10), landedRows(podStart, 300)...),
			train: map[string]int{eraPreSandbox: 10},
			hold:  map[string]int{eraPod: 300},
		},
		{
			name:  "a last era of 300 with no earlier landing is cut inside the era",
			rows:  landedRows(podStart, 300),
			train: map[string]int{eraPod: 210},
			hold:  map[string]int{eraPod: 90},
		},
		{
			name: "a short last era keeps every earlier era in train",
			rows: append(append(landedRows(preSandboxStart, 10),
				landedRows(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), 300)...),
				landedRows(podStart, 5)...),
			train: map[string]int{eraPreSandbox: 10, eraSandbox: 300, eraPod: 3},
			hold:  map[string]int{eraPod: 2},
		},
		{
			name: "a landed_at that names no instant is its own era",
			rows: append(landedRows(preSandboxStart, 10), Row{
				"landed_at":               "soon",
				"rejected_before_landing": "0",
			}),
			train: map[string]int{eraPreSandbox: 10},
			hold:  map[string]int{eraUnknown: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			train, holdout := SplitByEra(tc.rows)
			if got := eraCounts(train); !maps.Equal(got, tc.train) {
				t.Errorf("train eras = %v, want %v", got, tc.train)
			}
			if got := eraCounts(holdout); !maps.Equal(got, tc.hold) {
				t.Errorf("holdout eras = %v, want %v", got, tc.hold)
			}
		})
	}
}

// TestSplitByEraEmptyRows: an empty table splits into two empty sides.
func TestSplitByEraEmptyRows(t *testing.T) {
	t.Parallel()
	train, holdout := SplitByEra(nil)
	if len(train) != 0 || len(holdout) != 0 {
		t.Errorf("SplitByEra(nil) = %d train, %d holdout; want none", len(train), len(holdout))
	}
}

// TestPrepareSplitsByRuntimeEra: the split the mango stage trains and checks on
// is the era split: a last era of 299 landings is cut inside the era, and one of
// 300 is held out whole.
func TestPrepareSplitsByRuntimeEra(t *testing.T) {
	t.Parallel()
	header := []string{"landed_at", "rejected_before_landing", "reached_overrun_stop"}
	tests := []struct {
		name        string
		pod         int
		train, hold int
	}{
		{"a 299 last era", 299, 219, 90},
		{"a 300 last era", 300, 10, 300},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := append(landedRows(preSandboxStart, 10), landedRows(podStart, tc.pod)...)
			p := Prepare(header, rows, nil, "rejected_before_landing")
			if len(p.Train) != tc.train || len(p.Holdout) != tc.hold {
				t.Errorf("Prepare split %d train / %d holdout, want %d / %d",
					len(p.Train), len(p.Holdout), tc.train, tc.hold)
			}
		})
	}
}

// TestEraCountsTextWritesEachEraInOrder: the digest's words put each era's count
// in era order and name an empty side as none.
func TestEraCountsTextWritesEachEraInOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		counts map[string]int
		want   string
	}{
		{"all three eras", map[string]int{eraPod: 90, eraPreSandbox: 50, eraSandbox: 70}, "pre-sandbox 50, sandbox 70, pod 90"},
		{"an unknown era last", map[string]int{eraUnknown: 2, eraPod: 1}, "pod 1, unknown 2"},
		{"no landing", nil, "none"},
		{"a zero count is absent", map[string]int{eraPod: 0}, "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := eraCountsText(tc.counts); got != tc.want {
				t.Errorf("eraCountsText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMangoHeadNamesTheSplitsEras: the digest's head for a target puts each
// side's era counts beside its row counts, read from the split Prepare cut.
func TestMangoHeadNamesTheSplitsEras(t *testing.T) {
	t.Parallel()
	header := []string{"landed_at", "rejected_before_landing"}
	rows := append(landedRows(preSandboxStart, 10), landedRows(podStart, 299)...)
	p := Prepare(header, rows, nil, "rejected_before_landing")
	head := mangoHead("rejected_before_landing", p)
	want := "Train eras: pre-sandbox 10, pod 209. Holdout eras: pod 90."
	if !slices.Contains(head, want) {
		t.Errorf("mangoHead = %q, want a line %q", head, want)
	}
}

// TestBuildPanelWritesTheEraCounts: panel.json names each side's era counts
// when the split has landings, and leaves the keys out when it has none.
func TestBuildPanelWritesTheEraCounts(t *testing.T) {
	t.Parallel()
	withEras, err := json.Marshal(BuildPanel(nil, PanelMeta{
		SplitEras: PanelEras{
			Train:   map[string]int{eraPreSandbox: 10, eraPod: 209},
			Holdout: map[string]int{eraPod: 90},
		},
	}, panelGoldenNow))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"train_eras":{"pod":209,"pre-sandbox":10}`, `"holdout_eras":{"pod":90}`} {
		if !strings.Contains(string(withEras), want) {
			t.Errorf("panel with eras writes no %s: %s", want, withEras)
		}
	}

	bare, err := json.Marshal(BuildPanel(nil, PanelMeta{}, panelGoldenNow))
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"train_eras"`, `"holdout_eras"`} {
		if strings.Contains(string(bare), absent) {
			t.Errorf("panel with no landings writes %s: %s", absent, bare)
		}
	}
}
