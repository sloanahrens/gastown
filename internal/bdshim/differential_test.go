package bdshim

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdshim/golden"
)

// The differential test replays the goldens the recorder took from the real bd
// (cmd/bdshim/testdata/golden, gt-p85pw.93) through the shim's tracker
// handlers: for every golden row the same argv runs against a fixture tracker
// store, and the shim must answer with the stdout and exit code bd did. A
// handler that drifts from bd turns its row red, and the failure names the row
// and prints the difference (gt-p85pw.118).
//
// The goldens are bd's own answers, so the test never runs bd. A verb the shim
// has no handler for yet is named in knownUnhandledVerbs; a golden row that is
// neither answered nor listed fails, and so does a listed verb that has gained
// a handler, so the list only shrinks.

// differentialRow is one golden row: the file stem the recorder wrote and the
// argv bd answered, in the recorder's own order (cmd/bdshim-record/table.go).
type differentialRow struct {
	stem string
	argv []string
}

// differentialCase is what one differential run reads. Every part is a
// parameter so a test can drive the run with a golden directory and handlers of
// its own.
type differentialCase struct {
	// dir is the golden directory: <stem>.json and <stem>.exit, written by the
	// recorder.
	dir string
	// rows is the argv of every golden, by stem.
	rows []differentialRow
	// handlers is the shim's tracker verb registry.
	handlers map[string]Handler
	// known is the verbs the shim has no handler for yet.
	known map[string]bool
	// diverge is the rows whose shim answer differs from bd's by design, by
	// stem; awaiting is the ones whose golden is not recorded yet.
	diverge  map[string]seatDivergence
	awaiting map[string]bool
	// keep names the ids the fixture file holds, which golden.Normalize leaves
	// as recorded.
	keep map[string]bool
	// run answers one row's argv through the shim and returns what it wrote as
	// bd's stdout and its exit code.
	run func(argv []string) (stdout []byte, exit int, err error)
}

// differentialReport is what one run found.
type differentialReport struct {
	// goldens is how many golden rows the run walked.
	goldens int
	// compared is how many rows a handler answered and the run diffed.
	compared int
	// diverged is how many rows were checked against a listed difference.
	diverged int
	// unhandled is the verbs the run skipped, once each, in golden order.
	unhandled []string
}

// runDifferential walks every golden, runs it through the handler for its verb
// and compares the answer. It reports every failure it finds rather than the
// first, so one red run names every row that drifted.
func runDifferential(c differentialCase) (differentialReport, error) {
	var report differentialReport
	var failures []string

	stems, err := goldenStems(c.dir)
	if err != nil {
		return report, err
	}
	if len(stems) == 0 {
		return report, fmt.Errorf("no goldens in %s: the differential test never passes with zero rows; record them with bdshim-record (gt-p85pw.93)", c.dir)
	}

	rows := make(map[string]differentialRow, len(c.rows))
	for _, row := range c.rows {
		rows[row.stem] = row
	}

	for _, stem := range stems {
		report.goldens++
		want, wantExit, err := readGolden(c.dir, stem)
		if err != nil {
			return report, err
		}
		row, ok := rows[stem]
		if !ok {
			failures = append(failures, fmt.Sprintf("golden %s: no argv in the differential table for this row (add it to differentialRows)", stem))
			continue
		}
		// The registry is keyed by bd's verb, so the row is classified by the
		// same name Main looks up.
		verb := verbOf(row.argv)
		if _, handled := c.handlers[verb]; !handled {
			if !c.known[verb] {
				failures = append(failures, fmt.Sprintf("golden %s: verb %q has no tracker handler and is not in knownUnhandledVerbs", stem, verb))
				continue
			}
			if !slices.Contains(report.unhandled, verb) {
				report.unhandled = append(report.unhandled, verb)
			}
			continue
		}
		got, exit, err := c.run(row.argv)
		if err != nil {
			failures = append(failures, fmt.Sprintf("golden %s: %v", stem, err))
			continue
		}
		if div, ok := c.diverge[stem]; ok {
			// The row differs from bd by design: the shim must give the listed
			// answer, and that answer must still differ from bd's.
			report.diverged++
			got = golden.Normalize(got, c.keep)
			if string(got) != div.stdout || exit != div.exit {
				failures = append(failures, fmt.Sprintf("golden %s: the shim answered exit %d stdout %q, the listed difference is exit %d stdout %q", stem, exit, got, div.exit, div.stdout))
			}
			if bytes.Equal(got, want) && exit == wantExit {
				failures = append(failures, fmt.Sprintf("golden %s: knownSeatDivergences lists the row but the shim now answers as bd does; remove the entry", stem))
			}
			continue
		}
		report.compared++
		// The shim's answer carries the ids and times of this run, and the
		// golden holds the recorder's; normalizing both the same way is what
		// makes the two answers comparable (golden.Normalize).
		got = golden.Normalize(got, c.keep)
		if !bytes.Equal(got, want) {
			failures = append(failures, fmt.Sprintf("golden %s: stdout differs from bd's answer\n%s", stem, lineDiff(want, got)))
		}
		if exit != wantExit {
			failures = append(failures, fmt.Sprintf("golden %s: exit code is %d, want bd's %d", stem, exit, wantExit))
		}
	}

	// A divergence row whose golden is not recorded yet is checked on the shim's
	// side alone; one whose golden exists must leave the awaiting list.
	for _, stem := range slices.Sorted(maps.Keys(c.awaiting)) {
		if slices.Contains(stems, stem) {
			failures = append(failures, fmt.Sprintf("awaitingRecording lists %q, but its golden exists; remove the entry", stem))
			continue
		}
		div, ok := c.diverge[stem]
		row, rowOK := rows[stem]
		if !ok || !rowOK {
			failures = append(failures, fmt.Sprintf("awaitingRecording lists %q, which is not in knownSeatDivergences and differentialRows", stem))
			continue
		}
		got, exit, err := c.run(row.argv)
		if err != nil {
			failures = append(failures, fmt.Sprintf("awaiting %s: %v", stem, err))
			continue
		}
		if got = golden.Normalize(got, c.keep); string(got) != div.stdout || exit != div.exit {
			failures = append(failures, fmt.Sprintf("awaiting %s: the shim answered exit %d stdout %q, the listed difference is exit %d stdout %q", stem, exit, got, div.exit, div.stdout))
		}
	}

	// A verb that has gained a handler must leave the list, whether or not a
	// golden row still names it: the list only shrinks.
	for _, verb := range slices.Sorted(maps.Keys(c.known)) {
		if _, handled := c.handlers[verb]; handled {
			failures = append(failures, fmt.Sprintf("knownUnhandledVerbs lists %q, but the shim has a handler for it; remove the entry", verb))
		}
	}

	if len(failures) > 0 {
		return report, errors.New(strings.Join(failures, "\n\n"))
	}
	return report, nil
}

