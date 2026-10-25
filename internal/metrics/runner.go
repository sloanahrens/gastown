package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// killGrace is how long a child gets after its context ends before its pipes
// are closed and Wait returns: a child that forks keeps the pipes open, and
// without a grace Wait would outlive the deadline.
const killGrace = 2 * time.Second

// DefaultAllowEnv is the environment a source's child commands inherit: enough
// to find binaries and the user's config, and nothing else.
var DefaultAllowEnv = []string{"PATH", "HOME", "TMPDIR", "LANG"}

// OSRunner returns a Runner that runs argv as a child process with no
// environment beyond the allowlist.
func OSRunner(allowEnv []string) Runner {
	run := OSRunnerEnv(allowEnv)
	return func(ctx context.Context, argv []string) ([]byte, error) {
		return run(ctx, argv, nil)
	}
}

// OSRunnerEnv returns a RunnerEnv that runs argv as a child process under the
// caller's context. The child's environment holds the variables named in
// allowEnv that are set, then extraEnv, so a secret in the snapshotting
// process's environment never reaches a command a source runs. A non-zero exit
// is an error carrying the tail of stderr.
func OSRunnerEnv(allowEnv []string) RunnerEnv {
	return func(ctx context.Context, argv, extraEnv []string) ([]byte, error) {
		if len(argv) == 0 {
			return nil, errors.New("run: empty command")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = childEnv(allowEnv, extraEnv, os.LookupEnv)
		cmd.WaitDelay = killGrace
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("%s: %w", argv[0], ctxErr)
			}
			return nil, fmt.Errorf("%s: %w: %s", argv[0], err, tailString(stderr.String(), 400))
		}
		return stdout.Bytes(), nil
	}
}

// childEnv builds the child's environment from the allowlisted names that
// lookup reports set, in allowEnv order, then extraEnv. An extra variable
// replaces the allowlisted one of the same name rather than repeating it, so a
// caller's value wins once.
func childEnv(allowEnv, extraEnv []string, lookup func(string) (string, bool)) []string {
	extra := make(map[string]bool, len(extraEnv))
	for _, kv := range extraEnv {
		name, _, _ := strings.Cut(kv, "=")
		extra[name] = true
	}
	env := make([]string, 0, len(allowEnv)+len(extraEnv))
	for _, name := range allowEnv {
		if extra[name] {
			continue
		}
		if v, ok := lookup(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return append(env, extraEnv...)
}

// tailString returns the last at most maxBytes bytes of s, trimmed, starting
// at a rune boundary: a child's stderr can be any UTF-8, and a byte cut that
// lands mid-sequence would put a replacement character in a user-facing error.
func tailString(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	s = s[len(s)-maxBytes:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}
