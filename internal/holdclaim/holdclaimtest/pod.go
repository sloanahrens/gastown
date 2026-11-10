// Package holdclaimtest is a fake daemon pod for the tests of the packages that
// mirror sentinels into the pod's claim. Exec answers the kubectl calls the
// claim writer makes from a map, so a test reads the claim's files and every
// call it made without a cluster. It imports nothing from holdclaim, so a test
// in the holdclaim package can use it too.
package holdclaimtest

import (
	"context"
	"errors"
	"fmt"
)

// Pod is the claim of a fake daemon pod.
type Pod struct {
	// Files are the claim's files, keyed by their path in the pod.
	Files map[string][]byte
	// Calls are the argv of every kubectl call, in order.
	Calls [][]string
	// Fail maps a command the pod runs (tee, cat, rm) to the error it returns.
	Fail map[string]error
	// Corrupt makes cat return bytes other than the ones written.
	Corrupt bool
}

// New returns an empty pod with no failures.
func New() *Pod {
	return &Pod{Files: map[string][]byte{}, Fail: map[string]error{}}
}

// Exec runs one kubectl call against the pod. It is the function a
// holdclaim.Writer is built with.
func (p *Pod) Exec(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	p.Calls = append(p.Calls, argv)
	// The command the pod runs follows the "--" that ends kubectl's flags.
	sep := -1
	for i, a := range argv {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(argv) {
		return nil, errors.New("holdclaimtest: no command after --")
	}
	command, args := argv[sep+1], argv[sep+2:]
	if err := p.Fail[command]; err != nil {
		return nil, err
	}
	switch command {
	case "tee":
		p.Files[args[0]] = append([]byte(nil), stdin...)
		return nil, nil
	case "cat":
		content, ok := p.Files[args[0]]
		if !ok {
			return nil, fmt.Errorf("cat %s: no such file", args[0])
		}
		if p.Corrupt {
			return []byte("something else"), nil
		}
		return content, nil
	case "rm":
		delete(p.Files, args[len(args)-1])
		return nil, nil
	}
	return nil, fmt.Errorf("holdclaimtest: unexpected pod command %q", command)
}