// goldenStems returns the stem of every golden .json in dir, sorted.
func goldenStems(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the golden directory %s: %w (the goldens are recorded by gt-p85pw.93)", dir, err)
	}
	var stems []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		stems = append(stems, strings.TrimSuffix(name, ".json"))
	}
	slices.Sort(stems)
	return stems, nil
}

// readGolden returns one row's recorded stdout and exit code. A row missing its
// .exit file, or whose .exit is not a number, fails the read: an exit code the
// test cannot compare must not read as a pass.
func readGolden(dir, stem string) ([]byte, int, error) {
	stdout, err := os.ReadFile(filepath.Join(dir, stem+".json"))
	if err != nil {
		return nil, 0, fmt.Errorf("reading golden %s: %w", stem, err)
	}
	exitText, err := os.ReadFile(filepath.Join(dir, stem+".exit"))
	if err != nil {
		return nil, 0, fmt.Errorf("golden %s: reading its .exit: %w", stem, err)
	}
	exit, err := strconv.Atoi(strings.TrimSpace(string(exitText)))
	if err != nil {
		return nil, 0, fmt.Errorf("golden %s: exit file holds %q, not a number: %w", stem, exitText, err)
	}
	return stdout, exit, nil
}

// diffLines bounds how many differing lines a failure prints, so one large
// answer does not bury the row name.
const diffLines = 5

