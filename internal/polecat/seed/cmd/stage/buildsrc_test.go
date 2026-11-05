package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	buildSHA = "f05e821cda74197a137049fc69f9a46c0c23c78f"
	pinSHA   = "92b9e510ac2f9ff8c80b0291f312ac482bf96c38"
	testMod  = "module example.com/gastown\n\ngo 1.21\n\nrequire github.com/x/a v1.0.0\n"
)

// recordingGit answers the stage command's read-only git calls and records the
// directory each was asked of, so a test can tell which checkout was read. A
// verb in errs fails instead, which drives the refusal paths.
type recordingGit struct {
	t     *testing.T
	calls []string
	errs  map[string]error
}

func (g *recordingGit) run(dir string, args ...string) ([]byte, error) {
	g.t.Helper()
	g.calls = append(g.calls, dir+" "+strings.Join(args, " "))
	if len(args) == 0 {
		return nil, fmt.Errorf("no git verb")
	}
	if err := g.errs[args[0]]; err != nil {
		return nil, err
	}
	switch args[0] {
	case "cat-file":
		return []byte("commit\n"), nil
	case "ls-tree":
		return []byte("go.mod\n"), nil
	case "show":
		return []byte(testMod), nil
	case "archive":
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.Close(); err != nil {
			g.t.Fatalf("close the archive: %v", err)
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("unexpected git call: %v", args)
}

// TestBuildSrcFlag pins the flag: absent, the build rig is read from the town
// checkout; given, from that directory; and an empty or missing directory is
// refused naming the flag before any git call.
func TestBuildSrcFlag(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	townCheckout := filepath.Join(town, "gastown", "mayor", "rig")
	if err := os.MkdirAll(townCheckout, 0o755); err != nil {
		t.Fatalf("make the town checkout: %v", err)
	}
	buildSrc := t.TempDir()
	pins := filepath.Join(t.TempDir(), "pins")
	if err := os.WriteFile(pins, []byte("gastown "+pinSHA+"\n"), 0o644); err != nil {
		t.Fatalf("write the pins file: %v", err)
	}

	tests := []struct {
		name    string
		args    []string
		wantSrc string // every git read must name this directory
		wantRC  int
		wantErr string // substring the refusal must print
	}{
		{
			name:    "without the flag the town checkout is read",
			args:    []string{"-pins", pins, "-town", town, "-dest", t.TempDir(), "-build-commit", buildSHA},
			wantSrc: townCheckout,
		},
		{
			name:    "with the flag the given directory is read",
			args:    []string{"-pins", pins, "-town", town, "-dest", t.TempDir(), "-build-commit", buildSHA, "-build-src", buildSrc},
			wantSrc: buildSrc,
		},
		{
			name:    "an empty build source is refused",
			args:    []string{"-pins", pins, "-town", town, "-dest", t.TempDir(), "-build-commit", buildSHA, "-build-src", ""},
			wantRC:  2,
			wantErr: "-build-src",
		},
		{
			name:    "a missing build source is refused",
			args:    []string{"-pins", pins, "-town", town, "-dest", t.TempDir(), "-build-commit", buildSHA, "-build-src", filepath.Join(t.TempDir(), "absent")},
			wantRC:  2,
			wantErr: "-build-src",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &recordingGit{t: t}
			var stdout, stderr bytes.Buffer
			rc := runWith(tt.args, &stdout, &stderr, g.run)
			if rc != tt.wantRC {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", rc, tt.wantRC, stdout.String(), stderr.String())
			}
			if tt.wantErr != "" {
				if !strings.Contains(stderr.String(), tt.wantErr) {
					t.Errorf("refusal does not name %q: %s", tt.wantErr, stderr.String())
				}
				if len(g.calls) != 0 {
					t.Errorf("refused after git calls: %v", g.calls)
				}
				return
			}
			if len(g.calls) == 0 {
				t.Fatal("the stage made no git calls")
			}
			for _, c := range g.calls {
				if !strings.HasPrefix(c, tt.wantSrc+" ") {
					t.Errorf("read %q, want every read under %s", c, tt.wantSrc)
				}
			}
		})
	}
}

// TestBuildSrcAbsentKeepsTheTownsAdvice pins that omitting the flag is
// behaviour-identical to the command before it existed: the error names the
// town's canonical checkout and advises fetching through the town, never
// naming a build source or offering build-checkout advice.
func TestBuildSrcAbsentKeepsTheTownsAdvice(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	townCheckout := filepath.Join(town, "gastown", "mayor", "rig")
	pins := filepath.Join(t.TempDir(), "pins")
	if err := os.WriteFile(pins, []byte("gastown "+pinSHA+"\n"), 0o644); err != nil {
		t.Fatalf("write the pins file: %v", err)
	}
	args := []string{"-pins", pins, "-town", town, "-dest", t.TempDir(), "-build-commit", buildSHA}

	tests := []struct {
		name    string
		errs    map[string]error
		want    []string
		notWant []string
	}{
		{
			name:    "the build checkout lacks the build commit",
			errs:    map[string]error{"cat-file": errors.New("fatal: Not a valid object name")},
			want:    []string{"cannot read the build commit", townCheckout, "fetch that checkout through the town's path"},
			notWant: []string{"build from a checkout that holds"},
		},
		{
			name:    "the pin cannot be read for the module comparison",
			errs:    map[string]error{"ls-tree": errors.New("fatal: Not a valid object name")},
			want:    []string{"not readable in its canonical checkout"},
			notWant: []string{"the build source"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &recordingGit{t: t, errs: tt.errs}
			var stdout, stderr bytes.Buffer
			if rc := runWith(args, &stdout, &stderr, g.run); rc != 1 {
				t.Fatalf("exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", rc, stdout.String(), stderr.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("refusal lacks %q: %s", want, stderr.String())
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(stderr.String(), notWant) {
					t.Errorf("refusal names %q, but the flag was not given: %s", notWant, stderr.String())
				}
			}
		})
	}
}
