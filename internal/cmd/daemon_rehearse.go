package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/templates"
	"gopkg.in/yaml.v3"
)

// The container-config rehearsal (gt-unkq3). The 2026-11-01 L3 attempt found its
// first container-only failure (no time zone database) inside the window. The
// rehearsal finds every such failure beforehand: it stages a scratch copy of the
// live town's settings (daemon.env values blanked), checks the image against that
// copy, starts the image's entrypoint on the copy to its first heartbeat or its
// bd handshake, and lists ALL failures, not the first.
//
// Read-only against the live town: the only reads of it are the settings files
// and mayor/town.json, and every write, mount and container is the scratch
// copy's. The entrypoint container runs with no network, so it cannot reach the
// live Dolt server whatever the copied settings name.

// Rehearsal defaults and names.
const (
	rehearseDefaultBudget = 3 * time.Minute
	rehearsePoll          = 2 * time.Second
	// rehearseCallBudget bounds one docker call; a pull has the longer
	// rehearsePullBudget.
	rehearseCallBudget  = 3 * time.Minute
	rehearsePullBudget  = 10 * time.Minute
	rehearseAddrPattern = `host\.docker\.internal:[0-9]+`
	// rehearseTailLines is how much of the entrypoint container's log a failed
	// heartbeat prints.
	rehearseTailLines = 40
)

// rehearseHandshakeLine is the line the daemon logs when its bd handshake fails
// for want of Dolt. The entrypoint container has no network, so the handshake
// always fails there and the daemon exits on it. Reaching this line proves the
// entrypoint, its exec, the time zone and the environment all worked up to that
// step, so the heartbeat check counts it as the pass.
const rehearseHandshakeLine = "Error: bd handshake failed: Dolt is not reachable"

// rehearseBinaries are the programs the settings and the daemon exec; each must
// be on the image's PATH.
var rehearseBinaries = []string{"docker", "kubectl", "openshell", "git", "tmux", "dolt", "claude"}

// rehearseEnvRef is a ${NAME} reference, which names a secret without holding one.
var rehearseEnvRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

var rehearseAddr = regexp.MustCompile(rehearseAddrPattern)

