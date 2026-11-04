package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// claudeExecFixtureToken is an obviously fake subscription token. It shares no
// 8-character run with the command's own messages, so the leak check fails only
// on a real leak.
const claudeExecFixtureToken = "Tk9xQ2vB-7rLmZ4pW-0sHd"

const claudeExecFixturePath = "/run/secrets/om-token"

// claudeExecFake is the host the command reaches through claudeExecSeams: its
// environment, the files it can read, and the one child it may start.
type claudeExecFake struct {
	env     map[string]string
	environ []string
	files   map[string]string
	readErr error
	code    int
	runErr  error

	runs     int
	argv     []string
	childEnv []string

	exited   bool
	exitCode int
}

func (f *claudeExecFake) seams() claudeExecSeams {
	return claudeExecSeams{
		getenv:  func(name string) string { return f.env[name] },
		environ: func() []string { return f.environ },
		readFile: func(path string) (string, error) {
			if f.readErr != nil {
				return "", f.readErr
			}
			text, ok := f.files[path]
			if !ok {
				return "", errors.New("no such file")
			}
			return text, nil
		},
		run: func(argv, env []string) (int, error) {
			f.runs++
			f.argv, f.childEnv = argv, env
			return f.code, f.runErr
		},
		exit: func(code int) {
			f.exited, f.exitCode = true, code
		},
	}
}

// claudeChildExitError stands in for the *exec.ExitError a child that ran
// returns, so the status mapping is tested without starting a process.
type claudeChildExitError struct{ code int }

func (e claudeChildExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e claudeChildExitError) ExitCode() int { return e.code }

// claudeExecLeaks reports whether msg carries any 8-character run of token.
func claudeExecLeaks(msg, token string) bool {
	for i := 0; i+8 <= len(token); i++ {
		if strings.Contains(msg, token[i:i+8]) {
			return true
		}
	}
	return false
}

// The child gets the cleaned token in its environment; the parent's environment
// slice is not changed, and a token the parent already carries is replaced, not
// duplicated.
func TestClaudeExecSetsTheTokenInTheChildEnvOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		environ []string
	}{
		{"parent without a token", []string{"PATH=/usr/bin", "HOME=/home/claude"}},
		{"parent with a stale token", []string{"PATH=/usr/bin", "CLAUDE_CODE_OAUTH_TOKEN=stale-value"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &claudeExecFake{
				environ: tc.environ,
				files:   map[string]string{claudeExecFixturePath: "  " + claudeExecFixtureToken + " \n"},
			}
			parentBefore := slices.Clone(tc.environ)

			if _, err := claudeExec(f.seams(), claudeExecFixturePath, nil); err != nil {
				t.Fatalf("claudeExec: %v", err)
			}

			if f.runs != 1 {
				t.Fatalf("started claude %d times, want 1", f.runs)
			}
			var tokens []string
			for _, kv := range f.childEnv {
				if strings.HasPrefix(kv, omTokenEnv+"=") {
					tokens = append(tokens, kv)
				}
			}
			if len(tokens) != 1 || tokens[0] != omTokenEnv+"="+claudeExecFixtureToken {
				t.Errorf("child carries %d %s entries, want exactly the cleaned token (values not printed)", len(tokens), omTokenEnv)
			}
			if !slices.Equal(f.environ, parentBefore) {
				t.Error("the parent's environment changed")
			}
		})
	}
}

// The four characters the entrypoint strips go wherever they sit, not only at
// the ends: a token split over two lines is one token.
func TestClaudeExecStripsWhitespaceInsideTheToken(t *testing.T) {
	t.Parallel()
	f := &claudeExecFake{
		files: map[string]string{claudeExecFixturePath: "Tk9xQ2vB-7rLmZ4pW-\n  0sHd\r\n\t"},
	}

	if _, err := claudeExec(f.seams(), claudeExecFixturePath, nil); err != nil {
		t.Fatalf("claudeExec: %v", err)
	}
	if want := omTokenEnv + "=" + claudeExecFixtureToken; !slices.Contains(f.childEnv, want) {
		t.Errorf("child environment does not carry the token as one line (values not printed)")
	}
}

// The strip is the container entrypoint's, not every character Unicode calls
// whitespace: a no-break space stays, as it does in the entrypoint's token.
func TestClaudeExecKeepsWhitespaceTheEntrypointKeeps(t *testing.T) {
	t.Parallel()
	const noBreakSpace = "\u00a0"
	f := &claudeExecFake{
		files: map[string]string{claudeExecFixturePath: " \t" + claudeExecFixtureToken + noBreakSpace},
	}

	if _, err := claudeExec(f.seams(), claudeExecFixturePath, nil); err != nil {
		t.Fatalf("claudeExec: %v", err)
	}
	if want := omTokenEnv + "=" + claudeExecFixtureToken + noBreakSpace; !slices.Contains(f.childEnv, want) {
		t.Errorf("child environment does not carry the token with its no-break space (values not printed)")
	}
}

