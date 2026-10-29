// Package diskmaint prunes what fills a town's disks behind one three-stage
// contract — Measure, Plan, Apply — and records every run in a JSON-lines
// ledger. Dry-run is the default and an input that cannot be read refuses the
// plan rather than producing a guess.
package diskmaint

import (
	"context"
	"time"
)

// Runner runs one program and returns its stdout; env is appended to the
// process environment, and a non-zero exit is an error that carries stderr.
type Runner func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

// Measurement reports the bytes a target holds and the capacity it shares.
type Measurement struct {
	Used, Capacity int64  // bytes; Capacity 0 means unknown
	Detail         string // one human line
}

// Action is one change a plan would make.
type Action struct {
	Kind  string // "delete-manifest"
	Ref   string // "repo@sha256:..."
	Bytes int64  // best-effort size, 0 if unknown
	Why   string
}

// Kept is a ref left alone, with the rule that kept it, so a dry run explains
// itself. Ref is "repo:tag" or "repo@digest".
type Kept struct{ Ref, Rule string }

// Plan is a target's decisions for one run; it is empty when Refused is set.
type Plan struct {
	Target  string
	Actions []Action
	Kept    []Kept
	Refused string // non-empty: the plan is empty on purpose, and why
	// Policy is the policy Plan was called with. A target that gates Apply on
	// the dry-run flag reads it here, so the gate follows the plan it is handed
	// rather than a setting the target keeps between runs.
	Policy Policy
}

// Failure is an action that did not complete.
type Failure struct {
	Action Action
	Err    string
}

// Result is what an apply did.
type Result struct {
	Target     string
	Done       []Action
	Failed     []Failure
	BytesFreed int64
	Collected  bool   // registry garbage collect ran
	Deferred   string // non-empty: nothing was applied, and why (lock held, push in flight)
}

// Policy is the caller's limits for one run.
type Policy struct {
	Keep  int  // newest per-commit tags to keep
	Apply bool // false is a dry run
	Now   time.Time
}

// Target is one thing that fills a disk and can be pruned.
type Target interface {
	Name() string
	Measure(ctx context.Context) (Measurement, error)
	Plan(ctx context.Context, p Policy) (Plan, error)
	Apply(ctx context.Context, plan Plan) (Result, error)
}