// rehearseProbeAddr is the only shape the address probe dials: a host name or an
// IPv4 address, a colon and a port. The probe runs as shell text inside the image,
// so an address of any other shape (a town.json host with a space or a semicolon)
// is refused before it reaches that text.
var rehearseProbeAddr = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9.-]*):([0-9]{1,5})$`)

// probeTarget splits addr into the host and port the probe dials. ok is false for
// any address outside rehearseProbeAddr's shape, or with a port outside 1-65535.
func probeTarget(addr string) (host, port string, ok bool) {
	m := rehearseProbeAddr.FindStringSubmatch(addr)
	if m == nil {
		return "", "", false
	}
	if n, err := strconv.Atoi(m[2]); err != nil || n < 1 || n > 65535 {
		return "", "", false
	}
	return m[1], m[2], true
}

// rehearseFlags are the values `gt daemon rehearse` parsed.
type rehearseFlags struct {
	liveRoot string
	image    string
	budget   time.Duration
	// root and runDir are set by the run, never by a flag: runDir is the unique
	// directory this run creates and marks under <live>/.runtime/rehearsal, and
	// root is the scratch town inside it. The run deletes runDir and nothing a
	// caller named.
	root   string
	runDir string
}

// rehearseDeps is what the rehearsal takes from the host. A test supplies fakes,
// so no unit test starts Docker, touches a town or waits on a real clock.
type rehearseDeps struct {
	run daemonimage.Runner
	// capture runs a command and returns its output, for the one command whose
	// text the rehearsal reads: docker logs of the entrypoint container.
	capture  func(ctx context.Context, dir string, env, argv []string) ([]byte, error)
	environ  []string
	out      io.Writer
	readFile func(path string) ([]byte, error)
	readDir  func(path string) ([]os.DirEntry, error)
	mkdirAll func(path string, perm os.FileMode) error
	write    func(path string, data []byte, perm os.FileMode) error
	// removeAll deletes the scratch town; evalSymlinks resolves a path for the
	// root guard.
	removeAll    func(path string) error
	evalSymlinks func(path string) (string, error)
	// callBudget bounds one docker call; zero means rehearseCallBudget.
	callBudget time.Duration
	// userHomeDir is the account's home directory, under which two accepted
	// notify paths lie; a test supplies a temp directory.
	userHomeDir func() (string, error)
	// patrolKeys overrides the daemon config type's patrol keys, for a test.
	patrolKeys func() []string
	// newID is a unique run id (the pid and the time in the real one).
	newID func() string
	now   func() time.Time
	sleep func(time.Duration)
}

func realRehearseDeps(out io.Writer) rehearseDeps {
	return rehearseDeps{
		run: execImageRunner, capture: execImageCapture, environ: os.Environ(), out: out,
		readFile: os.ReadFile, readDir: os.ReadDir, mkdirAll: os.MkdirAll, write: os.WriteFile,
		removeAll: os.RemoveAll, evalSymlinks: filepath.EvalSymlinks,
		userHomeDir: os.UserHomeDir,
		newID:       func() string { return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()) },
		now:         time.Now, sleep: time.Sleep,
	}
}

// execImageCapture runs argv in dir with env as its whole environment and returns
// its output, stdout and stderr together.
func execImageCapture(ctx context.Context, dir string, env, argv []string) ([]byte, error) {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is built by this package from fixed names and the scratch town's container name
	c.Dir = dir
	c.Env = env
	return c.CombinedOutput()
}

// rehearsalCheck is one row of the rehearsal's table: what it inspects, whether
// a failure fails the rehearsal, and the check itself, which returns one line
// per failure.
type rehearsalCheck struct {
	name     string
	inspects string
	required bool
	run      func(ctx context.Context, r *rehearsal) []string
}

// rehearsalChecks is the table, in the order it prints. It lives here and not
// beside the config types because every check runs the image through docker,
// and the config package has no docker seam; each row names the setting kind it
// reads (config.json values, daemon.env keys, the compose binds).
var rehearsalChecks = []rehearsalCheck{
	{"config-validate", "gt config validate loads the scratch settings inside the image", true, checkConfigValidates},
	{"binaries", "every program the settings and the daemon exec is on the image's PATH", true, checkImageBinaries},
	{"paths", "every absolute path a setting names exists in the image, lies under a bind of the live compose, or is accepted", true, checkSettingPaths},
	{"addresses", "every host.docker.internal address a setting names, and the Dolt address town.json names, accepts a TCP connect from the image", true, checkSettingAddresses},
	{"plugins", "every plugin and patrol is classified, the ones that cannot run in the container are in daemon.container_off, and the image carries what the rest exec", true, checkDaemonSteps},
	{"images", "every image the live compose pulls (the sweep engine's) is served by digest on the engine the daemon uses", true, checkComposeImagesPull},
	{"heartbeat", "the entrypoint on the scratch town records a heartbeat, or reaches its bd handshake, within the budget", true, checkFirstHeartbeat},
}

// rehearsal is one run: the flags, the host seams and the staged settings text
// the checks read.
type rehearsal struct {
	f      rehearseFlags
	d      rehearseDeps
	config []byte         // the staged config.json, which the plugins check reads
	values []settingValue // the string values of the staged JSON settings, with their key paths; what the path and address checks scan
	// notes are lines a check reports without failing it (an accepted path). The
	// run prints them under the check's row, after its failures.
	notes []string
}

func newDaemonRehearseCmd(run func(ctx context.Context, out io.Writer, f rehearseFlags) error) *cobra.Command {
	var f rehearseFlags
	cmd := &cobra.Command{
		Use:   "rehearse",
		Short: "Rehearse the daemon container's configuration on a scratch copy of the live settings",
		Long: `Stages a scratch copy of the live town's settings (JSON settings with secret-named
values blanked, daemon.env with every value blanked, mayor/town.json; no other file),
checks the daemon image against it, starts the image's
entrypoint on the copy to its first heartbeat or its bd handshake failure, and prints a table of every check
with its result. It lists all failures and exits non-zero if any required check
failed.

