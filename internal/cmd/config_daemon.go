package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt config daemon (gt-yjzepk.4): settings/config.json is edited by a command
// instead of a text editor, so it can live on the daemon's volume. Every write
// takes the file's lock without waiting, backs the file up, validates the new
// document through the loader the daemon runs, and replaces the file
// atomically; a document the daemon would refuse is never saved.

var configDaemonLabel string

// daemonConfigRetention is the backups a write keeps: the newest 50, and every
// copy younger than 30 days. The operator edits this file rarely and a change
// is expensive to reconstruct, so a backup lives longer than the patrol
// config's week (gt-yjzepk.4).
var daemonConfigRetention = config.BackupRetention{Count: 50, Age: 30 * 24 * time.Hour}

// daemonConfigLabelPattern matches a label that is safe in a file name: the
// label is the operator's text and becomes part of the backup's path.
var daemonConfigLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// daemonConfigStampFormat is the UTC timestamp in a backup's name. It carries
// nanoseconds so two writes in one second order by their name and never
// collide (gt-yjzepk.4).
const daemonConfigStampFormat = "20060102T150405.000000000Z"

var configDaemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Read and change settings/config.json the way the daemon loads it",
	Long: `Read and change settings/config.json as a JSON document, in the daemon's own
terms: the new document is validated by the loader and the patrol validators
the daemon runs, so a change it would refuse is refused here and nothing is
written.

A path addresses objects by key and lists by index: daemon.patrols.overrun_guard
names the overrun guard's block, and disabled_patrols.0 the first entry. A missing
parent is created for 'set' as an object; a list element is addressed in place and
never created.

Each write takes the file's lock without waiting (a lock another writer holds is
refused), copies the file to config.json.bak-<UTC timestamp>-<label>, and prints a
unified diff of the old and new documents. A value that looks like a token is
refused, and the diff redacts values under such keys; 'gt config secrets migrate'
moves a literal token to settings/daemon.env.

A value that looks like a token is refused on every route, as an argument and
from standard input alike: settings/config.json never holds a literal token. Put
it in settings/daemon.env and reference it here as "${VAR}". 'get' and 'diff' show
a token already in the file as <redacted>, never its value.

Examples:
  gt config daemon get daemon.patrols.overrun_guard
  gt config daemon get                       # the whole file
  gt config daemon set cli_theme '"dark"' --label theme
  gt config daemon set daemon.env '{"GT_SEAT_REFILL_MAX_PRIORITY": "50"}' --label env
  gt config daemon set daemon.env - --label env < env.json   # the value from stdin
  gt config daemon unset disabled_patrols.0 --label patrols
  gt config daemon diff                      # what the last write changed`,
	RunE: requireSubcommand,
}

var configDaemonGetCmd = &cobra.Command{
	Use:   "get [path]",
	Short: "Print a value from settings/config.json as JSON",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runConfigDaemonGet,
}

var configDaemonSetCmd = &cobra.Command{
	Use:   "set <path> <json>",
	Short: "Set one value in settings/config.json",
	Args:  cobra.ExactArgs(2),
	RunE:  runConfigDaemonSet,
}

var configDaemonUnsetCmd = &cobra.Command{
	Use:   "unset <path>",
	Short: "Remove one value from settings/config.json",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigDaemonUnset,
}

var configDaemonDiffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Print what the last write changed",
	Args:  cobra.NoArgs,
	RunE:  runConfigDaemonDiff,
}

func init() {
	for _, c := range []*cobra.Command{configDaemonSetCmd, configDaemonUnsetCmd} {
		c.Flags().StringVar(&configDaemonLabel, "label", "", "Why the change is made; names the backup config.json.bak-<timestamp>-<label> (required)")
	}
	configDaemonCmd.AddCommand(configDaemonGetCmd, configDaemonSetCmd, configDaemonUnsetCmd, configDaemonDiffCmd)
	configCmd.AddCommand(configDaemonCmd)
}

// configDaemonEnv is where a gt config daemon command finds its town and its
// streams. The cobra entry points use the cwd's town and the process streams;
// a test gives a temp town and buffers, so it needs no chdir.
type configDaemonEnv struct {
	findTown func() (string, error)
	out      io.Writer
	errOut   io.Writer
	in       io.Reader
	now      func() time.Time
	// update is how a write reaches the file. Nil is
	// config.UpdateConfigJSONNoWait; a test injects a writer that fails after
	// the mutate (and its backup) ran, so the cleanup has a failure to answer.
	update func(path string, perm os.FileMode, mutate func(*json.RawMessage, bool) error) error
}

