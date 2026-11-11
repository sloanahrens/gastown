package landing

import (
	"os"
	"path/filepath"
	"strings"
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

// writeSyntheticKey points GT_TOWN_ROOT at a fresh town under t and puts a
// synthetic API key in its settings, where the client's key search finds one,
// so Open builds its client without reading a key from the host or from a
// town the parallel tests share. It sets the environment, so the calling test
// must not run in parallel.
func writeSyntheticKey(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	t.Setenv("GT_TOWN_ROOT", town)
	path := filepath.Join(town, "settings", "daemon.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(jev.EnvKey+"=synthetic-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return town
}

// Open wires the rig's git base too: a town whose rig has its beads and a
// mayor/rig clone but no .repo.git is refused with an error naming the bare
// path, not opened over the clone.
func TestOpenRefusesARigWithoutBareRepo(t *testing.T) {
	town := writeSyntheticKey(t)
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