// lineDiff names each line two answers differ at, the golden's line marked -
// and the shim's +, up to diffLines of them.
func lineDiff(want, got []byte) string {
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(string(got), "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		wantLine, gotLine := "", ""
		if i < len(wantLines) {
			wantLine = wantLines[i]
		}
		if i < len(gotLines) {
			gotLine = gotLines[i]
		}
		if wantLine == gotLine {
			continue
		}
		if shown == diffLines {
			fmt.Fprintf(&b, "  ... more differing lines\n")
			break
		}
		fmt.Fprintf(&b, "  line %d:\n  - %s\n  + %s\n", i+1, wantLine, gotLine)
		shown++
	}
	if b.Len() == 0 {
		// The bytes differ but no line does: only the trailing newline does.
		fmt.Fprintf(&b, "  the answers differ after their last line\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// TestDifferentialGoldens replays every committed golden through the shim's
// tracker handlers. It is the test the other differential tests are models of:
// it reads the real golden directory, the real fixture and the real registry.
func TestDifferentialGoldens(t *testing.T) {
	t.Parallel()
	c := newDifferentialCase(t)
	report, err := runDifferential(c)
	if err != nil {
		t.Fatalf("the shim's answers differ from bd's:\n%v", err)
	}
	t.Logf("differential: %d golden rows, %d compared through a handler, %d not yet handled (%s)",
		report.goldens, report.compared, len(report.unhandled), strings.Join(report.unhandled, ", "))
	if report.goldens == 0 {
		t.Fatal("the differential test walked no golden rows")
	}
}

// TestDifferentialRefusesAnUnlistedVerb: a golden row whose verb the shim
// cannot answer and knownUnhandledVerbs does not name is a row that would be
// silently skipped, so the run fails and names it.
func TestDifferentialRefusesAnUnlistedVerb(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeGolden(t, dir, "show-json", `{"id":"fx-1"}`, 0)
	c := differentialCase{
		dir:      dir,
		rows:     []differentialRow{{stem: "show-json", argv: []string{"show", "--json", "fx-1"}}},
		handlers: map[string]Handler{},
		known:    map[string]bool{},
		run: func([]string) ([]byte, int, error) {
			t.Fatal("an unhandled verb was run")
			return nil, 0, nil
		},
	}
	_, err := runDifferential(c)
	if err == nil {
		t.Fatal("runDifferential passed a golden row whose verb has no handler and is not listed")
	}
	if !strings.Contains(err.Error(), "show-json") || !strings.Contains(err.Error(), "knownUnhandledVerbs") {
		t.Errorf("error = %q, want it to name the row and knownUnhandledVerbs", err)
	}
}

// TestDifferentialRefusesAGoldenWithNoArgv: a golden the row table does not
// name is a row the run cannot replay, and skipping it would let a newly
// recorded golden go uncompared.
func TestDifferentialRefusesAGoldenWithNoArgv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeGolden(t, dir, "show-json", `{"id":"fx-1"}`, 0)
	c := differentialCase{
		dir:      dir,
		handlers: map[string]Handler{},
		known:    map[string]bool{},
	}
	_, err := runDifferential(c)
	if err == nil {
		t.Fatal("runDifferential passed a golden with no argv in the differential table")
	}
	if !strings.Contains(err.Error(), "show-json") || !strings.Contains(err.Error(), "differentialRows") {
		t.Errorf("error = %q, want it to name the golden and where its argv belongs", err)
	}
}

// TestDifferentialRefusesABrokenGolden: a row whose exit was not recorded, or
// recorded as something that is not a number, cannot be compared, so the run
// fails instead of reading the exit as zero.
func TestDifferentialRefusesABrokenGolden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		exit    string
		writeIt bool
		want    string
	}{
		{name: "an exit file that is missing", want: ".exit"},
		{name: "an exit file that is not a number", exit: "zero\n", writeIt: true, want: "not a number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "show-json.json"), []byte(`{"id":"fx-1"}`), 0o600); err != nil {
				t.Fatalf("writing the golden: %v", err)
			}
			if tc.writeIt {
				if err := os.WriteFile(filepath.Join(dir, "show-json.exit"), []byte(tc.exit), 0o600); err != nil {
					t.Fatalf("writing the golden's exit: %v", err)
				}
			}
			c := differentialCase{
				dir:      dir,
				rows:     []differentialRow{{stem: "show-json", argv: []string{"show", "--json", "fx-1"}}},
				handlers: map[string]Handler{},
				known:    map[string]bool{},
			}
			_, err := runDifferential(c)
			if err == nil {
				t.Fatal("runDifferential passed a golden with no usable exit code")
			}
			if !strings.Contains(err.Error(), "show-json") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name the golden and hold %q", err, tc.want)
			}
		})
	}
}

// TestDifferentialRefusesAListedVerbWithAHandler: knownUnhandledVerbs only
// shrinks, so an entry for a verb the shim now answers is a stale exemption and
// fails the run.
func TestDifferentialRefusesAListedVerbWithAHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeGolden(t, dir, "show-json", `{"id":"fx-1"}`, 0)
	c := differentialCase{
		dir:      dir,
		rows:     []differentialRow{{stem: "show-json", argv: []string{"show", "--json", "fx-1"}}},
		handlers: map[string]Handler{"show": func(Env, Decision) int { return 0 }},
		known:    map[string]bool{"show": true},
		run: func([]string) ([]byte, int, error) {
			return []byte(`{"id":"fx-1"}`), 0, nil
		},
	}
	_, err := runDifferential(c)
	if err == nil {
		t.Fatal("runDifferential passed a knownUnhandledVerbs entry whose verb has a handler")
	}
	if !strings.Contains(err.Error(), `lists "show"`) || !strings.Contains(err.Error(), "remove the entry") {
		t.Errorf("error = %q, want it to name the stale entry and say to remove it", err)
	}
}

