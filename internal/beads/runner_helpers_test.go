package beads

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// recordedCall is one bd call a recorder saw, with the --allow-stale probe
// and the wrapper's own dir/env kept for assertions.
type recordedCall struct {
	bin   string
	dir   string
	env   []string
	args  []string
	stdin []byte
	plain bool
}

// exitError is a scripted bd failure carrying an exit status, as
// *exec.ExitError does.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitError) ExitCode() int { return e.code }

// reply is a recorder's answer to one call.
type reply struct {
	stdout string
	stderr string
	err    error
}

// recorder is a bdRunFunc that records every call and answers from answer.
// The --allow-stale capability probe is answered by allowStale and kept out
// of calls().
type recorder struct {
	mu         sync.Mutex
	recorded   []recordedCall
	probes     int
	allowStale bool
	answer     func(args []string) reply
}

func newRecorder(answer func(args []string) reply) *recorder {
	if answer == nil {
		answer = func([]string) reply { return reply{} }
	}
	return &recorder{answer: answer}
}

func (r *recorder) exec(_ context.Context, c bdCall) ([]byte, []byte, error) {
	r.mu.Lock()
	if len(c.args) == 2 && c.args[0] == "--allow-stale" && c.args[1] == "version" {
		r.probes++
		ok := r.allowStale
		r.mu.Unlock()
		if ok {
			return []byte("bd version 1.2.2\n"), nil, nil
		}
		return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
	}
	r.recorded = append(r.recorded, recordedCall{
		bin:   c.bin,
		dir:   c.dir,
		env:   append([]string(nil), c.env...),
		args:  append([]string(nil), c.args...),
		stdin: c.stdin,
		plain: c.plain,
	})
	answer := r.answer
	r.mu.Unlock()
	rep := answer(c.args)
	return []byte(rep.stdout), []byte(rep.stderr), rep.err
}

func (r *recorder) calls() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCall(nil), r.recorded...)
}

// argvs returns each recorded call's argv joined by spaces.
func (r *recorder) argvs() []string {
	var out []string
	for _, c := range r.calls() {
		out = append(out, strings.Join(c.args, " "))
	}
	return out
}

// newRecordedBeads returns an isolated Beads for dir whose bd calls go to r.
func newRecordedBeads(dir string, r *recorder) *Beads {
	return newBeads(beadsFields{workDir: dir, isolated: true, noRoute: true, exec: r.exec})
}

// lastEnvValue returns the last value of key in env, and whether it was set.
func lastEnvValue(env []string, key string) (string, bool) {
	val, ok := "", false
	for _, kv := range env {
		if k, v, found := strings.Cut(kv, "="); found && k == key {
			val, ok = v, true
		}
	}
	return val, ok
}
