package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

// The one writer for town config files (gt-y3pgh.1, D5). Every write:
//
//   - holds an exclusive flock on <file>.lock for the whole read-modify-write,
//     so two gt processes never lose each other's update;
//   - re-reads the existing file under the lock and decodes it strictly into
//     the same type, refusing with the *ParseError when it does not parse
//     (G3-03: a file gt could not read is never replaced by what gt built
//     from defaults);
//   - writes a temp file in the same directory, fsyncs it, renames it over
//     the target and fsyncs the directory, so readers see the old file or
//     the new one and never a torn one;
//   - changes only what the caller changed: the difference between the
//     decoded file and the caller's value is applied onto the file's own
//     JSON, so an absent key stays absent (a struct's zero value is not
//     written for it) and an untouched value keeps its exact form;
//   - keeps an existing file's mode; perm applies only to a new file.

// WriteConfigJSON replaces path with v. It refuses when an existing file
// does not decode strictly into T.
func WriteConfigJSON[T any](path string, v *T, perm os.FileMode) error {
	return UpdateConfigJSON(path, perm, func(cur *T, _ bool) error {
		*cur = *v
		return nil
	})
}

// WriteOption adjusts how a config write treats the file it rewrites.
type WriteOption func(*writeOptions)

// writeOptions is what a write's options collect.
type writeOptions struct {
	foldRetiredSeatKeys bool
}

// FoldRetiredPoolSeatKeys is the write option for a change to the pool seat: it
// rewrites the retired polecat_pool spellings in the file, overflow_agent and
// max_overflow, onto the current keys holding the value each folded into
// (types.go retiredPoolSeatKeys). The loader folds a retired spelling into the
// current key on every read, so one left behind keeps supplying the seat after
// the write (gt-aq7zo). A write without the option leaves it as the operator
// wrote it.
func FoldRetiredPoolSeatKeys() WriteOption {
	return func(o *writeOptions) { o.foldRetiredSeatKeys = true }
}

// newWriteOptions collects the options of one write.
func newWriteOptions(opts []WriteOption) writeOptions {
	var o writeOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// UpdateConfigJSON decodes path into a T under the file's lock (a zero T and
// exists=false when the file is absent), calls mutate, and writes the
// result. When mutate returns an error nothing is written. A file that does
// not parse is never written.
func UpdateConfigJSON[T any](path string, perm os.FileMode, mutate func(v *T, exists bool) error, opts ...WriteOption) error {
	options := newWriteOptions(opts)
	// A retired file of the two-file layout is written in its host
	// (layout.go). The check repeats under the file's lock: gt config
	// migrate holds that lock while it moves the file, and a writer that
	// waited on it must follow the file to its new place.
	if host, key, ok, _, err := sectionFor(path); err != nil {
		return err
	} else if ok {
		return updateSection(host, key, mutate, options)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	unlock, err := lockConfigFile(path)
	if err != nil {
		return err
	}
	if host, key, ok, _, err := sectionFor(path); err != nil || ok {
		unlock()
		if err != nil {
			return err
		}
		return updateSection(host, key, mutate, options)
	}
	defer unlock()

	var cur T
	exists := false
	mode := perm
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is an internal config path
	switch {
	case err == nil:
		exists = true
		if err := DecodeJSONFile(path, data, &cur); err != nil {
			return err
		}
		if fi, statErr := os.Stat(path); statErr == nil {
			mode = fi.Mode().Perm()
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return fmt.Errorf("reading %s before writing: %w", path, err)
	}

	base, err := jsonValue(&cur)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	if err := mutate(&cur, exists); err != nil {
		return err
	}
	next, err := jsonValue(&cur)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	result := next
	if exists {
		orig, err := decodeTree(data)
		if err != nil {
			return fmt.Errorf("reading %s before writing: %w", path, err)
		}
		options.foldRetiredKeys(orig, base)
		result = applyChanges(orig, base, next)
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	return replaceFile(path, append(out, '\n'), mode)
}

// foldRetiredKeys puts the value a retired polecat_pool spelling supplied under
// the key it folds into, then drops the spelling, so the changes merge onto a
// file that says what the loader read. base is the file decoded, so a current
// key that disagrees with the file's own tree is one applyDeprecatedKeys filled
// in: the file carried only the retired spelling, or carried a current key the
// fold did not use. Written under the current name, the value outlives the
// write — a change that leaves it alone keeps it, one that moves it writes the
// new one — where left under the retired name it folds back in on the next read
// and reopens a seat the operator closed (gt-aq7zo).
func (o writeOptions) foldRetiredKeys(orig, base any) {
	if !o.foldRetiredSeatKeys {
		return
	}
	for _, r := range retiredPoolSeatKeys {
		file := objectAt(orig, r.parent)
		if _, ok := file[r.old]; !ok {
			continue
		}
		if v, ok := objectAt(base, r.parent)[r.current]; ok && !reflect.DeepEqual(file[r.current], v) {
			file[r.current] = v
		}
		delete(file, r.old)
	}
}

// objectAt follows path through nested JSON objects to the object it names, or
// nil when any step is absent or not an object.
func objectAt(tree any, path []string) map[string]any {
	m, ok := tree.(map[string]any)
	if !ok {
		return nil
	}
	for _, k := range path {
		if m, ok = m[k].(map[string]any); !ok {
			return nil
		}
	}
	return m
}

// jsonValue is v's JSON form as a generic tree.
func jsonValue(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return decodeTree(data)
}

// decodeTree decodes JSON into maps, slices and json.Number, so numbers
// keep their exact text.
func decodeTree(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// applyChanges returns orig with the changes from base to next applied:
// keys whose value did not change keep orig's form (or stay absent), keys
// that changed take next's value (recursing into objects), keys next added
// are added and keys next dropped are removed.
func applyChanges(orig, base, next any) any {
	if reflect.DeepEqual(base, next) {
		return orig
	}
	bm, bok := base.(map[string]any)
	nm, nok := next.(map[string]any)
	if !bok || !nok {
		return next
	}
	om, ook := orig.(map[string]any)
	if !ook {
		om = map[string]any{}
	}
	out := make(map[string]any, len(om))
	for k, v := range om {
		out[k] = v
	}
	for k, nv := range nm {
		bv, inBase := bm[k]
		switch {
		case !inBase:
			out[k] = nv
		case !reflect.DeepEqual(bv, nv):
			out[k] = applyChanges(om[k], bv, nv)
		}
	}
	for k := range bm {
		if _, inNext := nm[k]; !inNext {
			delete(out, k)
		}
	}
	return out
}

// replaceFile writes data to path atomically and durably.
func replaceFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: directory of an internal config path
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
