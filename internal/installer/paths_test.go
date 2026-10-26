package installer

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// ResolvePaths is the script's environment block: the install directory has
// three names in order, and the daemon and checkout come from their own
// variables or from the town root.
func TestResolvePaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		env     map[string]string
		want    Paths
		wantErr bool
	}{
		{
			name: "each directory from its own variable needs no town",
			env:  map[string]string{"INSTALL_GT_BIN_DIR": "/bin", "INSTALL_GT_DAEMON_DIR": "/daemon", "INSTALL_GT_RIG_DIR": "/rig"},
			want: Paths{BinDir: "/bin", DaemonDir: "/daemon", RigDir: "/rig"},
		},
		{
			name: "INSTALL_DIR is the install directory's next name",
			env:  map[string]string{"INSTALL_DIR": "/install", "INSTALL_GT_DAEMON_DIR": "/daemon", "INSTALL_GT_RIG_DIR": "/rig"},
			want: Paths{BinDir: "/install", DaemonDir: "/daemon", RigDir: "/rig"},
		},
		{
			name: "home/.local/bin is the last name",
			env:  map[string]string{"INSTALL_GT_DAEMON_DIR": "/daemon", "INSTALL_GT_RIG_DIR": "/rig"},
			want: Paths{BinDir: "/home/u/.local/bin", DaemonDir: "/daemon", RigDir: "/rig"},
		},
		{
			name: "the town root supplies the daemon and the checkout",
			env:  map[string]string{"GT_TOWN_ROOT": "/town"},
			want: Paths{BinDir: "/home/u/.local/bin", DaemonDir: "/town/daemon", RigDir: "/town/gastown/mayor/rig"},
		},
		{
			name:    "no town and no directory variables is an error",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(key string) string { return tc.env[key] }

			got, err := resolvePaths(getenv, "/home/u", t.TempDir(), noTown)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolvePaths() = %+v, want an error", got)
				}
				if err.Error() != errNoTown.Error() {
					t.Errorf("error = %q, want %q", err, errNoTown)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolvePaths: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ResolvePaths() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// noTown is a tree with no town above any of it.
func noTown(string) bool { return false }

// Without GT_TOWN_ROOT the town is found above the directory the installer
// runs from, and the outermost marker wins: a rig can carry a mayor/ of its
// own. The marker check is the case's own, so which directories sit above the
// test's temporary tree cannot change the answer.
func TestResolvePathsFindsTheOutermostTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	start := filepath.Join(town, "gastown", "scripts")
	isTown := func(dir string) bool {
		return dir == town || dir == filepath.Join(town, "gastown")
	}

	got, err := resolvePaths(func(string) string { return "" }, "/home/u", start, isTown)

	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	want := Paths{
		BinDir:    "/home/u/.local/bin",
		DaemonDir: filepath.Join(town, "daemon"),
		RigDir:    filepath.Join(town, "gastown", "mayor", "rig"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolvePaths() = %+v, want %+v", got, want)
	}
}

// isTownRoot is the real check: the marker file, nothing else.
func TestIsTownRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if isTownRoot(town) {
		t.Fatalf("isTownRoot(%q) = true with no marker", town)
	}
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isTownRoot(town) {
		t.Errorf("isTownRoot(%q) = false with the marker", town)
	}
}

func TestPathsLocations(t *testing.T) {
	t.Parallel()
	p := Paths{BinDir: "/bin", DaemonDir: "/daemon"}
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"the installed binary", p.GT(), "/bin/gt"},
		{"the previous binary", p.Prev(), "/bin/gt.prev"},
		{"the restart marker", p.Marker(), "/daemon/restart-pending.json"},
		{"the receipts", p.Receipts(), "/daemon/install-receipts.jsonl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("path = %q, want %q", tc.got, tc.want)
			}
		})
	}
}
