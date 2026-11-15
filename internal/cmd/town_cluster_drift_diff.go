package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
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

// deploymentDrift lists every rendered Deployment field the live one lacks or
// carries differently, one sorted line per field naming its JSON path and both
// values, and refuses either side that decoded to nothing (gt-jqfk62).
func deploymentDrift(rendered, live map[string]any) ([]string, error) {
	if len(rendered) == 0 {
		return nil, errors.New("deployment drift: the rendered Deployment has no fields, so it was not read; an unreadable render is not a cluster in step")
	}
	if len(live) == 0 {
		return nil, errors.New("deployment drift: the live Deployment has no fields, so it was not read; an unreadable object is not a cluster in step")
	}
	w := &driftWalk{}
	w.object("", rendered, live)
	slices.Sort(w.drift)
	return w.drift, nil
}

// driftWalk collects the differences a nested comparison finds.
type driftWalk struct {
	drift []string
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
			w.missingField(childPath, rendered[key])
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
			w.missingField(childPath, rendered[key])
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
			w.missingEntry(entryPath, entry)
			continue
		}
		w.value(entryPath, entry, liveEntry)
	}
	return true
}

// difference records a field the two decodings carry differently.
func (w *driftWalk) difference(path string, rendered, live any) {
	w.drift = append(w.drift, path+": rendered "+driftText(rendered)+", live "+driftText(live))
}

// missingField records a field the render names and the live object lacks.
func (w *driftWalk) missingField(path string, rendered any) {
	w.drift = append(w.drift, path+": rendered "+driftText(rendered)+", live has no such field")
}

// missingEntry records a list entry the render names and the live list lacks.
func (w *driftWalk) missingEntry(path string, rendered any) {
	w.drift = append(w.drift, path+": rendered "+driftText(rendered)+", live has no such entry")
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

// driftText renders a decoded value for a drift line: JSON quotes a string and
// keeps a list or object on one line.
func driftText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
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
