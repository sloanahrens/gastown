package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// testEpoch is the fixed instant the package's test clock starts at: never
// time.Now, so a stamp a test writes does not depend on when it runs.
func testEpoch() time.Time {
	return time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
}

// newTestClock returns the package's clock seam: a fake clock at testEpoch
// that moves only when a test advances it, for the steps the plan gives a
// clock.
func newTestClock() *clockwork.FakeClock {
	return clockwork.NewFakeClockAt(testEpoch())
}

// testSleeper is the package's sleeper seam: it records each wait and moves
// the clock past it, so a test of a bounded backoff asserts the waits without
// sleeping.
type testSleeper struct {
	clk   *clockwork.FakeClock
	waits []time.Duration
}

// Sleep records d and advances the clock by it.
func (s *testSleeper) Sleep(d time.Duration) {
	s.waits = append(s.waits, d)
	s.clk.Advance(d)
}

// The fake stands in for the package's one Dolt seam (gt-kkbbb2).
var _ Exec = (*FakeExec)(nil)

// fakeResponse is one scripted answer.
type fakeResponse struct {
	rows Rows
	err  error
}

// FakeExec is an Exec for tests: it records the statements it receives and
// answers them from a script keyed by statement prefix. It is safe for
// concurrent use, so a test may drive the code under test from several
// goroutines with one fake.
type FakeExec struct {
	mu         sync.Mutex
	statements []string
	script     map[string][]fakeResponse
	failIndex  int
	failErr    error
}

// NewFakeExec returns a fake with no script, so every statement it receives
// fails until one is queued.
func NewFakeExec() *FakeExec { return &FakeExec{} }

// Respond queues rows as the answer for statements that start with prefix. It
// panics when a record does not hold one value per column, so a malformed
// script fails the test that wrote it instead of answering a shape the fake
// never promised.
func (f *FakeExec) Respond(prefix string, rows Rows) {
	checkRows(prefix, rows)
	f.queue(prefix, fakeResponse{rows: rows})
}

// RespondErr queues err as the answer for statements that start with prefix.
func (f *FakeExec) RespondErr(prefix string, err error) {
	f.queue(prefix, fakeResponse{err: err})
}

func (f *FakeExec) queue(prefix string, r fakeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.script == nil {
		f.script = make(map[string][]fakeResponse)
	}
	f.script[prefix] = append(f.script[prefix], r)
}

// checkRows panics unless every record holds one value per column, naming the
// prefix and the record so the failure points at the script that broke the
// invariant.
func checkRows(prefix string, rows Rows) {
	for i, record := range rows.Data {
		if len(record) != len(rows.Columns) {
			panic(fmt.Sprintf("fake exec: Respond(%q): record %d has %d values, want %d, one per column %q",
				prefix, i, len(record), len(rows.Columns), rows.Columns))
		}
	}
}

// FailAt makes statement n, counting from 1 over the statements the fake
// receives, fail with err before the script is consulted.
func (f *FakeExec) FailAt(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failIndex = n
	f.failErr = err
}

// Statements returns the statements the fake has received, in order.
func (f *FakeExec) Statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.statements)
}

// Exec records the statement and answers it from the script.
//
// Queued answers for one prefix are used in order, one per call, so a repeated
// statement can be answered differently; the last queued answer repeats for
// every later call. The longest matching prefix wins, and an empty prefix
// answers any statement.
func (f *FakeExec) Exec(ctx context.Context, statement string) (Rows, error) {
	if err := ctx.Err(); err != nil {
		return Rows{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statements = append(f.statements, statement)
	n := len(f.statements)
	if n == f.failIndex {
		return Rows{}, f.failErr
	}
	prefix, ok := f.matchLocked(statement)
	if !ok {
		return Rows{}, fmt.Errorf("fake exec: statement %d has no scripted answer: %q", n, statement)
	}
	queued := f.script[prefix]
	if len(queued) > 1 {
		f.script[prefix] = queued[1:]
	}
	return queued[0].rows, queued[0].err
}

// matchLocked returns the longest scripted prefix that statement starts with.
// The caller holds f.mu.
func (f *FakeExec) matchLocked(statement string) (string, bool) {
	best, found := "", false
	for prefix := range f.script {
		if strings.HasPrefix(statement, prefix) && (!found || len(prefix) > len(best)) {
			best, found = prefix, true
		}
	}
	return best, found
}

func TestFakeExecRecordsAndAnswersByPrefix(t *testing.T) {
	t.Parallel()
	f := NewFakeExec()
	count := Rows{Columns: []string{"n"}, Data: [][]string{{"42"}}}
	names := Rows{Columns: []string{"name"}, Data: [][]string{{"gt"}}}
	f.Respond("SELECT n FROM t", count)
	f.Respond("SHOW TABLES", names)

	statements := []string{
		"SELECT n FROM t",
		"SHOW TABLES",
		"SELECT n FROM t WHERE id = 7",
	}
	want := []Rows{count, names, count}
	for i, statement := range statements {
		got, err := f.Exec(context.Background(), statement)
		if err != nil {
			t.Fatalf("statement %d %q: %v", i+1, statement, err)
		}
		if !reflect.DeepEqual(got, want[i]) {
			t.Errorf("statement %d %q returned %+v, want %+v", i+1, statement, got, want[i])
		}
	}
	if got := f.Statements(); !slices.Equal(got, statements) {
		t.Errorf("Statements() = %q, want %q", got, statements)
	}
}

func TestFakeExecFailsTheNthStatement(t *testing.T) {
	t.Parallel()
	refused := errors.New("server closed the connection")
	for _, failAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("statement_%d", failAt), func(t *testing.T) {
			f := NewFakeExec()
			f.Respond("SELECT", Rows{Columns: []string{"ok"}, Data: [][]string{{"1"}}})
			f.FailAt(failAt, refused)

			for i := 1; i <= 3; i++ {
				_, err := f.Exec(context.Background(), fmt.Sprintf("SELECT %d", i))
				switch {
				case i == failAt && !errors.Is(err, refused):
					t.Fatalf("statement %d returned %v, want the injected error", i, err)
				case i != failAt && err != nil:
					t.Fatalf("statement %d returned %v, want it answered from the script", i, err)
				}
			}
			if got := len(f.Statements()); got != 3 {
				t.Errorf("recorded %d statements, want 3: the failing statement is attempted and recorded too", got)
			}
		})
	}
}

