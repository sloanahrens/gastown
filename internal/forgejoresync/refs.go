package forgejoresync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// Ref is one branch or tag of a repository and the sha it points at.
type Ref struct {
	Name string // refs/heads/<branch> or refs/tags/<tag>
	SHA  string
}

// Refspecs are the two refspecs the mirror is fetched and pushed with: every
// branch and every tag, forced, so a ref at another sha takes GitHub's.
var Refspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// ParseRefs reads an ls-remote listing, one "<sha> <ref>" line per ref, and returns
// the refs sorted by name in byte order: the script's LC_ALL=C sort. A line
// that is not two fields is an error, not a ref to skip: a dropped line would
// make the plan delete a ref GitHub still holds. The error names the field
// count, not the line, since the listing is captured output.
func ParseRefs(out []byte) ([]Ref, error) {
	var refs []Ref
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("ls-remote line has %d fields, want 2", len(fields))
		}
		refs = append(refs, Ref{Name: fields[1], SHA: fields[0]})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// Plan says what a pruning push from GitHub's refs to Forgejo's will do. push
// names every GitHub ref Forgejo lacks or holds at another sha; del names every
// Forgejo ref GitHub lacks. A ref at another sha is pushed, not deleted.
func Plan(github, forgejo []Ref) (push, del []string) {
	have := make(map[string]string, len(forgejo))
	for _, r := range forgejo {
		have[r.Name] = r.SHA
	}
	want := make(map[string]bool, len(github))
	for _, r := range github {
		want[r.Name] = true
		if sha, ok := have[r.Name]; !ok || sha != r.SHA {
			push = append(push, r.Name)
		}
	}
	for _, r := range forgejo {
		if !want[r.Name] {
			del = append(del, r.Name)
		}
	}
	return push, del
}

// Equal reports whether a and b hold the same refs at the same shas. Both come
// from ParseRefs, so both are sorted and a positional compare is a set compare.
func Equal(a, b []Ref) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Git is the set of git operations the port needs. ExecGit runs git; tests
// inject a fake.
type Git interface {
	LsRemote(url string) ([]byte, error)
	InitBare(dir string) error
	Fetch(dir, url string, refspecs ...string) error
	PushPrune(dir, url string, refspecs ...string) error
}

// Runner runs argv, argv[0] being the program, and returns its stdout. ctx is the
// run's context: a canceled ctx ends the call.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// ExecGit is Git backed by the git binary, run through Run under the run's
// context.
type ExecGit struct {
	// Context is the run's context, which each call derives its own from; nil
	// means context.Background().
	Context context.Context
	Run     Runner
}

// NewExecGit returns the Git the command uses in production, running under ctx.
func NewExecGit(ctx context.Context) ExecGit {
	return ExecGit{Context: ctx, Run: RunExec}
}

// runContext never returns nil, so a call's context.WithTimeout cannot panic.
func (g ExecGit) runContext() context.Context {
	if g.Context != nil {
		return g.Context
	}
	return context.Background()
}

// gitTimeout bounds one git call, so a stalled remote cannot hold the run open.
// It is long on purpose: one full fetch of a large repository over a slow link
// can take many minutes, and a shorter bound would fail a healthy transfer.
const gitTimeout = 30 * time.Minute

// gitWaitDelay caps how long a killed git call waits for its output pipes to
// close. A remote helper git spawned can hold stdout open. The group kill reaches
// a helper that is still in the call's group, but not one that has left it, and
// without this cap Output would wait on that helper past the bound. It matches
// the wait internal/git gives a timed-out command.
var gitWaitDelay = util.ProcessGroupKillGrace + 3*time.Second

// RunExec is the production Runner: each git call is bounded by gitTimeout.
var RunExec Runner = gitRunner(gitTimeout)

// gitRunner is the Runner that bounds each call to bound: the call's context
// ends after bound or when the run's context does, and gitCommand kills the
// process group. RunExec passes gitTimeout; a test passes a short bound with
// gitRunnerWith.
func gitRunner(bound time.Duration) Runner {
	return gitRunnerWith(bound, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return gitCommand(ctx, name, args...).Output()
	})
}

// gitRunnerWith is gitRunner with the process starter injected, the way execRunner
// takes one: a test reads the context a call runs under without starting a process.
func gitRunnerWith(bound time.Duration, start func(ctx context.Context, name string, args ...string) ([]byte, error)) Runner {
	return execRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, bound)
		defer cancel()
		return start(ctx, name, args...)
	})
}

// gitCommand builds the process for one git call without starting it. It turns
// terminal prompting off, so a credential prompt fails the call instead of
// blocking the run on a terminal nobody reads. It runs in its own process group
// and ctx ending kills the whole group, so a remote helper git spawned goes too;
// WaitDelay then stops Output waiting on a pipe a helper outside the group holds.
func gitCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = gitWaitDelay
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// execRunner wraps a process starter as a Runner. Its error names the program
// and subcommand, never the output, which can carry a secret. The starter is a
// seam: a test drives the failure branch without starting a process.
func execRunner(start func(ctx context.Context, name string, args ...string) ([]byte, error)) Runner {
	return func(ctx context.Context, argv []string) ([]byte, error) {
		out, err := start(ctx, argv[0], argv[1:]...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", strings.Join(argv[:2], " "), err)
		}
		return out, nil
	}
}

// LsRemote lists the branches and tags of url. The "--" ends the options, so a
// url that starts with "-" is read as a repository, never as a git option.
func (g ExecGit) LsRemote(url string) ([]byte, error) {
	return g.Run(g.runContext(), []string{"git", "ls-remote", "--heads", "--tags", "--refs", "--", url})
}

// InitBare creates a bare repository at dir, the scratch the mirror is built in.
func (g ExecGit) InitBare(dir string) error {
	_, err := g.Run(g.runContext(), []string{"git", "init", "--bare", "--quiet", dir})
	return err
}

// Fetch fetches refspecs from url into the repository at dir. The "--" keeps a
// url that starts with "-" from being read as a git option.
func (g ExecGit) Fetch(dir, url string, refspecs ...string) error {
	_, err := g.Run(g.runContext(), append([]string{"git", "-C", dir, "fetch", "--quiet", "--", url}, refspecs...))
	return err
}

// PushPrune pushes refspecs from the repository at dir to url, deleting every
// ref at url that the refspecs do not name. The "--" keeps a url that starts
// with "-" from being read as a git option.
func (g ExecGit) PushPrune(dir, url string, refspecs ...string) error {
	_, err := g.Run(g.runContext(), append([]string{"git", "-C", dir, "push", "--prune", "--quiet", "--", url}, refspecs...))
	return err
}

var _ Git = ExecGit{}
