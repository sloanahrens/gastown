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
	"strconv"
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
	// NoKeep makes the gateway delete the sandbox when Command exits
	// (openshell sandbox create --no-keep). openshell refuses --no-keep
	// together with --output, so a NoKeep create asks for no JSON.
	NoKeep bool
}

// Status is the part of `sandbox get` the backend acts on.
type Status struct {
	Name, Phase string
	Ready       bool // the Ready condition is True
	ReadyKnown  bool // the status carried a Ready condition at all
	// ReadyReason and ReadyMessage are the Ready condition's reason and message,
	// which say why a sandbox is not Ready yet. Empty when the gateway gives none.
	ReadyReason, ReadyMessage string
	// NotReadySince is when the Ready condition last turned False, which for
	// a terminal phase is when the failure began. Zero when the gateway does
	// not say (gt-j18af).
	NotReadySince time.Time
	// Unschedulable is the pod's PodScheduled condition read False with reason
	// Unschedulable: no node has room for it. UnschedulableMsg is the
	// scheduler's message, and UnschedulableSince is when the condition last
	// turned False, zero when the gateway does not say.
	Unschedulable      bool
	UnschedulableMsg   string
	UnschedulableSince time.Time
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
	if s.NoKeep {
		a = append(a, "--detach", "--no-keep")
	} else {
		a = append(a, "--detach", "--output", "json")
	}
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
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
			// The condition time arrives camelCase in the JSON the gateway
			// prints; the snake_case proto spelling is read too. RawMessage
			// keeps a future shape from failing the whole decode: losing the
			// phase answer would leave the seat unjudged for as long as the
			// spelling differs (gt-j18af).
			LastTransitionTime      json.RawMessage `json:"lastTransitionTime"`
			LastTransitionTimeProto json.RawMessage `json:"last_transition_time"`
		} `json:"conditions"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return Status{}, fmt.Errorf("openshell sandbox get %s: decode: %w", name, err)
	}
	st := Status{Name: raw.Name, Phase: raw.Phase}
	for _, cond := range raw.Conditions {
		switch cond.Type {
		case "Ready":
			st.ReadyKnown = true
			st.ReadyReason, st.ReadyMessage = cond.Reason, cond.Message
			if strings.EqualFold(cond.Status, "True") {
				st.Ready = true
				continue
			}
			st.NotReadySince = conditionTime(cond.LastTransitionTime, cond.LastTransitionTimeProto)
		case "PodScheduled":
			if !strings.EqualFold(cond.Status, "False") || cond.Reason != "Unschedulable" {
				continue
			}
			st.Unschedulable = true
			st.UnschedulableMsg = cond.Message
			st.UnschedulableSince = conditionTime(cond.LastTransitionTime, cond.LastTransitionTimeProto)
		}
	}
	return st, nil
}

// conditionTime reads a condition's last transition time, as an RFC3339 string
// or a protobuf Timestamp object. An unreadable value is zero, so the caller
// measures from its own reading instead of the failure's start.
func conditionTime(raws ...json.RawMessage) time.Time {
	for _, raw := range raws {
		if t, ok := parseConditionTime(raw); ok {
			return t
		}
	}
	return time.Time{}
}

func parseConditionTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		t, err := time.Parse(time.RFC3339Nano, s)
		return t, err == nil
	}
	var ts struct {
		Seconds int64 `json:"seconds"`
		Nanos   int32 `json:"nanos"`
	}
	if err := json.Unmarshal(raw, &ts); err == nil && (ts.Seconds != 0 || ts.Nanos != 0) {
		return time.Unix(ts.Seconds, int64(ts.Nanos)).UTC(), true
	}
	return time.Time{}, false
}

// Entry is one row of `sandbox list`: the sandbox's name and its phase
// (Ready, Provisioning, ...).
type Entry struct {
	Name, Phase string
}

// listPageSize is the rows one `sandbox list` call asks for. List follows the
// continuation token whatever a page's length, so the size only sets the
// number of calls.
const listPageSize = 100

// listMaxPages bounds the paging loop, so a gateway that keeps returning a
// token cannot hold a sling in the loop.
const listMaxPages = 1000

// listAllWorkspaces makes `sandbox list` return every workspace's sandboxes.
// Rig sandboxes are created in per-rig workspaces, and the listing may be
// scoped to one workspace by default: without the flag, a seat in another
// workspace reads as absent.
const listAllWorkspaces = "--all-workspaces"

// List reads every sandbox the gateway lists. The only end of a listing is a
// page with no continuation token: a short page that carries a token is
// followed, because a gateway may return fewer rows than asked and still have
// more behind it. It reads `-o json`: the table form prints the token on
// stderr, where a stdout parse never sees it. A failed call is an error, and so
// is a listing that cannot be read whole: output that is not a sandbox page, a
// page with no sandboxes field, a row with no name or phase, a token that
// repeats, or more than listMaxPages pages. An unknown listing must never read
// as an empty one.
func (c *CLI) List(ctx context.Context) ([]Entry, error) {
	var out []Entry
	token := ""
	seen := map[string]bool{}
	for page := 0; page < listMaxPages; page++ {
		a := c.argv("sandbox", "list", listAllWorkspaces, "--page-size", strconv.Itoa(listPageSize), "--output", "json")
		if token != "" {
			a = append(a, "--page-token", token)
		}
		stdout, _, err := c.run(ctx, callTimeout, "sandbox list", a, nil, nil)
		if err != nil {
			return nil, err
		}
		rows, next, err := parseListPage(stdout)
		if err != nil {
			return nil, fmt.Errorf("openshell sandbox list: %w", err)
		}
		out = append(out, rows...)
		if next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("openshell sandbox list: the continuation token repeated (%q)", next)
		}
		seen[next] = true
		token = next
	}
	return nil, fmt.Errorf("openshell sandbox list: more than %d pages", listMaxPages)
}

// listPage is one `sandbox list -o json` reply. NextPageToken is empty on the
// last page. Sandboxes is a pointer, so a reply with no sandboxes field is
// refused rather than read as an empty page.
type listPage struct {
	NextPageToken string `json:"next_page_token"`
	Sandboxes     *[]struct {
		Name  string `json:"name"`
		Phase string `json:"phase"`
	} `json:"sandboxes"`
}

// parseListPage reads one page of `sandbox list -o json`: the name and phase of
// each sandbox, and the token that asks for the next page.
func parseListPage(stdout []byte) ([]Entry, string, error) {
	var page listPage
	if err := json.Unmarshal(stdout, &page); err != nil {
		return nil, "", fmt.Errorf("the page is not a sandbox list: %w", err)
	}
	if page.Sandboxes == nil {
		return nil, "", fmt.Errorf("the page has no sandboxes field")
	}
	entries := make([]Entry, 0, len(*page.Sandboxes))
	for _, s := range *page.Sandboxes {
		if s.Name == "" || s.Phase == "" {
			return nil, "", fmt.Errorf("sandbox %q has no name or phase", s.Name)
		}
		entries = append(entries, Entry{Name: s.Name, Phase: s.Phase})
	}
	return entries, page.NextPageToken, nil
}

// Exists reports whether the gateway has a sandbox of that name. Only the
// gateway's "not found" answer means it does not: a gateway that cannot be
// reached, or any other failure, is an error, so a caller never reads an
// unknown as absent.
func (c *CLI) Exists(ctx context.Context, name string) (bool, error) {
	_, stderr, err := c.run(ctx, callTimeout, "sandbox get", c.argv("sandbox", "get", name, "--output", "json"), nil, nil)
	switch {
	case err == nil:
		return true, nil
	case isNotFoundReply(stderr):
		return false, nil
	default:
		return false, err
	}
}

// notFoundMessage is the gateway status's message field for a missing
// sandbox, quoted as openshell prints it:
//
//	Error:   × code: 'Some requested entity was not found', message: "sandbox not found"
//
// Matching the quoted field keeps the same words elsewhere in stderr (a
// proxy's or a log line's) from reading as the gateway's answer.
const notFoundMessage = `message: "sandbox not found"`

// isNotFoundReply reports whether stderr carries the gateway's not-found status.
func isNotFoundReply(stderr []byte) bool {
	return bytes.Contains(bytes.ToLower(stderr), []byte(notFoundMessage))
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

// Download copies the sandbox path remote into localDir. A failure is returned
// with the command's stderr; a missing sandbox is not special-cased here.
func (c *CLI) Download(ctx context.Context, name, remote, localDir string) error {
	a := c.argv("sandbox", "download", name, remote, localDir)
	_, _, err := c.run(ctx, callTimeout, "sandbox download", a, nil, nil)
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
