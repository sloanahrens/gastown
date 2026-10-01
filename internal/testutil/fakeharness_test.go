package testutil

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// mapEnv is an environment in memory, for the harness's own tests.
type mapEnv struct {
	mu sync.Mutex
	m  map[string]string
}

// newMapEnv returns an environment holding the KEY=value entries given.
func newMapEnv(entries ...string) *mapEnv {
	e := &mapEnv{m: map[string]string{}}
	for _, kv := range entries {
		k, v, _ := strings.Cut(kv, "=")
		e.m[k] = v
	}
	return e
}

func (e *mapEnv) LookupEnv(key string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.m[key]
	return v, ok
}

func (e *mapEnv) Setenv(key, value string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.m[key] = value
	return nil
}

func (e *mapEnv) Unsetenv(key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.m, key)
	return nil
}

func (e *mapEnv) Environ() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.m))
	for k, v := range e.m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func (e *mapEnv) get(key string) string {
	v, _ := e.LookupEnv(key)
	return v
}

// fakeHarness is a harnessHost whose outside world is in memory: env, a go
// and tmux on "PATH" that answer from outputs, a working directory outside
// any town, no live tmux server, and a Dolt container that is never there.
// It records every command and the tmux socket the harness binds.
type fakeHarness struct {
	*harnessHost
	env     *mapEnv
	stderr  *bytes.Buffer
	mu      sync.Mutex
	calls   []string
	outputs map[string]fakeOutput // joined argv (tool name first) -> result
	socket  string
}

type fakeOutput struct {
	out string
	err error
}

func newFakeHarness(t *testing.T, env ...string) *fakeHarness {
	t.Helper()
	f := &fakeHarness{env: newMapEnv(env...), stderr: &bytes.Buffer{}, outputs: map[string]fakeOutput{}}
	cwd := t.TempDir()
	f.harnessHost = &harnessHost{
		env:           f.env,
		run:           f.run,
		lookPath:      func(file string) (string, error) { return "/fake/bin/" + file, nil },
		getwd:         func() (string, error) { return cwd, nil },
		findTown:      func() (string, error) { return "", errors.New("not in a workspace") },
		isWorkspace:   func(string) (bool, error) { return false, nil },
		pid:           4242,
		setTmuxSocket: func(s string) { f.mu.Lock(); f.socket = s; f.mu.Unlock() },
		tmuxSocketDir: func() string { return cwd },
		ensureDolt:    func() error { return errors.New("fake: no Docker") },
		terminateDolt: func() error { return nil },
		startUnitTier: func(...unittier.Option) (*unittier.Run, error) { return nil, nil },
		forbidden:     func(string) bool { return false },
		stderr:        f.stderr,
		tempDir:       t.TempDir(),
	}
	return f
}

// answer scripts the result of a command, keyed by its argv joined with
// spaces (the tool as the harness names it, e.g. "/fake/bin/go env GOENV").
func (f *fakeHarness) answer(argv, out string, err error) *fakeHarness {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs[argv] = fakeOutput{out, err}
	return f
}

// noTool makes lookPath fail for file, as when it is not installed.
func (f *fakeHarness) noTool(file string) *fakeHarness {
	look := f.lookPath
	f.lookPath = func(name string) (string, error) {
		if name == file {
			return "", exec.ErrNotFound
		}
		return look(name)
	}
	return f
}

func (f *fakeHarness) run(name string, args ...string) ([]byte, error) {
	argv := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	if o, ok := f.outputs[argv]; ok {
		return []byte(o.out), o.err
	}
	return nil, fmt.Errorf("fake harness: unscripted command %q", argv)
}

func (f *fakeHarness) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
