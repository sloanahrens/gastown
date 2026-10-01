package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gtBinaryOnce guards the single build of the gt binary that the CLI-level
// tests in this package share.
//
// Why a sync.Once and not just a cached string (gt-fo3h): linking ./cmd/gt
// pulls in the whole dependency tree, dolt included, and Go does not cache
// the link step — measured 2026-09-19 at ~100-120s per invocation on a loaded
// host, warm build cache and identical inputs included. A bare `var` cache
// let concurrent t.Parallel callers each start their own `go build` against
// the same output path, so the package paid the link more than once; and
// because a failure left the cache empty, every caller after a failed build
// retried it from scratch. One build, one result — success or failure — per
// test process.
var (
	gtBinaryOnce sync.Once
	gtBinaryPath string
	gtBinaryErr  error
	gtBinaryDir  string // this process's private build dir; see removeBuiltGT
)

// removeBuiltGT deletes the directory buildGT linked into. TestMain calls it
// after m.Run, so each test process that exits normally leaves no binary
// behind; one killed first (timeout panic, SIGTERM, SIGKILL) is cleaned up by
// the next process's sweepStaleGTBinaries.
func removeBuiltGT() {
	if gtBinaryDir != "" {
		_ = os.RemoveAll(gtBinaryDir)
	}
}

// gtBinaryDirPrefix names a test process's build directory; the owning pid
// follows it, so a sweep can tell a live owner from a dead one.
const gtBinaryDirPrefix = "gt-test-bin-"

// legacyGTBinaryPrefix is the per-checkout-path binary name buildGTBinary used
// before 539a156. Checkouts of older revisions still create and exec it, so
// only a file untouched for legacyGTBinaryMaxAge (far past any test binary's
// timeout) is taken to be abandoned.
const (
	legacyGTBinaryPrefix = "gt-integration-test-"
	legacyGTBinaryMaxAge = time.Hour
)

// sweepStaleGTBinaries removes gt build directories whose owning test process
// is gone, and abandoned legacy per-path binaries (~180MB each). Both
// TestMains call it on entry. Errors are ignored: this is housekeeping, and a
// directory that cannot be removed now is retried by the next run.
func sweepStaleGTBinaries() {
	tmp := os.TempDir()
	dirs, _ := filepath.Glob(filepath.Join(tmp, gtBinaryDirPrefix+"*"))
	for _, dir := range dirs {
		rest := strings.TrimPrefix(filepath.Base(dir), gtBinaryDirPrefix)
		pidStr, _, ok := strings.Cut(rest, "-")
		pid, err := strconv.Atoi(pidStr)
		if !ok || err != nil || pid <= 0 {
			// Pre-pid name (gt-test-bin-<random>): no owner to check, and no
			// current code creates one, so it is abandoned.
			_ = os.RemoveAll(dir)
			continue
		}
		// processAlive (process_alive_*.go) is the package's own probe; the
		// directories are this user's, so its owner is never another user's.
		if pid != os.Getpid() && !processAlive(pid) {
			_ = os.RemoveAll(dir)
		}
	}
	legacy, _ := filepath.Glob(filepath.Join(tmp, legacyGTBinaryPrefix+"*"))
	for _, path := range legacy {
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) > legacyGTBinaryMaxAge {
			_ = os.Remove(path)
		}
	}
}

// icu4cHeader is the header Dolt's go-icu-regex dependency includes from
// C++; the probe below compiles a translation unit that includes it.
const icu4cHeader = "<unicode/regex.h>"

// shellSplit splits s like cmd/go's shellQuotedSplit: runs of unquoted
// whitespace separate fields; single and double quotes group, with backslash
// escapes inside double quotes.
func shellSplit(s string) []string {
	var fields []string
	var cur strings.Builder
	inField := false
	i := 0
	for i < len(s) {
		switch c := s[i]; c {
		case ' ', '\t':
			if inField {
				fields = append(fields, cur.String())
				cur.Reset()
				inField = false
			}
			i++
		case '"':
			inField = true
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					if n := s[i+1]; n == '"' || n == '\\' {
						cur.WriteByte(n)
						i += 2
						continue
					}
				}
				cur.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++ // closing quote
			}
		case '\'':
			inField = true
			i++
			for i < len(s) && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++ // closing quote
			}
		default:
			inField = true
			if c == '\\' && i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i += 2
				continue
			}
			cur.WriteByte(c)
			i++
		}
	}
	if inField || cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields
}
