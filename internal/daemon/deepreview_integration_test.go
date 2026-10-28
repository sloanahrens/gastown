//go:build integration

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/deepreview"
	"github.com/steveyegge/gastown/internal/land"
)

// deepReviewRunCap bounds the runs of the shadow job on the real ledger's
// records. It is the bead's cap, not a number to tune: a job that cannot plan
// the host's ledger inside it is a finding about the job, so raising it hides
// one (gt-6y692).
const deepReviewRunCap = 30 * time.Second

// deepReviewCopyNow is the tick every run below plans at. It sits after the
// newest record in the copy, and its default window (24h back, no cursor)
// starts before the oldest, so the first run sees the whole copy.
var deepReviewCopyNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// deepReviewCopyRig is the rig whose ledger the copy holds.
const deepReviewCopyRig = "gastown"

// deepReviewSliceFiles is the row bound the runs below set on the job: three
// rows a run, so the copy's risky landings take three runs and the cursor has
// to cross two run-bead notes to reach the end. It bounds the job; the runs it
// produces are checked against whole landed_at groups of the copy, not
// computed from the bound here.
const deepReviewSliceFiles = 3

// deepReviewRow is the part of a reviewed landing's JSON line the assertions
// read: the job writes the plan's records back in the landings.jsonl shape.
type deepReviewRow struct {
	Bead         string   `json:"bead"`
	Head         string   `json:"head"`
	LandedCommit string   `json:"landed_commit"`
	LandedAt     string   `json:"landed_at"`
	RiskPaths    []string `json:"risk_paths"`
}

// deepReviewCopySince is the window the first run starts from: the default
// window before the tick, which is the window a plan with no cursor answers.
var deepReviewCopySince = deepReviewCopyNow.Add(-deepreview.DefaultWindow).Format(time.RFC3339)