// updateConfig runs a write through the writer the command shares with the
// daemon's reload, or the one a test injected.
func (e configDaemonEnv) updateConfig(path string, perm os.FileMode, mutate func(*json.RawMessage, bool) error) error {
	if e.update != nil {
		return e.update(path, perm, mutate)
	}
	return config.UpdateConfigJSONNoWait(path, perm, mutate)
}

func cwdConfigDaemonEnv() configDaemonEnv {
	return configDaemonEnv{
		findTown: workspace.FindFromCwdOrError,
		out:      os.Stdout,
		errOut:   os.Stderr,
		in:       os.Stdin,
		now:      time.Now,
	}
}

func runConfigDaemonGet(cmd *cobra.Command, args []string) error {
	return configDaemonGet(cwdConfigDaemonEnv(), args)
}

func runConfigDaemonSet(cmd *cobra.Command, args []string) error {
	return configDaemonSet(cwdConfigDaemonEnv(), args[0], args[1], configDaemonLabel)
}

func runConfigDaemonUnset(cmd *cobra.Command, args []string) error {
	return configDaemonUnset(cwdConfigDaemonEnv(), args[0], configDaemonLabel)
}

func runConfigDaemonDiff(cmd *cobra.Command, args []string) error {
	return configDaemonDiff(cwdConfigDaemonEnv())
}

func configDaemonGet(e configDaemonEnv, args []string) error {
	_, path, err := daemonConfigPath(e)
	if err != nil {
		return err
	}
	tree, err := readDaemonConfig(path)
	if err != nil {
		return err
	}
	var value any = tree
	leaf := ""
	if len(args) == 1 {
		if value, err = getAtPath(tree, args[0]); err != nil {
			return err
		}
		leaf = args[0]
		if segments, segErr := jsonPathSegments(args[0]); segErr == nil {
			leaf = segments[len(segments)-1]
		}
	}
	// A token the file already holds is never printed: it would reach stdout
	// and any transcript or log that runs this command, the same exposure the
	// diff redacts (gt-yjzepk.4).
	fmt.Fprintf(e.out, "%s", indentDaemonConfigJSON(redactDaemonConfigTree(value, nil, leaf, "<redacted>")))
	return nil
}

func configDaemonSet(e configDaemonEnv, pathArg, valueArg, label string) error {
	value, err := parseDaemonConfigValue(valueArg, e.in)
	if err != nil {
		return err
	}
	if err := refuseSecretLiteral(pathArg, value); err != nil {
		return err
	}
	return applyDaemonConfigChange(e, label, func(tree map[string]any) error {
		return setAtPath(tree, pathArg, value)
	})
}

func configDaemonUnset(e configDaemonEnv, pathArg, label string) error {
	return applyDaemonConfigChange(e, label, func(tree map[string]any) error {
		return unsetAtPath(tree, pathArg)
	})
}

func configDaemonDiff(e configDaemonEnv) error {
	townRoot, path, err := daemonConfigPath(e)
	if err != nil {
		return err
	}
	backup, err := newestDaemonConfigBackup(path)
	if err != nil {
		return err
	}
	rel := config.SourcePathRel(townRoot, path)
	if backup == "" {
		return fmt.Errorf("%s has no .bak-* copy: no change has been written", rel)
	}
	oldTree, err := config.ReadRawJSON(backup)
	if err != nil {
		return fmt.Errorf("reading the last backup: %w", err)
	}
	newTree, err := readDaemonConfig(path)
	if err != nil {
		return err
	}
	diff := daemonConfigDiff(rel, oldTree, newTree)
	if diff == "" {
		fmt.Fprintf(e.out, "%s is unchanged since %s\n", rel, filepath.Base(backup))
		return nil
	}
	fmt.Fprintf(e.out, "%s", diff)
	return nil
}

// daemonConfigPath resolves the town root and the file the daemon reads.
func daemonConfigPath(e configDaemonEnv) (townRoot, path string, err error) {
	townRoot, err = e.findTown()
	if err != nil {
		return "", "", fmt.Errorf("finding town root: %w", err)
	}
	return townRoot, config.TownSettingsPath(townRoot), nil
}

