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

// TestEnvPrintGuardDetectsAndExempts drives every shape the guard judges: each
// env slice in a failure message is reported, a count is not, and the
// env-print-ok comment skips the call it marks.
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
func TestX(t *testing.T) {
	if env := []string{"A=1"}; len(env) != 2 {
		t.Fatalf("env = %v", env)
	}
}`,
			want: []string{"5:env-print"},
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
func TestX(t *testing.T) {
	env := []string{"A=1"}
	t.Fatalf("env has %d entries", len(env))
}`,
			want: nil,
		},
		{
			name: "a selector that only ends in the letters Env is not the field",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	t.Fatalf("want = %v", wantEnv)
}`,
			want: nil,
		},
		{
			name: "exemption comment on the call line",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	env := []string{"BEADS_DOLT_SERVER_HOST=127.0.0.1", "BEADS_DOLT_PORT=4407"}
	t.Fatalf("endpoint = %v", env) // testpolicy:env-print-ok a host and a port
}`,
			want: nil,
		},
		{
			name: "exemption comment on the line above",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	env := []string{"BEADS_DOLT_SERVER_HOST=127.0.0.1", "BEADS_DOLT_PORT=4407"}
	// testpolicy:env-print-ok a host and a port
	t.Fatalf("endpoint = %v", env)
}`,
			want: nil,
		},
		{
			name: "a comment on an unrelated line does not exempt",
			src: `package p
import "testing"
func TestX(t *testing.T) {
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