// TestIntegrationShadowRunReviewsACopyOfTheRealLedger runs the daemon's
// deep-review job over a copy of the real landings ledger: on a temp town
// whose landings file is the copy, the way the daemon reads the host's, in
// bounded runs that carry their cursor from one run bead's notes to the next.
//
// The copy is testdata/deepreview/gastown-ledger.jsonl: the newest thirteen
// records of gastown's ledger. The test reads that file and writes its bytes
// into the town's landings path, so the job reads a copy; the live file is
// never opened. Before changing the copy or the check that holds it to the
// ledger's shape, read the rules it must satisfy: they are stated once, in
// testdata/deepreview/README.md beside it (gt-hd53j).
//
// Both rows take the acceptance reading: every risky landing in the copy is
// reviewed exactly once, in plan order, the cursor moves to the newest one,
// and the runs finish inside deepReviewRunCap. The second row is the ledger a
// crash leaves behind: a torn last record, which the plan counts as unparsed
// rather than refusing the run.
//
// The runs are driven by the job's own cursor and judged against the copy's
// records, not against a partition computed from the job's planner: the risky
// landings come from risk_paths in the copy, and each run's rows must be the
// next whole landed_at groups in ledger order, with a group left for the next
// run only when it does not fit the row bound. A mutant in the risky filter,
// the grouping or the bound fails here instead of being restated by the
// expectations (gt-4ur7s, gt-t2drw).
//
// The job's only write is the plan, in the notes of the bead each run creates:
// the copy and the town's file are byte-identical afterwards, no other bead
// gains a note, and no note carries a line the plan refused (gt-4ur7s).
func TestIntegrationShadowRunReviewsACopyOfTheRealLedger(t *testing.T) {
	t.Parallel()
	fixture := filepath.Join("testdata", "deepreview", "gastown-ledger.jsonl")
	copyBytes, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("reading the ledger copy: %v", err)
	}
	copyInfo, err := os.Stat(fixture)
	if err != nil {
		t.Fatalf("stat of the ledger copy: %v", err)
	}
	deepReviewCheckCopyShape(t, copyBytes)

	// The test needs two runs so the cursor crosses a run-bead note: more risky
	// landings than one run's row bound, in groups the bound can cut between.
	// It is checked on the copy, the ledger the rows below are built from.
	_, copyRisky := deepReviewRecords(t, copyBytes)
	if groups := deepReviewLandingGroups(copyRisky); len(copyRisky) <= deepReviewSliceFiles || groups < 2 {
		t.Fatalf("the copy holds %d risky landings in %d landed_at group(s) at %d rows a run; the test needs two runs so the cursor crosses a run-bead note",
			len(copyRisky), groups, deepReviewSliceFiles)
	}
	// A crash or a short write stops the writer mid-record, and the file ends
	// without a newline: the plan reads that one line as unparsed.
	torn := append(append([]byte{}, copyBytes...), []byte(`{"bead":"gt-torn`)...)

	// The copy's landings are minutes apart, so it never asks the cut to hold a
	// group together. The two grouped rows move two or four of its risky
	// landings onto one instant, so a row-at-a-time cut would split a group and
	// a cut that cannot take a group past the bound would take nothing.
	tests := []struct {
		name string
		// ledger is the town's landings file.
		ledger []byte
	}{
		{name: "the copy", ledger: copyBytes},
		{name: "a torn last record", ledger: torn},
		{name: "a pair the bound cannot fit", ledger: deepReviewGrouped(t, copyBytes, 2, 4)},
		{name: "a head group past the bound", ledger: deepReviewGrouped(t, copyBytes, 0, 4)},
	}
	start := time.Now()
	runsRun := 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeScheduledRunner{now: deepReviewCopyNow}
			e := shadowEntry()
			e.Vars[shadowSliceFilesVar] = strconv.Itoa(deepReviewSliceFiles)
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
			d.config.TownRoot = t.TempDir()
			writeShadowLedger(t, d.config.TownRoot, deepReviewCopyRig, string(tt.ledger))

			// This ledger's own records by bead, and its risky ones in the order
			// the plan must review them: both read from the bytes the job is
			// given, so a reviewed line is judged against the ledger and not
			// against the plan that produced it (gt-t2drw).
			copyRows, risky := deepReviewRecords(t, tt.ledger)

			// The ledger's own counts, as the plan takes them: a line that
			// parses as a landing record is a record, and one that does not is
			// unparsed. Both come from the bytes the test hands the job.
			records, unparsed := deepReviewCounts(t, tt.ledger)

			// The runs are driven by the job's own cursor: each run's note is
			// the next run's cursor, and the loop ends when the copy's risky
			// landings have all been reviewed. A run that reviews none, or more
			// than the plan left, ends it too: either is the cursor failing.
			var runIDs []string
			since := deepReviewCopySince
			taken := 0
			for run := 1; taken < len(risky); run++ {
				if run > len(risky) {
					t.Fatalf("after %d runs, %d of %d risky landings are still unreviewed: the cursor is not advancing",
						run-1, len(risky)-taken, len(risky))
				}
				f.createID = fmt.Sprintf("gt-shadow%d", run)
				runIDs = append(runIDs, f.createID)
				if err := d.runShadowDeepReview(context.Background(), f, e, f.beads, "deep-review 2026-10-27", deepReviewCopyNow); err != nil {
					t.Fatalf("run %d: %v", run, err)
				}
				notes := f.notes[f.createID]
				if len(notes) != 1 {
					t.Fatalf("run %d appended %d notes to %s, want 1", run, len(notes), f.createID)
				}
				note := notes[0]
				head, lines := splitDeepReviewNote(t, note)
				landings, convErr := strconv.Atoi(head["landings="])
				if convErr != nil {
					t.Fatalf("run %d landings= %q is not a count: %v", run, head["landings="], convErr)
				}
				if landings == 0 {
					t.Fatalf("run %d reviewed no landing with %d risky landings left: the cursor moved past them", run, len(risky)-taken)
				}
				if len(lines) != landings+1 || !strings.HasPrefix(lines[len(lines)-1], "cursor: ") {
					t.Fatalf("run %d's note carries %d lines for landings=%d, want the rows and a cursor line", run, len(lines), landings)
				}
				if landings > len(risky)-taken {
					t.Fatalf("run %d reviewed %d landings with %d left: it reviewed past the plan", run, landings, len(risky)-taken)
				}

				// The rows the copy says come next, by bead: the run took the
				// risky landings in ledger order, none skipped and none that
				// carry no risk path.
				var runRows []deepReviewRow
				for k, line := range lines[:landings] {
					var row deepReviewRow
					if err := json.Unmarshal([]byte(line), &row); err != nil {
						t.Fatalf("run %d reviewed line is not a landing record: %v: %q", run, err, line)
					}
					if want := risky[taken+k]; row.Bead != want.Bead {
						t.Fatalf("run %d reviewed %s at position %d, want %s: the plan's order or its risky filter changed",
							run, row.Bead, k+1, want.Bead)
					}
					copied := copyRows[row.Bead]
					if row.Head != copied.Head || row.LandedCommit != copied.LandedCommit ||
						row.LandedAt != copied.LandedAt || strings.Join(row.RiskPaths, "\n") != strings.Join(copied.RiskPaths, "\n") {
						t.Errorf("run %d's line for %s = %+v, want the copy's %+v", run, row.Bead, row, copied)
					}
					runRows = append(runRows, row)
				}

				// The cut is by landed_at group: the next run's window starts
				// strictly after the cursor's instant, so a group split across
				// runs would leave its tail unreviewed, and a whole group that
				// fits the bound is taken rather than left.
				next := taken + len(runRows)
				last := runRows[len(runRows)-1]
				singleGroup := runRows[0].LandedAt == last.LandedAt
				for _, row := range runRows {
					singleGroup = singleGroup && row.LandedAt == last.LandedAt
				}
				if len(runRows) > deepReviewSliceFiles && !singleGroup {
					t.Errorf("run %d reviewed %d landings over the %d-row bound, and they are not one landed_at group: the cut split a group or ignored the bound",
						run, len(runRows), deepReviewSliceFiles)
				}
				if next < len(risky) {
					if risky[next].LandedAt == last.LandedAt {
						t.Fatalf("run %d stopped inside the landed_at group at %s: the next run's window starts after it, so the rest would never be reviewed",
							run, last.LandedAt)
					}
					if fits := len(runRows) + deepReviewGroupSize(risky, next); fits <= deepReviewSliceFiles {
						t.Errorf("run %d reviewed %d landings and left a %d-landing group that fits the %d-row bound whole for the next run",
							run, len(runRows), deepReviewGroupSize(risky, next), deepReviewSliceFiles)
					}
				}

				for key, wantValue := range map[string]string{
					"mode=":               "landings",
					"quiet=":              "false",
					"landings=":           strconv.Itoa(len(runRows)),
					"skipped=":            strconv.Itoa(len(risky) - next),
					"records=":            strconv.Itoa(records),
					"unparsed=":           strconv.Itoa(unparsed),
					"reviewed_until_old=": since,
					"reviewed_until_new=": last.LandedAt,
				} {
					if got := head[key]; got != wantValue {
						t.Errorf("run %d %s%q, want %q", run, key, got, wantValue)
					}
				}
				if lines[len(lines)-1] != "cursor: "+last.LandedAt {
					t.Errorf("run %d cursor line = %q, want %q", run, lines[len(lines)-1], "cursor: "+last.LandedAt)
				}
				since = last.LandedAt
				taken = next

				// The note is the next run's cursor: the job reads it back from
				// the run bead's notes, so the fake's bead must carry what was
				// appended.
				for j := range f.beads {
					if f.beads[j].ID == f.createID {
						f.beads[j].Notes = note
					}
				}
			}

			// The read-only side of the job's write: the ledger is not touched by
			// the runs below, and neither is any bead outside the ones they
			// created. A finished run bead's note is written once and left
			// alone, and no note carries a line the plan refused — the torn row
			// is the one with such a line (gt-4ur7s).
			runIDSet := map[string]bool{}
			for _, id := range runIDs {
				runIDSet[id] = true
			}
			for id, notes := range f.notes {
				if !runIDSet[id] {
					t.Errorf("the job appended a note to %s, a bead it did not create", id)
				}
				if len(notes) != 1 {
					t.Errorf("%s holds %d notes after the runs, want the one its run wrote", id, len(notes))
				}
			}
			for _, refused := range deepReviewUnparsed(tt.ledger) {
				for id, notes := range f.notes {
					for _, note := range notes {
						if strings.Contains(note, refused) {
							t.Errorf("the note on %s carries %q, a line the plan counted as unparsed", id, refused)
						}
					}
				}
			}

			if len(f.created) != len(runIDs) {
				t.Errorf("created %d run beads, want one per run (%d): %v", len(f.created), len(runIDs), f.created)
			}
			if len(f.slungIDs) != 0 || len(f.slung) != 0 {
				t.Errorf("the shadow runs slung %v, want no sling", f.slungIDs)
			}
			if len(f.closedIDs) != len(runIDs) {
				t.Errorf("closed %v, want every run bead closed", f.closedIDs)
			}
			runsRun += len(runIDs)
			for i, why := range f.closedWhy {
				if why != shadowCloseReason {
					t.Errorf("run %d close reason = %q, want %q", i+1, why, shadowCloseReason)
				}
			}

			// A ledger is the landing worker's to write. The town's file must
			// not change under the job.
			ledgerPath, err := shadowLedgerPath(d.config.TownRoot, e)
			if err != nil {
				t.Fatalf("the town's ledger path: %v", err)
			}
			if town, err := os.ReadFile(ledgerPath); err != nil {
				t.Fatalf("reading the town's ledger after the runs: %v", err)
			} else if string(town) != string(tt.ledger) {
				t.Error("the job wrote the town's ledger; it may only read it")
			}
		})
	}
	if elapsed := time.Since(start); elapsed > deepReviewRunCap {
		t.Fatalf("the runs took %v on the ledger copy, over the %v cap; the job, not the cap, has to change",
			elapsed.Round(time.Millisecond), deepReviewRunCap)
	} else {
		t.Logf("%d runs x %d ledgers in %v (cap %v)", runsRun, len(tests), elapsed.Round(time.Millisecond), deepReviewRunCap)
	}

	after, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("reading the ledger copy after the runs: %v", err)
	}
	if string(after) != string(copyBytes) {
		t.Error("the ledger copy changed; the test must read it only")
	}
	if info, err := os.Stat(fixture); err != nil {
		t.Fatalf("stat of the ledger copy after the runs: %v", err)
	} else if !info.ModTime().Equal(copyInfo.ModTime()) {
		t.Errorf("the ledger copy's mtime moved from %v to %v", copyInfo.ModTime(), info.ModTime())
	}
}