// readDaemonConfig reads and validates settings/config.json as the daemon
// would. A file that does not load is an error, never an empty document.
func readDaemonConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the town's own settings path
	if err != nil {
		return nil, err
	}
	// The loader reports an offset into the file's bytes, so it validates the
	// bytes this read returned, and the tree comes from those same bytes.
	if _, err := validateDaemonConfigDocument(path, data); err != nil {
		return nil, err
	}
	return config.DecodeRawJSON(path, data)
}

// validateDaemonConfigDocument runs the validators the daemon runs over a
// settings/config.json document: the kernel load and the one daemon patrol
// validator, which includes the container_off step audit. Nothing here is a
// second copy of a daemon check (gt-yjzepk.4).
func validateDaemonConfigDocument(path string, data []byte) (*config.TownSettings, error) {
	settings, _, err := townconfig.ValidateSettingsDocument(path, data)
	if err != nil {
		return nil, err
	}
	if settings.Daemon != nil {
		if err := config.ValidateDaemonPatrolConfig(settings.Daemon); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return settings, nil
}

// applyDaemonConfigChange applies change to the file's document under its lock. A
// document the loader refuses, one holding a new literal token, or a held lock
// leaves the file and the backups as they were: a write that fails after its
// backup removes that backup again (gt-yjzepk.4).
func applyDaemonConfigChange(e configDaemonEnv, label string, change func(tree map[string]any) error) error {
	if err := validateDaemonConfigLabel(label); err != nil {
		return err
	}
	townRoot, path, err := daemonConfigPath(e)
	if err != nil {
		return err
	}
	rel := config.SourcePathRel(townRoot, path)
	var before, after map[string]any
	backup := ""
	err = e.updateConfig(path, 0o600, func(cur *json.RawMessage, exists bool) error {
		oldTree := map[string]any{}
		if exists {
			var decodeErr error
			if oldTree, decodeErr = config.DecodeRawJSON(path, *cur); decodeErr != nil {
				return decodeErr
			}
		}
		newTree := cloneJSONTree(oldTree)
		if err := change(newTree); err != nil {
			return err
		}
		if reflect.DeepEqual(oldTree, newTree) {
			return nil
		}
		// A tree decoded from JSON, with values parsed the same way, always
		// encodes.
		data, _ := json.Marshal(newTree)
		settings, err := validateDaemonConfigDocument(path, data)
		if err != nil {
			return err
		}
		if err := refuseNewLiteralSecrets(path, oldTree, newTree, settings); err != nil {
			return err
		}
		if exists {
			stamp := e.now().UTC().Format(daemonConfigStampFormat) + "-" + label
			created, err := config.BackupFileRetainedWithoutPrune(path, stamp)
			if err != nil {
				return err
			}
			backup = created
		}
		before, after = oldTree, newTree
		*cur = json.RawMessage(data)
		return nil
	})
	if err != nil {
		return errors.Join(err, removeDaemonConfigBackup(backup))
	}
	if backup != "" {
		// The write landed, so the retention window is pruned now: a write that
		// failed above deletes no older copy (gt-yjzepk.4).
		pruneDaemonConfigBackups(e, path, backup)
	}
	if before == nil {
		return nil
	}
	// The change is written: the diff reports what it did, not what it meant
	// to do.
	fmt.Fprintf(e.out, "%s", daemonConfigDiff(rel, before, after))
	return nil
}

// pruneDaemonConfigBackups prunes the retention window over the copies beside
// path after the write that made keep landed. Each copy removed is one line on
// errOut; a prune that fails is reported there too, because a directory that
// cannot be pruned must not fail a write that succeeded (gt-yjzepk.4).
func pruneDaemonConfigBackups(e configDaemonEnv, path, keep string) {
	err := config.PruneBackups(path, keep, daemonConfigRetention, func(removed string) {
		fmt.Fprintf(e.errOut, "removed backup %s\n", removed)
	})
	if err != nil {
		fmt.Fprintf(e.errOut, "pruning old backups of %s: %v\n", path, err)
	}
}

// removeDaemonConfigBackup deletes the copy a failed write leaves behind, so
// the last backup stays the last change's. A deletion that fails is returned,
// never dropped (gt-yjzepk.4).
func removeDaemonConfigBackup(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the backup %s after a failed write: %w", path, err)
	}
	return nil
}

