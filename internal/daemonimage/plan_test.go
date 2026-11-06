package daemonimage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/buildstamp"
	"github.com/steveyegge/gastown/internal/templates"
)

// planPins and planPinsPath are the four pins planBuilder is given: the same
// shape the committed file has, with the test commits.
var planPins = []Pin{
	{Name: "bd", URL: "http://forgejo:3000/sloan/beads.git", Commit: testBDCommit},
	{Name: "om", URL: "http://forgejo:3000/sloan/organic-mechanic.git", Commit: testOMCommit},
	{Name: "hm", URL: "http://forgejo:3000/sloan/history-man.git", Commit: testHMCommit},
	{Name: "devops", URL: "http://forgejo:3000/sloan/devops.git", Commit: testDevopsCommit},
}

const planPinsPath = "/repo/deploy/daemon/image-pins"

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
		Out:          out,
		GTSrc:        "/src/gt",
		BDSrc:        "/src/beads",
		BDCommit:     testBDCommit,
		OMSrc:        "/src/om",
		OMCommit:     testOMCommit,
		HMSrc:        "/src/hm",
		HMCommit:     testHMCommit,
		DevopsSrc:    "/src/devops",
		DevopsCommit: testDevopsCommit,
		MangoSrc:     "/src/mango",
		MangoCommit:  testMangoCommit,
		Pins:         planPins,
		PinsPath:     planPinsPath,
		Dockerfile:   dockerfile,
	}
}

// Plan names the pins file and each pin first, then each step a full build runs,
// in order, with the argv the build runs, and runs, writes and fetches nothing.
// The claude step names the URL that FetchClaudeCLI requests, taken from the
// fetch itself. The gt line stamps the first nine characters of the named
// commit, the cut the build stamps. The bd line prints planBuildID for
// main.Build, the argv value only the worktree a build makes can answer: Plan
// makes none, and the Git seam above fails the test if it reads one from
// anywhere. The om line names its pin, never HEAD.
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
		{"pins: ", planPinsPath},
		{"pin bd: ", "http://forgejo:3000/sloan/beads.git at " + testBDCommit},
		{"pin om: ", "http://forgejo:3000/sloan/organic-mechanic.git at " + testOMCommit},
		{"pin hm: ", "http://forgejo:3000/sloan/history-man.git at " + testHMCommit},
		{"pin devops: ", "http://forgejo:3000/sloan/devops.git at " + testDevopsCommit},
		{"build gt: ", "go build -trimpath -ldflags " + gtLdflags(testRev[:buildstamp.Len]) + " -o " + filepath.Join(b.Out, "gt") + " " + GTPackage + "   (clean worktree of /src/gt at " + testRev + ")"},
		{"build bd: ", "go build -trimpath -ldflags " + bdBuildFlag + planBuildID + " " + bdCommitFlag + testBDCommit + " -o " + filepath.Join(b.Out, "bd") + " " + BDPackage + "   (clean worktree of /src/beads at " + testBDCommit + ")"},
		{"build om: ", "go build -trimpath -o " + filepath.Join(b.Out, "om") + " " + OMPackage + "   (clean worktree of /src/om at " + testOMCommit + ")"},
		{"build app-promote: ", "go build -trimpath -o " + filepath.Join(b.Out, "plugins", "app-promote", "app-promote") + " ./plugins/app-promote   (clean worktree of /src/gt at " + testRev + ")"},
		{"build town-metrics: ", "go build -trimpath -o " + filepath.Join(b.Out, "plugins", "town-metrics", "town-metrics") + " ./plugins/town-metrics   (clean worktree of /src/gt at " + testRev + ")"},
		{"build hm: ", "go build -trimpath -o " + filepath.Join(b.Out, "hm") + " " + HMPackage + "   (clean worktree of /src/hm at " + testHMCommit + ")"},
		{"build devops-reporter: ", "go build -trimpath -o " + filepath.Join(b.Out, "devops-reporter") + " " + DevopsReporterPackage + "   (clean worktree of /src/devops at " + testDevopsCommit + ")"},
		{"build release-reporter: ", "go build -trimpath -o " + filepath.Join(b.Out, "release-reporter") + " " + ReleaseReporterPackage + "   (clean worktree of /src/devops at " + testDevopsCommit + ")"},
		{"check release-reporter: ", "go version -m " + filepath.Join(b.Out, "release-reporter") + "   (refused unless vMAJOR.MINOR.PATCH[-pre])"},
		{"build mango: ", "docker --context " + DockerContext + " run --rm --platform linux/arm64 -v " + planMangoWorktree + ":/src -v " + planMangoStage + ":/out -w /src/go-api golang:1.27-bookworm sh -c apt-get update && apt-get install -y --no-install-recommends gcc g++ && CGO_ENABLED=1 GOOS=linux go build -trimpath -buildvcs=false -ldflags=\"-w -s\" -o /out/mango ./cmd/mango   (clean worktree of /src/mango at " + testMangoCommit + ")"},
		{"fetch claude: ", "GET " + fetched + " -> " + filepath.Join(b.Out, claudeCLIName) + "   (sha256 " + templates.ClaudeCLISHA256 + ")"},
		{"fetch openshell: ", "GET " + templates.DaemonClientTools[0].URL + " -> " + filepath.Join(b.Out, "openshell") + "   (sha256 " + templates.DaemonClientTools[0].SHA256 + ")"},
		{"fetch docker: ", "GET " + templates.DaemonClientTools[1].URL + " -> " + filepath.Join(b.Out, "docker") + "   (sha256 " + templates.DaemonClientTools[1].SHA256 + ")"},
		{"fetch kubectl: ", "GET " + templates.DaemonClientTools[2].URL + " -> " + filepath.Join(b.Out, "kubectl") + "   (sha256 " + templates.DaemonClientTools[2].SHA256 + ")"},
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

