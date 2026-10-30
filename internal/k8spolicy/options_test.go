package k8spolicy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// fakeInfo is the FileInfo the scripted Stat reports for a path it can see.
type fakeInfo struct{ name string }

func (f fakeInfo) Name() string     { return f.name }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() fs.FileMode  { return 0 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return false }
func (fakeInfo) Sys() any           { return nil }

// moduleStat reports go.mod under each of the named directories as present and
// every other path as absent, so the repository-root walk runs against a list
// rather than a tree built on disk.
func moduleStat(modules ...string) func(string) (fs.FileInfo, error) {
	return func(path string) (fs.FileInfo, error) {
		for _, dir := range modules {
			if path == filepath.Join(dir, "go.mod") {
				return fakeInfo{name: "go.mod"}, nil
			}
		}
		return nil, fs.ErrNotExist
	}
}

// fixedWd is the Host seam's working directory.
func fixedWd(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// TestParseOptions: the flags the script offered, the positional BASE it took,
// and the refusals. The default BASE is one directory higher than the working
// directory, so the row for no arguments also pins the repository-root rule.
func TestParseOptions(t *testing.T) {
	t.Parallel()
	const root = "/checkout"
	host := Host{
		Getwd: fixedWd(root + "/cmd/render-policy"),
		Stat:  moduleStat(root),
	}
	defaults := Options{
		DoltHost:    DefaultDoltHost,
		ForgejoHost: DefaultForgejoHost,
		TrackerHost: DefaultTrackerHost,
		AllowedIP:   DefaultAllowedIP,
		Base:        filepath.Join(root, "deploy", "polecat", "policy.yaml"),
	}
	with := func(change func(*Options)) Options {
		opts := defaults
		change(&opts)
		return opts
	}
	cases := []struct {
		name    string
		args    []string
		want    Options
		wantErr string
	}{
		{name: "no arguments is the script's defaults", want: defaults},
		{
			name: "the dolt host",
			args: []string{"--dolt-host", "dolt.example"},
			want: with(func(o *Options) { o.DoltHost = "dolt.example" }),
		},
		{
			name: "the forgejo host",
			args: []string{"--forgejo-host", "forgejo.example"},
			want: with(func(o *Options) { o.ForgejoHost = "forgejo.example" }),
		},
		{
			name: "the tracker host",
			args: []string{"--tracker-host", "tracker.example"},
			want: with(func(o *Options) { o.TrackerHost = "tracker.example" }),
		},
		{
			name:    "the tracker host with no value",
			args:    []string{"--tracker-host"},
			wantErr: "render-policy: --tracker-host needs a value",
		},
		{
			name: "the pinned address",
			args: []string{"--allowed-ip", "10.0.0.9"},
			want: with(func(o *Options) { o.AllowedIP = "10.0.0.9" }),
		},
		{
			name: "every flag together",
			args: []string{"--dolt-host", "dolt.example", "--forgejo-host", "forgejo.example", "--tracker-host", "tracker.example", "--allowed-ip", "10.0.0.9"},
			want: with(func(o *Options) {
				o.DoltHost = "dolt.example"
				o.ForgejoHost = "forgejo.example"
				o.TrackerHost = "tracker.example"
				o.AllowedIP = "10.0.0.9"
			}),
		},
		{
			name: "a repeated flag takes its last value",
			args: []string{"--dolt-host", "first.example", "--dolt-host", "last.example"},
			want: with(func(o *Options) { o.DoltHost = "last.example" }),
		},
		{
			name: "a value that looks like a flag is the value",
			args: []string{"--dolt-host", "--allowed-ip"},
			want: with(func(o *Options) { o.DoltHost = "--allowed-ip" }),
		},
		{
			name: "the positional BASE",
			args: []string{"scratch/policy.yaml"},
			want: with(func(o *Options) { o.Base = "scratch/policy.yaml" }),
		},
		{
			name: "the last positional wins",
			args: []string{"first.yaml", "second.yaml"},
			want: with(func(o *Options) { o.Base = "second.yaml" }),
		},
		{
			name: "a positional among the flags",
			args: []string{"--allowed-ip", "10.0.0.9", "scratch/policy.yaml"},
			want: with(func(o *Options) {
				o.AllowedIP = "10.0.0.9"
				o.Base = "scratch/policy.yaml"
			}),
		},
		{name: "an unknown long option", args: []string{"--bogus"}, wantErr: "render-policy: unknown option --bogus"},
		{name: "an unknown short option", args: []string{"-x"}, wantErr: "render-policy: unknown option -x"},
		{name: "a lone dash is an unknown option", args: []string{"-"}, wantErr: "render-policy: unknown option -"},
		{name: "a flag with no value", args: []string{"--dolt-host"}, wantErr: "render-policy: --dolt-host needs a value"},
		{
			name:    "a flag with no value after others",
			args:    []string{"--allowed-ip", "10.0.0.9", "--forgejo-host"},
			wantErr: "render-policy: --forgejo-host needs a value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.args, host)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("Parse(%q) error = %v, want %q", tc.args, err, tc.wantErr)
				}
				var usage *UsageError
				if !errors.As(err, &usage) {
					t.Errorf("Parse(%q) error is %T, want *UsageError so the command exits 2", tc.args, err)
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

// TestReadBase: the failure names the path alone, so a missing file and an
// unreadable one reach an operator as the same line.
func TestReadBase(t *testing.T) {
	t.Parallel()
	t.Run("the text of the file", func(t *testing.T) {
		t.Parallel()
		host := Host{ReadFile: func(string) ([]byte, error) { return []byte("policy"), nil }}
		text, err := ReadBase(host, "policy.yaml")
		if err != nil {
			t.Fatalf("ReadBase(): %v", err)
		}
		if string(text) != "policy" {
			t.Errorf("ReadBase() = %q, want %q", text, "policy")
		}
	})
	t.Run("a file that cannot be read", func(t *testing.T) {
		t.Parallel()
		host := Host{ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist }}
		_, err := ReadBase(host, "no/policy.yaml")
		const want = "render-policy: cannot read no/policy.yaml"
		if err == nil || err.Error() != want {
			t.Fatalf("ReadBase() error = %v, want %q", err, want)
		}
	})
}

// TestParseDefaultBaseFromTheRepositoryRoot: the default BASE hangs off the
// repository root, not the working directory, so a run started anywhere in the
// checkout renders the same file (the landing review of attempt 1,
// gt-tpg0p.81).
func TestParseDefaultBaseFromTheRepositoryRoot(t *testing.T) {
	t.Parallel()
	const root = "/checkout"
	want := filepath.Join(root, DefaultBasePath)
	for _, wd := range []string{root, root + "/cmd/render-policy", root + "/a/b/c"} {
		opts, err := Parse(nil, Host{Getwd: fixedWd(wd), Stat: moduleStat(root)})
		if err != nil {
			t.Fatalf("Parse() from %s: %v", wd, err)
		}
		if opts.Base != want {
			t.Errorf("Parse() from %s Base = %s, want %s", wd, opts.Base, want)
		}
	}
}

// TestParseWithoutARepositoryRoot: the walk fails closed. A directory it cannot
// examine is not a checkout without a module, and a command line that names its
// own BASE does not need the walk at all.
func TestParseWithoutARepositoryRoot(t *testing.T) {
	t.Parallel()
	unreadable := func(path string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
	}
	cases := []struct {
		name    string
		host    Host
		args    []string
		wantErr string
	}{
		{
			name:    "no go.mod above the working directory",
			host:    Host{Getwd: fixedWd("/nowhere/deep"), Stat: moduleStat()},
			wantErr: "render-policy: no go.mod in /nowhere/deep or any parent, so the default BASE cannot be found",
		},
		{
			name:    "an unreadable working directory",
			host:    Host{Getwd: func() (string, error) { return "", errors.New("boom") }},
			wantErr: "render-policy: working directory: boom",
		},
		{
			name:    "a directory the walk cannot examine",
			host:    Host{Getwd: fixedWd("/checkout"), Stat: unreadable},
			wantErr: "render-policy: reading /checkout/go.mod",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(tc.args, tc.host); err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("Parse(%q) error = %v, want one starting %q", tc.args, err, tc.wantErr)
			}
		})
	}

	t.Run("a named BASE needs no repository root", func(t *testing.T) {
		t.Parallel()
		// Stat is nil: a walk would panic, so reaching the options proves none
		// ran.
		host := Host{Getwd: func() (string, error) { return "", errors.New("boom") }}
		opts, err := Parse([]string{"policy.yaml"}, host)
		if err != nil {
			t.Fatalf("Parse(policy.yaml): %v", err)
		}
		if opts.Base != "policy.yaml" {
			t.Errorf("Base = %s, want policy.yaml", opts.Base)
		}
	})
}