// validateDaemonConfigLabel refuses the label that names nothing in a backup,
// or that would move the backup out of the file's directory.
func validateDaemonConfigLabel(label string) error {
	if label == "" {
		return errors.New("--label is required: it names the backup, config.json.bak-<timestamp>-<label>")
	}
	if !daemonConfigLabelPattern.MatchString(label) {
		return fmt.Errorf("--label %q: use letters, digits, '.', '_' and '-' only, starting with a letter or digit", label)
	}
	return nil
}

// refuseSecretLiteral refuses a value that looks like a literal token, whichever
// way it arrived: on argv it would show in the process list, and from standard
// input it would be written as plaintext into settings/config.json, which the
// town keeps free of literals (gt-y3pgh.5). Neither the value nor any part of it
// is named. The token belongs in settings/daemon.env, referenced here as
// "${VAR}".
func refuseSecretLiteral(pathArg string, value any) error {
	leaf := pathArg
	if segments, err := jsonPathSegments(pathArg); err == nil {
		leaf = segments[len(segments)-1]
	}
	if secretLikeValue(leaf, value) {
		return fmt.Errorf("the value at %s looks like a literal token, which settings/config.json must not hold: put it in settings/daemon.env and reference it here as \"${VAR}\" (gt config secrets migrate)", pathArg)
	}
	return nil
}

// secretLikeValue reports whether any string in value looks like a token under
// key, which is the value's own key or, inside a list, the key it was set for.
func secretLikeValue(key string, value any) bool {
	switch v := value.(type) {
	case string:
		return config.LooksLikeSecret(key, v)
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if secretLikeValue(k, v[k]) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if secretLikeValue(key, item) {
				return true
			}
		}
	}
	return false
}

// parseDaemonConfigValue parses the JSON value 'set' carries, read from stdin
// when arg is "-".
func parseDaemonConfigValue(arg string, in io.Reader) (any, error) {
	if arg == "-" {
		data, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("reading the value from standard input: %w", err)
		}
		arg = string(data)
	}
	dec := json.NewDecoder(strings.NewReader(arg))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("the value is not JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("the value is not one JSON value: text follows it")
	}
	return value, nil
}

// refuseNewLiteralSecrets refuses a document that adds a literal token or
// changes one, naming the key and never the value (gt-y3pgh.5). A token the
// file already held at the same value is left alone: the town warns about it,
// and refusing would block every unrelated change.
func refuseNewLiteralSecrets(path string, oldTree, newTree map[string]any, settings *config.TownSettings) error {
	var added []string
	for _, lit := range config.FindLiteralSecrets(settings) {
		p := lit.Path()
		next, nextErr := getAtPath(newTree, p)
		prev, prevErr := getAtPath(oldTree, p)
		if nextErr == nil && prevErr == nil && reflect.DeepEqual(prev, next) {
			continue
		}
		added = append(added, p)
	}
	if len(added) == 0 {
		return nil
	}
	return fmt.Errorf("%s: refusing to write a literal token at %s; put it in settings/daemon.env and reference it here as \"${VAR}\" (gt config secrets migrate)",
		path, strings.Join(added, ", "))
}

// daemonConfigDiff renders the change from oldTree to newTree as a unified
// diff. Both sides pass through the redaction first, so a token the file
// already holds is never printed, and a token that changed keeps a distinct
// marker on each side so the change is still visible (gt-yjzepk.4).
func daemonConfigDiff(rel string, oldTree, newTree map[string]any) string {
	oldText := indentDaemonConfigJSON(redactDaemonConfigTree(oldTree, newTree, "", "<redacted: before>"))
	newText := indentDaemonConfigJSON(redactDaemonConfigTree(newTree, oldTree, "", "<redacted: after>"))
	return unifiedDiff(rel+" (before)", rel+" (after)", oldText, newText)
}

// indentDaemonConfigJSON renders value as indented JSON and a final newline. It
// leaves characters such as & and < unescaped so a URL reads as it was written.
func indentDaemonConfigJSON(value any) string {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	// The value came from JSON, so the encoder cannot refuse it.
	_ = enc.Encode(value)
	return out.String()
}

