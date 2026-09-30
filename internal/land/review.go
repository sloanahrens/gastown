package land

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Verdict values om writes.
const (
	VerdictApprove        = "approve"
	VerdictRequestChanges = "request_changes"
)

// Verdict is om's decision on one range.
type Verdict struct {
	Verdict  string    `json:"verdict"`
	Score    float64   `json:"score"`
	Findings []Finding `json:"findings"`
}

// Approved reports whether the verdict lets the work land.
func (v Verdict) Approved() bool { return v.Verdict == VerdictApprove }

// Finding is one om finding, in om's own JSON shape.
type Finding struct {
	ID       string `json:"id,omitempty"`
	Severity string `json:"severity,omitempty"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title,omitempty"`
}

// Reviewer reviews the diff base..head in the tree at dir. An error means no
// verdict exists; it is never read as approval.
type Reviewer interface {
	Review(ctx context.Context, dir, base, head string) (Verdict, error)
}

// OMReviewer runs `om review -C <dir> --base <base> --head <head> --out <file>`.
// om exits 0 for approve, 1 for request_changes and 2 for an execution error.
// The exit code decides; the verdict file must agree with it and supplies the
// score and findings.
type OMReviewer struct {
	// Path is the om binary; "" means "om" on PATH.
	Path string
	// OutDir holds verdict files. It is created 0700; never a fixed /tmp name.
	OutDir string
	// Out, when set, receives om's own output.
	Out io.Writer

	run runFunc // nil means realRun
}

// Review runs om once and returns its verdict.
func (r OMReviewer) Review(ctx context.Context, dir, base, head string) (Verdict, error) {
	path := r.Path
	if path == "" {
		path = "om"
	}
	run := r.run
	if run == nil {
		run = realRun
	}
	if r.OutDir == "" {
		return Verdict{}, fmt.Errorf("om review: no verdict directory configured")
	}
	if err := os.MkdirAll(r.OutDir, 0o700); err != nil {
		return Verdict{}, fmt.Errorf("om review: creating verdict dir: %w", err)
	}
	f, err := os.CreateTemp(r.OutDir, "om-verdict-*.json")
	if err != nil {
		return Verdict{}, fmt.Errorf("om review: creating verdict file: %w", err)
	}
	outPath := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(outPath) }()

	var buf bytes.Buffer
	var w io.Writer = &buf
	if r.Out != nil {
		w = io.MultiWriter(&buf, r.Out)
	}
	argv := []string{path, "review", "-C", dir, "--base", base, "--head", head, "--out", outPath}
	code, runErr := run(ctx, dir, nil, argv, w)
	if runErr != nil {
		return Verdict{}, fmt.Errorf("om review did not run: %w", runErr)
	}
	if code != 0 && code != 1 {
		return Verdict{}, fmt.Errorf("om review exited %d (execution error): %s", code, strings.TrimSpace(lastLines(buf.String(), 10)))
	}
	data, err := os.ReadFile(outPath) //nolint:gosec // G304: path is the temp file created above
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return Verdict{}, fmt.Errorf("om review exited %d but wrote no verdict", code)
	}
	var v Verdict
	if err := json.Unmarshal(data, &v); err != nil {
		return Verdict{}, fmt.Errorf("om review verdict is not JSON: %w", err)
	}
	want := VerdictApprove
	if code == 1 {
		want = VerdictRequestChanges
	}
	if v.Verdict != want {
		return Verdict{}, fmt.Errorf("om review exited %d but its verdict file says %q", code, v.Verdict)
	}
	return v, nil
}
