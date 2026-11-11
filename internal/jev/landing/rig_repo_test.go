package landing

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
)

// The landing wire reads the rig's git base from its shared .repo.git only. A
// rig with a legacy mayor/rig clone and no .repo.git is an error naming the
// bare path, never a fallback to the clone.
func TestRigRepoReadsOnlyTheBareRepo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, rigRoot string)
		want  bool
	}{
		{
			name: "a shared bare repo is the base",
			setup: func(t *testing.T, rigRoot string) {
				if err := os.MkdirAll(constants.RigBareRepoPath(rigRoot), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
		{
			name: "a mayor/rig clone with no bare repo is refused",
			setup: func(t *testing.T, rigRoot string) {
				if err := os.MkdirAll(filepath.Join(rigRoot, "mayor", "rig"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			name:  "a rig with neither is refused",
			setup: func(t *testing.T, rigRoot string) {},
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rigRoot := t.TempDir()
			tc.setup(t, rigRoot)

			repo, err := rigRepo(rigRoot)
			if !tc.want {
				want := "no .repo.git under " + rigRoot
				if err == nil || err.Error() != want {
					t.Fatalf("rigRepo err = %v, want %q", err, want)
				}
				if strings.Contains(err.Error(), "mayor/rig") {
					t.Fatalf("rigRepo err = %q, want no mention of mayor/rig", err)
				}
				return
			}
			if err != nil || repo != constants.RigBareRepoPath(rigRoot) {
				t.Fatalf("rigRepo = %q, %v; want the bare repo path", repo, err)
			}
		})
	}
}

// syntheticKeyOnce writes the synthetic key once per process: the hermetic
// harness's GT_TOWN_ROOT is one town shared by every parallel test, so the
// write runs once and each other caller waits for it instead of racing it.
var (
	syntheticKeyOnce sync.Once
	syntheticKeyErr  error
)

// ensureSyntheticKey puts a synthetic API key where the client's key search
// finds one under the hermetic harness's GT_TOWN_ROOT, so Open builds its
// client without reading a key from the host.
func ensureSyntheticKey(t *testing.T) {
	t.Helper()
	syntheticKeyOnce.Do(func() {
		town := os.Getenv("GT_TOWN_ROOT")
		if town == "" {
			syntheticKeyErr = errors.New("the hermetic harness sets GT_TOWN_ROOT to its sandbox town")
			return
		}
		path := filepath.Join(town, "settings", "daemon.env")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			syntheticKeyErr = err
			return
		}
		syntheticKeyErr = os.WriteFile(path, []byte(jev.EnvKey+"=synthetic-test-key\n"), 0o600)
	})
	if syntheticKeyErr != nil {
		t.Fatal(syntheticKeyErr)
	}
}

// Open wires the rig's git base too: a town whose rig has its beads and a
// mayor/rig clone but no .repo.git is refused with an error naming the bare
// path, not opened over the clone.
func TestOpenRefusesARigWithoutBareRepo(t *testing.T) {
	t.Parallel()
	ensureSyntheticKey(t)
	town := t.TempDir()
	rigDir := filepath.Join(town, RigName)
	for _, dir := range []string{
		filepath.Join(rigDir, ".beads"),
		filepath.Join(rigDir, "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Open(town, Settings{USDPerBillionTokens: 3}, nil)
	want := "no .repo.git under " + rigDir
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Open err = %v, want an error naming %q", err, want)
	}
}
