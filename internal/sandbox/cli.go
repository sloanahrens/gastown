// Package sandbox wraps the openshell CLI so the session backend and the
// doctor check never build openshell argv themselves.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	// callTimeout bounds every call except Create and Exec.
	callTimeout = 2 * time.Minute
	// createTimeout covers the image pull a first Create may trigger.
	createTimeout = 10 * time.Minute
	// stderrLimit bounds the stderr text an error message carries.
	stderrLimit = 4 << 10
	// envFromPrefix names the child-process variables that carry sandbox
	// environment values, one per key.
	envFromPrefix = "GT_SANDBOX_ENV_"
)

// Runner runs argv (argv[0] is the program) with stdin and env added to the
// process environment, and returns its output. CLI sends every openshell
// invocation through one, so tests record argv with Fake instead of starting
// a process.
type Runner func(ctx context.Context, argv []string, stdin []byte, env []string) (stdout, stderr []byte, err error)

// CLI drives the openshell binary with JSON output.
type CLI struct {
	Gateway string // passed as -g; empty uses openshell's active gateway
	Bin     string // defaults to "openshell"
	Run     Runner // defaults to os/exec
}

// CreateSpec describes one `sandbox create`. Empty fields are omitted from
// the argv.
type CreateSpec struct {
	Name, Image, PolicyPath string
	Providers               []string
	CPU, Memory             string
	Labels                  map[string]string
	Env                     map[string]string
	DriverConfigJSON        string   // "" omits the flag
	Command                 []string // the main process argv after "--"
}

// Status is the part of `sandbox get` the backend acts on.
type Status struct {
	Name, Phase string
	Ready       bool // the Ready condition is True
	ReadyKnown  bool // the status carried a Ready condition at all
}

func (c *CLI) bin() string {
	if c.Bin == "" {
		return "openshell"
	}
	return c.Bin
}

func (c *CLI) runner() Runner {
	if c.Run == nil {
		return execRunner
	}
	return c.Run
}

// argv is the openshell invocation for sub, global flags first.
func (c *CLI) argv(sub ...string) []string {
	a := []string{c.bin()}
	if c.Gateway != "" {
		a = append(a, "-g", c.Gateway)
	}
	return append(a, sub...)
}

// run executes argv under a timeout (none when timeout is 0) and wraps a
// failure with the command verb and its stderr, since openshell reports
// the reason there and exits non-zero.
func (c *CLI) run(ctx context.Context, timeout time.Duration, verb string, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	stdout, stderr, err := c.runner()(ctx, argv, stdin, env)
	stderr = redactEnv(stderr, env)
	if err != nil {
		return stdout, stderr, commandError(verb, err, stderr)
	}
	return stdout, stderr, nil
}

// redactEnv replaces each value carried in env with [redacted], so a stderr
// that echoes a secret back does not carry it into an error. It runs on the
// whole stderr, before commandError cuts it, so a value the cut would split
// cannot survive.
func redactEnv(stderr []byte, env []string) []byte {
	for _, kv := range env {
		_, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		stderr = bytes.ReplaceAll(stderr, []byte(v), []byte("[redacted]"))
	}
	return stderr
}

// commandError wraps a failed invocation with the verb and the tail of its
// stderr. An empty stderr adds no separator, and a long one is cut to its
// last stderrLimit bytes, where the reason usually is.
func commandError(verb string, err error, stderr []byte) error {
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		return fmt.Errorf("openshell %s: %w", verb, err)
	}
	if len(msg) > stderrLimit {
		// The cut can split a multi-byte rune; ToValidUTF8 drops the fragment.
		msg = "...(truncated) " + strings.ToValidUTF8(msg[len(msg)-stderrLimit:], "")
	}
	return fmt.Errorf("openshell %s: %w: %s", verb, err, msg)
}

// Create starts a sandbox and returns once the gateway has accepted it
// (--detach); callers poll Get for readiness. Env values travel through
// --env-from, so they stay off the argv.
func (c *CLI) Create(ctx context.Context, s CreateSpec) error {
	a := c.argv("sandbox", "create", "--name", s.Name)
	if s.Image != "" {
		a = append(a, "--from", s.Image)
	}
	if s.PolicyPath != "" {
		a = append(a, "--policy", s.PolicyPath)
	}
	for _, p := range s.Providers {
		a = append(a, "--provider", p)
	}
	if s.CPU != "" {
		a = append(a, "--cpu", s.CPU)
	}
	if s.Memory != "" {
		a = append(a, "--memory", s.Memory)
	}
	if s.DriverConfigJSON != "" {
		a = append(a, "--driver-config-json", s.DriverConfigJSON)
	}
	a = appendSortedPairs(a, "--label", s.Labels)
	a, env := appendEnvFrom(a, s.Env)
	a = append(a, "--detach", "--output", "json")
	if len(s.Command) > 0 {
		a = append(a, "--")
		a = append(a, s.Command...)
	}
	_, _, err := c.run(ctx, createTimeout, "sandbox create", a, nil, env)
	return err
}