Read-only against the live town: it reads the settings files and mayor/town.json
and writes only inside a run directory it creates and marks under
<live-root>/.runtime/rehearsal, which it removes when it exits (it deletes that
directory and nothing a caller names; a run killed outright leaves its marked
directory for you to delete). Settings are redacted by key name only: a secret
under a key whose name does not look like one is copied as it stands into that
mode-0700 directory. The entrypoint container has no network, so it
cannot reach the live Dolt server, so its bd handshake fails for want of Dolt.
That handshake line passes the heartbeat check; any other end prints the
container log tail. The Dolt address mayor/town.json names is probed from the
image over the bridge network. The accepted paths are listed, not probed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.liveRoot, "live-root", "", "absolute path of the live town (read only)")
	cmd.Flags().StringVar(&f.image, "image", "", "daemon image tag, gt-daemon:<rev>")
	cmd.Flags().DurationVar(&f.budget, "budget", rehearseDefaultBudget, "how long to wait for the first heartbeat")
	return cmd
}

var daemonRehearseCmd = newDaemonRehearseCmd(func(ctx context.Context, out io.Writer, f rehearseFlags) error {
	return runDaemonRehearse(ctx, f, realRehearseDeps(out))
})

func init() {
	daemonCmd.AddCommand(daemonRehearseCmd)
}

// runDaemonRehearse stages the scratch copy, runs every check, prints the table
// and fails if a required check did. Every refusal comes before anything is
// written.
func runDaemonRehearse(ctx context.Context, f rehearseFlags, d rehearseDeps) (err error) {
	if f.liveRoot == "" || f.image == "" || !filepath.IsAbs(f.liveRoot) {
		return errors.New("--live-root (absolute) and --image are required")
	}
	r := &rehearsal{f: f, d: d}
	if err := r.chooseRunDir(); err != nil {
		return err
	}
	defer func() {
		if rmErr := r.cleanup(); rmErr != nil && err == nil {
			err = rmErr
		}
	}()
	if err := r.stage(); err != nil {
		return err
	}
	failed := 0
	for _, c := range rehearsalChecks {
		r.notes = nil
		fails := c.run(ctx, r)
		status := "ok"
		if len(fails) > 0 {
			status = "FAIL"
			if c.required {
				failed++
			}
		}
		_, _ = fmt.Fprintf(d.out, "%-16s %-5s %s\n", c.name, status, c.inspects)
		for _, line := range fails {
			_, _ = fmt.Fprintf(d.out, "    %s\n", line)
		}
		for _, line := range r.notes {
			_, _ = fmt.Fprintf(d.out, "    %s\n", line)
		}
	}
	if failed > 0 {
		return fmt.Errorf("the container rehearsal failed %d required check(s); the table above lists each", failed)
	}
	_, _ = fmt.Fprintln(d.out, "the container rehearsal passed every required check")
	return nil
}

// rehearseMarker is the file that makes a directory this run's: cleanup deletes
// the run directory only when the marker in it names this run.
const rehearseMarker = ".gt-rehearsal-run"

// chooseRunDir picks this run's unique directory under the live town's
// .runtime/rehearsal, on resolved paths: a link that leads the directory out of
// the live town is refused, so the rehearsal never writes or deletes outside it.
func (r *rehearsal) chooseRunDir() error {
	live, err := r.d.evalSymlinks(r.f.liveRoot)
	if err != nil {
		return fmt.Errorf("--live-root %q cannot be resolved: %w", r.f.liveRoot, err)
	}
	dir := filepath.Join(live, ".runtime", "rehearsal", "run-"+r.d.newID())
	resolved, err := resolveThroughExisting(dir, r.d.evalSymlinks)
	if err != nil {
		return fmt.Errorf("the run directory %q cannot be resolved: %w", dir, err)
	}
	if !pathWithin(resolved, live) || resolved != dir {
		return fmt.Errorf("the run directory %q resolves to %q, outside the live town %q; the rehearsal writes and deletes only inside the live town's .runtime/rehearsal", dir, resolved, live)
	}
	r.f.runDir, r.f.root = dir, filepath.Join(dir, "town")
	return nil
}

