package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The fake stands in for the package's one Dolt seam (gt-kkbbb2).
var _ Exec = (*FakeExec)(nil)

// fakeResponse is one scripted answer.
type fakeResponse struct {
	rows Rows
	err  error
}

// FakeExec is an Exec for tests: it records the statements it receives and
// answers them from a script keyed by statement prefix.
type FakeExec struct {
	statements []string
	script     map[string][]fakeResponse
	failIndex  int
	failErr    error
}

// NewFakeExec returns a fake with no script, so every statement it receives
// fails until one is queued.
func NewFakeExec() *FakeExec { return &FakeExec{} }

// Respond queues rows as the answer for statements that start with prefix.
func (f *FakeExec) Respond(prefix string, rows Rows) {
	f.queue(prefix, fakeResponse{rows: rows})
}

// RespondErr queues err as the answer for statements that start with prefix.
func (f *FakeExec) RespondErr(prefix string, err error) {
	f.queue(prefix, fakeResponse{err: err})
}

func (f *FakeExec) queue(prefix string, r fakeResponse) {
	if f.script == nil {
		f.script = make(map[string][]fakeResponse)
	}
	f.script[prefix] = append(f.script[prefix], r)
}

// FailAt makes statement n, counting from 1 over the statements the fake
// receives, fail with err before the script is consulted.
func (f *FakeExec) FailAt(n int, err error) {
	f.failIndex = n
	f.failErr = err
}

// Statements returns the statements the fake has received, in order.
func (f *FakeExec) Statements() []string {
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
	f.statements = append(f.statements, statement)
	n := len(f.statements)
	if n == f.failIndex {
		return Rows{}, f.failErr
	}
	prefix, ok := f.match(statement)
	if !ok {
		return Rows{}, fmt.Errorf("fake exec: statement %d has no scripted answer: %q", n, statement)
	}
	queued := f.script[prefix]
	if len(queued) > 1 {
		f.script[prefix] = queued[1:]
	}
	return queued[0].rows, queued[0].err
}

// match returns the longest scripted prefix that statement starts with.
func (f *FakeExec) match(statement string) (string, bool) {
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

func TestPackageCommentStatesOneStatementPerCall(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "exec.go", nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		t.Fatalf("parse exec.go: %v", err)
	}
	if file.Doc == nil {
		t.Fatal("exec.go has no package comment")
	}
	if !strings.Contains(file.Doc.Text(), "one statement per call") {
		t.Errorf("package comment = %q, want it to name the one statement per call rule", file.Doc.Text())
	}
}
