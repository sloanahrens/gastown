package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// gitReply is what a scripted sandbox exec answers one git command with.
type gitReply struct {
	stdout string
	stderr string
	err    error
}

// scriptedGit answers the git commands a stop runs in its clone, keyed by the
// git arguments after "git -C <dir>". A key with several replies gives them in
// order, and its last reply repeats. Every argv is recorded.
type scriptedGit struct {
	replies map[string][]gitReply
	calls   [][]string
}

// run is the sandbox CLI's runner: it finds the git command after the "--" of
// the openshell exec argv and answers it from the script.
func (s *scriptedGit) run(_ context.Context, argv []string, _ []byte, _ []string) ([]byte, []byte, error) {
	s.calls = append(s.calls, append([]string(nil), argv...))
	at := -1
	for i, arg := range argv {
		if arg == "--" {
			at = i
			break
		}
	}
	if at < 0 || at+4 > len(argv) || argv[at+1] != "git" || argv[at+2] != "-C" {
		return nil, nil, fmt.Errorf("unexpected exec argv %v", argv)
	}
	key := strings.Join(argv[at+4:], " ")
	list := s.replies[key]
	if len(list) == 0 {
		return nil, nil, fmt.Errorf("unscripted git command %q", key)
	}
	r := list[0]
	if len(list) > 1 {
		s.replies[key] = list[1:]
	}
	return []byte(r.stdout), []byte(r.stderr), r.err
}

// verbCalls is how many recorded exec calls ran git with verb as its command.
func (s *scriptedGit) verbCalls(verb string) int {
	n := 0
	for _, argv := range s.calls {
		for i, arg := range argv {
			if arg == "git" && i+3 < len(argv) && argv[i+3] == verb {
				n++
				break
			}
		}
	}
	return n
}

// scriptedStopBackend is an openshell backend whose sandbox exec answers from
// the script.
func scriptedStopBackend(t *testing.T, replies map[string][]gitReply) (*openshellBackend, *scriptedGit) {
	t.Helper()
	s := &scriptedGit{replies: replies}
	b, _ := opsBackend(t, &sandbox.Fake{}, newSandboxProbe(t, false))
	b.cli = &sandbox.CLI{Run: s.run}
	return b, s
}

// refusedPushReply is git's refusal of a non-fast-forward push. Its stdout
// carries a line of the remote's own, so a test sees both streams.
var refusedPushReply = gitReply{
	stdout: "remote: hook says the branch is behind",
	stderr: "To http://control-plane.openshell.svc.cluster.local:3000/sloan/gastown.git\n ! [rejected]        b -> b (non-fast-forward)\nerror: failed to push some refs",
	err:    errors.New("exit status 1"),
}

// A refused push whose origin head holds the clone's commits loses nothing, so
// the stop goes on.
func TestOpenShellOpsPushWorkRemoteAheadLetsTheStopProceed(t *testing.T) {
	t.Parallel()
	b, s := scriptedStopBackend(t, map[string][]gitReply{
		"push origin b":                   {refusedPushReply},
		"fetch origin b":                  {{}},
		"rev-parse --verify refs/heads/b": {{stdout: "aaa\n"}},
		"rev-parse --verify FETCH_HEAD":   {{stdout: "bbb\n"}},
		"merge-base aaa bbb":              {{stdout: "aaa\n"}},
	})

	if err := b.pushWork(pushRec("b")); err != nil {
		t.Fatalf("pushWork with origin ahead = %v, want nil: the stop goes on", err)
	}
	if n := s.verbCalls("push"); n != 1 {
		t.Errorf("push calls = %d, want the refused push only", n)
	}
}

// A refused push whose origin head is ahead of the clone's, with the clone
// holding no commits origin lacks, is refused by the plain push again: the
// refusal was stale, and the push goes through.
func TestOpenShellOpsPushWorkStaleRefusalPushesAgain(t *testing.T) {
	t.Parallel()
	b, s := scriptedStopBackend(t, map[string][]gitReply{
		"push origin b":                   {refusedPushReply, {}},
		"fetch origin b":                  {{}},
		"rev-parse --verify refs/heads/b": {{stdout: "aaa\n"}},
		"rev-parse --verify FETCH_HEAD":   {{stdout: "bbb\n"}},
		"merge-base aaa bbb":              {{stdout: "bbb\n"}},
		"merge-base bbb aaa":              {{stdout: "bbb\n"}},
	})

	if err := b.pushWork(pushRec("b")); err != nil {
		t.Fatalf("pushWork with a stale refusal = %v, want the plain push to go through", err)
	}
	if n := s.verbCalls("push"); n != 2 {
		t.Errorf("push calls = %d, want the refused push and one plain retry", n)
	}
}

// A diverged branch keeps the sandbox. Its error names the branch, both heads,
// the merge-base and the counts, and carries git's refusal and stdout on one
// line. Nothing is forced, and the push is not retried.
func TestOpenShellOpsPushWorkDivergedKeepsTheSandbox(t *testing.T) {
	t.Parallel()
	b, s := scriptedStopBackend(t, map[string][]gitReply{
		"push origin b":                   {refusedPushReply},
		"fetch origin b":                  {{}},
		"rev-parse --verify refs/heads/b": {{stdout: "aaa\n"}},
		"rev-parse --verify FETCH_HEAD":   {{stdout: "bbb\n"}},
		"merge-base aaa bbb":              {{stdout: "mmm\n"}},
		"merge-base bbb aaa":              {{stdout: "mmm\n"}},
		"rev-list --count bbb..aaa":       {{stdout: "2\n"}},
		"rev-list --count aaa..bbb":       {{stdout: "1\n"}},
	})

	err := b.pushWork(pushRec("b"))
	if err == nil {
		t.Fatal("pushWork of a diverged branch = nil, want an error so the sandbox is kept")
	}
	if errors.Is(err, ErrNothingToPush) {
		t.Fatalf("pushWork of a diverged branch = %v: a sandbox that is kept is not nothing to push", err)
	}
	for _, want := range []string{
		"branch b diverged from origin", "local aaa", "origin bbb", "merge-base mmm",
		"local holds 2 commits origin lacks", "origin holds 1 local lacks",
		"! [rejected]        b -> b (non-fast-forward)", "error: failed to push some refs",
		"remote: hook says the branch is behind", "nothing is forced",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diverged error = %q, want it to name %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("diverged error spans lines: %q", err.Error())
	}
	if n := s.verbCalls("push"); n != 1 {
		t.Errorf("push calls = %d, want the refused push only", n)
	}
	for _, argv := range s.calls {
		for _, arg := range argv {
			if strings.Contains(arg, "force") {
				t.Errorf("exec argv %v carries a force option", argv)
			}
		}
	}
}

// When the clone cannot fetch origin's head, the refused push stays the error:
// the stop keeps the sandbox and says why the head could not be compared.
func TestOpenShellOpsPushWorkFetchFailureKeepsTheRefusal(t *testing.T) {
	t.Parallel()
	b, _ := scriptedStopBackend(t, map[string][]gitReply{
		"push origin b":  {refusedPushReply},
		"fetch origin b": {{err: errors.New("exit status 128"), stderr: "fatal: unable to access the remote"}},
	})

	err := b.pushWork(pushRec("b"))
	if err == nil {
		t.Fatal("pushWork with an unreadable origin head = nil, want the refusal kept")
	}
	for _, want := range []string{"[rejected]", "fetching origin's head to compare failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error spans lines: %q", err.Error())
	}
}