// cleanup removes the run directory, after reading its marker: a directory
// that does not carry this run's marker is not deleted.
func (r *rehearsal) cleanup() error {
	got, err := r.d.readFile(filepath.Join(r.f.runDir, rehearseMarker))
	if err != nil || string(got) != r.marker() {
		return fmt.Errorf("the run directory %s does not carry this run's marker, so it was not removed; delete it by hand if it is leftover", r.f.runDir)
	}
	if err := r.d.removeAll(r.f.runDir); err != nil {
		return fmt.Errorf("the rehearsal's run directory %s could not be removed: %w", r.f.runDir, err)
	}
	return nil
}

func (r *rehearsal) marker() string { return "gt rehearsal run " + filepath.Base(r.f.runDir) + "\n" }

// stage writes the scratch copy under r.f.root: the live JSON settings files
// with secret-named values blanked, daemon.env with every value blanked, and
// mayor/town.json. Nothing else in settings/ is copied, so a file the daemon
// does not read cannot carry a credential into the copy.
func (r *rehearsal) stage() error {
	src := filepath.Join(r.f.liveRoot, "settings")
	entries, err := r.d.readDir(src)
	if err != nil {
		return fmt.Errorf("read the live settings directory: %w", err)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		name := e.Name()
		isEnv, isJSON := name == "daemon.env", strings.HasSuffix(name, ".json")
		if !e.Type().IsRegular() || !(isEnv || isJSON) {
			continue
		}
		data, err := r.d.readFile(filepath.Join(src, name))
		if err != nil {
			return fmt.Errorf("read the live setting %s: %w", name, err)
		}
		if isEnv {
			data = blankEnvValues(data)
		} else {
			var values []settingValue
			if data, values, err = redactSettingsJSON(data); err != nil {
				return fmt.Errorf("the live setting %s: %w", name, err)
			}
			r.values = append(r.values, values...)
			if name == "config.json" {
				r.config = data
			}
		}
		files[filepath.Join("settings", name)] = data
	}
	town, err := r.d.readFile(filepath.Join(r.f.liveRoot, "mayor", "town.json"))
	if err != nil {
		return fmt.Errorf("read the live mayor/town.json: %w", err)
	}
	files[filepath.Join("mayor", "town.json")] = town
	if err := r.d.mkdirAll(r.f.runDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", r.f.runDir, err)
	}
	if err := r.d.write(filepath.Join(r.f.runDir, rehearseMarker), []byte(r.marker()), 0o600); err != nil {
		return fmt.Errorf("write the run marker: %w", err)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		dst := filepath.Join(r.f.root, n)
		if err := r.d.mkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
		}
		if err := r.d.write(dst, files[n], 0o600); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return nil
}

// rehearseSecretKey matches the JSON keys whose string values are credentials.
var rehearseSecretKey = regexp.MustCompile(`(?i)token|secret|passw|credential|api[_-]?key|private`)

// blankedJSON is the empty value of v's JSON type, so the staged file keeps the
// shape the config loader expects and none of the content.
func blankedJSON(v any) any {
	switch v.(type) {
	case string:
		return ""
	case float64:
		return float64(0)
	case bool:
		return false
	case map[string]any:
		return map[string]any{}
	case []any:
		return []any{}
	}
	return nil
}

// settingValue is one string value of the staged JSON settings and the key path
// that holds it, rendered so two different paths cannot read alike: a member
// that starts with a letter or '_' and holds only letters, digits, '_' and '-'
// joins with a dot, any other member is quoted inside brackets, and a list
// index is [i]. A value at the top of its file has the empty path, and a
// top-level list is rooted at rehearseKeyRoot so its indices are not left bare.
type settingValue struct {
	key  string
	text string
}

// rehearseKeyRoot names the value at the top of its setting file, which no key
// segment reaches. It prefixes a top-level list's first index.
const rehearseKeyRoot = "(top level)"

// rehearsePlainMember matches a JSON member name that reads unambiguously as a
// dotted segment: it starts with a letter or '_' and holds only letters, digits,
// '_' and '-'.
var rehearsePlainMember = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// rehearseMemberKey appends one JSON object member to a key path.
func rehearseMemberKey(key, member string) string {
	if rehearsePlainMember.MatchString(member) {
		if key == "" {
			return member
		}
		return key + "." + member
	}
	return key + "[" + strconv.Quote(member) + "]"
}

