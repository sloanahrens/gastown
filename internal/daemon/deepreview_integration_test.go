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
// records of gastown's ledger, rebuilt from the landing records the worker
// wrote (see the README beside it). The test reads that file and writes its
// bytes into the town's landings path, so the job reads a copy; the live file
// is never opened. The copy is left byte-identical, and the town's file is
// too: the job reads a ledger and writes only bead notes.
//
// Both rows take the acceptance reading: every risky landing in the copy is
// reviewed exactly once, in plan order, the cursor moves to the newest one,
// and the runs finish inside deepReviewRunCap. The second row is the ledger a
// crash leaves behind: a torn last record, which the plan counts as unparsed
// rather than refusing the run.
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
	// The copy's own records, by bead: every reviewed line must re-encode the
	// one the copy holds, which is what tells a copy read from a line invented
	// along the way.
	copyRows := map[string]deepReviewRow{}
	for _, line := range strings.Split(strings.TrimSuffix(string(copyBytes), "\n"), "\n") {
		var row deepReviewRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("the ledger copy's line is not a landing record: %v: %q", err, line)
		}
		copyRows[row.Bead] = row
	}

	// Three rows a run, so the copy's eight risky landings take three runs and
	// the cursor has to cross two notes to reach the end.
	want := []deepReviewRunWant{
		{rows: 3, skipped: 5, since: "2026-10-26T12:00:00Z", until: "2026-10-27T04:29:57Z"},
		{rows: 3, skipped: 2, since: "2026-10-27T04:29:57Z", until: "2026-10-27T06:43:22Z"},
		{rows: 2, skipped: 0, since: "2026-10-27T06:43:22Z", until: "2026-10-27T07:33:50Z"},
	}
	wantReviewed := []string{
		"gt-b9j3o.6", "gt-b9j3o.8", "gt-uxqb9",
		"gt-l3zak", "gt-lh0sc", "gt-pyxj0",
		"gt-p85pw.13", "gt-37ne7",
	}
	// A crash or a short write stops the writer mid-record, and the file ends
	// without a newline: the plan reads that one line as unparsed.
	torn := append(append([]byte{}, copyBytes...), []byte(`{"bead":"gt-torn`)...)

	tests := []struct {
		name string
		// ledger is the town's landings file.
		ledger            []byte
		records, unparsed string
	}{
		{name: "the copy", ledger: copyBytes, records: "13", unparsed: "0"},
		{name: "a torn last record", ledger: torn, records: "13", unparsed: "1"},
	}
	start := time.Now()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeScheduledRunner{now: deepReviewCopyNow}
			e := shadowEntry()
			e.Vars[shadowSliceFilesVar] = "3"
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
			d.config.TownRoot = t.TempDir()
			writeShadowLedger(t, d.config.TownRoot, deepReviewCopyRig, string(tt.ledger))

			var reviewed []string
			for i, w := range want {
				run := i + 1
				f.createID = fmt.Sprintf("gt-shadow%d", run)
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
					"records=":            tt.records,
					"unparsed=":           tt.unparsed,
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
