package daemonimage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// planBuilder is a Builder whose every seam fails the test if it is used: Plan
// runs nothing, so it needs none of them.
func planBuilder(t *testing.T) Builder {
	t.Helper()
	root := t.TempDir()
	dockerfile := filepath.Join(root, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	return Builder{
		Run: func(context.Context, string, []string, []string) error {
			t.Error("Plan ran a command")
			return nil
		},
		Git: func(string, ...string) ([]byte, error) {
			t.Error("Plan ran git")
			return nil, nil
		},
		Fetch: func(context.Context, string) (io.ReadCloser, error) {
			t.Error("Plan reached the network")
			return nil, nil
		},
		Out:        out,
		GTSrc:      "/src/gt",
		BDSrc:      "/src/beads",
		BDCommit:   testBDCommit,
		OMSrc:      "/src/om",
		Dockerfile: dockerfile,
	}
}

// Plan names each step a full build runs, in order, with the argv the build runs,
// and runs, writes and fetches nothing. The claude step names the URL that
// FetchClaudeCLI requests, taken from the fetch itself.
func TestPlanNamesEachStepAndRunsNothing(t *testing.T) {
	t.Parallel()
	b := planBuilder(t)
	var fetched string
	b.Fetch = func(_ context.Context, url string) (io.ReadCloser, error) {
		fetched = url
		return nil, errors.New("fetch stubbed: Plan must print the URL, not fetch it")
	}
	if _, err := b.FetchClaudeCLI(context.Background()); err == nil {
		t.Fatal("FetchClaudeCLI with a stubbed fetch returned no error")
	}
	if fetched == "" {
		t.Fatal("FetchClaudeCLI requested no URL")
	}

	lines, err := b.Plan(testRev)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	steps := []struct{ label, names string }{
		{"build gt: ", "go build -trimpath -ldflags " + gtCommitFlag + testRev + " -o " + filepath.Join(b.Out, "gt") + " " + GTPackage + "   (clean worktree of /src/gt at " + testRev + ")"},
		{"build bd: ", "go build -trimpath -ldflags " + bdCommitFlag + testBDCommit + " -o " + filepath.Join(b.Out, "bd") + " " + BDPackage},
		{"build om: ", "go build -trimpath -o " + filepath.Join(b.Out, "om") + " " + OMPackage + "   (clean worktree of /src/om at HEAD)"},
		{"fetch claude: ", "GET " + fetched + " -> " + filepath.Join(b.Out, claudeCLIName) + "   (sha256 " + templates.ClaudeCLISHA256 + ")"},
		{"image: ", "docker --context " + DockerContext + " build -f " + b.Dockerfile + " -t " + ImageTag(testRev) + " " + b.Out},
	}
	if len(lines) != len(steps) {
		t.Fatalf("plan has %d lines, want %d:\n%s", len(lines), len(steps), strings.Join(lines, "\n"))
	}
	for i, step := range steps {
		if !strings.HasPrefix(lines[i], step.label) || !strings.Contains(lines[i], step.names) {
			t.Errorf("plan line %d = %q; want the %s step naming %q", i, lines[i], step.label, step.names)
		}
	}
	entries, err := os.ReadDir(b.Out)
	if err != nil || len(entries) != 0 {
		t.Errorf("Out holds %d entries after Plan (err %v), want none", len(entries), err)
	}
}

// Plan refuses each input the build's own checks refuse before it runs anything,
// and a refused call prints no step.
func TestPlanRefusesWhatABuildRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*Builder)
		rev     string
		want    string
	}{
		{"no output directory", func(b *Builder) { b.Out = "" }, testRev, "no output directory"},
		{"a relative output directory", func(b *Builder) { b.Out = filepath.Join("rel", "out") }, testRev, "is not absolute"},
		{"no rev", func(*Builder) {}, " ", "no rev"},
		{"a rev that is an option", func(*Builder) {}, "--upload-pack=sh", "is not a 7+ character hex commit"},
		{"no gt source", func(b *Builder) { b.GTSrc = "" }, testRev, "no gt source"},
		{"no bd source", func(b *Builder) { b.BDSrc = "" }, testRev, "no bd source"},
		{"no om source", func(b *Builder) { b.OMSrc = "" }, testRev, "no om source"},
		{"no bd commit", func(b *Builder) { b.BDCommit = "" }, testRev, "no bd commit"},
		{"a bd commit that is not hex", func(b *Builder) { b.BDCommit = "main" }, testRev, "is not a 7+ character hex commit"},
		{"no Dockerfile", func(b *Builder) { b.Dockerfile = "" }, testRev, "no Dockerfile"},
		{"a relative Dockerfile", func(b *Builder) { b.Dockerfile = "Dockerfile" }, testRev, "Dockerfile \"Dockerfile\" is not absolute"},
		{"a Dockerfile that is not a file", func(b *Builder) { b.Dockerfile = filepath.Join(b.Out, "absent") }, testRev, "is not a file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := planBuilder(t)
			tc.breakIt(&b)
			lines, err := b.Plan(tc.rev)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Plan = %q, %v; want an error naming %q", lines, err, tc.want)
			}
			if len(lines) != 0 {
				t.Errorf("a refused plan listed %q", lines)
			}
		})
	}
}

// DockerEnv keeps what docker needs and drops the variables that would pick
// another engine, whatever the shell holds.
func TestDockerEnvDropsTheShellsEngineChoice(t *testing.T) {
	t.Parallel()
	got := DockerEnv([]string{"PATH=/usr/bin", "DOCKER_CONTEXT=other", "DOCKER_HOST=tcp://x:2375", "HOME=/h", "GOFLAGS=-mod=mod"})
	if strings.Join(got, " ") != "PATH=/usr/bin HOME=/h" {
		t.Errorf("DockerEnv = %v, want PATH and HOME only", got)
	}
}