// deepReviewCheckCopyShape holds the copy to the shape the landing worker
// writes, so it is a copy of the real ledger and not a reconstruction beside
// it (gt-hd53j). Before changing what it accepts or rejects, read the rules it
// enforces: they are stated once, in testdata/deepreview/README.md beside the
// copy.
func deepReviewCheckCopyShape(t *testing.T, copyBytes []byte) {
	t.Helper()
	var prev land.LandingRecord
	for i, line := range deepReviewLines(copyBytes) {
		var rec land.LandingRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("copy line %d is not a landing record: %v", i+1, err)
		}
		encoded, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("copy line %d: %v", i+1, err)
		}
		if string(encoded) != line {
			t.Errorf("copy line %d is not what the landing worker's encoder writes:\n got: %s\nwant: %s", i+1, line, encoded)
		}
		if rec.Rig != deepReviewCopyRig {
			t.Errorf("copy line %d (bead %s) names rig %q, want %q", i+1, rec.BeadID, rec.Rig, deepReviewCopyRig)
		}
		if i > 0 {
			if rec.Base != prev.LandedCommit {
				t.Errorf("copy line %d (bead %s) bases on %s, want the previous record's landed_commit %s",
					i+1, rec.BeadID, rec.Base, prev.LandedCommit)
			}
			if !rec.LandedAt.After(prev.LandedAt) {
				t.Errorf("copy line %d (bead %s) landed at %s, not after line %d's %s: the copy is not in ledger order",
					i+1, rec.BeadID, rec.LandedAt.Format(time.RFC3339), i, prev.LandedAt.Format(time.RFC3339))
			}
		}
		prev = rec
	}
}

