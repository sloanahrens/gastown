package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
)

// restartedAtAnnotation is what `kubectl rollout restart` stamps on a pod
// template, so the live object carries it and the render never names it.
const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// serverSetMetadataKeys are the Deployment metadata fields the API server fills
// in after the render is applied. The one-way walk already skips a field the
// render never names; the skip keeps a render that ever names one from
// reporting the server's value as drift (gt-jqfk62).
var serverSetMetadataKeys = map[string]bool{
	"generation":        true,
	"resourceVersion":   true,
	"uid":               true,
	"managedFields":     true,
	"creationTimestamp": true,
}

// serverSetRootKeys are the top-level Deployment fields the server sets.
var serverSetRootKeys = map[string]bool{
	"status": true,
}

// objectDrift lists every rendered field of a kind object the live one lacks or
// carries differently, one sorted line per field naming its JSON path and a
// fingerprint of each side, never either value, and refuses either side that
// decoded to nothing (gt-jqfk62, gt-wi4885). It compares Deployments and the
// gate runner's ConfigMap (gt-pkz70c), whose config.yml prints its changed
// lines after the sorted ones (gt-svm7ds).
func objectDrift(kind string, rendered, live map[string]any) ([]string, error) {
	if len(rendered) == 0 {
		return nil, fmt.Errorf("%s drift: the rendered %s has no fields, so it was not read; an unreadable render is not a cluster in step", strings.ToLower(kind), kind)
	}
	if len(live) == 0 {
		return nil, fmt.Errorf("%s drift: the live %s has no fields, so it was not read; an unreadable object is not a cluster in step", strings.ToLower(kind), kind)
	}
	return objectDriftLines(rendered, live, runnerConfigObject(kind, rendered))
}

// objectAbsent lists every rendered field of an object the cluster does not
// hold as absent, one sorted line per top-level field, never either value. It is
// the drift of a static object the apply never made (gt-w6g6ok).
func objectAbsent(kind string, rendered map[string]any) ([]string, error) {
	if len(rendered) == 0 {
		return nil, fmt.Errorf("%s drift: the rendered %s has no fields, so it was not read; an unreadable render is not a cluster in step", strings.ToLower(kind), kind)
	}
	return objectDriftLines(rendered, map[string]any{}, false)
}

// objectDriftLines walks rendered against live and returns its sorted lines,
// then the runner config's changed lines when runnerConfig is set, or the first
// value it could not fingerprint.
func objectDriftLines(rendered, live map[string]any, runnerConfig bool) ([]string, error) {
	w := &driftWalk{runnerConfig: runnerConfig}
	w.object("", rendered, live)
	if w.err != nil {
		return nil, w.err
	}
	slices.Sort(w.drift)
	return append(w.drift, w.text...), nil
}

// driftWalk collects the differences a nested comparison finds, and the first
// value it could not fingerprint.
type driftWalk struct {
	drift []string
	err   error
	// runnerConfig marks the gate runner's ConfigMap; text holds its changed lines.
	runnerConfig bool
	text         []string
}

// object compares the rendered object at path against the live object and walks
// only the render's keys, since a field only the live object carries, such as a
// server default, is not drift.
func (w *driftWalk) object(path string, rendered, live map[string]any) {
	for _, key := range slices.Sorted(maps.Keys(rendered)) {
		if driftIgnoresKey(path, key) {
			continue
		}
		childPath := joinPath(path, key)
		liveValue, ok := live[key]
		if !ok {
			w.absent(childPath, rendered[key])
			continue
		}
		if key == "requests" || key == "limits" {
			renderedQuantities, renderedOK := rendered[key].(map[string]any)
			liveQuantities, liveOK := liveValue.(map[string]any)
			if renderedOK && liveOK {
				w.quantityMap(childPath, renderedQuantities, liveQuantities)
				continue
			}
		}
		w.value(childPath, rendered[key], liveValue)
	}
}

// value compares one rendered value at path against the live value.
func (w *driftWalk) value(path string, rendered, live any) {
	switch r := rendered.(type) {
	case map[string]any:
		l, ok := live.(map[string]any)
		if !ok {
			w.difference(path, rendered, live)
			return
		}
		w.object(path, r, l)
	case []any:
		l, ok := live.([]any)
		if !ok {
			w.difference(path, rendered, live)
			return
		}
		w.list(path, r, l)
	default:
		if !driftValuesEqual(rendered, live) {
			w.difference(path, rendered, live)
		}
	}
}