// rehearseIndexKey appends one list index to a key path.
func rehearseIndexKey(key string, i int) string {
	if key == "" {
		key = rehearseKeyRoot
	}
	return key + "[" + strconv.Itoa(i) + "]"
}

// redactSettingsJSON blanks the value of every secret-named key, of whatever
// type or content, and returns the staged
// bytes with every string value the staged file holds, each with its key path. A
// file that is not JSON is refused rather than copied.
func redactSettingsJSON(data []byte) ([]byte, []settingValue, error) {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("is not JSON, so it cannot be redacted and is not staged: %w", err)
	}
	var values []settingValue
	var walk func(v any, key string) any
	walk = func(v any, key string) any {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if rehearseSecretKey.MatchString(k) {
					// Whatever the value's type or content, nothing of it is staged
					// (a ${NAME} reference included: the rehearsal needs no value).
					t[k] = blankedJSON(x)
					continue
				}
				t[k] = walk(x, rehearseMemberKey(key, k))
			}
		case []any:
			for i, x := range t {
				t[i] = walk(x, rehearseIndexKey(key, i))
			}
		case string:
			values = append(values, settingValue{key: key, text: t})
		}
		return v
	}
	doc = walk(doc, "")
	// A decoded JSON document always encodes.
	out, _ := json.MarshalIndent(doc, "", "  ")
	return out, values, nil
}

