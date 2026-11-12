package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// postLandTestContext is the status context the rig's settings name in these
// tests: a job "check" in a workflow named "post-land", posted on push.
const postLandTestContext = "post-land / check (push)"

// postLandGate is a workflow on main that posts no post-land context.
const postLandGate = "name: gate\non: push\njobs:\n  test:\n    runs-on: ci-base\n"

// postLandOnPush is a workflow on main that posts the context on every push.
const postLandOnPush = "name: post-land\non: push\njobs:\n  check:\n    runs-on: ci-base\n"

// postLandPathsFilter is a workflow on main that posts the context only for
// pushes that touch src/.
const postLandPathsFilter = "name: post-land\non:\n  push:\n    paths: ['src/**']\njobs:\n  check:\n    runs-on: ci-base\n"

// postLandHealthSources is healthTown's healthSources with the gastown rig's
// repository holding workflows on main, and its settings naming mergeQueue. The
// gitfake world answers every git read, so nothing runs git.
func postLandHealthSources(t *testing.T, workflows map[string]string, mergeQueue map[string]any) *healthSources {
	t.Helper()
	d, _ := healthTown(t, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	f := useGitfake(t, d)
	town := d.config.TownRoot
	repo := filepath.Join(town, "gastown", ".repo.git")
	f.InitBare(t, repo)
	files := map[string]string{"README.md": "# gastown\n"}
	for name, body := range workflows {
		files[".forgejo/workflows/"+name] = body
	}
	f.SetRef(t, repo, "refs/remotes/origin/main", f.Commit(t, repo, "main", "main's tip", files))
	writeJSONFile(t, filepath.Join(town, "gastown", "settings", "config.json"),
		map[string]any{"type": "rig-settings", "version": 1, "merge_queue": mergeQueue})
	return &healthSources{d: d}
}

// postLandMain is the one landing rig's main state from s.
func postLandMain(t *testing.T, s *healthSources) townhealth.RigMain {
	t.Helper()
	ms, err := s.Mains()
	if err != nil {
		t.Fatalf("Mains(): %v", err)
	}
	if len(ms) != 1 || ms[0].Rig != "gastown" {
		t.Fatalf("Mains() = %+v, want the one landing rig", ms)
	}
	return ms[0]
}

// workflowReadFails is a repository whose tree listing or file read fails, as a
// repository git cannot read does. A nil error passes the read through.
type workflowReadFails struct {
	daemonGit
	listErr error
	readErr error
}

func (r workflowReadFails) TreeFileBlobs(rev string) (map[string]string, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.daemonGit.TreeFileBlobs(rev)
}

func (r workflowReadFails) ShowFileAtRev(ref, file string) (string, error) {
	if r.readErr != nil {
		return "", r.readErr
	}
	return r.daemonGit.ShowFileAtRev(ref, file)
}

// A workflow on main that posts the rig's status context on no push leaves the
// main field UNKNOWN with the matcher's reason at the first read, and a
// workflow that posts it on every push leaves the field with neither an error
// nor a note (gt-gg9hv.7.4).
func TestMainsPostLandWorkflowMissReadsUnknown(t *testing.T) {
	t.Parallel()
	settings := map[string]any{"post_land_status_context": postLandTestContext}

	miss := postLandMain(t, postLandHealthSources(t, map[string]string{"gate.yml": postLandGate}, settings))
	_, why := landworker.WorkflowsPostContext(map[string][]byte{".forgejo/workflows/gate.yml": []byte(postLandGate)}, postLandTestContext)
	if miss.Err == nil {
		t.Fatalf("Mains() Err = nil for a context no workflow posts, want the miss")
	}
	if got := miss.Err.Error(); got != why || !strings.Contains(got, postLandTestContext) {
		t.Errorf("Mains() Err = %q, want the matcher's reason %q, which names the context", got, why)
	}
	if miss.PostLandNote != "" {
		t.Errorf("Mains() PostLandNote = %q for a miss, want none", miss.PostLandNote)
	}

	posts := postLandMain(t, postLandHealthSources(t, map[string]string{"post-land.yml": postLandOnPush}, settings))
	if posts.Err != nil || posts.PostLandNote != "" {
		t.Errorf("Mains() for a workflow that posts on every push = Err %v, note %q; want neither", posts.Err, posts.PostLandNote)
	}

	// Only the files directly in .forgejo/workflows/ are read, so a workflow
	// in a subdirectory of it posts nothing for the rig.
	nested := postLandMain(t, postLandHealthSources(t, map[string]string{"nested/post-land.yml": postLandOnPush}, settings))
	if nested.Err == nil {
		t.Errorf("Mains() Err = nil for a workflow in a subdirectory, want the miss")
	}
}

// A workflow that posts the context only for pushes touching a paths filter is
// not an error: the rig keeps its verdict, and the matcher's reason rides as the
// note beside it (gt-gg9hv.7.4).
func TestMainsPostLandWorkflowSomeCommitsWarns(t *testing.T) {
	t.Parallel()
	files := map[string][]byte{".forgejo/workflows/post-land.yml": []byte(postLandPathsFilter)}
	_, why := landworker.WorkflowsPostContext(files, postLandTestContext)

	m := postLandMain(t, postLandHealthSources(t, map[string]string{"post-land.yml": postLandPathsFilter},
		map[string]any{"post_land_status_context": postLandTestContext}))
	if m.Err != nil {
		t.Fatalf("Mains() Err = %v for a paths-filtered workflow, want none", m.Err)
	}
	if m.PostLandNote != why {
		t.Errorf("Mains() PostLandNote = %q, want the matcher's reason %q", m.PostLandNote, why)
	}
	if !strings.Contains(m.PostLandNote, postLandTestContext) {
		t.Errorf("Mains() PostLandNote = %q, want it to name the context", m.PostLandNote)
	}
}

// A read the repository cannot make is UNKNOWN with its reason, never a clean
// verdict. A rig with no status context, or only a command, reads no workflow
// at all, so a failing repository leaves it unchanged (gt-gg9hv.7.4).
func TestMainsPostLandWorkflowUnreadableAndUnconfigured(t *testing.T) {
	t.Parallel()
	readErr := errors.New("fatal: bad object")
	withContext := map[string]any{"post_land_status_context": postLandTestContext}

	tests := []struct {
		name       string
		workflows  map[string]string
		mergeQueue map[string]any
		fail       workflowReadFails
		wantErr    string
	}{
		{name: "tree listing fails", workflows: map[string]string{"post-land.yml": postLandOnPush},
			mergeQueue: withContext, fail: workflowReadFails{listErr: readErr}, wantErr: readErr.Error()},
		{name: "workflow file read fails", workflows: map[string]string{"post-land.yml": postLandOnPush},
			mergeQueue: withContext, fail: workflowReadFails{readErr: readErr}, wantErr: readErr.Error()},
		{name: "command only is not read", workflows: map[string]string{"post-land.yml": postLandOnPush},
			mergeQueue: map[string]any{"post_land_command": "make test-slow"}, fail: workflowReadFails{listErr: readErr}},
		{name: "neither is not read", workflows: map[string]string{"post-land.yml": postLandOnPush},
			mergeQueue: map[string]any{}, fail: workflowReadFails{listErr: readErr}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := postLandHealthSources(t, tt.workflows, tt.mergeQueue)
			open := s.d.openGitFn
			s.d.openGitFn = func(dir string) daemonGit {
				fail := tt.fail
				fail.daemonGit = open(dir)
				return fail
			}
			m := postLandMain(t, s)
			if tt.wantErr == "" {
				if m.Err != nil {
					t.Fatalf("Mains() Err = %v, want the rig unchanged", m.Err)
				}
				return
			}
			if m.Err == nil || !strings.Contains(m.Err.Error(), tt.wantErr) {
				t.Errorf("Mains() Err = %v, want the read error %q", m.Err, tt.wantErr)
			}
		})
	}

	// A rig config that exists but does not decode names no default branch, so
	// the workflows on main cannot be found and the field is UNKNOWN.
	t.Run("unreadable rig config is UNKNOWN", func(t *testing.T) {
		t.Parallel()
		s := postLandHealthSources(t, map[string]string{"post-land.yml": postLandOnPush}, withContext)
		writeTownFile(t, s.d.config.TownRoot, filepath.Join("gastown", "config.json"), "{not json")
		m := postLandMain(t, s)
		if m.Err == nil || !strings.Contains(m.Err.Error(), "reading the workflows that post") {
			t.Errorf("Mains() Err = %v, want the rig config read named", m.Err)
		}
	})

	t.Run("no repository is UNKNOWN", func(t *testing.T) {
		t.Parallel()
		d, _ := healthTown(t, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
		writeJSONFile(t, filepath.Join(d.config.TownRoot, "gastown", "settings", "config.json"),
			map[string]any{"type": "rig-settings", "version": 1, "merge_queue": withContext})
		m := postLandMain(t, &healthSources{d: d})
		if m.Err == nil || !strings.Contains(m.Err.Error(), "rig repository") {
			t.Errorf("Mains() Err = %v, want the missing repository named", m.Err)
		}
	})
}