// quantityMap compares a resources requests or limits map, whose values are
// Kubernetes quantities.
func (w *driftWalk) quantityMap(path string, rendered, live map[string]any) {
	for _, key := range slices.Sorted(maps.Keys(rendered)) {
		childPath := joinPath(path, key)
		liveValue, ok := live[key]
		if !ok {
			w.absent(childPath, rendered[key])
			continue
		}
		renderedText, renderedOK := rendered[key].(string)
		liveText, liveOK := liveValue.(string)
		if renderedOK && liveOK {
			equal, known := quantityEqual(renderedText, liveText)
			if known {
				if !equal {
					w.difference(childPath, rendered[key], liveValue)
				}
				continue
			}
		}
		w.value(childPath, rendered[key], liveValue)
	}
}

// list compares a rendered list at path against the live list.
func (w *driftWalk) list(path string, rendered, live []any) {
	if keyField := listKeyField(lastPathField(path)); keyField != "" {
		if w.keyedList(path, keyField, rendered, live) {
			return
		}
	}
	// A positional list's length is part of its value: a live command with one
	// extra argument runs something the render never named, and is drift even
	// though that entry alone is live-only (gt-jqfk62).
	if len(rendered) != len(live) {
		w.difference(path, rendered, live)
		return
	}
	for i, entry := range rendered {
		w.value(path+"["+strconv.Itoa(i)+"]", entry, live[i])
	}
}

// keyedList matches entries by the field that identifies them, so an order the
// server changed is not drift. It reports false when either list cannot be
// indexed, and the caller falls back to the positional compare.
func (w *driftWalk) keyedList(path, keyField string, rendered, live []any) bool {
	liveByKey, ok := indexByKey(live, keyField)
	if !ok {
		return false
	}
	renderedKeys, ok := entryKeys(rendered, keyField)
	if !ok {
		return false
	}
	for i, entry := range rendered {
		entryPath := path + "[" + keyField + "=" + renderedKeys[i] + "]"
		liveEntry, ok := liveByKey[renderedKeys[i]]
		if !ok {
			w.absent(entryPath, entry)
			continue
		}
		w.value(entryPath, entry, liveEntry)
	}
	return true
}

// difference records a field the two decodings carry differently, or a config text.
func (w *driftWalk) difference(path string, rendered, live any) {
	if w.runnerConfig && path == runnerConfigPath && w.configText(path, rendered, live) {
		return
	}
	texts, marker, ok := w.lineSides(path, rendered, live)
	if !ok {
		return
	}
	w.record(path, texts[0], texts[1], marker)
}

// absent records a field or list entry the render names and the live object
// lacks. The path says which it is: a keyed list's entry path carries the key.
func (w *driftWalk) absent(path string, rendered any) {
	texts, marker, ok := w.lineSides(path, rendered)
	if !ok {
		return
	}
	w.record(path, texts[0], "absent", marker)
}

// lineSides is each side's text for a drift line and the marker that ends it,
// empty when the line prints its values. A path driftValueSafe reviewed prints
// each side as its compact JSON, unless a side names a secret word: an argument
// can carry a token, so one such side hides the whole line (gt-2h7fjc). Any other
// path prints each side as its fingerprint (gt-wi4885), and its marker is
// driftHiddenMarker even when a side names a secret word. A side with no text
// fails the run, since a line cannot stand in for a value it could not print or
// fingerprint.
func (w *driftWalk) lineSides(path string, values ...any) ([]string, string, bool) {
	marker := ""
	if !driftValueSafe(path) {
		marker = driftHiddenMarker
	}
	readable := make([]string, len(values))
	for i, value := range values {
		if marker != "" {
			break
		}
		text, err := driftReadableValue(value)
		if err != nil {
			w.err = fmt.Errorf("%s: %w", path, err)
			return nil, "", false
		}
		if configTextSecretWord.MatchString(text) {
			marker = driftSecretMarker
		}
		readable[i] = text
	}
	texts := make([]string, len(values))
	for i, value := range values {
		if marker == "" {
			texts[i] = readable[i]
			continue
		}
		text, ok := w.fingerprint(path, value)
		if !ok {
			return nil, "", false
		}
		texts[i] = text
	}
	return texts, marker, true
}

