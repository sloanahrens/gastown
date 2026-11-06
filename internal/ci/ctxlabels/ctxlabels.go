// Package ctxlabels reads the labels the polecat image's build prints
// (gt-gg9hv.62): the printed listing names the green commit, the bd commit and
// the Go floor the image build takes. The workflow text in
// deploy/ci/images.workflow.yml pipes that listing through this package, so
// the read and its refusals are driven by a unit test rather than parsed by a
// shell line.
package ctxlabels

import (
	"fmt"
	"io"
	"strings"
)

// The labels the polecat image's build prints, as internal/polecat/imagectx
// writes them.
const (
	gtCommit = "org.gastown.gt-commit"
	bdCommit = "org.gastown.bd-commit"
	goFloor  = "go-floor"

	labelPrefix = "label: "
)

// Labels is what the image build takes: the bd commit it is built from and the
// Go floor it must accept.
type Labels struct {
	BDCommit string
	GoFloor  string
}

// Parse reads the build's stdout listing and holds it to the one commit: the
// listing names the green commit exactly once and carries a bd commit and a Go
// floor the build can use. Any other listing is refused, since an image built
// from it would be labeled with a commit the job did not pick.
func Parse(listing, sha string) (Labels, error) {
	values := map[string]string{}
	for i, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, labelPrefix) {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, labelPrefix), "=")
		if !ok {
			return Labels{}, fmt.Errorf("ctxlabels: line %d is not a key=value label: %q", i+1, line)
		}
		if _, seen := values[key]; seen {
			return Labels{}, fmt.Errorf("ctxlabels: the listing names %s twice", key)
		}
		values[key] = value
	}
	if got, ok := values[gtCommit]; !ok {
		return Labels{}, fmt.Errorf("ctxlabels: the listing names no %s label", gtCommit)
	} else if got != sha {
		return Labels{}, fmt.Errorf("ctxlabels: the context is at %s, not the green commit %s", got, sha)
	}
	var out Labels
	for _, field := range []struct {
		key     string
		target  *string
		setting string
	}{
		{bdCommit, &out.BDCommit, "bd commit"},
		{goFloor, &out.GoFloor, "Go floor"},
	} {
		value, ok := values[field.key]
		if !ok {
			return Labels{}, fmt.Errorf("ctxlabels: the listing names no %s label, so the image cannot be built with its %s", field.key, field.setting)
		}
		if value == "" || strings.ContainsAny(value, " \t") {
			return Labels{}, fmt.Errorf("ctxlabels: the %s label is %q, which the image build cannot take", field.key, value)
		}
		*field.target = value
	}
	return out, nil
}

// Env appends the two values to the file the later steps read, as the build
// arguments the polecat image step declares. A failed write is returned, so a
// value the step needs cannot be dropped silently.
func (l Labels) Env(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "BD_COMMIT=%s\nGO_FLOOR=%s\n", l.BDCommit, l.GoFloor); err != nil {
		return fmt.Errorf("ctxlabels: writing BD_COMMIT and GO_FLOOR to the environment: %w", err)
	}
	return nil
}