// ANTHROPIC_API_KEY, and the other API credential the entrypoint refuses, stop
// the start before the file is read, naming the variable and never its value.
// An empty variable is not set.
func TestClaudeExecRefusesAnAPIKeyInTheEnvironment(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &claudeExecFake{
				env:   map[string]string{name: "fixture-api-value"},
				files: map[string]string{claudeExecFixturePath: claudeExecFixtureToken},
			}

			_, err := claudeExec(f.seams(), claudeExecFixturePath, nil)

			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("error = %v, want a refusal naming %s", err, name)
			}
			if strings.Contains(err.Error(), "fixture-api-value") {
				t.Error("the refusal printed the variable's value")
			}
			if f.runs != 0 {
				t.Errorf("a refusal started claude %d times, want none", f.runs)
			}
		})
	}
	t.Run("empty value is not set", func(t *testing.T) {
		t.Parallel()
		f := &claudeExecFake{
			env:   map[string]string{"ANTHROPIC_API_KEY": ""},
			files: map[string]string{claudeExecFixturePath: claudeExecFixtureToken},
		}
		if _, err := claudeExec(f.seams(), claudeExecFixturePath, nil); err != nil {
			t.Fatalf("an empty API key refused the start: %v", err)
		}
	})
}

// The arguments after -- reach claude unchanged, and claude's exit status is the
// command's. The cobra row drives the same path through the real command parse.
func TestClaudeExecPassesArgsAndExitStatusThrough(t *testing.T) {
	t.Parallel()
	args := []string{"-p", "review this diff", "--output-format", "json"}

	t.Run("claudeExec", func(t *testing.T) {
		t.Parallel()
		f := &claudeExecFake{
			files: map[string]string{claudeExecFixturePath: claudeExecFixtureToken},
			code:  7,
		}

		code, err := claudeExec(f.seams(), claudeExecFixturePath, args)

		if err != nil {
			t.Fatalf("claudeExec: %v", err)
		}
		if want := append([]string{"claude"}, args...); !slices.Equal(f.argv, want) {
			t.Errorf("claude argv = %q, want %q", f.argv, want)
		}
		if code != 7 {
			t.Errorf("exit status = %d, want claude's 7", code)
		}
	})

	t.Run("through the command", func(t *testing.T) {
		t.Parallel()
		f := &claudeExecFake{
			files: map[string]string{claudeExecFixturePath: claudeExecFixtureToken},
		}
		c := newDaemonClaudeExecCmd(f.seams())
		c.SetArgs(append([]string{"--om-token-file", claudeExecFixturePath, "--"}, args...))
		c.SilenceUsage, c.SilenceErrors = true, true

		if err := c.Execute(); err != nil {
			t.Fatalf("execute claude-exec: %v", err)
		}
		if want := append([]string{"claude"}, args...); !slices.Equal(f.argv, want) {
			t.Errorf("claude argv through the command = %q, want %q", f.argv, want)
		}
	})
}

// The command ends with claude's non-zero status, and returns no error of its
// own; a zero status is left to the process that called the command.
func TestClaudeExecEndsTheCommandWithClaudesStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		code     int
		wantExit bool
		wantCode int
	}{
		{"a non-zero status ends the command", 7, true, 7},
		{"a zero status does not end it", 0, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &claudeExecFake{
				files: map[string]string{claudeExecFixturePath: claudeExecFixtureToken},
				code:  tc.code,
			}
			c := newDaemonClaudeExecCmd(f.seams())
			c.SetArgs([]string{"--om-token-file", claudeExecFixturePath, "--", "-p", "hi"})
			c.SilenceUsage, c.SilenceErrors = true, true

			if err := c.Execute(); err != nil {
				t.Fatalf("execute claude-exec: %v", err)
			}
			if f.exited != tc.wantExit || f.exitCode != tc.wantCode {
				t.Errorf("command exit = (exited %t, code %d), want (exited %t, code %d)",
					f.exited, f.exitCode, tc.wantExit, tc.wantCode)
			}
		})
	}
}