// fingerprint is one side's text for a drift line, or false when the value has
// none, which fails the run: a line cannot stand in for a value it could not
// fingerprint, since the stand-in would be the value (gt-wi4885).
func (w *driftWalk) fingerprint(path string, value any) (string, bool) {
	text, err := driftFingerprint(value)
	if err != nil {
		w.err = fmt.Errorf("%s: %w", path, err)
		return "", false
	}
	return text, true
}

// driftHiddenMarker ends a drift line whose sides are fingerprints because its
// path is not on the reviewed list, so a reader can tell a withheld value from a
// short one.
const driftHiddenMarker = " (values hidden: path not on the reviewed list)"

// driftSecretMarker ends a drift line whose sides are fingerprints because one
// side names a secret word on a path the reviewed list allows (gt-2h7fjc). It
// does not say the path is off the list: a listed path can carry a secret too.
const driftSecretMarker = " (values hidden: a side names a secret word)"

// record adds one line: the path and each side's text, then the marker that says
// why the values are hidden, empty when the line prints them. The reviewed list
// allows a value's print (gt-graamb) and every other path keeps fingerprints. A
// line never carries a value it hides, since a Deployment's spec can hold a
// secret and the line reaches a terminal and a log (gt-wi4885).
func (w *driftWalk) record(path, renderedText, observed, marker string) {
	w.drift = append(w.drift, path+": rendered "+renderedText+", observed "+observed+marker)
}

// runnerConfigPath is the gate runner's config.yml, whose changed lines print.
const runnerConfigPath = "data.config.yml"

// runnerConfigMapName is the gate runner's ConfigMap, as the render names it.
const runnerConfigMapName = forgejorunner.Name + "-config"

// configTextLineLimit is how many changed lines a runner config drift prints.
const configTextLineLimit = 40

// configTextCellLimit bounds the line-diff table; a larger diff is fingerprinted.
const configTextCellLimit = 1 << 20

// configTextSecretWord matches a token, secret, password or key, any case.
var configTextSecretWord = regexp.MustCompile(`(?i)token|secret|password|key`)

// runnerConfigObject reports whether rendered is the gate runner's ConfigMap,
// the one object whose config.yml may print its changed lines (gt-svm7ds).
func runnerConfigObject(kind string, rendered map[string]any) bool {
	if kind != "ConfigMap" {
		return false
	}
	metadata, ok := rendered["metadata"].(map[string]any)
	if !ok {
		return false
	}
	name, ok := metadata["name"].(string)
	return ok && name == runnerConfigMapName
}

// configChange is one changed line: "-" for observed, or "+" for rendered.
type configChange struct{ sign, line string }

// configText records the runner config's changed lines under path, at most
// configTextLineLimit and a count of the rest. A line naming a token is
// fingerprinted (gt-wi4885). It reports false, recording nothing, when a side is
// not text or the diff is too large, so the caller fingerprints the value.
func (w *driftWalk) configText(path string, rendered, live any) bool {
	renderedText, renderedOK := rendered.(string)
	liveText, liveOK := live.(string)
	if !renderedOK || !liveOK {
		return false
	}
	renderedLines := strings.Split(renderedText, "\n")
	liveLines := strings.Split(liveText, "\n")
	if len(renderedLines)*len(liveLines) > configTextCellLimit {
		return false
	}
	changes := configLineChanges(liveLines, renderedLines)
	w.text = append(w.text, path+":")
	for i, change := range changes {
		if i == configTextLineLimit {
			break
		}
		text := change.line
		if configTextSecretWord.MatchString(text) {
			fingerprint, ok := w.fingerprint(path, text)
			if !ok {
				return true
			}
			text = fingerprint
		}
		w.text = append(w.text, change.sign+" "+text)
	}
	if rest := len(changes) - configTextLineLimit; rest > 0 {
		w.text = append(w.text, fmt.Sprintf("... %d more changed lines not shown", rest))
	}
	return true
}

