package forgejoprov

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// testWorkflowList is the hook list the workflow tests write, under a fixture path so
// no test touches the real one.
const testWorkflowList = "/fixture/hooks/pre-receive.d/gt-protected-workflow-repos"

// workflowListMode is the mode the seeded list carries; a write must keep it.
const workflowListMode fs.FileMode = 0o640

// errWorkflowFixture is the filesystem error a row injects into the list's reads, stats or
// writes.
var errWorkflowFixture = errors.New("fixture filesystem error")

// ensureWorkflow runs EnsureWorkflowProtected for repo over files and returns the
// progress lines it logged with the error it returned.
func ensureWorkflow(files *memFiles, repo string, dryRun bool) (string, error) {
	var logs strings.Builder
	p := &Provisioner{
		Client: &Client{DryRun: dryRun, Log: func(line string) { logs.WriteString(line + "\n") }},
		Files:  files,
	}
	err := p.EnsureWorkflowProtected(repo, testWorkflowList)
	return logs.String(), err
}

// countWorkflowWrites counts the writes files recorded for the hook list.
func countWorkflowWrites(files *memFiles) int {
	want := "write " + testWorkflowList + " " + workflowListMode.String()
	n := 0
	for _, op := range files.ops {
		if op == want {
			n++
		}
	}
	return n
}

// TestEnsureWorkflowProtected: each row sets up the hook list (seeded, or absent), provisions
// one repository, and asserts the list left behind, whether a write was sent, and whether the
// call failed.
func TestEnsureWorkflowProtected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		list      string // seeded content; ignored when absent
		absent    bool   // no list file at all
		repo      string
		dryRun    bool
		readErr   bool
		statErr   bool
		writeErr  bool
		wantList  string
		wantWrite bool
		wantErr   bool
	}{
		{
			name:      "missing repository is appended, keeping every existing line",
			list:      "acme/other\nacme/third\n",
			repo:      protectionRepo,
			wantList:  "acme/other\nacme/third\nacme/rig\n",
			wantWrite: true,
		},
		{
			name:      "a list without a final newline gets one before the append",
			list:      "acme/other",
			repo:      protectionRepo,
			wantList:  "acme/other\nacme/rig\n",
			wantWrite: true,
		},
		{
			name:     "a repository the list already names in another case is not written again",
			list:     "Acme/Rig\n",
			repo:     protectionRepo,
			wantList: "Acme/Rig\n",
		},
		{
			name:     "a line naming it with surrounding space and CRLF counts as named",
			list:     "acme/other\r\n  ACME/rig \r\n",
			repo:     protectionRepo,
			wantList: "acme/other\r\n  ACME/rig \r\n",
		},
		{
			name:      "a line that only starts with the repository does not count as named",
			list:      "acme/rig-old\n",
			repo:      protectionRepo,
			wantList:  "acme/rig-old\nacme/rig\n",
			wantWrite: true,
		},
		{
			name:     "a list that cannot be read fails the provision",
			absent:   true,
			repo:     protectionRepo,
			wantList: "",
			wantErr:  true,
		},
		{
			name:     "a list read error fails the provision",
			list:     "acme/other\n",
			repo:     protectionRepo,
			readErr:  true,
			wantList: "acme/other\n",
			wantErr:  true,
		},
		{
			name:     "an empty list is refused, not passed",
			list:     "",
			repo:     protectionRepo,
			wantList: "",
			wantErr:  true,
		},
		{
			name:     "a whitespace-only list is refused, not passed",
			list:     " \n\t\n",
			repo:     protectionRepo,
			wantList: " \n\t\n",
			wantErr:  true,
		},
		{
			name:     "a stat error stops the write",
			list:     "acme/other\n",
			repo:     protectionRepo,
			statErr:  true,
			wantList: "acme/other\n",
			wantErr:  true,
		},
		{
			name:      "a write error is returned",
			list:      "acme/other\n",
			repo:      protectionRepo,
			writeErr:  true,
			wantList:  "acme/other\n",
			wantWrite: true,
			wantErr:   true,
		},
		{
			name:     "a dry run reads the list and sends no write",
			list:     "acme/other\n",
			repo:     protectionRepo,
			dryRun:   true,
			wantList: "acme/other\n",
		},
		{
			name:     "a repository that is not owner/name is refused before the list is read",
			list:     "acme/other\n",
			repo:     "acme/rig/extra",
			wantList: "acme/other\n",
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := newMemFiles()
			if !tc.absent {
				files.seed(testWorkflowList, tc.list, workflowListMode)
			}
			if tc.readErr {
				files.readErr = errWorkflowFixture
			}
			if tc.statErr {
				files.statErr = errWorkflowFixture
			}
			if tc.writeErr {
				files.writeErr = errWorkflowFixture
			}

			_, err := ensureWorkflow(files, tc.repo, tc.dryRun)
			if (err != nil) != tc.wantErr {
				t.Fatalf("EnsureWorkflowProtected(%q) error = %v, want error %t", tc.repo, err, tc.wantErr)
			}
			if got := string(files.files[testWorkflowList]); got != tc.wantList {
				t.Errorf("list = %q, want %q", got, tc.wantList)
			}
			if got := countWorkflowWrites(files); (got > 0) != tc.wantWrite {
				t.Errorf("writes of the list = %d, want a write %t", got, tc.wantWrite)
			}
			if !tc.absent && files.modes[testWorkflowList] != workflowListMode {
				t.Errorf("list mode = %v, want %v kept", files.modes[testWorkflowList], workflowListMode)
			}
		})
	}
}

