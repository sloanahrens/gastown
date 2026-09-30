package daemon

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// gtReply is what a fakeGt answers one call with.
type gtReply struct {
	stdout, stderr string
	code           int // exit status; 0 is success
}

// gtExitError is a fakeGt call's non-zero exit. It carries its status through
// ExitCode(), the way *exec.ExitError does, and says what exec says.
type gtExitError struct{ code int }

func (e *gtExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *gtExitError) ExitCode() int { return e.code }

// fakeGt is an in-process gt for the daemon's gtRunFunc seam: it records
// every call and answers each from answer (nil answers every call with
// success and no output).
type fakeGt struct {
	answer func(args []string) gtReply

	mu    sync.Mutex
	calls []gtCall
}

func newFakeGt(answer func(args []string) gtReply) *fakeGt {
	return &fakeGt{answer: answer}
}

// run is the gtRunFunc. Like exec.CommandContext, a context already done
// fails the call before anything runs.
func (f *fakeGt) run(ctx context.Context, _ string, c gtCall) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, gtCall{dir: c.dir, env: slices.Clone(c.env), args: slices.Clone(c.args)})
	f.mu.Unlock()
	var r gtReply
	if f.answer != nil {
		r = f.answer(slices.Clone(c.args))
	}
	var err error
	if r.code != 0 {
		err = &gtExitError{code: r.code}
	}
	return []byte(r.stdout), []byte(r.stderr), err
}

// argvs returns the args of every recorded call whose argv starts with
// prefix, in call order.
func (f *fakeGt) argvs(prefix ...string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c.args) >= len(prefix) && slices.Equal(c.args[:len(prefix)], prefix) {
			out = append(out, c.args)
		}
	}
	return out
}

// gtBySub answers a fakeGt by subcommand: the key is the argv's first word,
// or its first two joined by a space ("convoy stranded"), the longer match
// winning. An argv with no entry succeeds with no output.
func gtBySub(replies map[string]gtReply) func(args []string) gtReply {
	return func(args []string) gtReply {
		if len(args) >= 2 {
			if r, ok := replies[args[0]+" "+args[1]]; ok {
				return r
			}
		}
		if len(args) >= 1 {
			if r, ok := replies[args[0]]; ok {
				return r
			}
		}
		return gtReply{}
	}
}

func TestFakeGtRecordsAndAnswers(t *testing.T) {
	t.Parallel()
	f := newFakeGt(gtBySub(map[string]gtReply{
		"convoy stranded": {stdout: "[]"},
		"sling":           {stderr: "boom", code: 2},
	}))
	out, _, err := f.run(context.Background(), "gt", gtCall{args: []string{"convoy", "stranded", "--json"}})
	if err != nil || string(out) != "[]" {
		t.Fatalf("convoy stranded = %q, %v; want [] and success", out, err)
	}
	_, stderr, err := f.run(context.Background(), "gt", gtCall{args: []string{"sling", "gt-1"}})
	if err == nil || string(stderr) != "boom" {
		t.Fatalf("sling = %q, %v; want boom and a failure", stderr, err)
	}
	if e, ok := err.(interface{ ExitCode() int }); !ok || e.ExitCode() != 2 {
		t.Fatalf("sling error %v (%T) does not carry exit status 2", err, err)
	}
	if got := f.argvs("sling"); len(got) != 1 || !slices.Equal(got[0], []string{"sling", "gt-1"}) {
		t.Fatalf("argvs(sling) = %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := f.run(ctx, "gt", gtCall{args: []string{"sling"}}); err == nil {
		t.Fatal("a call under a done context succeeded")
	}
	if n := len(f.argvs()); n != 2 {
		t.Fatalf("recorded %d calls, want 2 (the cancelled one never ran)", n)
	}
}