func TestFakeExecAnswersTheSamePrefixDifferentlyOnLaterCalls(t *testing.T) {
	t.Parallel()
	f := NewFakeExec()
	before := Rows{Columns: []string{"state"}, Data: [][]string{{"before"}}}
	after := Rows{Columns: []string{"state"}, Data: [][]string{{"after"}}}
	f.Respond("SELECT state", before)
	f.Respond("SELECT state", after)

	wants := []Rows{before, after, after}
	for i, want := range wants {
		got, err := f.Exec(context.Background(), "SELECT state FROM t")
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("call %d returned %+v, want %+v", i+1, got, want)
		}
	}
}

func TestFakeExecRefusesAnUnscriptedStatement(t *testing.T) {
	t.Parallel()
	f := NewFakeExec()
	f.Respond("SHOW TABLES", Rows{})
	const statement = "DROP TABLE t"
	got, err := f.Exec(context.Background(), statement)
	if err == nil {
		t.Fatalf("%q returned %+v, want an error: an unscripted statement must not answer as healthy", statement, got)
	}
	if !strings.Contains(err.Error(), statement) {
		t.Errorf("error %q does not name the statement %q", err, statement)
	}
}

func TestFakeExecRefusesACancelledContext(t *testing.T) {
	t.Parallel()
	f := NewFakeExec()
	f.Respond("SHOW TABLES", Rows{Columns: []string{"tables"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.Exec(ctx, "SHOW TABLES"); !errors.Is(err, context.Canceled) {
		t.Errorf("Exec on a cancelled context returned %v, want context.Canceled", err)
	}
	if got := f.Statements(); len(got) != 0 {
		t.Errorf("a cancelled call recorded %q, want nothing executed", got)
	}
}

func TestFakeExecAnswersConcurrentCalls(t *testing.T) {
	t.Parallel()
	f := NewFakeExec()
	f.Respond("", Rows{Columns: []string{"n"}, Data: [][]string{{"1"}}})

	const workers, calls = 8, 25
	errs := make(chan error, workers*calls)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < calls; i++ {
				if _, err := f.Exec(context.Background(), fmt.Sprintf("SELECT %d.%d", w, i)); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent call failed: %v", err)
	}

	got := f.Statements()
	if len(got) != workers*calls {
		t.Fatalf("recorded %d statements, want %d", len(got), workers*calls)
	}
	want := make([]string, 0, workers*calls)
	for w := 0; w < workers; w++ {
		for i := 0; i < calls; i++ {
			want = append(want, fmt.Sprintf("SELECT %d.%d", w, i))
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the recorded statements are not the ones sent: got %q", got)
	}
}

func TestFakeExecRefusesRowsThatDoNotMatchTheirColumns(t *testing.T) {
	t.Parallel()
	columns := []string{"id", "name"}
	cases := []struct {
		name string
		rows Rows
	}{
		{"a record one value short", Rows{Columns: columns, Data: [][]string{{"1"}}}},
		{"a record one value too many", Rows{Columns: columns, Data: [][]string{{"1", "gt", "extra"}}}},
		{"a record with no columns at all", Rows{Data: [][]string{{"1"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("Respond accepted rows that do not match their columns: %+v", tc.rows)
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, "SELECT") || !strings.Contains(msg, "record 0") {
					t.Errorf("panic %q does not name the prefix and the record", msg)
				}
			}()
			NewFakeExec().Respond("SELECT", tc.rows)
		})
	}
}

func TestPackageClockStartsAtTheFixedEpochAndMovesOnlyWhenDriven(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	if got, want := clk.Now(), testEpoch(); !got.Equal(want) {
		t.Fatalf("the clock starts at %v, want the fixed epoch %v", got, want)
	}
	clk.Advance(time.Minute)
	if got, want := clk.Now(), testEpoch().Add(time.Minute); !got.Equal(want) {
		t.Errorf("the clock is at %v after a minute, want %v", got, want)
	}
}

func TestPackageSleeperRecordsEachWaitAndMovesTheClock(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	sleep.Sleep(300 * time.Millisecond)
	sleep.Sleep(time.Second)

	if want := []time.Duration{300 * time.Millisecond, time.Second}; !slices.Equal(sleep.waits, want) {
		t.Errorf("recorded waits = %v, want %v", sleep.waits, want)
	}
	if got, want := clk.Now(), testEpoch().Add(1300*time.Millisecond); !got.Equal(want) {
		t.Errorf("the clock is at %v after the two waits, want %v", got, want)
	}
}
