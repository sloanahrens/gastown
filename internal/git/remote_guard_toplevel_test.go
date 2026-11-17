package git

import (
	"errors"
	"strings"
	"testing"
)

// sandboxRunner answers a top-level read in the call's own directory with top,
// and every other call with nothing. It states no rule of its own: the tests
// check the call the remote guard makes, so the form the in-sandbox runner
// accepts is checked against what the guard sends instead of being stated
// twice. With failRead the read fails, as a sandbox that cannot answer; an
// empty top answers the read with an empty path, as a sandbox that could not
// report one.
func sandboxRunner(top string, failRead bool, calls *[]gitCall) runFunc {
	return func(c gitCall) (string, string, error) {
		*calls = append(*calls, c)
		if len(c.args) == 2 && c.args[0] == "rev-parse" && c.args[1] == "--show-toplevel" {
			if failRead {
				return "", "", errors.New("the sandbox could not answer")
			}
			return top + "\n", "", nil
		}
		return "", "", nil
	}
}

// checkTopLevelRead fails unless the run made exactly one top-level read, and
// made it in the clone's own directory with no -C argument: the call the
// in-sandbox runner accepts.
func checkTopLevelRead(t *testing.T, calls []gitCall, clone string) {
	t.Helper()
	var reads []gitCall
	for _, c := range calls {
		if len(c.args) > 0 && c.args[len(c.args)-1] == "--show-toplevel" {
			reads = append(reads, c)
		}
	}
	if len(reads) != 1 {
		t.Fatalf("top-level reads = %d, want 1 (%v)", len(reads), calls)
	}
	read := reads[0]
	if read.dir != clone {
		t.Errorf("top-level read ran in %q, want the clone %q", read.dir, clone)
	}
	if got, want := strings.Join(read.args, " "), "rev-parse --show-toplevel"; got != want {
		t.Errorf("top-level read args = %q, want %q", got, want)
	}
}

// TestRunawayBeadRemoteGuardTopLevelRead: a reset through a remote Git whose
// top level is its clone is allowed, is refused when the top level it reads is
// a configured town root, and is refused when the read fails or reports an
// empty path, which the remote guard refuses rather than pass as checked. Each
// run also checks the guard read the top level in the clone's own directory
// with no -C.
func TestRunawayBeadRemoteGuardTopLevelRead(t *testing.T) {
	t.Parallel()
	const townRoot = "/sandbox/gt"
	const clone = "/sandbox/gt/gastown/polecats/furiosa/gastown"
	tests := []struct {
		name        string
		topLevel    string
		failRead    bool
		wantRefusal string // a substring of the refusal; "" means the reset is allowed
	}{
		{"a clone below the town root allows the reset", clone, false, ""},
		{"the town root itself refuses the reset", townRoot, false, "resolves to town root git worktree"},
		{"a top level the runner cannot read refuses the reset", "", true, "could not read the repository top level"},
		{"an empty top level refuses the reset", "", false, "could not read the repository top level"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []gitCall
			g := NewGitWithRemoteExec(clone, sandboxRunner(tc.topLevel, tc.failRead, &calls), townRoot)
			err := g.ResetHard("origin/main")

			checkTopLevelRead(t, calls, clone)

			if tc.wantRefusal == "" {
				if err != nil {
					t.Fatalf("ResetHard through a remote Git whose top level is its clone: %v", err)
				}
				last := calls[len(calls)-1]
				if last.dir != clone {
					t.Errorf("reset ran in %q, want the clone %q", last.dir, clone)
				}
				if got := strings.Join(last.args, " "); got != "reset --hard origin/main" {
					t.Errorf("last call args = %q, want the reset", got)
				}
				return
			}

			if !errors.Is(err, ErrUnsafeTownRootGitMutation) {
				t.Fatalf("err = %v, want ErrUnsafeTownRootGitMutation", err)
			}
			if !strings.Contains(err.Error(), tc.wantRefusal) {
				t.Errorf("refusal = %q, want it to name the town-root resolution (%q)", err, tc.wantRefusal)
			}
			for _, c := range calls {
				if len(c.args) > 0 && c.args[0] == "reset" {
					t.Errorf("the reset reached the runner (%q) after the guard refused it", c.args)
				}
			}
		})
	}
}

// TestRunawayBeadHostGuardTopLevelCallUnchanged: a host Git still reads the
// top level with the -C form, in a call that names no directory of its own.
func TestRunawayBeadHostGuardTopLevelCallUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var calls []gitCall
	run := func(c gitCall) (string, string, error) {
		calls = append(calls, c)
		if len(c.args) > 0 && c.args[len(c.args)-1] == "--show-toplevel" {
			return dir + "\n", "", nil
		}
		return "", "", nil
	}

	if err := NewGitWithExec(dir, run).ResetHard("origin/main"); err != nil {
		t.Fatalf("ResetHard through the host guard: %v", err)
	}

	want := "-C " + dir + " rev-parse --show-toplevel"
	for _, c := range calls {
		if len(c.args) == 0 || c.args[len(c.args)-1] != "--show-toplevel" {
			continue
		}
		if c.dir != "" {
			t.Errorf("host top-level call dir = %q, want the empty directory", c.dir)
		}
		if got := strings.Join(c.args, " "); got != want {
			t.Errorf("host top-level call args = %q, want %q", got, want)
		}
		return
	}
	t.Fatal("the host guard made no top-level call")
}
