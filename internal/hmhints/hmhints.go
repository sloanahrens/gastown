// Package hmhints asks history-man for the records closest to a text, so a
// caller can show prior work before a slow test is investigated from scratch
// (gt-kifr7).
package hmhints

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/util"
)

// ErrUnavailable reports that hm is not installed. A caller that finds it in
// the error chain shows no hints rather than failing.
var ErrUnavailable = errors.New("hm is not installed")

// maxTextBytes bounds the query text. hm scores the text as a document, and a
// whole test output would push the words that name the problem out of it.
const maxTextBytes = 400

// Hit is one record hm ranked for the text. The body is dropped because a
// caller shows the title and links the id.
type Hit struct {
	Rank  int     `json:"rank"`
	Score float64 `json:"score"`
	Kind  string  `json:"kind"`
	Rig   string  `json:"rig"`
	ID    string  `json:"id"`
	Title string  `json:"title"`
}

// Runner runs argv as a program and returns its stdout. Tests pass a fake so
// no process starts.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// maxOutputBytes bounds what ExecRunner reads from hm. A runaway hm must not
// grow the caller's memory, so an answer past the bound is an error and never
// a truncated hit list.
const maxOutputBytes = 1 << 20

// maxStderrBytes bounds hm's stderr. An error message is diagnostics, not a
// verdict, so a cut there loses nothing ExecRunner decides on.
const maxStderrBytes = 64 << 10

// execWaitDelay bounds how long Wait waits, after hm exits or ctx ends, for
// hm's output pipes to close, so a child hm leaves holding one cannot block
// the caller past its context (gt-negzu).
const execWaitDelay = time.Second

// errOutputTooLong reports an hm answer past maxOutputBytes.
var errOutputTooLong = errors.New("hmhints: output over the limit")

// ExecRunner runs argv[0] with the rest as arguments without a shell, reading
// at most maxOutputBytes of stdout, and returns at most execWaitDelay after hm
// exits or ctx ends.
func ExecRunner(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("hmhints: ExecRunner called with an empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Its own process group, so a deadline that kills hm also reaches a child
	// it started that holds the output pipe. The cancel is set here rather
	// than by util.SetProcessGroup: that helper waits out
	// util.ProcessGroupKillGrace before escalating, which would put a grace
	// the caller never asked for past its context.
	util.SetDetachedProcessGroup(cmd)
	cancel := realGroupCancel()
	cmd.Cancel = func() error { return cancel.cancel(cmd) }
	cmd.WaitDelay = execWaitDelay
	out := &cappedOutput{limit: maxOutputBytes}
	errOut := &cappedOutput{limit: maxStderrBytes}
	cmd.Stdout = out
	cmd.Stderr = errOut
	return execOutcome(argv[0], out, errOut, cmd.Run())
}

// groupCancel ends cmd's whole process group at once when ctx ends. hm query
// holds nothing a SIGTERM would flush, and waiting out a grace period is time
// the caller spent before its context ended. Its signals are fields so a test
// drives every outcome without a process.
type groupCancel struct {
	killGroup func(pid int, sig syscall.Signal) error // a negative pid is a group
	killPid   func(p *os.Process) error
}

// realGroupCancel is groupCancel wired to the kernel.
func realGroupCancel() groupCancel {
	return groupCancel{
		killGroup: syscall.Kill,
		killPid:   func(p *os.Process) error { return p.Kill() },
	}
}

func (g groupCancel) cancel(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := g.killGroup(-cmd.Process.Pid, syscall.SIGKILL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ESRCH):
		// The group was already empty, which exec.Cmd.Cancel documents as a
		// finished command: returning it keeps a success from being reported
		// as the context's error (gt-7npde).
		return os.ErrProcessDone
	case errors.Is(err, syscall.EPERM):
		// An OpenShell sandbox refuses a group signal with EPERM while
		// allowing a single pid; hm itself is what the caller waits on, so
		// kill it and let WaitDelay cut off a child still holding the pipe.
		return g.killPid(cmd.Process)
	}
	return err
}

// execOutcome turns what Run returned into ExecRunner's result: a cut answer
// is the bound's error, a child still holding the pipe after a successful exit
// is not, because hm's answer was read in full, and an exit error carries hm's
// stderr the way exec.Cmd.Output records it.
func execOutcome(name string, out, errOut *cappedOutput, err error) ([]byte, error) {
	if out.truncated {
		return nil, fmt.Errorf("%w: %s wrote more than %d bytes", errOutputTooLong, name, maxOutputBytes)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitErr.Stderr = errOut.buf.Bytes()
		}
		return nil, err
	}
	return out.buf.Bytes(), nil
}

// cappedOutput keeps the first limit bytes written to it and drops the rest,
// so a command that writes past the bound cannot grow the caller's memory; it
// records the cut so the caller reports an error instead of the cut answer.
type cappedOutput struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (o *cappedOutput) Write(p []byte) (int, error) {
	room := o.limit - o.buf.Len()
	if room >= len(p) {
		o.buf.Write(p)
		return len(p), nil
	}
	o.truncated = true
	if room > 0 {
		o.buf.Write(p[:room])
	}
	return len(p), nil
}

// Query returns at most limit records hm ranked for text, best rank first. run
// nil uses ExecRunner; ctx bounds the call.
func Query(ctx context.Context, run Runner, text string, limit int) ([]Hit, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("hmhints: limit must be positive, got %d", limit)
	}
	if run == nil {
		run = ExecRunner
	}
	out, err := run(ctx, []string{"hm", "query", "--json", "--", clipText(text)})
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		return nil, fmt.Errorf("hm query: %w", err)
	}
	hits, err := parseHits(out)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Rank < hits[j].Rank })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// clipText cuts text to maxTextBytes at a rune boundary, so the query never
// carries a truncated character.
func clipText(text string) string {
	if len(text) <= maxTextBytes {
		return text
	}
	cut := maxTextBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// parseHits decodes hm query --json output, which must be a JSON array. An
// empty or non-array answer is an error, so a broken hm is never read as "no
// prior records".
func parseHits(out []byte) ([]Hit, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, errors.New("hm query: empty output")
	}
	var hits []Hit
	if err := json.Unmarshal(trimmed, &hits); err != nil {
		return nil, fmt.Errorf("hm query: decoding hits: %w", err)
	}
	// A JSON null decodes into a nil slice without error; an empty array
	// decodes into an empty non-nil one.
	if hits == nil {
		return nil, errors.New("hm query: output is not a JSON array")
	}
	return hits, nil
}