// TestRunAppendsProvisionedRepoToWorkflowList: a run that provisions a repository appends
// it to the default list the hook reads, keeping the lines already there.
func TestRunAppendsProvisionedRepoToWorkflowList(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(protectionRepo)
	files := newMemFiles()
	files.seed(DefaultWorkflowList, "acme/other\n", workflowListMode)

	res := runProvisioning(t, f, files, nil, "--repo", protectionRepo)
	if res.code != ExitOK {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitOK, res.shown())
	}
	if got, want := string(files.files[DefaultWorkflowList]), "acme/other\nacme/rig\n"; got != want {
		t.Errorf("hook list = %q, want %q", got, want)
	}
}

// TestRunFailsWhenWorkflowListCannotBeRead: a list the run cannot read fails the run, the
// run names the list, and it writes no list of its own.
func TestRunFailsWhenWorkflowListCannotBeRead(t *testing.T) {
	t.Parallel()
	const absent = "/fixture/absent/gt-protected-workflow-repos"
	f := forgejotest.New()
	f.AddRepo(protectionRepo)
	files := newMemFiles()

	res := runProvisioning(t, f, files, nil, "--repo", protectionRepo, "--workflow-list", absent)
	if res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.shown())
	}
	if !strings.Contains(res.logs, "read the workflow-file hook list "+absent) {
		t.Errorf("logs = %q, want the unreadable list named", res.shown())
	}
	if _, ok := files.files[absent]; ok {
		t.Errorf("an unreadable list was written: %q", files.files[absent])
	}
}

// TestRunRefusesEmptyWorkflowList: an empty list fails the run and is left empty, so the
// provision does not pass while the hook protects nothing.
func TestRunRefusesEmptyWorkflowList(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(protectionRepo)
	files := newMemFiles()
	files.seed(DefaultWorkflowList, "", workflowListMode)

	res := runProvisioning(t, f, files, nil, "--repo", protectionRepo)
	if res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.shown())
	}
	if !strings.Contains(res.logs, "is empty") {
		t.Errorf("logs = %q, want the empty list refused", res.shown())
	}
	if got := string(files.files[DefaultWorkflowList]); got != "" {
		t.Errorf("empty list = %q, want it left empty", got)
	}
}

// TestRunFailsWhenCandidateRuleCannotBeRemoved: a candidate-branch rule the run cannot remove
// fails the run before the hook list is written, so a repository whose branch rules did not
// finish is not added to the list.
func TestRunFailsWhenCandidateRuleCannotBeRemoved(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.ProtectedLandBranch = true
	f.AddRepo(protectionRepo)
	files := newMemFiles()
	files.seed(DefaultWorkflowList, "acme/other\n", workflowListMode)

	rt := failingRoute{next: f, match: "/branch_protections/land", status: http.StatusInternalServerError}
	res := runProvisioning(t, rt, files, nil, "--repo", protectionRepo)
	if res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.shown())
	}
	if got, want := string(files.files[DefaultWorkflowList]), "acme/other\n"; got != want {
		t.Errorf("hook list = %q, want it untouched at %q", got, want)
	}
}

// TestEnsureWorkflowProtectedAppendsOnce: a second provision of the same repository finds it
// named by the first, so the list holds one line for it and the second call sends no write.
func TestEnsureWorkflowProtectedAppendsOnce(t *testing.T) {
	t.Parallel()
	files := newMemFiles()
	files.seed(testWorkflowList, "acme/other\n", workflowListMode)

	if _, err := ensureWorkflow(files, protectionRepo, false); err != nil {
		t.Fatalf("first provision = %v, want nil", err)
	}
	if _, err := ensureWorkflow(files, protectionRepo, false); err != nil {
		t.Fatalf("second provision = %v, want nil", err)
	}
	want := "acme/other\nacme/rig\n"
	if got := string(files.files[testWorkflowList]); got != want {
		t.Errorf("list after two provisions = %q, want %q", got, want)
	}
	if got := countWorkflowWrites(files); got != 1 {
		t.Errorf("writes of the list over two provisions = %d, want 1", got)
	}
}
