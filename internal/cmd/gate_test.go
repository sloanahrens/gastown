package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestGateNestedModules pins the walk that tells the model which directories
// belong to their own module, and which the go tool never descends into.
func TestGateNestedModules(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, dir := range []string{"sub", "sub/deeper", ".hidden", "vendor", "testdata", "plain"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	for _, dir := range []string{".", "sub", "sub/deeper", ".hidden", "vendor", "testdata"} {
		if err := os.WriteFile(filepath.Join(root, dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatalf("writing %s/go.mod: %v", dir, err)
		}
	}

	got, err := gateNestedModules(root)
	if err != nil {
		t.Fatalf("gateNestedModules: %v", err)
	}
	// sub/deeper is inside sub, so sub covers it; the go tool skips the
	// hidden, vendor and testdata trees.
	if want := []string{"sub"}; !slices.Equal(got, want) {
		t.Errorf("nested modules = %v, want %v", got, want)
	}
}

// TestParseLandingRefs pins the git log record parser and its refusal of a
// cut-short stream.
func TestParseLandingRefs(t *testing.T) {
	t.Parallel()
	sha1, sha2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	tests := []struct {
		name     string
		out      string
		wantSHAs []string
		wantErr  bool
	}{
		{
			name:     "two records",
			out:      sha1 + "\x00s1\x00land: one\x00\n" + sha2 + "\x00s2\x00land: two\x00\n",
			wantSHAs: []string{sha1, sha2},
		},
		{name: "no landings", out: ""},
		{name: "cut-short record", out: sha1 + "\x00s1\x00", wantErr: true},
		{name: "not a sha", out: "--all\x00s1\x00land: one\x00\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			landings, err := parseLandingRefs(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseLandingRefs(%q) = %v, want an error", tt.out, landings)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLandingRefs: %v", err)
			}
			var shas []string
			for _, l := range landings {
				shas = append(shas, l.SHA)
			}
			if !slices.Equal(shas, tt.wantSHAs) {
				t.Errorf("shas = %v, want %v", shas, tt.wantSHAs)
			}
		})
	}
}

// TestParseGateGraph pins the refusal of a go list run that did not finish: a
// partial listing would miss the dependents of the packages it did not print,
// so the graph is unreadable and the model runs everything.
func TestParseGateGraph(t *testing.T) {
	t.Parallel()
	stream := []byte(`{"ImportPath":"example.com/fx/internal/a","Module":{"Path":"example.com/fx","Main":true}}` + "\n")

	tests := []struct {
		name    string
		out     []byte
		runErr  error
		wantNil bool
	}{
		{name: "complete listing", out: stream},
		{name: "listing exited non-zero", out: stream, runErr: errors.New("exit status 1"), wantNil: true},
		{name: "no output", out: nil, wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := parseGateGraph(tt.out, tt.runErr)
			if (g == nil) != tt.wantNil {
				t.Fatalf("parseGateGraph graph nil = %v, want %v", g == nil, tt.wantNil)
			}
		})
	}
}

// TestGateModelRefusesBadFlags drives the command's own refusals before it
// reads any repository.
func TestGateModelRefusesBadFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts gateModelOptions
		want string
	}{
		{name: "no logs", opts: gateModelOptions{Last: 1, CPUs: 1}, want: "--logs"},
		{name: "last below one", opts: gateModelOptions{Logs: "logs", Last: 0, CPUs: 1}, want: "--last"},
		{name: "cpus below one", opts: gateModelOptions{Logs: "logs", Last: 1, CPUs: -1}, want: "--cpus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := gateModel(&cobra.Command{}, tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("gateModel = %v, want a refusal naming %s", err, tt.want)
			}
		})
	}
}
