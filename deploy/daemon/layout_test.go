package daemon

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The bd layout the image ships (gt-p4xncz): this test holds the Dockerfile to
// the two paths docs/architecture.md states.
const (
	dockerfileName = "Dockerfile"
	bdWrapperBuild = "gt-wrap"
	bdWrapperPath  = "/usr/local/bin/bd"
	realBdBuild    = "bd"
	realBdPath     = "/usr/local/bin/bd.real"
	execMode       = "0755"
)

// contextCopy is one COPY that reads the build context: the file's name in the
// context, the path it is installed at, and the mode --chmod gives it (empty
// when the COPY sets none, which docker reads as 0644).
type contextCopy struct{ src, dst, mode string }

// contextCopies returns each COPY that reads the build context, in file order.
// A COPY with --from reads another stage, so it is not one.
func contextCopies(body string) []contextCopy {
	var copies []contextCopy
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "COPY") {
			continue
		}
		c := contextCopy{}
		var operands []string
		fromStage := false
		for _, f := range fields[1:] {
			switch {
			case strings.HasPrefix(f, "--from="):
				fromStage = true
			case strings.HasPrefix(f, "--chmod="):
				c.mode = strings.TrimPrefix(f, "--chmod=")
			case strings.HasPrefix(f, "--"):
			default:
				operands = append(operands, f)
			}
		}
		if fromStage || len(operands) != 2 {
			continue
		}
		c.src, c.dst = operands[0], operands[1]
		copies = append(copies, c)
	}
	return copies
}

// bdLayoutProblems returns one problem per way body's bd installation differs
// from the image's: the forwarder installed once as /usr/local/bin/bd and the
// real bd once as /usr/local/bin/bd.real, both mode 0755, and neither installed
// anywhere else.
func bdLayoutProblems(body string) []string {
	var problems []string
	wrapper, realBd := 0, 0
	for _, c := range contextCopies(body) {
		switch c.src {
		case bdWrapperBuild:
			if c.dst != bdWrapperPath {
				problems = append(problems, fmt.Sprintf("COPY %s %s installs the forwarder at %s; a bd call runs it only at %s", c.src, c.dst, c.dst, bdWrapperPath))
				continue
			}
			wrapper++
		case realBdBuild:
			if c.dst != realBdPath {
				problems = append(problems, fmt.Sprintf("COPY %s %s installs the real bd at %s; the forwarder finds it only at %s", c.src, c.dst, c.dst, realBdPath))
				continue
			}
			realBd++
		default:
			continue
		}
		if c.mode != execMode {
			problems = append(problems, modeProblem(c))
		}
	}
	if wrapper != 1 {
		problems = append(problems, fmt.Sprintf("%s installs the forwarder %d times; want it once from %s at %s", dockerfileName, wrapper, bdWrapperBuild, bdWrapperPath))
	}
	if realBd != 1 {
		problems = append(problems, fmt.Sprintf("%s installs the real bd %d times; want it once from %s at %s", dockerfileName, realBd, realBdBuild, realBdPath))
	}
	return problems
}

// modeProblem names the mode the COPY gives a binary that must run.
func modeProblem(c contextCopy) string {
	if c.mode == "" {
		return fmt.Sprintf("COPY %s %s sets no --chmod, so docker installs it 0644; want --chmod=%s", c.src, c.dst, execMode)
	}
	return fmt.Sprintf("COPY %s %s sets --chmod=%s; want --chmod=%s", c.src, c.dst, c.mode, execMode)
}

// readDockerfile returns the Dockerfile's text, read from this package's
// directory the way BuildImage reads it from the gt checkout.
func readDockerfile(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(dockerfileName)
	if err != nil {
		t.Fatalf("reading %s: %v", dockerfileName, err)
	}
	return string(body)
}

// TestDockerfileBdLayout: the image installs the gt-wrap forwarder as
// /usr/local/bin/bd and the real bd as /usr/local/bin/bd.real beside it, both
// mode 0755.
func TestDockerfileBdLayout(t *testing.T) {
	t.Parallel()
	if problems := bdLayoutProblems(readDockerfile(t)); len(problems) != 0 {
		t.Errorf("%s: %s", dockerfileName, strings.Join(problems, "; "))
	}
}

// TestDockerfileBdLayoutRefusesAnotherPlacement: each way the layout can go
// wrong makes the same check report a problem, so TestDockerfileBdLayout cannot
// pass on a file that installs the real bd as bd or drops the forwarder.
func TestDockerfileBdLayoutRefusesAnotherPlacement(t *testing.T) {
	t.Parallel()
	body := readDockerfile(t)
	wrapperLine := "COPY --chmod=0755 " + bdWrapperBuild + " " + bdWrapperPath + "\n"
	realBdLine := "COPY --chmod=0755 " + realBdBuild + " " + realBdPath + "\n"
	for _, tc := range []struct{ name, old, new string }{
		{
			name: "the forwarder installed under another name",
			old:  wrapperLine,
			new:  "COPY --chmod=0755 " + bdWrapperBuild + " /usr/local/bin/bd-wrap\n",
		},
		{
			name: "the real bd installed under another name",
			old:  realBdLine,
			new:  "COPY --chmod=0755 " + realBdBuild + " /usr/local/bin/bd-old\n",
		},
		{
			name: "the real bd left at /usr/local/bin/bd",
			old:  realBdLine,
			new:  "COPY --chmod=0755 " + realBdBuild + " " + bdWrapperPath + "\n",
		},
		{
			name: "the forwarder installed without its executable mode",
			old:  wrapperLine,
			new:  "COPY " + bdWrapperBuild + " " + bdWrapperPath + "\n",
		},
		{
			name: "the real bd installed without its executable mode",
			old:  realBdLine,
			new:  "COPY " + realBdBuild + " " + realBdPath + "\n",
		},
		{
			name: "the forwarder not installed at all",
			old:  wrapperLine,
			new:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if n := strings.Count(body, tc.old); n != 1 {
				t.Fatalf("%s holds %d lines at %q, want the one the mutation replaces", dockerfileName, n, tc.old)
			}
			mutated := strings.Replace(body, tc.old, tc.new, 1)
			if problems := bdLayoutProblems(mutated); len(problems) == 0 {
				t.Errorf("%s with %s passed the bd layout check; want it refused", dockerfileName, tc.name)
			}
		})
	}
}
