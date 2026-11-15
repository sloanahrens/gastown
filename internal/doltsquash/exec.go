// Package doltsquash is the home of the rolling Dolt squash designed in
// docs/plans/2026-11-15-dolt-squash-design.md.
//
// Dolt is reached through Exec, one statement per call: Dolt stops a batch at
// its first error, so a batch reports a failure the caller cannot trace to the
// statement that caused it.
package doltsquash

import "context"

// Rows is the result of one statement: Columns names each column, and Data
// holds one entry per record, each with len(Columns) values.
type Rows struct {
	Columns []string
	Data    [][]string
}

// Exec runs one statement against Dolt and returns the rows it produced.
type Exec interface {
	Exec(ctx context.Context, statement string) (Rows, error)
}
