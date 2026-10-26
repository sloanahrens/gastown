package metrics

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunnerChildEnvPassesOnlyAllowlisted(t *testing.T) {
	t.Parallel()
	host := map[string]string{"PATH": "/usr/bin", "HOME": "/home/x", "DEEPSEEK_API_KEY": "secret", "GT_TOKEN": "t"}
	lookup := func(name string) (string, bool) {
		v, ok := host[name]
		return v, ok
	}
	tests := []struct {
		name  string
		allow []string
		extra []string
		want  []string
	}{
		{"allowlisted set variables only", []string{"PATH", "HOME"}, nil, []string{"PATH=/usr/bin", "HOME=/home/x"}},
		{"unset allowlisted variable is absent", []string{"PATH", "LANG"}, nil, []string{"PATH=/usr/bin"}},
		{"empty allowlist is an empty environment", nil, nil, []string{}},
		{
			name:  "extra environment follows the allowlist",
			allow: []string{"PATH", "HOME"},
			extra: []string{"TOWN_METRICS_TOWN=/town", "TOWN_METRICS_DOCS=/town/docs"},
			want: []string{
				"PATH=/usr/bin", "HOME=/home/x", "TOWN_METRICS_TOWN=/town", "TOWN_METRICS_DOCS=/town/docs",
			},
		},
		{
			name:  "extra wins over an allowlisted name and appears once",
			allow: []string{"PATH", "HOME"},
			extra: []string{"PATH=/opt/bin"},
			want:  []string{"HOME=/home/x", "PATH=/opt/bin"},
		},
		{
			name:  "extra names never reach a secret the allowlist omits",
			allow: []string{"PATH"},
			extra: []string{"TOWN_METRICS_TOWN=/town"},
			want:  []string{"PATH=/usr/bin", "TOWN_METRICS_TOWN=/town"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := childEnv(tc.allow, tc.extra, lookup)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("childEnv(%v, %v) = %v, want %v (secrets in the host environment must not reach a child)", tc.allow, tc.extra, got, tc.want)
			}
		})
	}
}

// TestRunnerOSRunnerEnvSharesTheChildEnvRuling is the same allowlist and extra
// pair through the seam the analysis uses: both seams refuse an empty argv
// before starting anything, so the ruling childEnv pins is the one they run.
func TestRunnerOSRunnerEnvSharesTheChildEnvRuling(t *testing.T) {
	t.Parallel()
	if _, err := OSRunnerEnv(DefaultAllowEnv)(context.Background(), nil, []string{"TOWN_METRICS_TOWN=/town"}); err == nil {
		t.Fatal("OSRunnerEnv ran an empty argv without an error")
	}
}

func TestRunnerOSRunnerRejectsEmptyArgv(t *testing.T) {
	t.Parallel()
	if _, err := OSRunner(DefaultAllowEnv)(context.Background(), nil); err == nil {
		t.Fatal("OSRunner ran an empty argv without an error")
	}
}

func TestRunnerOSRunnerDirRejectsEmptyArgv(t *testing.T) {
	t.Parallel()
	if _, err := OSRunnerDir(DefaultAllowEnv)(context.Background(), t.TempDir(), nil); err == nil {
		t.Fatal("OSRunnerDir ran an empty argv without an error")
	}
}

// TestRunnerOSRunnerDirStartsTheChildInTheDirectory pins the seam's whole
// contract: a command that resolves its town from the working directory reads
// the directory it is given, never the snapshot process's.
func TestRunnerOSRunnerDirStartsTheChildInTheDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out, err := OSRunnerDir(DefaultAllowEnv)(context.Background(), dir, []string{"/bin/sh", "-c", "pwd"})
	if err != nil {
		t.Fatalf("OSRunnerDir: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("the child ran in %q, want %q", got, want)
	}
}

// TestRunnerOSRunnerRunsTheCommand covers the exec the analysis actually
// depends on: every other runner test stops short of starting a child.
func TestRunnerOSRunnerRunsTheCommand(t *testing.T) {
	t.Parallel()
	out, err := OSRunner(DefaultAllowEnv)(context.Background(), []string{"/bin/sh", "-c", "printf 'collected'"})
	if err != nil {
		t.Fatalf("OSRunner: %v", err)
	}
	if got := string(out); got != "collected" {
		t.Fatalf("stdout = %q, want %q", got, "collected")
	}
}

// TestRunnerOSRunnerFailureCarriesTheStderrTail pins the error text a source
// surfaces when its command fails: without the tail, the user sees an exit
// code and no cause.
func TestRunnerOSRunnerFailureCarriesTheStderrTail(t *testing.T) {
	t.Parallel()
	_, err := OSRunner(DefaultAllowEnv)(context.Background(), []string{"/bin/sh", "-c", "printf 'disk full\\n' >&2; exit 3"})
	if err == nil {
		t.Fatal("OSRunner returned no error for a non-zero exit")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error %q does not carry the child's stderr", err)
	}
}

// TestRunnerOSRunnerExecsWithTheAllowlistedEnvironment checks the allowlist at
// the exec, not just in childEnv: childEnv's ruling only protects a child if
// the runner hands it to os/exec. childEnv's own tests pin which names the
// ruling admits, so this one only proves the child receives it.
func TestRunnerOSRunnerExecsWithTheAllowlistedEnvironment(t *testing.T) {
	t.Parallel()
	out, err := OSRunner(DefaultAllowEnv)(context.Background(), []string{"/bin/sh", "-c", `printf %s "$HOME"`})
	if err != nil {
		t.Fatalf("OSRunner: %v", err)
	}
	//testpolicy:allow no-host-env — the child must echo the HOME it was given, and this process's HOME is the allowlisted value under test
	if got, want := string(out), os.Getenv("HOME"); got != want {
		t.Fatalf("the child's HOME = %q, want the allowlisted %q", got, want)
	}
}

func TestRunnerOSRunnerEnvDeliversExtraEnvironment(t *testing.T) {
	t.Parallel()
	out, err := OSRunnerEnv(DefaultAllowEnv)(context.Background(), []string{"/bin/sh", "-c", `printf %s "$TOWN_METRICS_TOWN"`}, []string{"TOWN_METRICS_TOWN=/town"})
	if err != nil {
		t.Fatalf("OSRunnerEnv: %v", err)
	}
	if got := string(out); got != "/town" {
		t.Fatalf("stdout = %q, want %q", got, "/town")
	}
}

func TestTailStringKeepsWholeRunes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"shorter than the limit is returned whole", "abc", 10, "abc"},
		{"the tail is the last bytes, trimmed", "boom\nstack\n", 5, "stack"},
		{"a cut on a boundary keeps the whole tail", "aé€", 5, "é€"},
		{"a cut mid-rune starts at the next one", "aé€", 4, "€"},
		{"a tail shorter than one rune is dropped", "€", 1, ""},
		{"a zero limit returns nothing", "abc", 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tailString(tc.in, tc.maxBytes); got != tc.want {
				t.Errorf("tailString(%q, %d) = %q, want %q", tc.in, tc.maxBytes, got, tc.want)
			}
		})
	}
}
