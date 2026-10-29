package uninstallgt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// vars answers getenv from m, so no test reads the developer's environment.
func vars(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestParseTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		want    Options
		wantErr string
		help    bool
	}{
		{name: "no arguments", want: Options{}},
		{
			name: "the value as the next argument",
			args: []string{"--install-dir", "/opt/bin"},
			want: Options{InstallDir: "/opt/bin"},
		},
		{
			name: "the value after =",
			args: []string{"--install-dir=/opt/bin"},
			want: Options{InstallDir: "/opt/bin"},
		},
		{
			name: "a value holding =",
			args: []string{"--install-dir=/a=b"},
			want: Options{InstallDir: "/a=b"},
		},
		{name: "--force", args: []string{"--force"}, want: Options{Force: true}},
		{
			name: "both flags in either order",
			args: []string{"--force", "--install-dir", "/opt/bin"},
			want: Options{InstallDir: "/opt/bin", Force: true},
		},
		{
			name: "the last --install-dir wins",
			args: []string{"--install-dir", "/first", "--install-dir=/second"},
			want: Options{InstallDir: "/second"},
		},
		{
			name: "an empty value names no directory",
			args: []string{"--install-dir="},
			want: Options{},
		},
		{name: "-h asks for help", args: []string{"-h"}, help: true},
		{name: "--help asks for help", args: []string{"--help"}, help: true},
		{
			name:    "--install-dir with no value",
			args:    []string{"--install-dir"},
			wantErr: "uninstall-gt: --install-dir needs a directory",
		},
		{
			name:    "an unknown flag",
			args:    []string{"--bogus"},
			wantErr: "uninstall-gt: unknown argument: --bogus",
		},
		{
			name:    "a positional argument is unknown",
			args:    []string{"/opt/bin"},
			wantErr: "uninstall-gt: unknown argument: /opt/bin",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.args)
			if tt.help {
				if !errors.Is(err, ErrHelp) {
					t.Fatalf("Parse(%q) error = %v, want ErrHelp", tt.args, err)
				}
				return
			}
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("Parse(%q) error = %v, want %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error = %v, want none", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestResolvePrecedenceAndSource(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	flagDir := filepath.Join(base, "from-flag")
	gtBinDir := filepath.Join(base, "from-install-gt-bin-dir")
	installDir := filepath.Join(base, "from-install-dir")
	both := map[string]string{
		"HOME":               home,
		"INSTALL_GT_BIN_DIR": gtBinDir,
		"INSTALL_DIR":        installDir,
	}

	tests := []struct {
		name string
		opts Options
		vars map[string]string
		want Target
	}{
		{
			name: "the flag beats both variables",
			opts: Options{InstallDir: flagDir},
			vars: both,
			want: Target{Dir: flagDir, Source: SourceFlag},
		},
		{
			name: "INSTALL_GT_BIN_DIR beats INSTALL_DIR",
			vars: both,
			want: Target{Dir: gtBinDir, Source: SourceInstallGTBinDir},
		},
		{
			name: "INSTALL_DIR alone",
			vars: map[string]string{"HOME": home, "INSTALL_DIR": installDir},
			want: Target{Dir: installDir, Source: SourceInstallDir},
		},
		{
			name: "the default when nothing names a directory",
			vars: map[string]string{"HOME": home},
			want: Target{Dir: filepath.Join(home, ".local", "bin"), Source: SourceDefault},
		},
		{
			// Parse answers this for "--install-dir=", and the script fell
			// through to the environment for the same empty value.
			name: "an empty flag value falls to the variables",
			vars: both,
			want: Target{Dir: gtBinDir, Source: SourceInstallGTBinDir},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.opts, vars(tt.vars))
			if err != nil {
				t.Fatalf("Resolve(%+v) error = %v, want none", tt.opts, err)
			}
			if got != tt.want {
				t.Errorf("Resolve(%+v) = %+v, want %+v", tt.opts, got, tt.want)
			}
		})
	}
}

