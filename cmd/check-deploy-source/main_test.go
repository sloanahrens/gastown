package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/deploysource"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// fakeGit answers the check's questions from canned values and records each
// fetch; the zero value is a clean tree at HEAD abc1234 merged into
// origin/main. The internal package's fake has the error fields a failed git
// needs; this half only proves the environment reaches the check.
type fakeGit struct {
	fetchErr error
	fetched  [][2]string
	head     string
	headErr  error
	resolved bool
	ancestor bool
	modified []string
	behind   int
}

func (f *fakeGit) Fetch(remote, branch string) error {
	f.fetched = append(f.fetched, [2]string{remote, branch})
	return f.fetchErr
}

func (f *fakeGit) ShortHead() (string, error) {
	head := f.head
	if head == "" {
		head = "abc1234"
	}
	return head, f.headErr
}

func (f *fakeGit) Resolve(string) (bool, error)            { return f.resolved, nil }
func (f *fakeGit) IsAncestor(string, string) (bool, error) { return f.ancestor, nil }
func (f *fakeGit) ModifiedTracked() ([]string, error)      { return f.modified, nil }
func (f *fakeGit) CountBehind(string) (int, error)         { return f.behind, nil }

// merged is the repository that passes every rule.
func merged() *fakeGit {
	return &fakeGit{resolved: true, ancestor: true}
}

// runMain runs the command with env as its whole environment.
func runMain(t *testing.T, args []string, g deploysource.Git, env map[string]string) (int, string) {
	t.Helper()
	var errw bytes.Buffer
	code := run(args, &errw, g, func(key string) string { return env[key] })
	return code, errw.String()
}

func TestMainExitCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		args     []string
		g        *fakeGit
		wantCode int
		wantErr  string
	}{
		{
			name:     "a merged tree installs",
			g:        merged(),
			wantCode: 0,
		},
		{
			name:     "an unmerged tree is refused with exit 1",
			g:        &fakeGit{resolved: true, ancestor: false},
			wantCode: 1,
			wantErr: "ERROR: HEAD abc1234 is not merged into origin/main\n" +
				"  Deploy merged code only: build from an up-to-date main checkout.\n" +
				"  To install unmerged code anyway (operator hot-fix): ALLOW_UNMERGED=1\n",
		},
		{
			name:     "a git failure is not a pass",
			g:        &fakeGit{headErr: errors.New("exit status 128")},
			wantCode: 1,
			wantErr:  "check-deploy-source: read HEAD: exit status 128\n",
		},
		{
			name:     "an argument is refused with exit 2",
			args:     []string{"--bogus"},
			g:        merged(),
			wantCode: 2,
			wantErr:  "check-deploy-source: unknown argument: --bogus (usage: check-deploy-source)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, errOut := runMain(t, tc.args, tc.g, nil)
			if code != tc.wantCode {
				t.Fatalf("run = %d, want %d (stderr %q)", code, tc.wantCode, errOut)
			}
			if errOut != tc.wantErr {
				t.Errorf("stderr = %q, want %q", errOut, tc.wantErr)
			}
		})
	}
}

// The switches reach the check from the environment: an unmerged tree installs
// under ALLOW_UNMERGED, SKIP_UPDATE_CHECK leaves the fetch out, and
// DEPLOY_MAIN_REF names remote and branch.
func TestMainReadsTheEnvironment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		env         map[string]string
		g           *fakeGit
		wantCode    int
		wantFetched [][2]string
		wantErr     string
	}{
		{
			name:        "ALLOW_UNMERGED installs an unmerged tree",
			env:         map[string]string{deploysource.AllowUnmergedEnv: "1"},
			g:           &fakeGit{resolved: true, ancestor: false},
			wantCode:    0,
			wantFetched: [][2]string{{"origin", "main"}},
			wantErr: "WARNING: ALLOW_UNMERGED=1 — installing abc1234 without checking that it is merged to origin/main.\n" +
				"WARNING: every town session will run this UNMERGED build until the next install from main.\n",
		},
		{
			name:        "SKIP_UPDATE_CHECK fetches nothing",
			env:         map[string]string{deploysource.SkipUpdateCheckEnv: "1"},
			g:           merged(),
			wantCode:    0,
			wantFetched: nil,
		},
		{
			name:        "DEPLOY_MAIN_REF names the remote and branch",
			env:         map[string]string{deploysource.MainRefEnv: "upstream/feature/main"},
			g:           merged(),
			wantCode:    0,
			wantFetched: [][2]string{{"upstream", "feature/main"}},
		},
		{
			name:     "a DEPLOY_MAIN_REF that starts with a dash is a check error and fetches nothing",
			env:      map[string]string{deploysource.MainRefEnv: "--upload-pack=x"},
			g:        merged(),
			wantCode: 1,
			wantErr:  "check-deploy-source: DEPLOY_MAIN_REF \"--upload-pack=x\" has a remote or branch starting with '-', which git reads as an option\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, errOut := runMain(t, nil, tc.g, tc.env)
			if code != tc.wantCode {
				t.Fatalf("run = %d, want %d (stderr %q)", code, tc.wantCode, errOut)
			}
			if errOut != tc.wantErr {
				t.Errorf("stderr = %q, want %q", errOut, tc.wantErr)
			}
			got := strings.Join(fetchTuples(tc.g.fetched), " ")
			want := strings.Join(fetchTuples(tc.wantFetched), " ")
			if got != want {
				t.Errorf("fetched %q, want %q", got, want)
			}
		})
	}
}

// fetchTuples renders the fetches for a comparison message.
func fetchTuples(fetched [][2]string) []string {
	out := make([]string, len(fetched))
	for i, f := range fetched {
		out[i] = f[0] + "/" + f[1]
	}
	return out
}
