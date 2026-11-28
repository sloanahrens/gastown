package land

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

type unreadableRange struct {
	Repo
	err error
}

func (r unreadableRange) CommitMessages(string, string) ([]git.CommitMessage, error) {
	return nil, r.err
}

func TestLandReportsAnUnreadableMergeRange(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	open := l.openRepo
	l.openRepo = func(dir string) Repo {
		r := open(dir)
		if filepath.Dir(dir) == f.workRoot {
			return unreadableRange{Repo: r, err: errors.New("git log failed")}
		}
		return r
	}
	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "inspect range" {
		t.Fatalf("Land error = %T %v, want *InfraError at inspect range", err, err)
	}
	f.assertUntouched(t)
}

// panicLanding panics in the merge section's last call, the patch-id read,
// after the landing worktree is added.
type panicLanding struct {
	Repo
	v any
}

func (r panicLanding) PatchID(string, string) (string, error) {
	panic(r.v)
}

func TestLandPanicAfterTheWorktreeAddRemovesIt(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	open := l.openRepo
	var dir string
	sentinel := errors.New("merge section panic")
	l.openRepo = func(d string) Repo {
		r := open(d)
		if filepath.Base(d) == landWorktreeName && filepath.Dir(d) == f.workRoot {
			dir = d
			return panicLanding{Repo: r, v: sentinel}
		}
		return r
	}
	var got any
	func() {
		defer func() { got = recover() }()
		l.Land(context.Background(), f.work)
	}()
	if got != sentinel {
		t.Fatalf("recovered %v, want the merge section's panic unchanged", got)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("landing worktree %s after the panic: stat err = %v, want it removed", dir, err)
	}
}