// Get reports the sandbox's phase and whether its Ready condition is True.
func (c *CLI) Get(ctx context.Context, name string) (Status, error) {
	stdout, _, err := c.run(ctx, callTimeout, "sandbox get", c.argv("sandbox", "get", name, "--output", "json"), nil, nil)
	if err != nil {
		return Status{}, err
	}
	var raw struct {
		Name       string `json:"name"`
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return Status{}, fmt.Errorf("openshell sandbox get %s: decode: %w", name, err)
	}
	st := Status{Name: raw.Name, Phase: raw.Phase}
	for _, cond := range raw.Conditions {
		if cond.Type != "Ready" {
			continue
		}
		st.ReadyKnown = true
		if strings.EqualFold(cond.Status, "True") {
			st.Ready = true
		}
	}
	return st, nil
}

// Exec runs argv inside the sandbox with stdin piped. It adds no deadline of
// its own: the caller's ctx bounds a command whose length only the caller
// knows. Exec takes no env values: `sandbox exec` accepts them only as argv
// `--env`, where any local user running ps could read them, so a secret goes
// on the sandbox at create time instead.
func (c *CLI) Exec(ctx context.Context, name string, env map[string]string, argv []string, stdin []byte) (stdout, stderr []byte, err error) {
	if len(env) > 0 {
		return nil, nil, fmt.Errorf("openshell sandbox exec: env values would go on the argv; set them at sandbox create")
	}
	a := c.argv("sandbox", "exec", "-n", name, "--no-tty", "--no-login-shell", "--")
	a = append(a, argv...)
	return c.run(ctx, 0, "sandbox exec", a, stdin, nil)
}

// Upload copies localPath into remoteDir. It always passes --no-git-ignore:
// the skeleton archive is not inside a work tree, and a .gitignore match
// must never silently thin an upload.
func (c *CLI) Upload(ctx context.Context, name, localPath, remoteDir string) error {
	a := c.argv("sandbox", "upload", "--no-git-ignore", name, localPath, remoteDir)
	_, _, err := c.run(ctx, callTimeout, "sandbox upload", a, nil, nil)
	return err
}

// Stop stops the sandbox, keeping its workspace.
func (c *CLI) Stop(ctx context.Context, name string) error {
	_, _, err := c.run(ctx, callTimeout, "sandbox stop", c.argv("sandbox", "stop", name), nil, nil)
	return err
}

// Start starts a stopped sandbox.
func (c *CLI) Start(ctx context.Context, name string) error {
	_, _, err := c.run(ctx, callTimeout, "sandbox start", c.argv("sandbox", "start", name), nil, nil)
	return err
}

// Delete removes the sandbox. A missing sandbox is an error here; callers
// that want idempotence match it on the wrapped stderr.
func (c *CLI) Delete(ctx context.Context, name string) error {
	_, _, err := c.run(ctx, callTimeout, "sandbox delete", c.argv("sandbox", "delete", name), nil, nil)
	return err
}

// Logs returns the sandbox-source log lines from the last since.
func (c *CLI) Logs(ctx context.Context, name string, since time.Duration) (string, error) {
	a := c.argv("logs", name, "--since", formatSince(since), "--source", "sandbox")
	stdout, _, err := c.run(ctx, callTimeout, "logs", a, nil, nil)
	return string(stdout), err
}

// formatSince renders d in the unit openshell's --since parses: whole hours,
// whole minutes, else seconds (rounded up, at least 1s).
func formatSince(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	secs := int64((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("%ds", secs)
}

// appendSortedPairs appends `flag k=v` for each entry in key order, so the
// argv is the same on every call.
func appendSortedPairs(a []string, flag string, m map[string]string) []string {
	for _, k := range sortedKeys(m) {
		a = append(a, flag, k+"="+m[k])
	}
	return a
}

// appendEnvFrom appends `--env-from k=GT_SANDBOX_ENV_k` for each entry in key
// order, and returns the child environment that carries the values. openshell
// takes such a value from the environment of its own process instead of its
// argv, where any local user running ps could read it.
func appendEnvFrom(a []string, m map[string]string) ([]string, []string) {
	keys := sortedKeys(m)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		name := envFromPrefix + k
		env = append(env, name+"="+m[k])
		a = append(a, "--env-from", k+"="+name)
	}
	return a, env
}

// sortedKeys returns m's keys in order, so an argv or env built from a map is
// the same on every call.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func execRunner(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
	if len(argv) == 0 {
		return nil, nil, fmt.Errorf("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 100 * time.Millisecond
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Env = append(inheritedEnv(os.Environ()), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// inheritedEnv is environ without any envFromPrefix entry. A value a parent
// process left under that prefix would otherwise reach openshell in a call
// that did not set it.
func inheritedEnv(environ []string) []string {
	inherited := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, envFromPrefix) {
			inherited = append(inherited, kv)
		}
	}
	return inherited
}
