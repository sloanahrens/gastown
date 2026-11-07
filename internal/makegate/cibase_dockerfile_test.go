package makegate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dockerInstruction is one instruction of a Dockerfile: its keyword in upper
// case, its argument text with continuation lines joined, and the line it
// starts on.
type dockerInstruction struct {
	line    int
	keyword string
	args    string
}

// parseDockerfile splits a Dockerfile into its instructions the way the builder
// reads them: comment and blank lines are dropped, even inside a continued
// instruction, and a trailing backslash joins the next line.
func parseDockerfile(text string) []dockerInstruction {
	var out []dockerInstruction
	var joined strings.Builder
	start := 0
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if joined.Len() == 0 {
			start = i + 1
		}
		if rest, ok := strings.CutSuffix(line, "\\"); ok {
			joined.WriteString(rest + " ")
			continue
		}
		joined.WriteString(line)
		keyword, args, _ := strings.Cut(strings.TrimSpace(joined.String()), " ")
		out = append(out, dockerInstruction{line: start, keyword: strings.ToUpper(keyword), args: strings.TrimSpace(args)})
		joined.Reset()
	}
	return out
}

// checkCIBaseUsers reports why a ci-base Dockerfile does not default BASE_USER
// to ci, run every install step into /usr/local/bin as root, and end as the
// BASE_USER argument. It returns nil when all three hold.
func checkCIBaseUsers(text string) error {
	ins := parseDockerfile(text)
	if !hasDockerInstruction(ins, "ARG", "BASE_USER=ci") {
		return errors.New("no ARG BASE_USER=ci: the base user must default to ci")
	}
	user := ""
	installs := 0
	for _, in := range ins {
		switch in.keyword {
		case "USER":
			user = in.args
		case "RUN":
			if !strings.Contains(in.args, "/usr/local/bin") {
				continue
			}
			installs++
			if user != "root" {
				return fmt.Errorf("the install step at line %d has USER %q before it, want USER root", in.line, user)
			}
		}
	}
	if installs == 0 {
		return errors.New("no RUN installs into /usr/local/bin, so the root check has no install step to guard")
	}
	final := ""
	for _, in := range ins {
		if in.keyword == "USER" {
			final = in.args
		}
	}
	if final != "${BASE_USER}" {
		return fmt.Errorf("the last USER is %q, want ${BASE_USER} to restore the base user", final)
	}
	return nil
}

func hasDockerInstruction(ins []dockerInstruction, keyword, args string) bool {
	for _, in := range ins {
		if in.keyword == keyword && in.args == args {
			return true
		}
	}
	return false
}

// readCIBaseDockerfile reads deploy/ci-base/Dockerfile from the checkout. This
// package sits two directories below the repo root.
func readCIBaseDockerfile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "ci-base", "Dockerfile"))
	if err != nil {
		t.Fatalf("read the ci-base Dockerfile: %v", err)
	}
	return string(b)
}

// mutate replaces old with new in text, and fails the test unless old occurs
// exactly once, so a mutation that matches nothing cannot pass for a check.
func mutate(t *testing.T, text, old, new string) string {
	t.Helper()
	if n := strings.Count(text, old); n != 1 {
		t.Fatalf("%q occurs %d times in the shipped Dockerfile, want once", old, n)
	}
	return strings.Replace(text, old, new, 1)
}

// TestCIBaseDockerfileUsers checks the user steps the image build depends on:
// every install step runs as root, and the image ends as BASE_USER, which
// defaults to ci. Each mutation removes or breaks one of them, and the check
// must refuse it. Mutation: drop the USER root line and the installs run as ci,
// the permission-denied failure the image build hit; put a USER back before the
// second install and only that one runs unprivileged; drop the restoring line
// and the image ends as root.
func TestCIBaseDockerfileUsers(t *testing.T) {
	t.Parallel()
	shipped := readCIBaseDockerfile(t)
	tests := []struct {
		name     string
		old, new string // a line of the shipped file to replace; empty keeps the file
		text     string // a whole Dockerfile to check instead
		wantErr  bool
	}{
		{name: "shipped file"},
		{name: "USER root line removed", old: "USER root\n", wantErr: true},
		{name: "second install runs as another user", old: "ARG JQ_VERSION=1.7.1\n", new: "ARG JQ_VERSION=1.7.1\nUSER ${BASE_USER}\n", wantErr: true},
		{name: "second install as another user", text: "FROM base\nARG BASE_USER=ci\nUSER root\nRUN install -o /usr/local/bin/a\nUSER ci\nRUN install -o /usr/local/bin/b\nUSER ${BASE_USER}\n", wantErr: true},
		{name: "restoring USER line removed", old: "USER ${BASE_USER}\n", wantErr: true},
		{name: "restore names ci, not the argument", old: "USER ${BASE_USER}\n", new: "USER ci\n", wantErr: true},
		{name: "BASE_USER argument removed", old: "ARG BASE_USER=ci\n", wantErr: true},
		{name: "no install step", text: "FROM base\nARG BASE_USER=ci\nUSER root\nUSER ${BASE_USER}\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text
			switch {
			case tc.old != "":
				text = mutate(t, shipped, tc.old, tc.new)
			case tc.text == "":
				text = shipped
			}
			err := checkCIBaseUsers(text)
			if tc.wantErr && err == nil {
				t.Fatalf("checkCIBaseUsers accepted the Dockerfile, want a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkCIBaseUsers refused the Dockerfile: %v", err)
			}
		})
	}
}