// blankEnvValues keeps every key of an env file and drops every value, so no
// secret is copied. Comments and blank lines are kept; "export KEY=value"
// becomes "export KEY=".
func blankEnvValues(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, _, ok := strings.Cut(l, "="); ok {
			lines[i] = k + "="
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// docker runs one docker command; the context is the Docker Desktop one every
// image command uses.
func (r *rehearsal) docker(ctx context.Context, args ...string) error {
	budget := r.d.callBudget
	if budget == 0 {
		budget = rehearseCallBudget
	}
	return r.dockerWithin(ctx, budget, args...)
}

// dockerOutput is docker's output under the call budget, for a command whose text
// the rehearsal reads (the entrypoint container's log).
func (r *rehearsal) dockerOutput(ctx context.Context, args ...string) (string, error) {
	budget := r.d.callBudget
	if budget == 0 {
		budget = rehearseCallBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	argv := append([]string{"docker", "--context", daemonimage.DockerContext}, args...)
	out, err := r.d.capture(ctx, r.f.root, daemonimage.DockerEnv(r.d.environ), argv)
	return string(out), err
}

// dockerWithin is docker under a deadline of its own, so no call can outlast
// its budget (a hung engine fails the check, it does not hang the rehearsal).
func (r *rehearsal) dockerWithin(ctx context.Context, budget time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	argv := append([]string{"docker", "--context", daemonimage.DockerContext}, args...)
	return r.d.run(ctx, r.f.root, daemonimage.DockerEnv(r.d.environ), argv)
}

// inImage runs argv in a fresh container of the image with the scratch town
// bound read-only at its own path and GT_TOWN_ROOT naming it; network is the
// docker --network value.
func (r *rehearsal) inImage(ctx context.Context, network string, argv ...string) error {
	args := []string{"run", "--rm", "--network", network, "-v", r.f.root + ":" + r.f.root + ":ro",
		"-w", r.f.root, "-e", "GT_TOWN_ROOT=" + r.f.root, "--entrypoint", argv[0], r.f.image}
	return r.docker(ctx, append(args, argv[1:]...)...)
}

func checkConfigValidates(ctx context.Context, r *rehearsal) []string {
	err := r.inImage(ctx, "none", "gt", "config", "validate")
	switch {
	case err == nil:
		return nil
	case exitCodeOf(err) == 1:
		return []string{"gt config validate: the settings do not load: " + err.Error()}
	}
	return []string{"gt config validate: the probe did not run: " + err.Error()}
}

// probeSaidNo reports whether err is the probe's own "no" (one of its exit
// codes) and not a docker failure (the engine down, an unknown image, a run
// that never started).
func probeSaidNo(err error, codes ...int) bool { return slices.Contains(codes, exitCodeOf(err)) }

func checkImageBinaries(ctx context.Context, r *rehearsal) []string {
	var fails []string
	for _, b := range rehearseBinaries {
		err := r.inImage(ctx, "none", "bash", "-c", "command -v "+b)
		switch {
		case err == nil:
		case probeSaidNo(err, 1):
			fails = append(fails, b+": not on the image's PATH")
		default:
			// Not a "not found": docker could not run the probe (it is down, or
			// the image is unknown), so the binary was not looked for.
			fails = append(fails, b+": the probe did not run: "+err.Error())
		}
	}
	return fails
}

// rehearseAcceptedPaths returns the absolute paths a setting may name that the
// rehearsal does not probe, each with the reason it is accepted. An accepted path
// prints as accepted in the paths row and does not fail it. The two notify
// scripts are named under the account's home, so they follow the home directory
// of the account running the rehearsal rather than one literal path.
func rehearseAcceptedPaths(home string) map[string]string {
	return map[string]string{
		"/Users/Shared/gt-gotmp.noindex":                            "GOTMPDIR is host-only and dropped from the container env",
		filepath.Join(home, ".claude", "tools", "imessage-push.sh"): "escalation notify, deferred by the operator",
		filepath.Join(home, ".config", "gt", "ntfy-push.sh"):        "escalation notify, deferred by the operator",
	}
}

// valueTexts is the string value of each staged JSON setting, without its key.
func (r *rehearsal) valueTexts() []string {
	texts := make([]string, 0, len(r.values))
	for _, v := range r.values {
		texts = append(texts, v.text)
	}
	return texts
}

// settingPath is one file path a setting names, with the sorted key paths that
// name it.
type settingPath struct {
	path string
	keys []string
}

// settingPaths returns the distinct file paths the staged settings name, sorted,
// skipping any under one of binds (the live compose's bind targets, which the
// container sees as the host does). A value is split on whitespace, commas,
// colons and equals signs, so a PATH-like list gives each entry; a token is a
// path when it starts with one slash, which leaves out a URL (its "//host/path"
// remainder starts with two).
func (r *rehearsal) settingPaths(binds []string) []settingPath {
	keys := map[string][]string{}
	for _, v := range r.values {
		for _, tok := range strings.FieldsFunc(v.text, func(c rune) bool { return strings.ContainsRune(" \t\n,:=", c) }) {
			if !strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "//") || len(tok) < 2 {
				continue
			}
			if slices.ContainsFunc(binds, func(b string) bool { return pathWithin(tok, b) }) {
				continue
			}
			if !slices.Contains(keys[tok], v.key) {
				keys[tok] = append(keys[tok], v.key)
			}
		}
	}
	paths := make([]settingPath, 0, len(keys))
	for p, ks := range keys {
		sort.Strings(ks)
		paths = append(paths, settingPath{path: p, keys: ks})
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i].path < paths[j].path })
	return paths
}