// configLineChanges diffs observed against rendered by longest common
// subsequence. Each run of changes between unchanged lines pairs its observed
// and rendered lines in turn, so a replaced line reads as '-' then '+'.
func configLineChanges(observed, rendered []string) []configChange {
	// common[i][j] is the LCS length of observed[i:] and rendered[j:].
	common := make([][]int, len(observed)+1)
	for i := range common {
		common[i] = make([]int, len(rendered)+1)
	}
	for i := len(observed) - 1; i >= 0; i-- {
		for j := len(rendered) - 1; j >= 0; j-- {
			if observed[i] == rendered[j] {
				common[i][j] = common[i+1][j+1] + 1
			} else {
				common[i][j] = max(common[i+1][j], common[i][j+1])
			}
		}
	}
	var changes, removed, added []configChange
	// flush emits a pending run, pairing its k-th lines and keeping any extra.
	flush := func() {
		for k := range max(len(removed), len(added)) {
			if k < len(removed) {
				changes = append(changes, removed[k])
			}
			if k < len(added) {
				changes = append(changes, added[k])
			}
		}
		removed, added = removed[:0], added[:0]
	}
	i, j := 0, 0
	for i < len(observed) || j < len(rendered) {
		switch {
		case i < len(observed) && j < len(rendered) && observed[i] == rendered[j]:
			flush()
			i++
			j++
		case j == len(rendered) || (i < len(observed) && common[i+1][j] >= common[i][j+1]):
			removed = append(removed, configChange{"-", observed[i]})
			i++
		default:
			added = append(added, configChange{"+", rendered[j]})
			j++
		}
	}
	flush()
	return changes
}

// driftIgnoresKey reports whether the compare skips key in the object at
// parent. A container's image is skipped because an image roll is not drift,
// the two rollout annotations for the same reason, and the server-set fields
// because their value belongs to the cluster (gt-jqfk62). The annotation names
// are fully qualified, so they skip wherever they appear; the server-set keys
// skip only at the Deployment root and under metadata, since a label or env
// var may be named "generation" and still hold what the render names.
func driftIgnoresKey(parent, key string) bool {
	switch key {
	case "image":
		return containerEntryPath(parent)
	case k8sdaemon.PolecatImageAnnotation, restartedAtAnnotation:
		return true
	}
	if parent == "" {
		return serverSetRootKeys[key]
	}
	return parent == "metadata" && serverSetMetadataKeys[key]
}

// containerEntryPath reports whether path is the map of one container, the only
// place the compare reads an image key.
func containerEntryPath(path string) bool {
	open := strings.LastIndexByte(path, '[')
	if open < 0 || !strings.HasSuffix(path, "]") {
		return false
	}
	switch lastPathField(path[:open]) {
	case "containers", "initContainers", "ephemeralContainers":
		return true
	}
	return false
}

// driftValuesEqual compares two decoded scalars. A number is equal by value:
// the render's YAML decodes 1 as an int and kubectl's JSON as a float64, and a
// Deployment is not in drift because of that.
func driftValuesEqual(rendered, live any) bool {
	if renderedNumber, ok := driftNumber(rendered); ok {
		liveNumber, ok := driftNumber(live)
		return ok && renderedNumber == liveNumber
	}
	return reflect.DeepEqual(rendered, live)
}

