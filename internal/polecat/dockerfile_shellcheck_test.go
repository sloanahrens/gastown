package polecat

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// make lint runs shellcheck, and a sandbox polecat has no host tools, so the
// image must install shellcheck in its final stage. The make-build stage is
// discarded and ships only its make binary, so an install there does not count.
func TestPolecatDockerfileInstallsShellcheckInTheFinalStage(t *testing.T) {
	t.Parallel()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "polecat", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	installs, hasStage := finalStageInstalls(string(data), "shellcheck")
	if !hasStage {
		t.Fatal("deploy/polecat/Dockerfile has no FROM instruction")
	}
	if !installs {
		t.Error("the final stage of deploy/polecat/Dockerfile installs no shellcheck (add it to the apt-get install list that carries build-essential)")
	}
}

// Docker reads an instruction keyword in any case and after leading
// whitespace, so the check must not depend on how the Dockerfile spells each
// instruction: a lowercase or indented RUN that installs shellcheck passes, and
// a lowercase FROM still starts a stage.
func TestFinalStageInstallsIgnoresKeywordCaseAndIndent(t *testing.T) {
	t.Parallel()
	const (
		base    = "FROM ubuntu:24.04\n"
		install = "apt-get install -y --no-install-recommends shellcheck\n"
	)
	for _, tc := range []struct {
		name       string
		dockerfile string
		want       bool
		wantStage  bool
	}{
		{"uppercase keyword at column zero", base + "RUN " + install, true, true},
		{"lowercase keyword", "from ubuntu:24.04\nrun " + install, true, true},
		{"mixed-case keyword", "From ubuntu:24.04\nRun " + install, true, true},
		{"indented instructions", "  FROM ubuntu:24.04\n\tRUN " + install, true, true},
		{"lowercase second stage", "from ubuntu:24.04 AS make-build\nRUN true\nfrom ubuntu:24.04\nrun " + install, true, true},
		{"install only in a discarded earlier stage", "FROM ubuntu:24.04 AS make-build\nRUN " + install + "FROM ubuntu:24.04\nRUN true\n", false, true},
		{"comment naming the install", base + "# RUN " + install + "RUN true\n", false, true},
		{"no FROM instruction", "RUN " + install, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, hasStage := finalStageInstalls(tc.dockerfile, "shellcheck")
			if hasStage != tc.wantStage {
				t.Fatalf("hasStage = %v, want %v", hasStage, tc.wantStage)
			}
			if got != tc.want {
				t.Errorf("finalStageInstalls = %v, want %v", got, tc.want)
			}
		})
	}
}

// finalStageInstalls reports whether the final stage of a Dockerfile has a RUN
// instruction that apt-get installs pkg. hasStage is false when the text holds
// no FROM instruction.
func finalStageInstalls(dockerfile, pkg string) (installs, hasStage bool) {
	// Continuation lines joined, so each instruction is one line.
	lines := strings.Split(strings.ReplaceAll(dockerfile, "\\\n", " "), "\n")
	final := -1
	for i, line := range lines {
		if instructionKeyword(line) == "FROM" {
			final = i
		}
	}
	if final < 0 {
		return false, false
	}
	for _, line := range lines[final:] {
		if instructionKeyword(line) != "RUN" {
			continue
		}
		for _, part := range strings.Split(line, "&&") {
			if strings.Contains(part, "apt-get install") && slices.Contains(strings.Fields(part), pkg) {
				return true, true
			}
		}
	}
	return false, true
}

// instructionKeyword returns the line's Dockerfile instruction in upper case,
// or "" for a blank or comment line.
func instructionKeyword(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
		return ""
	}
	return strings.ToUpper(fields[0])
}
