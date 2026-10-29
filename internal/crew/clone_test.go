package crew

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/rig"
)

// cloneAttempt is one call Add made to a clone method.
type cloneAttempt struct {
	method    string
	url       string
	dest      string
	branch    string
	reference string
}

// recordingClones records each clone call and refuses it, so a test reads the
// fallback chain Add walks without a clone ever landing.
type recordingClones struct {
	crewRepo
	attempts *[]cloneAttempt
}

func (r recordingClones) record(method string, a cloneAttempt) error {
	a.method = method
	*r.attempts = append(*r.attempts, a)
	return errors.New("clone refused")
}

func (r recordingClones) Clone(url, dest string) error {
	return r.record("Clone", cloneAttempt{url: url, dest: dest})
}

func (r recordingClones) CloneWithReference(url, dest, reference string) error {
	return r.record("CloneWithReference", cloneAttempt{url: url, dest: dest, reference: reference})
}

func (r recordingClones) CloneBranch(url, dest, branch string) error {
	return r.record("CloneBranch", cloneAttempt{url: url, dest: dest, branch: branch})
}

func (r recordingClones) CloneBranchWithReference(url, dest, branch, reference string) error {
	return r.record("CloneBranchWithReference", cloneAttempt{url: url, dest: dest, branch: branch, reference: reference})
}

func (r recordingClones) CloneFull(url, dest string) error {
	return r.record("CloneFull", cloneAttempt{url: url, dest: dest})
}

func (r recordingClones) CloneFullWithReference(url, dest, reference string) error {
	return r.record("CloneFullWithReference", cloneAttempt{url: url, dest: dest, reference: reference})
}

func (r recordingClones) CloneBranchFull(url, dest, branch string) error {
	return r.record("CloneBranchFull", cloneAttempt{url: url, dest: dest, branch: branch})
}

func (r recordingClones) CloneBranchFullWithReference(url, dest, branch, reference string) error {
	return r.record("CloneBranchFullWithReference", cloneAttempt{url: url, dest: dest, branch: branch, reference: reference})
}

// TestAddClonesWithFullHistory covers gt-zyow0: every attempt Add makes to
// clone the rig clones with full history and every branch, because a crew
// session reads merge bases and other agents' branches.
//
// Mutation evidence: pointing any attempt back at Clone, CloneBranch,
// CloneWithReference or CloneBranchWithReference turns the recorded chain red.
func TestAddClonesWithFullHistory(t *testing.T) {
	t.Parallel()
	const (
		gitURL    = "/srv/rig.git"
		localRepo = "/srv/local-repo.git"
	)
	for _, tt := range []struct {
		name      string
		localRepo string
		want      func(crewPath string) []cloneAttempt
	}{
		{
			name:      "a rig with a local reference falls through all four attempts",
			localRepo: localRepo,
			want: func(crewPath string) []cloneAttempt {
				return []cloneAttempt{
					{method: "CloneBranchFullWithReference", url: gitURL, dest: crewPath, branch: "main", reference: localRepo},
					{method: "CloneBranchFull", url: gitURL, dest: crewPath, branch: "main"},
					{method: "CloneFullWithReference", url: gitURL, dest: crewPath, reference: localRepo},
					{method: "CloneFull", url: gitURL, dest: crewPath},
				}
			},
		},
		{
			name: "a rig without a local reference falls through both attempts",
			want: func(crewPath string) []cloneAttempt {
				return []cloneAttempt{
					{method: "CloneBranchFull", url: gitURL, dest: crewPath, branch: "main"},
					{method: "CloneFull", url: gitURL, dest: crewPath},
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rigPath := t.TempDir()
			var attempts []cloneAttempt
			mgr := &Manager{
				rig: &rig.Rig{Name: "test-rig", Path: rigPath, GitURL: gitURL, LocalRepo: tt.localRepo},
				git: recordingClones{crewRepo: gitfake.New().Open(rigPath).(crewRepo), attempts: &attempts},
			}
			if _, err := mgr.Add("dave", false); err == nil {
				t.Fatal("Add with every clone refused returned nil error")
			}
			if want := tt.want(filepath.Join(rigPath, "crew", "dave")); !reflect.DeepEqual(attempts, want) {
				t.Errorf("Add cloned with\n%+v\nwant\n%+v", attempts, want)
			}
		})
	}
}