// driftNumber reads a decoded number as a float64.
func driftNumber(value any) (float64, bool) {
	if number, ok := value.(json.Number); ok {
		f, err := number.Float64()
		return f, err == nil
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(reflected.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(reflected.Uint()), true
	case reflect.Float32, reflect.Float64:
		return reflected.Float(), true
	}
	return 0, false
}

// driftFingerprintHexLen is how many hex digits of the hash a drift line carries:
// enough to tell two values apart, and all a reader needs to see that two sides
// differ.
const driftFingerprintHexLen = 12

// driftFingerprint is what a drift line carries for one side of a difference: the
// first driftFingerprintHexLen hex digits of the SHA-256 of the value's canonical
// JSON, never the value itself, since a Deployment's spec can hold a secret
// (gt-wi4885). Canonical is encoding/json's encoding: it sorts a map's keys, so
// the order a decoder read them in does not change the fingerprint, and the
// render's int 2 and the live float64 2 encode alike.
func driftFingerprint(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("the value has no canonical JSON to fingerprint, so it cannot be reported: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])[:driftFingerprintHexLen], nil
}

// driftReadableLimit bounds what a drift line prints of one value: its compact
// JSON, at most this many bytes, ellipsis included.
const driftReadableLimit = 200

// driftReadableEllipsis marks a value cut at driftReadableLimit.
const driftReadableEllipsis = "..."

// driftReadableValue returns value as compact JSON for a drift line: the whole
// JSON when it fits driftReadableLimit, else its leading bytes and an ellipsis.
// The cut backs off to a rune boundary so the text stays valid UTF-8. A value
// with no JSON is refused: an empty string would read as a value that printed.
func driftReadableValue(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("the value has no JSON to print: %w", err)
	}
	if len(encoded) <= driftReadableLimit {
		return string(encoded), nil
	}
	cut := driftReadableLimit - len(driftReadableEllipsis)
	for !utf8.RuneStart(encoded[cut]) {
		cut--
	}
	return string(encoded[:cut]) + driftReadableEllipsis, nil
}

// driftPodSpecPath is where a Deployment's pod spec sits, the root of every
// container path the reviewed safe list names.
const driftPodSpecPath = "spec.template.spec"

// driftPodTemplateMetadataPath is where the pod template's own metadata sits.
const driftPodTemplateMetadataPath = "spec.template.metadata"

// driftValueSafe reports whether a drift line may print the values at path. The
// list is reviewed: each entry is a place where the rendered and the observed
// value both help a reader and printing them is safe (gt-graamb). Every other
// path, such as an env value, a volume, a Secret or a data key, keeps its
// fingerprints, since a spec can hold a secret (gt-wi4885). The object's own
// metadata labels and annotations are not on the list: every kind carries them,
// and the gt-sbx compare reports a Service label that can hold a token
// (gt-fl4i45).
func driftValueSafe(path string) bool {
	if path == "spec.replicas" || path == driftPodSpecPath+".terminationGracePeriodSeconds" {
		return true
	}
	if driftValueAtOrUnder(path, "spec.strategy") ||
		driftValueAtOrUnder(path, driftPodTemplateMetadataPath+".labels") ||
		driftValueAtOrUnder(path, driftPodTemplateMetadataPath+".annotations") {
		return true
	}
	return driftContainerValue(path)
}

// driftValueAtOrUnder reports whether path is field itself or a path beneath it.
// A map's child continues with '.' and a list entry's with '['.
func driftValueAtOrUnder(path, field string) bool {
	return path == field || strings.HasPrefix(path, field+".") || strings.HasPrefix(path, field+"[")
}

// driftContainerField matches what the reviewed safe list names below a
// container entry: the args list and its entries, the resources map, its
// requests and limits maps, and one quantity under them. A quantity's name may
// hold '.', as nvidia.com/gpu does, but never a bracket, which only a list path
// carries.
var driftContainerField = regexp.MustCompile(`^\.(args|args\[[0-9]+\]|resources|resources\.(requests|limits)(\.[^\[\]]+)?)$`)

// driftContainerValue reports whether path names a container's args or
// resources, or what sits under them, in one of the pod spec's container
// lists. It matches the whole path below the container entry rather than a
// prefix of it, because a container name can carry a ']' and make an env path
// read as an args path, which would print a value the list withholds (gt-graamb).
func driftContainerValue(path string) bool {
	for _, list := range []string{"containers", "initContainers", "ephemeralContainers"} {
		prefix := driftPodSpecPath + "." + list + "["
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		closing := strings.IndexByte(path[len(prefix):], ']')
		if closing < 0 {
			return false
		}
		return driftContainerField.MatchString(path[len(prefix)+closing+1:])
	}
	return false
}

// joinPath appends key to path as a JSON object path.
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// lastPathField returns the object key a path ends with. A list's path is its
// parent's, a dot and the key, so its last field is that key.
func lastPathField(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// listKeyField names the field that identifies an entry of the list at path,
// and "" for a list whose entries are positional, where order is part of the
// value as in a container's argument list.
func listKeyField(path string) string {
	switch path {
	case "containers", "initContainers", "env", "volumes":
		return "name"
	case "volumeMounts":
		return "mountPath"
	case "hostAliases":
		return "ip"
	case "ports":
		return "containerPort"
	}
	return ""
}

// entryKeys returns each rendered entry's identity in order, or false when an
// entry carries none.
func entryKeys(entries []any, keyField string) ([]string, bool) {
	keys := make([]string, len(entries))
	for i, entry := range entries {
		key, ok := listEntryKey(entry, keyField)
		if !ok {
			return nil, false
		}
		keys[i] = key
	}
	return keys, true
}

// indexByKey maps each live entry's identity to the entry. A later entry wins,
// since the API keeps the last of two env entries with one name, so the compare
// reads the value the pod runs with.
func indexByKey(entries []any, keyField string) (map[string]any, bool) {
	index := make(map[string]any, len(entries))
	for _, entry := range entries {
		key, ok := listEntryKey(entry, keyField)
		if !ok {
			return nil, false
		}
		index[key] = entry
	}
	return index, true
}

// listEntryKey returns the identity an entry of a keyed list is matched by. A
// number is formatted from its value, since the two decodings disagree on its
// Go type: a container port is an int in the render and a float64 in kubectl's
// JSON.
func listEntryKey(entry any, keyField string) (string, bool) {
	object, ok := entry.(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := object[keyField]
	if !ok {
		return "", false
	}
	if number, ok := driftNumber(value); ok {
		return strconv.FormatFloat(number, 'f', -1, 64), true
	}
	text, ok := value.(string)
	return text, ok
}

// quantityPattern is the Kubernetes quantity spelling the compare reads: a
// decimal number, an optional exponent, and an optional suffix. A value that
// does not match is compared as text.
var quantityPattern = regexp.MustCompile(`^([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))([eE][+-]?[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E)?$`)

// quantityMaxExponent bounds a decimal exponent before big.Rat parses it: the
// API's quantities fit an int64, whose worst case is 10^18, so a larger
// exponent is not a quantity and reads as text instead of building an integer
// too big to compare (gt-jqfk62).
const quantityMaxExponent = 18

// quantitySuffixes gives each suffix its base and exponent.
var quantitySuffixes = map[string]struct {
	base int64
	exp  int64
}{
	"":   {10, 0},
	"n":  {10, -9},
	"u":  {10, -6},
	"m":  {10, -3},
	"k":  {10, 3},
	"M":  {10, 6},
	"G":  {10, 9},
	"T":  {10, 12},
	"P":  {10, 15},
	"E":  {10, 18},
	"Ki": {2, 10},
	"Mi": {2, 20},
	"Gi": {2, 30},
	"Ti": {2, 40},
	"Pi": {2, 50},
	"Ei": {2, 60},
}

// quantityEqual compares two quantity spellings by value, reporting false for
// known when either side is not a quantity. The API stores a quantity in its
// canonical spelling, so the render's 1024Mi comes back as 1Gi and the render's
// 1000m as 1; a text compare would report drift on a cluster that matches the
// render (gt-jqfk62).
func quantityEqual(rendered, live string) (equal, known bool) {
	renderedValue, ok := quantityValue(rendered)
	if !ok {
		return false, false
	}
	liveValue, ok := quantityValue(live)
	if !ok {
		return false, false
	}
	return renderedValue.Cmp(liveValue) == 0, true
}

// quantityValue reads a quantity as an exact rational, so no rounding decides
// whether two spellings are one value.
func quantityValue(text string) (*big.Rat, bool) {
	match := quantityPattern.FindStringSubmatch(text)
	if match == nil {
		return nil, false
	}
	if exponent := match[2]; exponent != "" {
		value, err := strconv.Atoi(exponent[1:])
		if err != nil || value > quantityMaxExponent || value < -quantityMaxExponent {
			return nil, false
		}
	}
	number, ok := new(big.Rat).SetString(match[1] + match[2])
	if !ok {
		return nil, false
	}
	suffix, ok := quantitySuffixes[match[3]]
	if !ok {
		return nil, false
	}
	exp := suffix.exp
	negative := exp < 0
	if negative {
		exp = -exp
	}
	factor := new(big.Int).Exp(big.NewInt(suffix.base), big.NewInt(exp), nil)
	if negative {
		return new(big.Rat).Mul(number, new(big.Rat).SetFrac(big.NewInt(1), factor)), true
	}
	return new(big.Rat).Mul(number, new(big.Rat).SetInt(factor)), true
}
