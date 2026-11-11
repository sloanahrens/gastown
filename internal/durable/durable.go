// Package durable holds the fsync calls that make an atomic file write survive
// a crash: File flushes a file's bytes, Dir flushes a directory entry a rename
// added. It is a leaf package, so a low-level package can use it without an
// import cycle.
//
// The unit-test tier turns the flushes into no-ops (SkipForUnitTier). A test of
// a write's content, mode or atomicity cannot observe an fsync, and on a busy
// disk each one costs milliseconds, which across thousands of cases was most of a
// package's wall time. The policy is process-wide on purpose: a test binary is one
// process, and "the unit tier" is a property of the binary, not of one test, so a
// per-call switch would have to be threaded through every writer. What keeps it
// out of shipped code is SkipForUnitTier's guard, which panics in any binary that
// is not a test binary.
//
// The real fsync is therefore not run by the gate's unit tier, by design. What
// remains proven: the policy both ways (this package's tests), and, for the one
// writer that takes its sync functions as parameters (atomicfile.writeFile), the
// order they run in (atomicfile TestWriteFileSyncOrder). The other writers (the
// config writer and backup, the land landings and backoff files, pool_migrate)
// call File and Dir directly and have no such seam.
package durable

import (
	"os"
	"sync/atomic"
	"testing" // for testing.Testing, the guard that keeps SkipForUnitTier out of shipped binaries
)

// policy says whether the flushes run. The zero value flushes.
type policy struct{ skip atomic.Bool }

// process is the policy of this process.
var process policy

func (p *policy) file(f *os.File) error {
	if p.skip.Load() {
		return nil
	}
	return f.Sync()
}

func (p *policy) dir(dir string) error {
	if p.skip.Load() {
		return nil
	}
	d, err := os.Open(dir) //nolint:gosec // G304: the caller's own directory
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// File flushes f's contents and metadata to the disk.
func File(f *os.File) error { return process.file(f) }

// Dir flushes the directory entry a rename added. A filesystem that declines to
// sync a directory returns its error; the caller decides whether that matters.
func Dir(dir string) error { return process.dir(dir) }

// SkipForUnitTier makes File and Dir no-ops for the rest of the process. It is for
// a unit-tier TestMain and panics in any binary that is not a test binary, so a
// shipped gt can never lose a flush to it.
func SkipForUnitTier() { skipForUnitTier(&process, testing.Testing) }

// skipForUnitTier is SkipForUnitTier over a policy and a way to ask whether this
// is a test binary, so a test can reach both the skip and the guard.
func skipForUnitTier(p *policy, isTestBinary func() bool) {
	if !isTestBinary() {
		panic("durable.SkipForUnitTier called outside a test binary")
	}
	p.skip.Store(true)
}