// redactDaemonConfigTree replaces every string that looks like a token with a
// marker. key is the map key, or the list's key for an element, that governs
// the value; changed is the marker when the value differs from the one at the
// same place in other, so a changed token shows as a change and neither value
// is printed (gt-yjzepk.4).
func redactDaemonConfigTree(node, other any, key, changed string) any {
	switch n := node.(type) {
	case map[string]any:
		om, _ := other.(map[string]any)
		out := make(map[string]any, len(n))
		for k, v := range n {
			out[k] = redactDaemonConfigTree(v, om[k], k, changed)
		}
		return out
	case []any:
		oa, _ := other.([]any)
		out := make([]any, len(n))
		for i, v := range n {
			var ov any
			if i < len(oa) {
				ov = oa[i]
			}
			out[i] = redactDaemonConfigTree(v, ov, key, changed)
		}
		return out
	case string:
		if key == "" || !config.LooksLikeSecret(key, n) {
			return n
		}
		if prev, ok := other.(string); ok && prev == n {
			return "<redacted>"
		}
		return changed
	default:
		return node
	}
}

// newestDaemonConfigBackup is the newest config.json.bak-* copy beside path, ""
// when there is none. It is the last write's copy, which holds the document
// that write replaced. The name carries the UTC stamp with nanoseconds, so the
// greatest name is the last write's, even for two writes in one second
// (gt-yjzepk.4).
func newestDaemonConfigBackup(path string) (string, error) {
	dir := filepath.Dir(path)
	prefix := filepath.Base(path) + ".bak-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("reading %s: %w", dir, err)
	}
	newest := ""
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if entry.Name() > newest {
			newest = entry.Name()
		}
	}
	if newest == "" {
		return "", nil
	}
	return filepath.Join(dir, newest), nil
}

// jsonPathSegments splits a dotted path. An empty segment addresses nothing, so
// "a..b" is a typo to report rather than a key to look up.
func jsonPathSegments(dotted string) ([]string, error) {
	if dotted == "" {
		return nil, errors.New("empty path")
	}
	segments := strings.Split(dotted, ".")
	for _, segment := range segments {
		if segment == "" {
			return nil, fmt.Errorf("path %q has an empty segment", dotted)
		}
	}
	return segments, nil
}

// pathAt names the location reached by segments, for an error.
func pathAt(segments []string) string {
	if len(segments) == 0 {
		return "the document"
	}
	return strconv.Quote(strings.Join(segments, "."))
}

// childAt returns the value at key in node, which is an object or a list.
func childAt(node any, key string, at []string) (any, error) {
	switch n := node.(type) {
	case map[string]any:
		value, ok := n[key]
		if !ok {
			return nil, fmt.Errorf("no key %q at %s", key, pathAt(at))
		}
		return value, nil
	case []any:
		index, err := strconv.Atoi(key)
		if err != nil {
			return nil, fmt.Errorf("%s is a list: %q is not an index", pathAt(at), key)
		}
		if index < 0 || index >= len(n) {
			return nil, fmt.Errorf("%s: index %d is out of range (the list has %d)", pathAt(at), index, len(n))
		}
		return n[index], nil
	default:
		return nil, fmt.Errorf("%s is not an object or a list, so %q cannot be read from it", pathAt(at), key)
	}
}

// childOrObject returns the value at key in node, creating an empty object for
// a key the object does not hold: set creates a missing parent.
func childOrObject(node any, key string, at []string) (any, error) {
	object, ok := node.(map[string]any)
	if !ok {
		return childAt(node, key, at)
	}
	if value, ok := object[key]; ok {
		return value, nil
	}
	value := map[string]any{}
	object[key] = value
	return value, nil
}

func assignAt(node any, key string, value any, at []string) error {
	switch n := node.(type) {
	case map[string]any:
		n[key] = value
		return nil
	case []any:
		index, err := strconv.Atoi(key)
		if err != nil {
			return fmt.Errorf("%s is a list: %q is not an index", pathAt(at), key)
		}
		if index < 0 || index >= len(n) {
			return fmt.Errorf("%s: index %d is out of range (the list has %d)", pathAt(at), index, len(n))
		}
		n[index] = value
		return nil
	default:
		return fmt.Errorf("%s is not an object or a list, so %q cannot be set in it", pathAt(at), key)
	}
}

// getAtPath returns the value at dotted in tree.
func getAtPath(tree map[string]any, dotted string) (any, error) {
	segments, err := jsonPathSegments(dotted)
	if err != nil {
		return nil, err
	}
	var node any = tree
	for i, segment := range segments {
		if node, err = childAt(node, segment, segments[:i]); err != nil {
			return nil, err
		}
	}
	return node, nil
}

