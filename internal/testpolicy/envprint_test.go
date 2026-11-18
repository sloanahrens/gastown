package testpolicy

import (
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEnvPrintGuardDetectsAndExempts drives every shape the guard judges: an
// environment in a failure message is reported however it is reached (direct,
// indexed, nested in a formatting call, or a name-based fallback), while a
// count, a test-authored literal and the env-print-ok comment pass.
func TestEnvPrintGuardDetectsAndExempts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want []string // "line:rule", one per reported call
	}{
		{
			name: "identifier env",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	t.Fatalf("env = %v", env)
}`,
			want: []string{"4:env-print"},
		},
		{
			name: "every env identifier name",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	t.Errorf("a %v", environ)
	t.Errorf("b %v", cmdEnv)
	t.Errorf("c %v", gotEnv)
}`,
			want: []string{"4:env-print", "5:env-print", "6:env-print"},
		},
		{
			name: "selector ending in Env",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	if cmdEnv := command(); cmdEnv == nil {
		t.Errorf("cmd = %v", cmdEnv.Env)
	}
}`,
			want: []string{"5:env-print"},
		},
		{
			name: "os.Environ call",
			src: `package p
import (
	"os"
	"testing"
)
func TestX(t *testing.T) {
	t.Logf("host environment: %v", os.Environ())
}`,
			want: []string{"7:env-print"},
		},
		{
			name: "a count is not the slice",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	t.Fatalf("env has %d entries", len(env))
}`,
			want: nil,
		},
		{
			name: "an identifier whose name ends in Env",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	t.Fatalf("want = %v", wantEnv)
}`,
			want: []string{"4:env-print"},
		},
		{
			name: "an environment nested in a formatting call",
			src: `package p
import (
	"fmt"
	"testing"
)
func helper(t *testing.T, env []string) {
	t.Errorf("nested %v", fmt.Sprintf("%v", env))
}`,
			want: []string{"7:env-print"},
		},
		{
			name: "an indexed entry",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	t.Errorf("first %q", env[0])
}`,
			want: []string{"4:env-print"},
		},
		{
			name: "the non-format method",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	t.Error(env)
}`,
			want: []string{"4:env-print"},
		},
		{
			name: "a scalar named env is not the environment",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	for _, env := range []string{"kind", "cloud"} {
		t.Errorf("env %q", env)
	}
}`,
			want: nil,
		},
		{
			name: "a slice of test literals cannot carry a host value",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	expected := []string{"A=1"}
	t.Errorf("expected %v", expected)
}`,
			want: nil,
		},
		{
			name: "a slice literal holding an environment entry",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	expected := []string{env[0]}
	t.Errorf("expected %v", expected)
}`,
			want: []string{"5:env-print"},
		},
		{
			name: "a function that builds a []string for an environment",
			src: `package p
import "testing"
func childEnv() []string { return nil }
func TestX(t *testing.T) {
	got := childEnv()
	t.Errorf("child %v", got)
}`,
			want: []string{"6:env-print"},
		},
		{
			name: "a comparison prints a bool, not the field",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	var cmd struct{ Env []string }
	t.Errorf("nil = %v", cmd.Env == nil)
}`,
			want: nil,
		},
		{
			name: "exemption comment on the call line",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	t.Fatalf("endpoint = %v", env) // testpolicy:env-print-ok a host and a port
}`,
			want: nil,
		},
		{
			name: "exemption comment on the line above",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	// testpolicy:env-print-ok a host and a port
	t.Fatalf("endpoint = %v", env)
}`,
			want: nil,
		},
		{
			name: "a marked call does not exempt the call below it",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	t.Fatalf("endpoint = %v", env) // testpolicy:env-print-ok a host and a port
	t.Fatalf("env = %v", env)
}`,
			want: []string{"5:env-print"},
		},
		{
			name: "a marker above one call does not exempt the next call",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	// testpolicy:env-print-ok a host and a port
	t.Fatalf("endpoint = %v", env)
	t.Fatalf("env = %v", env)
}`,
			want: []string{"6:env-print"},
		},
		{
			name: "a marker above a line exempts only its first call",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	// testpolicy:env-print-ok a host and a port
	t.Fatalf("endpoint = %v", env); t.Fatalf("env = %v", env)
}`,
			want: []string{"5:env-print"},
		},
		{
			name: "an exemption without a reason is reported",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	t.Fatalf("env = %v", env) // testpolicy:env-print-ok
}`,
			want: []string{"4:env-print"},
		},
		{
			name: "a comment on an unrelated line does not exempt",
			src: `package p
