package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// driftExceptionDateLayout is how the exceptions file spells an expiry: one day,
// the last the entry holds.
const driftExceptionDateLayout = "2006-01-02"

// driftExceptionObjectParts is the number of "/"-separated fields an object
// holds. A cluster-scoped object leaves the namespace empty rather than dropping
// it, so "ClusterRole//gt-sweep-reader" and "Role/gt/gt-update" both have three.
const driftExceptionObjectParts = 3

// driftException is one live difference the operator has explained: the object
// as "<Kind>/<namespace>/<name>", the path the compare prints, the owner who
// decided the difference stands and why, and the last day the decision holds.
type driftException struct {
	Object  string
	Path    string
	Reason  string
	Owner   string
	Expires time.Time
}

// driftExceptionFile is the file's shape: a pointer to the list, so a file that
// lost its "exceptions:" key is refused rather than read as a list of none.
type driftExceptionFile struct {
	Exceptions *[]driftExceptionEntry `yaml:"exceptions"`
}

// driftExceptionEntry is one entry as the file spells it, before the expiry is
// parsed.
type driftExceptionEntry struct {
	Object  string `yaml:"object"`
	Path    string `yaml:"path"`
	Reason  string `yaml:"reason"`
	Owner   string `yaml:"owner"`
	Expires string `yaml:"expires"`
}

// loadDriftExceptions reads the checked-in exceptions file at now. An entry with
// no object, owner, reason or path, an object that is not
// "<Kind>/<namespace>/<name>", an expiry that is not a date or has passed, a key
// the format does not name, and a second YAML document are refused: an entry the
// compare cannot apply would leave a difference it was meant to explain, and one
// it applies wrongly would hide a difference nobody explained.
func loadDriftExceptions(data []byte, now time.Time) ([]driftException, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var file driftExceptionFile
	err := dec.Decode(&file)
	switch {
	case errors.Is(err, io.EOF):
		return nil, errors.New("the drift exceptions file is empty, so it names no exceptions: key")
	case err != nil:
		return nil, fmt.Errorf("reading the drift exceptions: %w", err)
	}
	if file.Exceptions == nil {
		return nil, errors.New("the drift exceptions file names no exceptions: key")
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, fmt.Errorf("reading the drift exceptions: %w", err)
	default:
		return nil, errors.New("the drift exceptions file holds more than one YAML document")
	}
	exceptions := make([]driftException, 0, len(*file.Exceptions))
	for i, entry := range *file.Exceptions {
		exception, err := entry.driftException(now)
		if err != nil {
			return nil, fmt.Errorf("drift exception %d: %w", i+1, err)
		}
		exceptions = append(exceptions, exception)
	}
	return exceptions, nil
}

// driftException validates one entry and parses its expiry.
func (e driftExceptionEntry) driftException(now time.Time) (driftException, error) {
	switch {
	case e.Object == "":
		return driftException{}, errors.New("names no object")
	case e.Owner == "":
		return driftException{}, fmt.Errorf("%s names no owner", e.Object)
	case e.Reason == "":
		return driftException{}, fmt.Errorf("%s gives no reason", e.Object)
	case e.Path == "":
		return driftException{}, fmt.Errorf("%s has no path, and a whole-object exception would hide a difference nobody explained", e.Object)
	}
	parts := strings.Split(e.Object, "/")
	if len(parts) != driftExceptionObjectParts || parts[0] == "" || parts[2] == "" {
		return driftException{}, fmt.Errorf("object %q is not \"<Kind>/<namespace>/<name>\"", e.Object)
	}
	expires, err := time.Parse(driftExceptionDateLayout, e.Expires)
	if err != nil {
		return driftException{}, fmt.Errorf("%s path %s has expiry %q, which is not a %s date", e.Object, e.Path, e.Expires, driftExceptionDateLayout)
	}
	exception := driftException{Object: e.Object, Path: e.Path, Reason: e.Reason, Owner: e.Owner, Expires: expires}
	if exception.expiredAt(now) {
		return driftException{}, fmt.Errorf("%s path %s expired after %s, so the difference must be re-judged", e.Object, e.Path, expires.Format(driftExceptionDateLayout))
	}
	return exception, nil
}

// expiredAt reports whether the entry's day has passed at now: an entry holds
// through its expiry date and is expired from the next UTC midnight.
func (e driftException) expiredAt(now time.Time) bool {
	return !now.UTC().Before(e.Expires.AddDate(0, 0, 1))
}

// filterExplained splits lines into the ones an exception explains for object and
// the ones none does. A line is explained only by an exception of the same object
// whose path equals the line's, so an exception never hides another object's
// difference or another field of the same one.
func filterExplained(lines []string, object string, exceptions []driftException) (unexplained, explained []string) {
	for _, line := range lines {
		path, ok := driftLinePath(line)
		if ok && exceptionExplains(exceptions, object, path) {
			explained = append(explained, line)
			continue
		}
		unexplained = append(unexplained, line)
	}
	return unexplained, explained
}

// exceptionExplains reports whether an exception of object names path.
func exceptionExplains(exceptions []driftException, object, path string) bool {
	for _, exception := range exceptions {
		if exception.Object == object && exception.Path == path {
			return true
		}
	}
	return false
}

// driftLinePath is the JSON path a compare line names: the text before the
// ": rendered " the line puts between its path and the fingerprints. A line
// without one has no path to match, so it stays unexplained.
func driftLinePath(line string) (string, bool) {
	path, _, ok := strings.Cut(line, ": rendered ")
	if !ok || path == "" {
		return "", false
	}
	return path, true
}
