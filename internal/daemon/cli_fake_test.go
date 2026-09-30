package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// cliCall is one subprocess a fakeCLI answered: the program's base name, the
// argv after it, and the directory and environment the daemon built it with.
type cliCall struct {
	name string
	args []string
	dir  string
	env  []string
}

// getenv returns the value of key in the call's environment.
func (c cliCall) getenv(key string) string {
	for _, kv := range slices.Backward(c.env) {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// cliReply is what a fakeCLI answers one call with.
type cliReply struct {
	stdout, stderr string
	code           int // exit status; 0 is success
}

// cliExitError is a fakeCLI call's non-zero exit. It carries its status
// through ExitCode(), the way *exec.ExitError does, and says what exec says.
type cliExitError struct{ code int }

func (e *cliExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *cliExitError) ExitCode() int { return e.code }

// fakeCLI answers the daemon's cmdRunFunc seam in process: it records every
// command and answers each from answer (nil answers every call with success
// and no output). Nothing is started.
type fakeCLI struct {
	answer func(c cliCall) cliReply

	mu    sync.Mutex
	calls []cliCall
}

// newFakeCLI returns a fakeCLI that answers from argv alone.
func newFakeCLI(answer func(args []string) cliReply) *fakeCLI {
	if answer == nil {
		return &fakeCLI{}
	}
	return &fakeCLI{answer: func(c cliCall) cliReply { return answer(c.args) }}
}

// newFakeCLIFor returns a fakeCLI whose answers can read the whole call,
// such as the BEADS_DIR a bd call was routed with.
func newFakeCLIFor(answer func(c cliCall) cliReply) *fakeCLI {
	return &fakeCLI{answer: answer}
}

// run is the cmdRunFunc. Output the caller wired to its own writers is
// written there too, as a real run would.
func (f *fakeCLI) run(cmd *exec.Cmd) ([]byte, []byte, error) {
	c := cliCall{name: filepath.Base(cmd.Path), dir: cmd.Dir, env: slices.Clone(cmd.Env)}
	if len(cmd.Args) > 1 {
		c.args = slices.Clone(cmd.Args[1:])
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	var r cliReply
	if f.answer != nil {
		r = f.answer(c)
	}
	if cmd.Stdout != nil {
		_, _ = cmd.Stdout.Write([]byte(r.stdout))
	}
	if cmd.Stderr != nil {
		_, _ = cmd.Stderr.Write([]byte(r.stderr))
	}
	var err error
	if r.code != 0 {
		err = &cliExitError{code: r.code}
	}
	return []byte(r.stdout), []byte(r.stderr), err
}

// recorded returns every call answered so far, in call order.
func (f *fakeCLI) recorded() []cliCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// argvs returns the args of every recorded call whose argv starts with
// prefix, in call order.
func (f *fakeCLI) argvs(prefix ...string) [][]string {
	var out [][]string
	for _, c := range f.recorded() {
		if len(c.args) >= len(prefix) && slices.Equal(c.args[:len(prefix)], prefix) {
			out = append(out, c.args)
		}
	}
	return out
}

// cliBySub answers a fakeCLI by subcommand: the key is the argv's first word,
// or its first two joined by a space ("convoy stranded"), the longer match
// winning. An argv with no entry succeeds with no output.
func cliBySub(replies map[string]cliReply) func(args []string) cliReply {
	return func(args []string) cliReply {
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
		return cliReply{}
	}
}

func TestFakeCLIRecordsAndAnswers(t *testing.T) {
	t.Parallel()
	f := newFakeCLI(cliBySub(map[string]cliReply{
		"convoy stranded": {stdout: "[]"},
		"sling":           {stderr: "boom", code: 2},
	}))
	cmd := exec.CommandContext(context.Background(), "/opt/bin/gt", "convoy", "stranded", "--json")
	cmd.Dir = "/town"
	cmd.Env = []string{"BEADS_DIR=/a", "BEADS_DIR=/town/.beads"}
	out, _, err := f.run(cmd)
	if err != nil || string(out) != "[]" {
		t.Fatalf("convoy stranded = %q, %v; want [] and success", out, err)
	}
	var stderr strings.Builder
	cmd = exec.CommandContext(context.Background(), "gt", "sling", "gt-1")
	cmd.Stderr = &stderr
	if _, _, err = f.run(cmd); err == nil || stderr.String() != "boom" {
		t.Fatalf("sling = %q, %v; want boom written to the caller's stderr, and a failure", stderr.String(), err)
	}
	if e, ok := err.(interface{ ExitCode() int }); !ok || e.ExitCode() != 2 {
		t.Fatalf("sling error %v (%T) does not carry exit status 2", err, err)
	}
	if got := f.argvs("sling"); len(got) != 1 || !slices.Equal(got[0], []string{"sling", "gt-1"}) {
		t.Fatalf("argvs(sling) = %v", got)
	}
	first := f.recorded()[0]
	if first.name != "gt" || first.dir != "/town" || first.getenv("BEADS_DIR") != "/town/.beads" {
		t.Fatalf("first call = %+v; want gt in /town, the last BEADS_DIR winning", first)
	}
}