// Each refusal names the path or the variable, never the token, and starts
// nothing. A start failure comes back as an error too.
func TestClaudeExecErrorsNeverContainTheToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		f    claudeExecFake
		path string
	}{
		{
			name: "read error that carries the token",
			f:    claudeExecFake{readErr: errors.New("boom " + claudeExecFixtureToken)},
			path: claudeExecFixturePath,
		},
		{
			name: "empty file",
			f:    claudeExecFake{files: map[string]string{claudeExecFixturePath: ""}},
			path: claudeExecFixturePath,
		},
		{
			name: "whitespace-only file",
			f:    claudeExecFake{files: map[string]string{claudeExecFixturePath: " \n\t "}},
			path: claudeExecFixturePath,
		},
		{
			name: "no token file flag",
			f:    claudeExecFake{files: map[string]string{claudeExecFixturePath: claudeExecFixtureToken}},
			path: "",
		},
		{
			name: "claude cannot start",
			f: claudeExecFake{
				files:  map[string]string{claudeExecFixturePath: claudeExecFixtureToken},
				runErr: errors.New(`exec: "claude": executable file not found in $PATH`),
			},
			path: claudeExecFixturePath,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.f

			_, err := claudeExec(f.seams(), tc.path, nil)

			if err == nil {
				t.Fatal("claudeExec returned no error")
			}
			if claudeExecLeaks(err.Error(), claudeExecFixtureToken) {
				t.Errorf("error carries an 8-character run of the token: %v", err)
			}
		})
	}
}

// A refusal through the real command returns the refusal as its error, and no
// child starts.
func TestClaudeExecRefusalThroughTheCommandStartsNothing(t *testing.T) {
	t.Parallel()
	f := &claudeExecFake{
		env:   map[string]string{"ANTHROPIC_API_KEY": "fixture-api-value"},
		files: map[string]string{claudeExecFixturePath: claudeExecFixtureToken},
	}
	c := newDaemonClaudeExecCmd(f.seams())
	c.SetArgs([]string{"--om-token-file", claudeExecFixturePath, "--", "-p", "hi"})
	c.SilenceUsage, c.SilenceErrors = true, true

	err := c.Execute()

	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("execute claude-exec = %v, want the refusal naming ANTHROPIC_API_KEY", err)
	}
	if f.runs != 0 {
		t.Errorf("a refusal started claude %d times, want none", f.runs)
	}
}

// A child that cannot start is an error, not an exit status. Both answers are
// read from the error the child's Run returned, so no process starts.
func TestClaudeExecTellsAStartFailureFromAnExitStatus(t *testing.T) {
	t.Parallel()

	t.Run("a start failure is an error", func(t *testing.T) {
		t.Parallel()
		startErr := errors.New(`exec: "claude": executable file not found in $PATH`)

		code, err := claudeChildStatus(startErr)

		if code != 0 || !errors.Is(err, startErr) {
			t.Fatalf("claudeChildStatus(start failure) = (%d, %v), want no status and the error", code, err)
		}
	})

	t.Run("a non-zero exit is the status", func(t *testing.T) {
		t.Parallel()

		code, err := claudeChildStatus(claudeChildExitError{code: 7})

		if err != nil || code != 7 {
			t.Fatalf("claudeChildStatus(exit status) = (%d, %v), want claude's 7 and no error", code, err)
		}
	})
}

// A missing --om-token-file is named as the missing flag, not reported as a file
// that could not be read under an empty path.
func TestClaudeExecNamesTheMissingTokenFileFlag(t *testing.T) {
	t.Parallel()
	f := &claudeExecFake{files: map[string]string{claudeExecFixturePath: claudeExecFixtureToken}}

	_, err := claudeExec(f.seams(), "", nil)

	if err == nil || !strings.Contains(err.Error(), "--om-token-file is required") {
		t.Fatalf("error = %v, want the missing flag named", err)
	}
}

// The refusals before the start start nothing: a missing path, an unreadable or
// empty file, and a set API credential each leave claude unstarted.
func TestClaudeExecRefusalsStartNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		f    claudeExecFake
		path string
	}{
		{"no token file flag", claudeExecFake{files: map[string]string{claudeExecFixturePath: claudeExecFixtureToken}}, ""},
		{"unreadable file", claudeExecFake{readErr: errors.New("permission denied")}, claudeExecFixturePath},
		{"empty file", claudeExecFake{files: map[string]string{claudeExecFixturePath: "\n"}}, claudeExecFixturePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.f

			_, err := claudeExec(f.seams(), tc.path, nil)

			if err == nil {
				t.Fatal("claudeExec returned no error")
			}
			if f.runs != 0 {
				t.Errorf("a refusal started claude %d times, want none", f.runs)
			}
		})
	}
}
