package sandbox

import (
	"context"
	"sync"
)

// Fake is a Runner that records argv and answers from tables, so backend and
// doctor tests never start an openshell process. Reply, Err and Stderr are
// keyed by the openshell verb (the first argument after the global flags
// and the optional "sandbox" noun): "create", "get", "exec", "upload",
// "stop", "start", "delete", "logs". An unkeyed call succeeds with empty
// output.
type Fake struct {
	Calls  [][]string        // every argv, program name first
	Reply  map[string]string // verb -> stdout
	Err    map[string]error  // verb -> error returned with the call
	Stderr map[string]string // verb -> stderr text returned with the call

	mu sync.Mutex
}

// Runner returns the Fake as a Runner for CLI.Run.
func (f *Fake) Runner() Runner {
	return func(_ context.Context, argv []string, _ []byte) ([]byte, []byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.Calls = append(f.Calls, append([]string(nil), argv...))
		verb := verbOf(argv)
		return []byte(f.Reply[verb]), []byte(f.Stderr[verb]), f.Err[verb]
	}
}

// verbOf skips the program, `-g <gateway>` and the `sandbox` noun.
func verbOf(argv []string) string {
	i := 1
	if i+1 < len(argv) && argv[i] == "-g" {
		i += 2
	}
	if i < len(argv) && argv[i] == "sandbox" {
		i++
	}
	if i < len(argv) {
		return argv[i]
	}
	return ""
}
