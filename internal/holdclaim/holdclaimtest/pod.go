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
	// Contexts are the contexts the calls were made with, one per call.
	Contexts []context.Context
	// Fail maps a command the pod runs (tee, cat, rm, test, find) to the error it returns.
	Fail map[string]error
	// Down makes every call fail as a pod that does not answer does.
	Down bool
	// Corrupt makes cat return bytes other than the ones written.
	Corrupt bool
	// KeepOnRemove makes rm report success and leave the file in the claim.
	KeepOnRemove bool
	// ProbeFail, when set, is the error the removal read-back (the find of the
	// claim file's name) returns in place of the listing.
	ProbeFail error
}

// ExitError is the error a pod call returns when the command ran and exited
// non-zero, as kubectl exec reports the exit of the command it ran.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("command exited %d", e.Code)
}

// ExitCode is the status the command exited with.
func (e *ExitError) ExitCode() int {
	return e.Code
}

// New returns an empty pod with no failures.
func New() *Pod {
	return &Pod{Files: map[string][]byte{}, Fail: map[string]error{}}
}

// Exec runs one kubectl call against the pod. It is the function a
// holdclaim.Writer is built with.
func (p *Pod) Exec(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	p.Calls = append(p.Calls, argv)
	p.Contexts = append(p.Contexts, ctx)
	if p.Down {
		return nil, errors.New("unable to connect to the server")
	}
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
		if !p.KeepOnRemove {
			delete(p.Files, args[len(args)-1])
		}
		return nil, nil
	case "test":
		return p.test(args)
	case "find":
		return p.find(args)
	}
	return nil, fmt.Errorf("holdclaimtest: unexpected pod command %q", command)
}

// test answers the one test the writer makes: -d asks whether the claim mount
// is there.
func (p *Pod) test(args []string) ([]byte, error) {
	if len(args) != 2 || args[0] != "-d" {
		return nil, fmt.Errorf("holdclaimtest: unexpected test call %q", args)
	}
	return nil, nil
}

// find answers the removal read-back, "find DIR -maxdepth 1 -name NAME": the
// path of the claim file when it is present, nothing when it is not, and exit 0
// either way, as find does.
func (p *Pod) find(args []string) ([]byte, error) {
	if len(args) != 5 || args[1] != "-maxdepth" || args[2] != "1" || args[3] != "-name" {
		return nil, fmt.Errorf("holdclaimtest: unexpected find call %q", args)
	}
	if p.ProbeFail != nil {
		return nil, p.ProbeFail
	}
	full := args[0] + "/" + args[4]
	if _, ok := p.Files[full]; ok {
		return []byte(full + "\n"), nil
	}
	return nil, nil
}