// TestDifferentialNamesADifferingRow: a handler that answers bd's shape with
// one field changed fails the run, which names the row and prints the line that
// differs. The handler runs through Main here, so the test also proves the
// answer reaches the comparison from the shim's own entry point.
func TestDifferentialNamesADifferingRow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := "{\n  \"id\": \"fx-1\",\n  \"priority\": 2\n}\n"
	writeGolden(t, dir, "show-json", want, 0)
	altered := strings.Replace(want, `"priority": 2`, `"priority": 3`, 1)
	handlers := map[string]Handler{
		"show": func(env Env, _ Decision) int {
			if _, err := fmt.Fprint(env.Stdout, altered); err != nil {
				t.Errorf("writing the fake answer: %v", err)
			}
			return 0
		},
	}
	town := newTown(t, routeFixture)
	c := differentialCase{
		dir:      dir,
		rows:     []differentialRow{{stem: "show-json", argv: []string{"show", "--json", "fx-1"}}},
		handlers: handlers,
		known:    map[string]bool{},
		// The fixture holds fx-1, so normalization keeps it and only the
		// altered field is left to differ.
		keep: map[string]bool{"fx-1": true},
		run:  runInShim(t, town, handlers),
	}
	_, err := runDifferential(c)
	if err == nil {
		t.Fatal("runDifferential passed a row whose stdout the handler altered")
	}
	for _, wantText := range []string{"show-json", `-   "priority": 2`, `+   "priority": 3`} {
		if !strings.Contains(err.Error(), wantText) {
			t.Errorf("error = %q, want it to hold %q", err, wantText)
		}
	}
}

// TestDifferentialNamesADifferingExit: an answer bd gave a non-zero status for
// must be answered the same way; an exit code the run cannot compare is a
// failure, not a skip.
func TestDifferentialNamesADifferingExit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeGolden(t, dir, "show-json", `{"id":"fx-1"}`, 1)
	c := differentialCase{
		dir:      dir,
		rows:     []differentialRow{{stem: "show-json", argv: []string{"show", "--json", "fx-1"}}},
		handlers: map[string]Handler{"show": func(Env, Decision) int { return 0 }},
		known:    map[string]bool{},
		run: func([]string) ([]byte, int, error) {
			return []byte(`{"id":"fx-1"}`), 0, nil
		},
	}
	_, err := runDifferential(c)
	if err == nil {
		t.Fatal("runDifferential passed a row whose exit code differs from bd's")
	}
	if !strings.Contains(err.Error(), "show-json") || !strings.Contains(err.Error(), "exit code") {
		t.Errorf("error = %q, want it to name the row and its exit code", err)
	}
}

// TestDifferentialRefusesAnEmptyGoldenDirectory: with no golden there is
// nothing to compare, and a test that passes on zero rows would hide exactly
// the drift it exists to catch.
func TestDifferentialRefusesAnEmptyGoldenDirectory(t *testing.T) {
	t.Parallel()
	c := differentialCase{
		dir:      t.TempDir(),
		handlers: map[string]Handler{},
		known:    map[string]bool{},
	}
	_, err := runDifferential(c)
	if err == nil {
		t.Fatal("runDifferential passed an empty golden directory")
	}
	if !strings.Contains(err.Error(), "gt-p85pw.93") {
		t.Errorf("error = %q, want it to name the bead that records the goldens", err)
	}
}

// writeGolden writes one recorded row where the differential test reads it.
func writeGolden(t *testing.T, dir, stem, stdout string, exit int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(stdout), 0o600); err != nil {
		t.Fatalf("writing golden %s: %v", stem, err)
	}
	if err := os.WriteFile(filepath.Join(dir, stem+".exit"), []byte(strconv.Itoa(exit)+"\n"), 0o600); err != nil {
		t.Fatalf("writing golden %s exit: %v", stem, err)
	}
}

