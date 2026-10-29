package openshellprov

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

func TestParseGatewayForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		want    Options
		wantErr string
	}{
		{name: "no flag leaves the CLI's default gateway", want: Options{}},
		{name: "-g NAME", args: []string{"-g", "k8s"}, want: Options{Gateway: "k8s"}},
		{name: "--gateway NAME", args: []string{"--gateway", "k8s"}, want: Options{Gateway: "k8s"}},
		{name: "--gateway=NAME", args: []string{"--gateway=k8s"}, want: Options{Gateway: "k8s"}},
		{name: "the last spelling wins", args: []string{"-g", "first", "--gateway=last"}, want: Options{Gateway: "last"}},
		{name: "a name that looks like a flag is the name", args: []string{"-g", "-h"}, want: Options{Gateway: "-h"}},
		{name: "-g with no name", args: []string{"-g"}, wantErr: "openshell-providers: -g needs a gateway name, e.g. -g k8s"},
		{name: "-g with an empty name", args: []string{"-g", ""}, wantErr: "openshell-providers: -g needs a gateway name, e.g. -g k8s"},
		{name: "--gateway with no name", args: []string{"--gateway"}, wantErr: "openshell-providers: --gateway needs a gateway name, e.g. -g k8s"},
		{name: "--gateway= with an empty name", args: []string{"--gateway="}, wantErr: "openshell-providers: --gateway needs a gateway name, e.g. -g k8s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.args)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("Parse(%q) error = %v, want %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestParseRefusesAnUnknownArgument drives the refusal in this package, where
// it lives; the exit code it becomes is the command's to assert.
func TestParseRefusesAnUnknownArgument(t *testing.T) {
	t.Parallel()
	const want = "openshell-providers: unknown argument: --bogus (" + Usage + ")"
	if _, err := Parse([]string{"--bogus"}); err == nil || err.Error() != want {
		t.Fatalf("Parse(--bogus) error = %v, want %q", err, want)
	}
}

func TestDerivedPaths(t *testing.T) {
	t.Parallel()
	repo := tempRepo(t)
	nested := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", nested, err)
	}
	profiles := filepath.Join(repo, "deploy", "polecat", "profiles")
	// A tree the walk cannot examine: the stat below the working directory
	// fails with a permission error, which is not "this directory has no
	// go.mod".
	unreadable := func(path string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
	}
	// An empty tree: no go.mod at the working directory or any parent.
	noModule := func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }

	cases := []struct {
		name    string
		wd      string
		env     map[string]string
		stat    func(string) (fs.FileInfo, error)
		want    Paths
		wantErr string
	}{
		{
			name: "XDG_CONFIG_HOME wins over HOME",
			wd:   nested,
			env:  map[string]string{"HOME": "/home/agent", "XDG_CONFIG_HOME": "/etc/xdg"},
			stat: os.Stat,
			want: Paths{Root: repo, Profiles: profiles, ConfigDir: "/etc/xdg/gt"},
		},
		{
			name: "HOME is the fallback",
			wd:   repo,
			env:  map[string]string{"HOME": "/home/agent"},
			stat: os.Stat,
			want: Paths{Root: repo, Profiles: profiles, ConfigDir: "/home/agent/.config/gt"},
		},
		{
			name: "an empty XDG_CONFIG_HOME counts as unset",
			wd:   repo,
			env:  map[string]string{"HOME": "/home/agent", "XDG_CONFIG_HOME": ""},
			stat: os.Stat,
			want: Paths{Root: repo, Profiles: profiles, ConfigDir: "/home/agent/.config/gt"},
		},
		{
			name:    "no go.mod above the working directory is an error",
			wd:      "/checkout/a/b",
			env:     map[string]string{"HOME": "/home/agent"},
			stat:    noModule,
			wantErr: "openshell-providers: no go.mod in /checkout/a/b or any parent, so the repository root cannot be found",
		},
		{
			name:    "a directory that cannot be examined is an error",
			wd:      "/checkout",
			env:     map[string]string{"HOME": "/home/agent"},
			stat:    unreadable,
			wantErr: "openshell-providers: reading /checkout/go.mod",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolvePaths(Host{
				Getwd:  func() (string, error) { return tc.wd, nil },
				Getenv: func(key string) string { return tc.env[key] },
				Stat:   tc.stat,
			})
			if tc.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
					t.Fatalf("ResolvePaths() error = %v, want one starting %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolvePaths(): %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolvePaths() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A walk that cannot finish is not evidence of absence: an unreadable working
// directory must fail the run, not report a root of "".
func TestDerivedPathsStopsOnAnUnreadableWorkingDirectory(t *testing.T) {
	t.Parallel()
	_, err := ResolvePaths(Host{Getwd: func() (string, error) { return "", errors.New("boom") }})
	if err == nil || err.Error() != "openshell-providers: working directory: boom" {
		t.Fatalf("ResolvePaths() error = %v, want openshell-providers: working directory: boom", err)
	}
}

// tempRepo is a directory shaped like the checkout root: it holds go.mod.
func tempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return dir
}
