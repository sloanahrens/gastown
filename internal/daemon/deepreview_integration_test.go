//go:build integration

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// to cross two run-bead notes to reach the end. It bounds the job, not the
// expectations: the rows each run reviews are derived from the copy.
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

// deepReviewRunWant is one run's expected note header: the rows it takes from
// the plan, the rows it leaves for the next run, and the window it moves.
type deepReviewRunWant struct {
	rows    int
	skipped int
	since   string
	until   string
}

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
// rather than refusing the run. The expectations — each run's rows, windows
// and landings — are derived from the copy, so a fixture edit moves them
// rather than being typed here twice (gt-4ur7s).
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

	// The copy's own records, by bead and in ledger order: every reviewed line
	// must re-encode the one the copy holds, which is what tells a copy read
	// from a line invented along the way.
	copyRows := map[string]deepReviewRow{}
	copyLine := map[string]string{}
	for _, line := range deepReviewLines(copyBytes) {
		var row deepReviewRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("the ledger copy's line is not a landing record: %v: %q", err, line)
		}
		copyRows[row.Bead] = row
		copyLine[row.Bead] = line
	}

	// The runs the job must produce, from the plan the job builds over the copy
	// and the cut boundLandings makes on it, so neither the risky filter nor the
	// grouping rule is typed here a second time (gt-4ur7s, gt-t2drw). Each run
	// starts where the previous one's cursor landed.
	plan, err := deepreview.BuildPlan("", copyBytes, deepReviewCopyNow)
	if err != nil {
		t.Fatalf("the copy plans the way the job plans it: %v", err)
	}
	want, wantReviewed := deepReviewRunsOfCopy(t, plan.Landings, plan.Since, deepReviewSliceFiles, deepreview.DefaultSliceBytes,
		func(rec deepreview.Record) string { return copyLine[rec.Bead] })
	if len(want) < 2 {
		t.Fatalf("the copy yields %d run(s) at %d rows a run; the test needs two so the cursor crosses a run-bead note",
			len(want), deepReviewSliceFiles)
	}
	// A crash or a short write stops the writer mid-record, and the file ends
	// without a newline: the plan reads that one line as unparsed.
	torn := append(append([]byte{}, copyBytes...), []byte(`{"bead":"gt-torn`)...)

	tests := []struct {
		name string
		// ledger is the town's landings file.
		ledger []byte
	}{
		{name: "the copy", ledger: copyBytes},
		{name: "a torn last record", ledger: torn},
	}
	start := time.Now()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeScheduledRunner{now: deepReviewCopyNow}
			e := shadowEntry()
			e.Vars[shadowSliceFilesVar] = strconv.Itoa(deepReviewSliceFiles)
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
			d.config.TownRoot = t.TempDir()
			writeShadowLedger(t, d.config.TownRoot, deepReviewCopyRig, string(tt.ledger))

			// The ledger's own counts, as the plan takes them: a line that
			// parses as a landing record is a record, and one that does not is
			// unparsed. Both come from the bytes the test hands the job.
			records, unparsed := deepReviewCounts(t, tt.ledger)

			var reviewed, runIDs []string
			for i, w := range want {
				run := i + 1
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
				head, rows := splitDeepReviewNote(t, note)
				for key, wantValue := range map[string]string{
					"mode=":               "landings",
					"quiet=":              "false",
					"landings=":           strconv.Itoa(w.rows),
					"skipped=":            strconv.Itoa(w.skipped),
					"records=":            strconv.Itoa(records),
					"unparsed=":           strconv.Itoa(unparsed),
					"reviewed_until_old=": w.since,
					"reviewed_until_new=": w.until,
				} {
					if got := head[key]; got != wantValue {
						t.Errorf("run %d %s%q, want %q", run, key, got, wantValue)
					}
				}
				if rows[len(rows)-1] != "cursor: "+w.until {
					t.Errorf("run %d cursor line = %q, want %q", run, rows[len(rows)-1], "cursor: "+w.until)
				}
				for _, line := range rows[:len(rows)-1] {
					var row deepReviewRow
					if err := json.Unmarshal([]byte(line), &row); err != nil {
						t.Fatalf("run %d reviewed line is not a landing record: %v: %q", run, err, line)
					}
					reviewed = append(reviewed, row.Bead)
					copied, ok := copyRows[row.Bead]
					if !ok {
						t.Errorf("run %d reviewed %s, which the copy does not hold", run, row.Bead)
						continue
					}
					if row.Head != copied.Head || row.LandedCommit != copied.LandedCommit ||
						row.LandedAt != copied.LandedAt || strings.Join(row.RiskPaths, "\n") != strings.Join(copied.RiskPaths, "\n") {
						t.Errorf("run %d's line for %s = %+v, want the copy's %+v", run, row.Bead, row, copied)
					}
				}
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

			if strings.Join(reviewed, ",") != strings.Join(wantReviewed, ",") {
				t.Errorf("reviewed landings = %v, want %v", reviewed, wantReviewed)
			}
			if len(f.created) != len(want) {
				t.Errorf("created %d run beads, want one per run (%d): %v", len(f.created), len(want), f.created)
			}
			if len(f.slungIDs) != 0 || len(f.slung) != 0 {
				t.Errorf("the shadow runs slung %v, want no sling", f.slungIDs)
			}
			if len(f.closedIDs) != len(want) {
				t.Errorf("closed %v, want every run bead closed", f.closedIDs)
			}
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
		t.Logf("%d runs x %d ledgers in %v (cap %v)", len(want), len(tests), elapsed.Round(time.Millisecond), deepReviewRunCap)
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

// deepReviewRunsOfCopy derives the runs the job must produce from the plan's
// risky landings: each run takes what boundLandings takes of what the previous
// run left, so the grouping and the bounds stay in the job's code rather than
// being typed here a second time (gt-t2drw). lineOf is the copy's own line for
// a record, so the byte bound weighs the bytes the note carries.
func deepReviewRunsOfCopy(t *testing.T, landings []deepreview.Record, since deepreview.Timestamp, maxRows, maxBytes int, lineOf func(deepreview.Record) string) (want []deepReviewRunWant, reviewed []string) {
	t.Helper()
	for taken := 0; taken < len(landings); {
		lines, err := boundLandings(landings[taken:], maxRows, maxBytes, func(rec deepreview.Record) (string, error) {
			return lineOf(rec), nil
		})
		if err != nil {
			t.Fatalf("bounding the landings from %d: %v", taken, err)
		}
		if len(lines) == 0 {
			t.Fatalf("the bound took no landing from %d of %d: that run would never move the cursor", taken, len(landings))
		}
		until := landings[taken+len(lines)-1].LandedAt
		for _, rec := range landings[taken : taken+len(lines)] {
			reviewed = append(reviewed, rec.Bead)
		}
		want = append(want, deepReviewRunWant{
			rows:    len(lines),
			skipped: len(landings) - taken - len(lines),
			since:   since.String(),
			until:   until.String(),
		})
		since = until
		taken += len(lines)
	}
	return want, reviewed
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