// closeForceCase is a one-row case for the close --force divergence: a golden
// of bd's success (or none), and a close handler that answers as given.
func closeForceCase(t *testing.T, golden bool, answer func(env Env) int, awaiting bool) differentialCase {
	t.Helper()
	dir := t.TempDir()
	const stem = "close-force-reason-fixture-close"
	// Another golden keeps the directory non-empty when this row has none yet.
	writeGolden(t, dir, "show-json", "{}\n", 0)
	if golden {
		writeGolden(t, dir, stem, "closed fx-1\n", 0)
	}
	hs := map[string]Handler{
		"show":  func(env Env, _ Decision) int { _, _ = fmt.Fprint(env.Stdout, "{}\n"); return 0 },
		"close": func(env Env, _ Decision) int { return answer(env) },
	}
	town := newTown(t, routeFixture)
	c := differentialCase{
		dir: dir,
		rows: []differentialRow{
			{stem: "show-json", argv: []string{"show", "--json", "fx-1"}},
			{stem: stem, argv: []string{"close", "--force", "--reason", "fixture-close", "fx-1"}},
		},
		handlers: hs,
		known:    map[string]bool{},
		diverge:  map[string]seatDivergence{stem: {stdout: "", exit: 1}},
		keep:     map[string]bool{"fx-1": true},
		run:      runInShim(t, town, hs),
	}
	if awaiting {
		c.awaiting = map[string]bool{stem: true}
	}
	return c
}

// TestDifferentialAssertsTheSeatDivergence: a row listed in knownSeatDivergences
// must be answered with the listed difference and must still differ from bd's
// golden; an answer that has become bd's, or any other answer, fails (gt-jbetd).
func TestDifferentialAssertsTheSeatDivergence(t *testing.T) {
	t.Parallel()
	refuse := func(env Env) int {
		_, _ = fmt.Fprintln(env.Stderr, "bdshim: flag --force is not implemented")
		return 1
	}
	asBD := func(env Env) int { _, _ = fmt.Fprint(env.Stdout, "closed fx-1\n"); return 0 }
	other := func(env Env) int { _, _ = fmt.Fprint(env.Stdout, "something\n"); return 1 }

	t.Run("the listed difference passes", func(t *testing.T) {
		t.Parallel()
		report, err := runDifferential(closeForceCase(t, true, refuse, false))
		if err != nil || report.diverged != 1 {
			t.Errorf("%s: report %+v err %v, want one diverged row and no failure", t.Name(), report, err)
		}
	})
	t.Run("an answer that became bd's fails", func(t *testing.T) {
		t.Parallel()
		_, err := runDifferential(closeForceCase(t, true, asBD, false))
		if err == nil || !strings.Contains(err.Error(), "close-force-reason-fixture-close") || !strings.Contains(err.Error(), "remove the entry") {
			t.Errorf("%s: error = %v, want one naming the stem and saying to remove the entry", t.Name(), err)
		}
	})
	t.Run("any other answer fails", func(t *testing.T) {
		t.Parallel()
		_, err := runDifferential(closeForceCase(t, true, other, false))
		if err == nil || !strings.Contains(err.Error(), "the listed difference") {
			t.Errorf("%s: error = %v, want one naming the listed difference", t.Name(), err)
		}
	})
}

// TestDifferentialAwaitingRecordingShrinks: a divergence row with no golden yet
// has its shim answer asserted anyway, an entry whose golden exists fails, and
// an entry that is not a divergence row fails.
func TestDifferentialAwaitingRecordingShrinks(t *testing.T) {
	t.Parallel()
	refuse := func(env Env) int { return 1 }
	asBD := func(env Env) int { _, _ = fmt.Fprint(env.Stdout, "closed fx-1\n"); return 0 }

	t.Run("no golden: the shim's answer is asserted", func(t *testing.T) {
		t.Parallel()
		if _, err := runDifferential(closeForceCase(t, false, refuse, true)); err != nil {
			t.Errorf("%s: %v", t.Name(), err)
		}
		_, err := runDifferential(closeForceCase(t, false, asBD, true))
		if err == nil || !strings.Contains(err.Error(), "awaiting close-force-reason-fixture-close") {
			t.Errorf("%s: error = %v, want the awaiting row's wrong shim answer named", t.Name(), err)
		}
	})
	t.Run("a golden that exists fails the entry", func(t *testing.T) {
		t.Parallel()
		_, err := runDifferential(closeForceCase(t, true, refuse, true))
		if err == nil || !strings.Contains(err.Error(), "awaitingRecording lists") || !strings.Contains(err.Error(), "its golden exists") {
			t.Errorf("%s: error = %v, want the entry removed", t.Name(), err)
		}
	})
	t.Run("an entry that is not a divergence row fails", func(t *testing.T) {
		t.Parallel()
		c := closeForceCase(t, false, refuse, true)
		c.diverge = map[string]seatDivergence{}
		_, err := runDifferential(c)
		if err == nil || !strings.Contains(err.Error(), "not in knownSeatDivergences") {
			t.Errorf("%s: error = %v, want the stray entry named", t.Name(), err)
		}
	})
}
