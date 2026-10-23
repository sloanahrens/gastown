package sandbox

import (
	"context"
	"strings"
	"sync"
)

// Fake is a Runner that records argv and answers from tables, so backend and
// doctor tests never start an openshell process. Reply, Err and Stderr are
// keyed by the openshell verb (the first argument after the global flags
// and the optional "sandbox" noun): "create", "get", "exec", "upload",
// "stop", "start", "delete", "logs". An unkeyed call succeeds with empty
// output.
type Fake struct {
	Reply  map[string]string // verb -> stdout
	Err    map[string]error  // verb -> error returned with the call
	Stderr map[string]string // verb -> stderr text returned with the call

	mu    sync.Mutex
	calls [][]string // every argv, program name first
	envs  [][]string // the process env of each call, aligned with calls
}

// Runner returns the Fake as a Runner for CLI.Run.
func (f *Fake) Runner() Runner {
	return func(_ context.Context, argv []string, _ []byte, env []string) ([]byte, []byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, append([]string(nil), argv...))
		f.envs = append(f.envs, append([]string(nil), env...))
		verb := verbOf(argv)
		return []byte(f.Reply[verb]), []byte(f.Stderr[verb]), f.Err[verb]
	}
}

// Calls returns a copy of every argv the Fake has seen, program name first.
func (f *Fake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyRows(f.calls)
}

// Envs returns the process environment of each call, index-aligned with
// Calls, so a test can check a value that was kept off the argv.
func (f *Fake) Envs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyRows(f.envs)
}

func copyRows(in [][]string) [][]string {
	out := make([][]string, len(in))
	for i, row := range in {
		out[i] = append([]string(nil), row...)
	}
	return out
}

// verbs are the openshell subcommands the Fake keys on.
var verbs = map[string]bool{
	"create": true, "get": true, "exec": true, "upload": true,
	"stop": true, "start": true, "delete": true, "logs": true,
}

// valueFlags are the global flags that consume the next argument.
var valueFlags = map[string]bool{"-g": true, "--gateway": true}

// verbOf reports the openshell verb of argv: the first positional argument
// after the program, the leading global flags with their values, and the
// optional "sandbox" noun. It answers "" unless that argument is a known
// verb, so a flag, a flag value or a sandbox name is never keyed as one.
func verbOf(argv []string) string {
	i := 1
	for i < len(argv) && strings.HasPrefix(argv[i], "-") {
		if valueFlags[argv[i]] && i+1 < len(argv) {
			i += 2
			continue
		}
		i++
	}
	if i < len(argv) && argv[i] == "sandbox" {
		i++
	}
	if i < len(argv) && verbs[argv[i]] {
		return argv[i]
	}
	return ""
}
