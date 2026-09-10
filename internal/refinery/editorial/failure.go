// Package editorial implements the om-gate editorial review records: the
// git note that proves a verdict was produced, and the receipt bead used
// for aggregation. See ~/.claude/docs/specs/2026-09-10-om-gate-coverage-design.md.
package editorial

// FailureClass names why gt mq review exited 2 (infra failure, never an
// approval) instead of producing a verdict. Routing on the class, not on
// log text, is what keeps the fail-closed table mechanical.
type FailureClass string

const (
	// BinaryMissing means om is not on PATH or not executable.
	BinaryMissing FailureClass = "binary_missing"
	// VersionMismatch means the om binary or rubric sha does not match the
	// harness manifest, or the installed om version is below min_version.
	VersionMismatch FailureClass = "version_mismatch"
	// ConfigError means om reported a rubric/config problem before review.
	ConfigError FailureClass = "config_error"
	// BackendTimeout means the review backend timed out.
	BackendTimeout FailureClass = "backend_timeout"
	// MalformedVerdict means the verdict file was absent, empty, or failed
	// its schema.
	MalformedVerdict FailureClass = "malformed_verdict"
	// Tooling means a supporting command (mktemp, jq, git) failed before a
	// verdict was obtained.
	Tooling FailureClass = "tooling"
	// RecordFailed means a verdict was obtained but writing the note or
	// receipt failed afterward.
	RecordFailed FailureClass = "record_failed"
	// Precondition means the push precondition refused to land an MR:
	// note missing, patch-id mismatch, or version below minimum.
	Precondition FailureClass = "precondition"
)

// Retryable reports whether gt mq review should retry the review once
// before giving up. Only transient backend conditions qualify — every
// other class is deterministic and a retry would just waste the attempt.
func (f FailureClass) Retryable() bool {
	return f == BackendTimeout || f == MalformedVerdict
}