func TestRefusalsTable(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "regular-file")
	if err := os.WriteFile(file, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(base, "bin")

	tests := []struct {
		name string
		opts Options
		vars map[string]string
		want string
	}{
		{
			name: "a relative path",
			opts: Options{InstallDir: "relative/bin"},
			vars: map[string]string{"HOME": home},
			want: "--install-dir must be an absolute path: 'relative/bin'",
		},
		{
			// The script's message names --install-dir whichever source gave
			// the path.
			name: "a relative variable",
			vars: map[string]string{"HOME": home, "INSTALL_DIR": "bin"},
			want: "--install-dir must be an absolute path: 'bin'",
		},
		{
			name: "the filesystem root",
			opts: Options{InstallDir: "/"},
			vars: map[string]string{"HOME": home},
			want: "the filesystem root is not an install directory",
		},
		{
			name: "HOME itself",
			opts: Options{InstallDir: home},
			vars: map[string]string{"HOME": home},
			want: "the home directory itself is not an install directory",
		},
		{
			name: "a path that is a regular file",
			opts: Options{InstallDir: file},
			vars: map[string]string{"HOME": home},
			want: "--install-dir is not a directory: " + file,
		},
		{
			name: "HOME unset even though the flag names a directory",
			opts: Options{InstallDir: binDir},
			vars: map[string]string{},
			want: "HOME is not set; name the directory with --install-dir",
		},
		{
			name: "HOME unset with only a variable",
			vars: map[string]string{"INSTALL_DIR": binDir},
			want: "HOME is not set; name the directory with --install-dir",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.opts, vars(tt.vars))
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("Resolve(%+v) error = %v, want a *Refusal", tt.opts, err)
			}
			if refusal.Reason != tt.want {
				t.Errorf("Resolve(%+v) refused with %q, want %q", tt.opts, refusal.Reason, tt.want)
			}
			if got != (Target{}) {
				t.Errorf("Resolve(%+v) = %+v beside its refusal, want the zero Target", tt.opts, got)
			}
		})
	}
}

func TestTrailingSlashesTrimmedButRootKept(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	v := vars(map[string]string{"HOME": home})

	trimmed := map[string]string{
		dir + "/":   dir,
		dir + "///": dir,
	}
	for arg, want := range trimmed {
		got, err := Resolve(Options{InstallDir: arg}, v)
		if err != nil {
			t.Fatalf("Resolve(--install-dir %s) error = %v, want none", arg, err)
		}
		if got.Dir != want {
			t.Errorf("Resolve(--install-dir %s) = %q, want %q", arg, got.Dir, want)
		}
	}

	// Trimming a lone "/" must not leave the empty string: that would report a
	// relative path instead of the root the run refuses.
	for _, arg := range []string{"/", "//", "///"} {
		_, err := Resolve(Options{InstallDir: arg}, v)
		var refusal *Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("Resolve(--install-dir %s) error = %v, want a *Refusal", arg, err)
		}
		const want = "the filesystem root is not an install directory"
		if refusal.Reason != want {
			t.Errorf("Resolve(--install-dir %s) refused with %q, want %q", arg, refusal.Reason, want)
		}
	}
}

func TestNeverResolvesFromPATH(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	live := filepath.Join(home, ".local", "bin")
	pathBin := filepath.Join(base, "path-bin")
	for _, dir := range []string{live, pathBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The files a PATH lookup would answer with. They are not executable
	// because a unit test may not create an executable file (docs/testing.md,
	// "The rules"); exec.LookPath still finds a non-executable file and
	// reports it is not runnable, so resolving through PATH lands anywhere but
	// the default below.
	for _, name := range []string{"gt", "bd"} {
		if err := os.WriteFile(filepath.Join(pathBin, name), []byte("not a program\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Resolve(Options{}, vars(map[string]string{"HOME": home, "PATH": pathBin}))
	if err != nil {
		t.Fatalf("Resolve with gt and bd only on PATH: %v", err)
	}
	if got.Dir != live {
		t.Errorf("Resolve took the directory %q from PATH, want %q", got.Dir, live)
	}
	if got.Source != SourceDefault {
		t.Errorf("Resolve named the source %q, want %q", got.Source, SourceDefault)
	}
}