// deepReviewRecords parses a ledger into a row per bead, skipping the lines
// that do not parse (the torn row), and returns its risky rows oldest first,
// ties in ledger order: the landings the plan must review. It reads the
// ledger's own risk_paths, so a plan whose risky filter dropped or added a
// landing is judged against the ledger and not against itself
// (gt-4ur7s, gt-t2drw).
func deepReviewRecords(t *testing.T, ledger []byte) (map[string]deepReviewRow, []deepReviewRow) {
	t.Helper()
	byBead := map[string]deepReviewRow{}
	var order []deepReviewRow
	for _, line := range deepReviewLines(ledger) {
		var row deepReviewRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		byBead[row.Bead] = row
		order = append(order, row)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return order[i].LandedAt < order[j].LandedAt
	})
	var risky []deepReviewRow
	for _, row := range order {
		if len(row.RiskPaths) > 0 {
			risky = append(risky, row)
		}
	}
	return byBead, risky
}

// deepReviewGrouped returns the copy's lines with the risky landings at
// indexes [from, to) moved onto the first of their instants: one landed_at
// group of to-from landings. The copy's own landings are a minute apart, so
// without this no row asks the cut to hold a group together.
func deepReviewGrouped(t *testing.T, copyBytes []byte, from, to int) []byte {
	t.Helper()
	lines := deepReviewLines(copyBytes)
	var riskyIdx []int
	for i, line := range lines {
		var row deepReviewRow
		if json.Unmarshal([]byte(line), &row) == nil && len(row.RiskPaths) > 0 {
			riskyIdx = append(riskyIdx, i)
		}
	}
	if len(riskyIdx) < to {
		t.Fatalf("the copy holds %d risky landings, want at least %d", len(riskyIdx), to)
	}
	var first deepReviewRow
	if err := json.Unmarshal([]byte(lines[riskyIdx[from]]), &first); err != nil {
		t.Fatalf("the copy's risky line %d: %v", riskyIdx[from], err)
	}
	for _, i := range riskyIdx[from:to] {
		var row deepReviewRow
		if err := json.Unmarshal([]byte(lines[i]), &row); err != nil {
			t.Fatalf("the copy's risky line %d: %v", i, err)
		}
		lines[i] = strings.Replace(lines[i], `"landed_at":"`+row.LandedAt+`"`, `"landed_at":"`+first.LandedAt+`"`, 1)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// deepReviewLandingGroups counts the landed_at groups in rows: the landings a
// run takes whole, since the next run's window starts strictly after the
// cursor's instant.
func deepReviewLandingGroups(rows []deepReviewRow) int {
	groups := 0
	for i := 0; i < len(rows); i += deepReviewGroupSize(rows, i) {
		groups++
	}
	return groups
}

// deepReviewGroupSize is how many rows from i landed at rows[i]'s instant.
func deepReviewGroupSize(rows []deepReviewRow, i int) int {
	end := i + 1
	for end < len(rows) && rows[end].LandedAt == rows[i].LandedAt {
		end++
	}
	return end - i
}

// deepReviewLines splits a landings file into its non-blank lines, blank lines
// ignored as both the plan and the landing worker's reader ignore them.
func deepReviewLines(ledger []byte) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(ledger), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// deepReviewCounts is a ledger's records and unparsed lines as the plan reads
// them: a line that parses as a landing record is a record, and one that does
// not is unparsed. Both are read from the bytes the test hands the job, so
// neither count is typed beside the fixture (gt-4ur7s).
func deepReviewCounts(t *testing.T, ledger []byte) (records, unparsed int) {
	t.Helper()
	for _, line := range deepReviewLines(ledger) {
		var row deepReviewRow
		if err := json.Unmarshal([]byte(line), &row); err == nil {
			records++
		} else {
			unparsed++
		}
	}
	return records, unparsed
}

// deepReviewUnparsed returns the ledger lines that do not parse as a landing
// record: the lines the plan counts as unparsed, which a run's note must not
// carry.
func deepReviewUnparsed(ledger []byte) []string {
	var out []string
	for _, line := range deepReviewLines(ledger) {
		var row deepReviewRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			out = append(out, line)
		}
	}
	return out
}

// splitDeepReviewNote splits a run note into its key=value header, keyed by the
// prefix the key is written with (a trailing "=" plus, for the window, "_old="
// or "_new="), and the lines after it. The last of those is the cursor line.
func splitDeepReviewNote(t *testing.T, note string) (map[string]string, []string) {
	t.Helper()
	head := map[string]string{}
	lines := strings.Split(note, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "mode="):
			head["mode="] = strings.TrimPrefix(line, "mode=")
		case strings.HasPrefix(line, "quiet="):
			head["quiet="] = strings.TrimPrefix(line, "quiet=")
		case strings.HasPrefix(line, "landings="):
			head["landings="] = strings.TrimPrefix(line, "landings=")
		case strings.HasPrefix(line, "skipped="):
			head["skipped="] = strings.TrimPrefix(line, "skipped=")
		case strings.HasPrefix(line, "records="):
			head["records="] = strings.TrimPrefix(line, "records=")
		case strings.HasPrefix(line, "unparsed="):
			head["unparsed="] = strings.TrimPrefix(line, "unparsed=")
		case strings.HasPrefix(line, "reviewed_until_old="):
			head["reviewed_until_old="] = strings.TrimPrefix(line, "reviewed_until_old=")
		case strings.HasPrefix(line, "reviewed_until_new="):
			head["reviewed_until_new="] = strings.TrimPrefix(line, "reviewed_until_new=")
		default:
			return head, lines[i:]
		}
	}
	t.Fatalf("the run note carries no reviewed landings: %q", note)
	return head, nil
}