// TestPlanShowsTheShortCommitStamp: a plan prints the stamp a build would give
// gt, so an operator reading it sees the short commit the tier sweep accepts
// (gt-c3sue).
func TestPlanShowsTheShortCommitStamp(t *testing.T) {
	t.Parallel()
	b := planBuilder(t)
	lines, err := b.Plan(testRev)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := gtCommitFlag + testRev[:buildstamp.Len] + " -o "
	for _, line := range lines {
		if !strings.HasPrefix(line, "build gt: ") {
			continue
		}
		if !strings.Contains(line, want) {
			t.Errorf("gt plan line = %q, want it to carry %q", line, want)
		}
		return
	}
	t.Fatalf("plan has no build gt: line:\n%s", strings.Join(lines, "\n"))
}

// TestPlanListsThePluginBuilds: the plan names the build of each plugin the
// container daemon runs, after the three binaries and before the fetches, with
// the package and the output path under the plugin's directory.
func TestPlanListsThePluginBuilds(t *testing.T) {
	t.Parallel()
	b := planBuilder(t)
	lines, err := b.Plan(testRev)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	om := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "build om: ") {
			om = i
		}
	}
	if om < 0 {
		t.Fatalf("plan has no build om: line:\n%s", strings.Join(lines, "\n"))
	}
	for _, p := range []struct{ name, pkg string }{
		{name: "app-promote", pkg: "./plugins/app-promote"},
		{name: "town-metrics", pkg: "./plugins/town-metrics"},
	} {
		want := "build " + p.name + ": go build -trimpath -o " + filepath.Join(b.Out, "plugins", p.name, p.name) + " " + p.pkg
		at := -1
		for i, line := range lines {
			if strings.HasPrefix(line, "build "+p.name+": ") {
				at = i
			}
		}
		if at < 0 {
			t.Errorf("plan has no build %s: line:\n%s", p.name, strings.Join(lines, "\n"))
			continue
		}
		if at <= om {
			t.Errorf("the build %s: line is at %d, want it after the three binaries (build om: is at %d)", p.name, at, om)
		}
		if !strings.HasPrefix(lines[at], want) {
			t.Errorf("the build %s: line = %q, want it to start %q", p.name, lines[at], want)
		}
	}
}

// TestPlanBuildIDIsAShellSafeToken: Plan prints main.Build's value inside a
// command line, so the placeholder standing in for the build id git answers in
// the worktree is one shell-safe token: letters, digits and underscores. A space
// there splits the printed argv into two words, and a shell reads angle brackets
// as redirection (om finding 31169d4cb6bf).
func TestPlanBuildIDIsAShellSafeToken(t *testing.T) {
	t.Parallel()
	b := planBuilder(t)
	lines, err := b.Plan(testRev)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var bd string
	for _, line := range lines {
		if strings.HasPrefix(line, "build bd: ") {
			bd = line
		}
	}
	if bd == "" {
		t.Fatalf("plan has no build bd: line:\n%s", strings.Join(lines, "\n"))
	}
	i := strings.Index(bd, bdBuildFlag)
	if i < 0 {
		t.Fatalf("the build bd: line does not stamp main.Build: %q", bd)
	}
	value, rest, _ := strings.Cut(bd[i+len(bdBuildFlag):], " ")
	if value == "" || rest == "" {
		t.Fatalf("the build bd: line stamps main.Build with %q, want a token followed by the commit stamp: %q", value, bd)
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			t.Errorf("the plan's main.Build placeholder %q holds %q, which a shell does not read as one word; want letters, digits and underscores", value, r)
		}
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
		{"no bd commit", func(b *Builder) { b.BDCommit = "" }, testRev, "no bd commit"},
		{"no om commit", func(b *Builder) { b.OMCommit = "" }, testRev, "no om commit"},
		{"no mango source", func(b *Builder) { b.MangoSrc = "" }, testRev, "--mango-src is required"},
		{"no mango commit", func(b *Builder) { b.MangoCommit = "" }, testRev, "no mango commit"},
		{"a mango commit that is not hex", func(b *Builder) { b.MangoCommit = "main" }, testRev, "is not a 7+ character hex commit"},
		{"a canonical mango source", func(b *Builder) { b.MangoSrc = "/town/gastown/mayor/rig/mango" }, testRev, "canonical clone"},
		{"no devops commit", func(b *Builder) { b.DevopsCommit = "" }, testRev, "no devops pin"},
		{"a bd commit that is not hex", func(b *Builder) { b.BDCommit = "main" }, testRev, "is not a 7+ character hex commit"},
		{"an om commit that is not hex", func(b *Builder) { b.OMCommit = "main" }, testRev, "is not a 7+ character hex commit"},
		{"no pins file", func(b *Builder) { b.PinsPath = "" }, testRev, "no pins file named"},
		{"three pins", func(b *Builder) { b.Pins = planPins[:3] }, testRev, "3 pins named, want the four"},
		{"an absent source with no pin", func(b *Builder) {
			b.OMSrc = ""
			b.Pins = []Pin{planPins[0], planPins[0], planPins[2], planPins[3]}
		}, testRev, "no --om-src and no om pin"},
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
