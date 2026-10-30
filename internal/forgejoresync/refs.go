package forgejoresync

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
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

// Runner runs argv, argv[0] being the program, and returns its stdout.
type Runner func(argv []string) ([]byte, error)

// ExecGit is Git backed by the git binary, run through Run.
type ExecGit struct {
	Run Runner
}

// NewExecGit returns the Git the command uses in production.
func NewExecGit() ExecGit {
	return ExecGit{Run: RunExec}
}

// RunExec starts the program in argv and returns its stdout. Its error names
// the program and subcommand, never the output, which can carry a secret.
func RunExec(argv []string) ([]byte, error) {
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(argv[:2], " "), err)
	}
	return out, nil
}

// LsRemote lists the branches and tags of url.
func (g ExecGit) LsRemote(url string) ([]byte, error) {
	return g.Run([]string{"git", "ls-remote", "--heads", "--tags", "--refs", url})
}

// InitBare creates a bare repository at dir, the scratch the mirror is built in.
func (g ExecGit) InitBare(dir string) error {
	_, err := g.Run([]string{"git", "init", "--bare", "--quiet", dir})
	return err
}

// Fetch fetches refspecs from url into the repository at dir.
func (g ExecGit) Fetch(dir, url string, refspecs ...string) error {
	_, err := g.Run(append([]string{"git", "-C", dir, "fetch", "--quiet", url}, refspecs...))
	return err
}

// PushPrune pushes refspecs from the repository at dir to url, deleting every
// ref at url that the refspecs do not name.
func (g ExecGit) PushPrune(dir, url string, refspecs ...string) error {
	_, err := g.Run(append([]string{"git", "-C", dir, "push", "--prune", "--quiet", url}, refspecs...))
	return err
}

var _ Git = ExecGit{}
