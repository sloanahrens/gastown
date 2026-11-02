package hmhints

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// threeHits is hm's answer shape: ranks out of order, with the body Query does
// not keep.
const threeHits = `[
  {"rank":3,"score":0.3,"kind":"bead","rig":"gastown","id":"gt-c","title":"third","body":"b3"},
  {"rank":1,"score":0.9,"kind":"doc","rig":"gastown","id":"doc-a","title":"first","body":"b1"},
  {"rank":2,"score":0.5,"kind":"bead","rig":"gastown","id":"gt-b","title":"second","body":"b2"}
]`

// candy answers with out and err, and records the argv it was handed.
type candy struct {
	out  []byte
	err  error
	argv []string
}

func (c *candy) runner() Runner {
	return func(_ context.Context, argv []string) ([]byte, error) {
		c.argv = argv
		return c.out, c.err
	}
}

func TestQueryReturnsTopHitsByRank(t *testing.T) {
	t.Parallel()
	c := &candy{out: []byte(threeHits)}
	got, err := Query(context.Background(), c.runner(), "slow tests", 2)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := []Hit{
		{Rank: 1, Score: 0.9, Kind: "doc", Rig: "gastown", ID: "doc-a", Title: "first"},
		{Rank: 2, Score: 0.5, Kind: "bead", Rig: "gastown", ID: "gt-b", Title: "second"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Query = %+v, want %+v", got, want)
	}
}

func TestQuerySendsHMQueryArgv(t *testing.T) {
	t.Parallel()
	c := &candy{out: []byte(`[]`)}
	const text = "slow tests keep coming back"
	if _, err := Query(context.Background(), c.runner(), text, 3); err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := []string{"hm", "query", "--json", "--", text}
	if !reflect.DeepEqual(c.argv, want) {
		t.Fatalf("argv = %q, want %q", c.argv, want)
	}
}

func TestQueryClipsLongTextAtARuneBoundary(t *testing.T) {
	t.Parallel()
	// The 400th byte falls inside the two-byte é, so the cut must back up to
	// byte 399.
	text := strings.Repeat("a", 399) + "é" + strings.Repeat("b", 200)
	c := &candy{out: []byte(`[]`)}
	if _, err := Query(context.Background(), c.runner(), text, 3); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(c.argv) != 5 {
		t.Fatalf("argv = %q, want the text as the fifth argument", c.argv)
	}
	sent := c.argv[4]
	if len(sent) > maxTextBytes {
		t.Fatalf("text sent is %d bytes, want at most %d", len(sent), maxTextBytes)
	}
	if !utf8.ValidString(sent) {
		t.Fatalf("text sent is not valid UTF-8: %q", sent)
	}
	if want := strings.Repeat("a", 399); sent != want {
		t.Fatalf("text sent = %q, want %q", sent, want)
	}
}

func TestQueryReturnsErrUnavailableWhenHMMissing(t *testing.T) {
	t.Parallel()
	c := &candy{err: exec.ErrNotFound}
	_, err := Query(context.Background(), c.runner(), "slow tests", 3)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Query error = %v, want ErrUnavailable in its chain", err)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("Query error = %v, want exec.ErrNotFound in its chain", err)
	}
}

func TestQueryWrapsAHMExitError(t *testing.T) {
	t.Parallel()
	exitErr := &exec.ExitError{}
	c := &candy{err: exitErr}
	_, err := Query(context.Background(), c.runner(), "slow tests", 3)
	var got *exec.ExitError
	if !errors.As(err, &got) {
		t.Fatalf("Query error = %v, want the exit error in its chain", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("Query error = %v, want no ErrUnavailable for a run that failed to exit zero", err)
	}
}

func TestQueryRejectsBadOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
	}{
		{name: "empty", out: ""},
		{name: "whitespace only", out: " \n\t"},
		{name: "object", out: `{"hits":[]}`},
		{name: "null", out: `null`},
		{name: "malformed array", out: `[{"rank":1}`},
		{name: "array of non-objects", out: `[1,2]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &candy{out: []byte(tt.out)}
			got, err := Query(context.Background(), c.runner(), "slow tests", 3)
			if err == nil {
				t.Fatalf("Query = %+v, want an error", got)
			}
			if !strings.Contains(err.Error(), "hm query") {
				t.Fatalf("Query error = %v, want one naming hm query", err)
			}
		})
	}
}

func TestQueryWrapsTheDecodeCause(t *testing.T) {
	t.Parallel()
	c := &candy{out: []byte(`[{"rank":1}`)}
	_, err := Query(context.Background(), c.runner(), "slow tests", 3)
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("Query error = %v, want the json syntax error in its chain", err)
	}
}

func TestQueryHonoursTheContextDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	run := func(ctx context.Context, _ []string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("ran with a live context")
	}
	_, err := Query(ctx, run, "slow tests", 3)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Query error = %v, want context.DeadlineExceeded in its chain", err)
	}
}

func TestQueryRejectsANonPositiveLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		c := &candy{out: []byte(`[]`)}
		_, err := Query(context.Background(), c.runner(), "slow tests", limit)
		if err == nil {
			t.Fatalf("Query with limit %d = nil error, want one", limit)
		}
		if c.argv != nil {
			t.Fatalf("Query with limit %d ran hm with %q, want no run", limit, c.argv)
		}
	}
}

func TestExecRunnerRejectsEmptyArgv(t *testing.T) {
	t.Parallel()
	if _, err := ExecRunner(context.Background(), nil); err == nil {
		t.Fatal("ExecRunner with an empty argv = nil error, want one")
	}
}

// TestExecOutcomeMapsRunResults covers what ExecRunner does with Run's
// result: a cut answer is an error, a child holding the pipe after a
// successful exit is not, and another failure is returned. A real process
// behind both paths is TestIntegrationExecRunner's.
func TestExecOutcomeMapsRunResults(t *testing.T) {
	t.Parallel()
	boom := errors.New("hm failed")
	tests := []struct {
		name    string
		limit   int
		written string
		err     error
		want    string
		wantErr error
	}{
		{name: "answer within the bound", limit: 8, written: `[]`, want: `[]`},
		{name: "answer past the bound", limit: 4, written: "abcdefgh", wantErr: errOutputTooLong},
		{name: "child holds the pipe after a successful exit", limit: 8, written: `[]`, err: exec.ErrWaitDelay, want: `[]`},
		{name: "hm fails", limit: 8, err: boom, wantErr: boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := &cappedOutput{limit: tt.limit}
			if _, err := out.Write([]byte(tt.written)); err != nil {
				t.Fatalf("seeding output: %v", err)
			}
			got, err := execOutcome("hm", out, tt.err)
			if !errors.Is(err, tt.wantErr) || (err == nil) != (tt.wantErr == nil) {
				t.Fatalf("execOutcome error = %v, want %v", err, tt.wantErr)
			}
			if string(got) != tt.want {
				t.Fatalf("execOutcome = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestExecRunnerReportsAMissingBinary: a bare name goes through exec's PATH
// lookup, which fails before any process starts, so the unit tier stays
// hermetic. The name is one no PATH entry holds.
func TestExecRunnerReportsAMissingBinary(t *testing.T) {
	t.Parallel()
	_, err := ExecRunner(context.Background(), []string{"gt-hmhints-no-such-binary", "query"})
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("ExecRunner error = %v, want exec.ErrNotFound in its chain", err)
	}
}
