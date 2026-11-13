package metrics

import (
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
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
			if got := runtimeEra(defaultEraCuts(), tc.landedAt); got != tc.want {
				t.Errorf("runtimeEra(%q) = %q, want %q", tc.landedAt, got, tc.want)
			}
		})
	}
}

// regimeLogLine is one line of the regime log: a record of kind for reason at ts.
func regimeLogLine(ts, kind, reason string) string {
	return `{"ts":"` + ts + `","kind":"` + kind + `","before_ref":"","after_ref":"","commit":"","reason":"` +
		reason + `","actor":"overseer"}` + "\n"
}

// TestReadEraCutsFromTheRegimeLog: each cut is the regime log's platform event
// with that move's reason, the earliest one when there are several. A platform
// event with any other reason, or a record of another kind under a move's reason,
// cuts no era, so the backfill constant answers for it.
func TestReadEraCutsFromTheRegimeLog(t *testing.T) {
	t.Parallel()
	runtimeDir := filepath.Join("/town", ".runtime", "metrics")
	logFile := filepath.Join(runtimeDir, "regime-events.jsonl")
	backfillFile := filepath.Join(runtimeDir, "regime-events-backfill-20261101.jsonl")
	entries := []DirEntry{{Name: "regime-events.jsonl"}, {Name: "regime-events-backfill-20261101.jsonl"}}
	const constants = "sandbox 2026-10-29T00:00:00Z (constants), pod 2026-11-10T00:00:00Z (constants)"
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "no regime log: both cuts are the backfill constants",
			want: constants,
		},
		{
			name:  "the logged sandbox move is the sandbox cut",
			files: map[string]string{logFile: regimeLogLine("2026-10-29T06:00:00Z", "platform", regimeSandboxMoveReason)},
			want:  "sandbox 2026-10-29T06:00:00Z (log), pod 2026-11-10T00:00:00Z (constants)",
		},
		{
			name: "a platform event with another reason cuts no era (the 11-01 resize)",
			files: map[string]string{
				logFile:      regimeLogLine("2026-10-15T09:00:00Z", "platform", "resized the seat pool"),
				backfillFile: regimeLogLine("2026-11-01T10:00:00Z", "platform", "resized the control cluster"),
			},
			want: constants,
		},
		{
			name:  "a record of another kind under a move's reason cuts no era",
			files: map[string]string{logFile: regimeLogLine("2026-10-29T06:00:00Z", "gate", regimeSandboxMoveReason)},
			want:  constants,
		},
		{
			name: "the earliest logged pod move is the pod cut",
			files: map[string]string{
				logFile:      regimeLogLine("2026-11-10T12:00:00Z", "platform", regimePodMoveReason),
				backfillFile: regimeLogLine("2026-11-10T08:00:00Z", "platform", regimePodMoveReason),
			},
			want: "sandbox 2026-10-29T00:00:00Z (constants), pod 2026-11-10T08:00:00Z (log)",
		},
		{
			name: "both moves logged: both cuts are from the log",
			files: map[string]string{
				logFile: regimeLogLine("2026-10-29T06:00:00Z", "platform", regimeSandboxMoveReason) +
					regimeLogLine("2026-11-10T09:00:00Z", "platform", regimePodMoveReason),
			},
			want: "sandbox 2026-10-29T06:00:00Z (log), pod 2026-11-10T09:00:00Z (log)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Both log files are read whether or not they hold a record, so an
			// absent log is an empty file here.
			files := map[string]string{logFile: "", backfillFile: ""}
			maps.Copy(files, tc.files)
			cuts := readEraCuts(runtimeRegimeEnv(files, entries))
			if got := cuts.describe(); got != tc.want {
				t.Errorf("era cuts = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoggedCutDecidesTheEraOfALanding: a landing reads as the era the logged cut
// puts it in, not the backfill constant's.
func TestLoggedCutDecidesTheEraOfALanding(t *testing.T) {
	t.Parallel()
	runtimeDir := filepath.Join("/town", ".runtime", "metrics")
	files := map[string]string{
		filepath.Join(runtimeDir, "regime-events.jsonl"): regimeLogLine("2026-10-29T06:00:00Z", "platform", regimeSandboxMoveReason),
	}
	cuts := readEraCuts(runtimeRegimeEnv(files, []DirEntry{{Name: "regime-events.jsonl"}}))
	if got := runtimeEra(cuts, "2026-10-29T03:00:00Z"); got != eraPreSandbox {
		t.Errorf("a landing at 03:00 under the logged 06:00 cut = %q, want %q", got, eraPreSandbox)
	}
	if got := runtimeEra(cuts, "2026-10-29T07:00:00Z"); got != eraSandbox {
		t.Errorf("a landing at 07:00 under the logged 06:00 cut = %q, want %q", got, eraSandbox)
	}
}

// TestReadEraCutsUnreadableLogFallsBack: a regime log that cannot be read leaves
// both cuts on the backfill constants, and the cuts say why the log did not
// answer, so the verdict names the fallback.
func TestReadEraCutsUnreadableLogFallsBack(t *testing.T) {
	t.Parallel()
	env := runtimeRegimeEnv(nil, nil)
	env.ReadDir = func(string) ([]DirEntry, error) { return nil, errors.New("listing refused") }
	cuts := readEraCuts(env)
	if cuts.SandboxFrom != eraCutFromConstants || cuts.PodFrom != eraCutFromConstants {
		t.Errorf("cut sources = %q, %q; want both constants", cuts.SandboxFrom, cuts.PodFrom)
	}
	if !strings.Contains(cuts.Note, "regime log unreadable") || !strings.Contains(cuts.describe(), "listing refused") {
		t.Errorf("cuts = %q; want the unreadable log named", cuts.describe())
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			split := SplitByEra(tc.rows, defaultEraCuts())
			if got := eraCounts(split.Train, split.Cuts); !maps.Equal(got, tc.train) {
				t.Errorf("train eras = %v, want %v", got, tc.train)
			}
			if got := eraCounts(split.Holdout, split.Cuts); !maps.Equal(got, tc.hold) {
				t.Errorf("holdout eras = %v, want %v", got, tc.hold)
			}
		})
	}
}

// TestSplitByEraExcludesLandedAtWithNoInstant: a landing whose landed_at names no
// instant sits on neither side and is counted as unknown, so it is never the
// last landing. Under the text order the empty and garbage rows would sort last
// and become the holdout.
func TestSplitByEraExcludesLandedAtWithNoInstant(t *testing.T) {
	t.Parallel()
	normal := landedRows(preSandboxStart, 10)
	rows := append(slices.Clone(normal),
		Row{"landed_at": "not-a-time", "rejected_before_landing": "1"},
		Row{"landed_at": "", "rejected_before_landing": "1"},
	)
	split := SplitByEra(rows, defaultEraCuts())
	if split.Unknown != 2 {
		t.Errorf("Unknown = %d, want 2", split.Unknown)
	}
	if len(split.Train) != 7 || len(split.Holdout) != 3 {
		t.Fatalf("split %d train / %d holdout, want 7 / 3 of the 10 dated landings", len(split.Train), len(split.Holdout))
	}
	for _, r := range append(slices.Clone(split.Train), split.Holdout...) {
		if _, err := ParseTS(r["landed_at"]); err != nil {
			t.Errorf("row with landed_at %q is on a side of the split", r["landed_at"])
		}
	}
	if last := split.Holdout[len(split.Holdout)-1]["landed_at"]; last != normal[len(normal)-1]["landed_at"] {
		t.Errorf("the holdout ends at %q, want the real last landing %q", last, normal[len(normal)-1]["landed_at"])
	}
	if got := eraCounts(split.Holdout, split.Cuts); !maps.Equal(got, map[string]int{eraPreSandbox: 3}) {
		t.Errorf("holdout eras = %v, want the last era pre-sandbox with 3", got)
	}
}

// TestSplitByEraOrdersByInstantNotText: two spellings of a zone order by the
// instant they name. 09:00Z is later than 10:00+02:00 (08:00Z), though the text
// sorts the other way, so the earlier instant trains and the later one holds out.
func TestSplitByEraOrdersByInstantNotText(t *testing.T) {
	t.Parallel()
	rows := []Row{
		{"landed_at": "2026-10-30T09:00:00Z", "rejected_before_landing": "0"},
		{"landed_at": "2026-10-30T10:00:00+02:00", "rejected_before_landing": "0"},
	}
	split := SplitByEra(rows, defaultEraCuts())
	if len(split.Train) != 1 || len(split.Holdout) != 1 {
		t.Fatalf("split %d train / %d holdout, want 1 / 1", len(split.Train), len(split.Holdout))
	}
	if got := split.Train[0]["landed_at"]; got != "2026-10-30T10:00:00+02:00" {
		t.Errorf("train holds %q, want the earlier instant 2026-10-30T10:00:00+02:00", got)
	}
	if got := split.Holdout[0]["landed_at"]; got != "2026-10-30T09:00:00Z" {
		t.Errorf("holdout holds %q, want the later instant 2026-10-30T09:00:00Z", got)
	}
}

// TestSplitByEraEmptyRows: an empty table splits into two empty sides and no
// unknown landing.
func TestSplitByEraEmptyRows(t *testing.T) {
	t.Parallel()
	split := SplitByEra(nil, defaultEraCuts())
	if len(split.Train) != 0 || len(split.Holdout) != 0 || split.Unknown != 0 {
		t.Errorf("SplitByEra(nil) = %d train, %d holdout, %d unknown; want none", len(split.Train), len(split.Holdout), split.Unknown)
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
			p := Prepare(header, SplitByEra(rows, defaultEraCuts()), nil, "rejected_before_landing")
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
// side's era counts beside its row counts, read from the split Prepare cut, and
// names the cuts with the source that answered and the unknown count.
func TestMangoHeadNamesTheSplitsEras(t *testing.T) {
	t.Parallel()
	header := []string{"landed_at", "rejected_before_landing"}
	rows := append(landedRows(preSandboxStart, 10), landedRows(podStart, 299)...)
	p := Prepare(header, SplitByEra(rows, defaultEraCuts()), nil, "rejected_before_landing")
	head := mangoHead("rejected_before_landing", p)
	for _, want := range []string{
		"Train eras: pre-sandbox 10, pod 209. Holdout eras: pod 90.",
		"Era cuts: sandbox 2026-10-29T00:00:00Z (constants), pod 2026-11-10T00:00:00Z (constants). Unknown landed_at, in neither side: 0.",
	} {
		if !slices.Contains(head, want) {
			t.Errorf("mangoHead = %q, want a line %q", head, want)
		}
	}
}

// TestBuildPanelWritesTheEraCounts: panel.json names each side's era counts, the
// unknown count and the cuts when the split has landings, and leaves the keys out
// when it has none.
func TestBuildPanelWritesTheEraCounts(t *testing.T) {
	t.Parallel()
	withEras, err := json.Marshal(BuildPanel(nil, PanelMeta{
		SplitEras: PanelEras{
			Train:   map[string]int{eraPreSandbox: 10, eraPod: 209},
			Holdout: map[string]int{eraPod: 90},
			Unknown: 2,
			Cuts:    "sandbox 2026-10-29T00:00:00Z (constants), pod 2026-11-10T00:00:00Z (constants)",
		},
	}, panelGoldenNow))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"train_eras":{"pod":209,"pre-sandbox":10}`,
		`"holdout_eras":{"pod":90}`,
		`"unknown_eras":2`,
		`"era_cuts":"sandbox 2026-10-29T00:00:00Z (constants), pod 2026-11-10T00:00:00Z (constants)"`,
	} {
		if !strings.Contains(string(withEras), want) {
			t.Errorf("panel with eras writes no %s: %s", want, withEras)
		}
	}

	bare, err := json.Marshal(BuildPanel(nil, PanelMeta{}, panelGoldenNow))
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"train_eras"`, `"holdout_eras"`, `"unknown_eras"`, `"era_cuts"`} {
		if strings.Contains(string(bare), absent) {
			t.Errorf("panel with no landings writes %s: %s", absent, bare)
		}
	}
}