// setAtPath stores value at dotted in tree, creating a missing parent object.
func setAtPath(tree map[string]any, dotted string, value any) error {
	segments, err := jsonPathSegments(dotted)
	if err != nil {
		return err
	}
	var node any = tree
	for i, segment := range segments[:len(segments)-1] {
		if node, err = childOrObject(node, segment, segments[:i]); err != nil {
			return err
		}
	}
	return assignAt(node, segments[len(segments)-1], value, segments[:len(segments)-1])
}

// unsetAtPath removes the value at dotted from tree. A list element is removed
// by rebuilding its list, so the elements after it keep their places.
func unsetAtPath(tree map[string]any, dotted string) error {
	segments, err := jsonPathSegments(dotted)
	if err != nil {
		return err
	}
	parentSegments := segments[:len(segments)-1]
	var parent, holder any = tree, nil
	holderKey := ""
	for i, segment := range parentSegments {
		holder, holderKey = parent, segment
		if parent, err = childAt(parent, segment, parentSegments[:i]); err != nil {
			return err
		}
	}
	key := segments[len(segments)-1]
	switch n := parent.(type) {
	case map[string]any:
		if _, ok := n[key]; !ok {
			return fmt.Errorf("no key %q at %s", key, pathAt(parentSegments))
		}
		delete(n, key)
		return nil
	case []any:
		index, err := strconv.Atoi(key)
		if err != nil {
			return fmt.Errorf("%s is a list: %q is not an index", pathAt(parentSegments), key)
		}
		if index < 0 || index >= len(n) {
			return fmt.Errorf("%s: index %d is out of range (the list has %d)", pathAt(parentSegments), index, len(n))
		}
		// holder is the container the list was read from; it is nil only for a
		// list at the document's root, which a settings document never is.
		return assignAt(holder, holderKey, append(n[:index:index], n[index+1:]...), parentSegments[:len(parentSegments)-1])
	default:
		return fmt.Errorf("%s is not an object or a list, so %q cannot be removed from it", pathAt(parentSegments), key)
	}
}

// diffOp is one line of a unified diff: ' ' unchanged, '-' removed, '+' added.
type diffOp struct {
	kind byte
	line string
}

// diffLines splits s into lines, dropping the empty element a trailing newline
// leaves behind.
func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiff returns the operations that turn a into b, by longest common
// subsequence.
func lineDiff(a, b []string) []diffOp {
	// maxDiffLines bounds the LCS table, so a corrupt document cannot ask for
	// one sized by its own length.
	const maxDiffLines = 2000
	n, m := len(a), len(b)
	if n > maxDiffLines || m > maxDiffLines {
		ops := make([]diffOp, 0, n+m)
		for _, line := range a {
			ops = append(ops, diffOp{'-', line})
		}
		for _, line := range b {
			ops = append(ops, diffOp{'+', line})
		}
		return ops
	}
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// unifiedDiff renders oldText and newText as a unified diff with three lines of
// context around the change, and "" when they are equal. One hunk covers every
// change: a valid unified diff, and a settings document is short.
func unifiedDiff(oldName, newName, oldText, newText string) string {
	ops := lineDiff(diffLines(oldText), diffLines(newText))
	first, last := -1, -1
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 {
		return ""
	}
	start, end := max(0, first-3), min(len(ops), last+4)
	oldStart, newStart := 1, 1
	for _, op := range ops[:start] {
		if op.kind != '+' {
			oldStart++
		}
		if op.kind != '-' {
			newStart++
		}
	}
	oldCount, newCount := 0, 0
	for _, op := range ops[start:end] {
		if op.kind != '+' {
			oldCount++
		}
		if op.kind != '-' {
			newCount++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", oldName, newName)
	fmt.Fprintf(&b, "@@ -%s +%s @@\n", diffRange(oldStart, oldCount), diffRange(newStart, newCount))
	for _, op := range ops[start:end] {
		b.WriteByte(op.kind)
		b.WriteString(op.line)
		b.WriteByte('\n')
	}
	return b.String()
}

// diffRange renders a unified diff range: a count of one is left out, and an
// empty range names the line before it.
func diffRange(start, count int) string {
	switch count {
	case 0:
		return strconv.Itoa(start-1) + ",0"
	case 1:
		return strconv.Itoa(start)
	default:
		return fmt.Sprintf("%d,%d", start, count)
	}
}