import "testing"
func helper(t *testing.T, env []string) {
	// testpolicy:env-print-ok says nothing about this call
	_ = 0
	t.Fatalf("env = %v", env)
}`,
			want: []string{"6:env-print"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeTemp(t, dir, "x_test.go", tc.src)
			vs, err := CheckEnvPrints(dir)
			if err != nil {
				t.Fatalf("CheckEnvPrints: %v", err)
			}
			var got []string
			for _, v := range vs {
				got = append(got, strconv.Itoa(v.Pos.Line)+":"+v.Rule)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("violations = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEnvPrintGuardRefusesAnUnparsableFile pins that a file the scan cannot
// read is an error, not a clean result.
func TestEnvPrintGuardRefusesAnUnparsableFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTemp(t, dir, "broken_test.go", "package p\nfunc TestX( {\n")
	if _, err := CheckEnvPrints(dir); err == nil {
		t.Fatal("CheckEnvPrints returned no error for an unparsable file")
	}
}

// TestEnvPrintGuardRefusesAnUnreadableFile pins that a test file the walk lists
// but cannot read is an error, not a clean result. A dangling symlink is the one
// entry a walk lists and a read always fails on, whatever the user can read.
func TestEnvPrintGuardRefusesAnUnreadableFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "missing.go"), filepath.Join(dir, "x_test.go")); err != nil {
		t.Fatalf("creating the dangling symlink: %v", err)
	}
	if _, err := CheckEnvPrints(dir); err == nil {
		t.Fatal("CheckEnvPrints returned no error for an unreadable test file")
	}
}

// TestEnvPrintGuardRefusesAnUnparsableSibling pins that a package file the scan
// cannot read is an error, not a package with fewer names to judge.
func TestEnvPrintGuardRefusesAnUnparsableSibling(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTemp(t, dir, "x_test.go", "package p\n")
	writeTemp(t, dir, "broken.go", "package p\nfunc Broken( {\n")
	if _, err := CheckEnvPrints(dir); err == nil {
		t.Fatal("CheckEnvPrints returned no error for an unparsable package file")
	}
}

// TestEnvPrintGuardRefusesAMissingRoot pins that a root the walk cannot enter
// is an error, not a clean result.
func TestEnvPrintGuardRefusesAMissingRoot(t *testing.T) {
	t.Parallel()
	if _, err := CheckEnvPrints(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("CheckEnvPrints returned no error for a missing root")
	}
}

// TestEnvPrintGuardOnTree runs the guard over every test file under internal
// and cmd. It is the gate that keeps an environment slice out of a failure
// message anywhere in the tree.
func TestEnvPrintGuardOnTree(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var vs []Violation
	for _, sub := range []string{"internal", "cmd"} {
		found, err := CheckEnvPrints(filepath.Join(root, sub))
		if err != nil {
			t.Fatalf("%s: %v", sub, err)
		}
		vs = append(vs, found...)
	}
	for _, v := range vs {
		rel, err := filepath.Rel(root, v.Pos.Filename)
		if err != nil {
			rel = v.Pos.Filename
		}
		t.Error(Violation{
			Pos:  token.Position{Filename: rel, Line: v.Pos.Line, Column: v.Pos.Column},
			Rule: v.Rule,
			Msg:  v.Msg,
		}.String())
	}
}

func writeTemp(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