// composeBindTargets reads the live compose file and returns the in-container
// target of every bind mount of its daemon service: the paths the container sees
// without the image.
func composeBindTargets(data []byte) ([]string, error) {
	var f struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `yaml:"type"`
				Target string `yaml:"target"`
			} `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("does not parse: %w", err)
	}
	var targets []string
	for _, v := range f.Services["daemon"].Volumes {
		if v.Type == "bind" && v.Target != "" {
			targets = append(targets, v.Target)
		}
	}
	return targets, nil
}

// exitCodeOf is the exit status a command's error carries (*exec.ExitError has
// ExitCode), or -1 for an error that is not a command's exit.
func exitCodeOf(err error) int {
	var e interface{ ExitCode() int }
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}

func checkSettingPaths(ctx context.Context, r *rehearsal) []string {
	home, err := r.d.userHomeDir()
	if err != nil {
		return []string{"the home directory cannot be determined, so the accepted paths are unknown: " + err.Error()}
	}
	home = strings.TrimSpace(home)
	if !filepath.IsAbs(home) {
		return []string{"the home directory is not an absolute path, so the accepted paths are unknown: " + strconv.Quote(home)}
	}
	accepted := rehearseAcceptedPaths(home)
	composePath := config.ContainerComposeFile(r.f.liveRoot)
	data, err := r.d.readFile(composePath)
	if err != nil {
		return []string{"the live compose file " + composePath + " cannot be read, so the binds are unknown (run gt daemon compose write first): " + err.Error()}
	}
	binds, err := composeBindTargets(data)
	if err != nil {
		return []string{"the live compose file " + composePath + " " + err.Error()}
	}
	var fails []string
	for _, sp := range r.settingPaths(binds) {
		if reason, ok := accepted[sp.path]; ok {
			r.notes = append(r.notes, "accepted: "+sp.path+" ("+reason+")")
			continue
		}
		err := r.inImage(ctx, "none", "test", "-e", sp.path)
		switch {
		case err == nil:
		case probeSaidNo(err, 1):
			fails = append(fails, fmt.Sprintf("%s: named by %s, absent from the image and not under a bind of the live compose; the row stays red until a bead lands whose fix makes the image carry it, mounts a bind of the live compose over it, or drops the setting", sp.path, settingKeyNames(sp.keys)))
		default:
			fails = append(fails, sp.path+": the probe did not run: "+err.Error())
		}
	}
	return fails
}

// settingKeyNames names the key paths that hold a value; a value at the top of
// its file has the empty path, which is named as such.
func settingKeyNames(keys []string) string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k
		if k == "" {
			names[i] = rehearseKeyRoot
		}
	}
	return strings.Join(names, ", ")
}

// townDoltAddress is the Dolt host:port the staged mayor/town.json names, as the
// container reaches it: dolt.container_host when set, else dolt.host, with
// dolt.port. It reads the staged copy, not the live file. An empty host is
// refused: inside the image an empty host is the container itself.
func (r *rehearsal) townDoltAddress() (string, error) {
	data, err := r.d.readFile(filepath.Join(r.f.root, "mayor", "town.json"))
	if err != nil {
		return "", fmt.Errorf("the staged mayor/town.json cannot be read, so the Dolt address is unknown: %w", err)
	}
	var town struct {
		Dolt *config.DoltEndpoint `json:"dolt"`
	}
	if err := json.Unmarshal(data, &town); err != nil {
		return "", fmt.Errorf("the staged mayor/town.json does not parse, so the Dolt address is unknown: %w", err)
	}
	if town.Dolt == nil || town.Dolt.Port == 0 {
		return "", errors.New("the staged mayor/town.json names no dolt port, so the Dolt address is unknown")
	}
	host := strings.TrimSpace(town.Dolt.ContainerHost)
	if host == "" {
		host = strings.TrimSpace(town.Dolt.Host)
	}
	if host == "" {
		return "", errors.New("the staged mayor/town.json names no dolt host (dolt.container_host or dolt.host), so the Dolt address is unknown")
	}
	return fmt.Sprintf("%s:%d", host, town.Dolt.Port), nil
}

func checkSettingAddresses(ctx context.Context, r *rehearsal) []string {
	var fails []string
	addrs := rehearseAddr.FindAllString(strings.Join(r.valueTexts(), "\n"), -1)
	dolt, doltErr := r.townDoltAddress()
	if doltErr != nil {
		fails = append(fails, doltErr.Error())
	} else {
		addrs = append(addrs, dolt)
	}
	seen := map[string]bool{}
	for _, a := range addrs {
		if seen[a] {
			continue
		}
		seen[a] = true
		host, port, ok := probeTarget(a)
		if !ok {
			fails = append(fails, a+": is not a host:port the probe can dial")
			continue
		}
		probe := "exec 3<>/dev/tcp/" + host + "/" + port
		err := r.inImage(ctx, "bridge", "timeout", "5", "bash", "-c", probe)
		switch {
		case err == nil:
		case probeSaidNo(err, 1, 124):
			fails = append(fails, a+": does not accept a TCP connect from the image")
		default:
			fails = append(fails, a+": the probe did not run: "+err.Error())
		}
	}
	return fails
}

// composeImages are the images the live compose pulls besides the daemon image
// itself. A pin the registry no longer serves is found here, loudly, and not by
// the cutover on an engine that has not cached it.
func composeImages() []string { return []string{templates.DindImage} }

func checkComposeImagesPull(ctx context.Context, r *rehearsal) []string {
	var fails []string
	for _, img := range composeImages() {
		if err := r.dockerWithin(ctx, rehearsePullBudget, "pull", img); err != nil {
			fails = append(fails, img+": cannot be pulled by digest on this engine: "+err.Error())
		}
	}
	return fails
}

// checkFirstHeartbeat starts the image's own entrypoint on the scratch town with
// no network and waits at most the budget for state.json to record a heartbeat.
// The container's log can also pass it: the bd handshake line, read once the
// container has exited or the budget is spent. Any other end fails the check with
// the tail of the log. The container is removed on every path.
func checkFirstHeartbeat(ctx context.Context, r *rehearsal) []string {
	// One name per run, so a container of another run is never this run's to remove.
	name := "gt-" + filepath.Base(r.f.runDir)
	defer func() { _ = r.docker(context.WithoutCancel(ctx), "rm", "-f", name) }()
	if err := r.docker(ctx, "run", "-d", "--name", name, "--network", "none",
		"-v", r.f.root+":"+r.f.root, "-e", config.ContainerModeEnv+"="+config.SupervisorContainer,
		"-e", "GT_TOWN_ROOT="+r.f.root, r.f.image); err != nil {
		return []string{"the entrypoint container did not start: " + err.Error()}
	}
	switch r.waitForHeartbeat(ctx, name) {
	case waitHeartbeat:
		return nil
	case waitExited:
		return r.heartbeatFailure(ctx, name, "the entrypoint container exited before a heartbeat")
	}
	return r.heartbeatFailure(ctx, name, fmt.Sprintf("no heartbeat in %s", r.f.budget))
}

// heartbeatFailure reads the entrypoint container's log and returns no failure
// when it holds the bd handshake line. Otherwise it returns why and names an
// empty log as empty; a non-empty log adds its last rehearseTailLines lines, one
// table line each.
func (r *rehearsal) heartbeatFailure(ctx context.Context, name, why string) []string {
	logText, err := r.dockerOutput(ctx, "logs", name)
	if err != nil {
		return []string{why + "; its log could not be read, so the bd handshake line is unknown: " + err.Error()}
	}
	if strings.Contains(logText, rehearseHandshakeLine) {
		return nil
	}
	if strings.TrimSpace(logText) == "" {
		return []string{why + "; the log is empty, so it holds no bd handshake line"}
	}
	lines := strings.Split(strings.TrimRight(logText, "\n"), "\n")
	if len(lines) > rehearseTailLines {
		lines = lines[len(lines)-rehearseTailLines:]
	}
	return append([]string{why + "; the log holds no bd handshake line; its last lines follow:"}, lines...)
}

// waitResult is how waitForHeartbeat ended.
type waitResult int

const (
	waitHeartbeat waitResult = iota // state.json records a heartbeat
	waitExited                      // the container is no longer running
	waitBudget                      // the budget is spent
)

// waitForHeartbeat polls the scratch town's state.json until it records a
// heartbeat, the container stops running (a crash is named when it happens, not
// after the budget), or the budget is spent.
func (r *rehearsal) waitForHeartbeat(ctx context.Context, name string) waitResult {
	// Every docker call below runs under this context, so a call that blocks
	// ends the wait at the budget, whatever the clock says.
	ctx, cancel := context.WithTimeout(ctx, r.f.budget)
	defer cancel()
	deadline := r.d.now().Add(r.f.budget)
	state := filepath.Join(r.f.root, "daemon", "state.json")
	for {
		if data, err := r.d.readFile(state); err == nil {
			var s daemon.State
			if json.Unmarshal(data, &s) == nil && !s.LastHeartbeat.IsZero() {
				return waitHeartbeat
			}
		}
		if r.docker(ctx, "exec", name, "true") != nil {
			if ctx.Err() != nil {
				return waitBudget
			}
			return waitExited
		}
		if !r.d.now().Before(deadline) {
			return waitBudget
		}
		r.d.sleep(rehearsePoll)
	}
}
