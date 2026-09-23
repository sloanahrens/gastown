package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
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
)

// buildGT returns the path to a gt binary built from this checkout, with
// BuiltProperly=1 set (without it the binary refuses to run).
func buildGT(t *testing.T) string {
	t.Helper()

	gtBinaryOnce.Do(func() {
		gtBinaryPath, gtBinaryErr = buildGTBinary()
	})
	if gtBinaryErr != nil {
		t.Fatalf("failed to build gt: %v", gtBinaryErr)
	}
	return gtBinaryPath
}

func buildGTBinary() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting working directory: %w", err)
	}

	projectRoot := wd
	for {
		if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(projectRoot)
		if parent == projectRoot {
			return "", fmt.Errorf("could not find project root (go.mod) above %s", wd)
		}
		projectRoot = parent
	}

	// The output path is shared across every process running as this user
	// (os.TempDir() is per-user, not per-worktree), so two concurrent test
	// runs — a polecat's and the refinery's, say — would otherwise link the
	// binary to the same file and race on it. Key the name on the project
	// root: one worktree, one binary.
	sum := sha256.Sum256([]byte(projectRoot))
	binaryName := "gt-integration-test-" + hex.EncodeToString(sum[:4])
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	tmpBinary := filepath.Join(os.TempDir(), binaryName)

	// Dolt's go-icu-regex cgo build only reaches this point, so only this
	// helper needs the include-path diagnosis (gt-mjll).
	if err := verifyCgoIncludePath(); err != nil {
		return "", err
	}

	// Must set BuiltProperly=1 via ldflags, otherwise binary refuses to run
	ldflags := "-X github.com/steveyegge/gastown/internal/cmd.BuiltProperly=1"
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", tmpBinary, "./cmd/gt")
	cmd.Dir = projectRoot
	cmd.Env = append(os.Environ(), icuCgoEnv()...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w\nOutput: %s", err, output)
	}
	return tmpBinary, nil
}

// icuCgoEnv returns CGO_CPPFLAGS/CGO_LDFLAGS additions pointing at a keg-only
// Homebrew icu4c, which go-icu-regex (a dolt dependency) needs on macOS.
//
// The Makefile detects the prefix and exports these for `make build` and
// `make test`, so a build under make works by inheritance; a bare `go test`
// gets no such help and dies with "unicode/regex.h file not found", failing
// every buildGT caller. Returns nil when the flags are already set, when not
// on darwin, or when brew cannot find icu4c.
func icuCgoEnv() []string {
	if runtime.GOOS != "darwin" || os.Getenv("CGO_CPPFLAGS") != "" || os.Getenv("CGO_LDFLAGS") != "" {
		return nil
	}
	out, err := exec.Command("brew", "--prefix", "icu4c").Output()
	if err != nil {
		return nil
	}
	prefix := strings.TrimSpace(string(out))
	if prefix == "" {
		return nil
	}
	return []string{
		"CGO_CPPFLAGS=-I" + filepath.Join(prefix, "include"),
		"CGO_LDFLAGS=-L" + filepath.Join(prefix, "lib"),
	}
}

// icu4cHeader is the header Dolt's go-icu-regex dependency includes from
// C++; the probe below compiles a translation unit that includes it.
const icu4cHeader = "<unicode/regex.h>"

// verifyCgoIncludePath fails loud when the cgo include flags that would
// reach the `go build` above cannot resolve icu4c's unicode/regex.h. It asks
// the go tool (not the process environment directly) for the effective
// CGO_CPPFLAGS/CGO_CXXFLAGS, so a value carried only through preserveGoEnv's
// pinned GOENV file still counts, then asks the C compiler itself whether
// the header resolves, reporting the flags, the compiler's line, and a
// repair when it does not (gt-mjll).
//
// Skipped with a stderr notice when no include directory is configured or
// no compiler is reachable: CGO_CFLAGS defaults to "-O2 -g" on every host,
// so gating on any cgo flag would fire everywhere. A compiler failure that
// is not a missing-header diagnosis still fails loud — it is not evidence
// the header resolves, and the `go build` a moment later would hit the
// identical failure with a worse message.
func verifyCgoIncludePath() error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil
	}
	out, err := exec.Command(goBin, "env", "-json", "CGO_CPPFLAGS", "CGO_CXXFLAGS").Output()
	if err != nil {
		return fmt.Errorf("asking the go tool for the effective cgo flags: %w", err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(out, &values); err != nil {
		return fmt.Errorf("parsing `go env -json CGO_CPPFLAGS CGO_CXXFLAGS`: %w", err)
	}
	cgoFlags := splitCgoFlags(values["CGO_CPPFLAGS"] + " " + values["CGO_CXXFLAGS"])
	if !hasIncludeDir(cgoFlags) {
		return nil
	}
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "cc"
	}
	// Split CC the way cmd/go does (quote-aware): "ccache clang" and
	// "/path/to/cc -g" are valid CC values that LookPath on the whole string
	// would read as one missing binary.
	ccParts := shellSplit(cc)
	if len(ccParts) == 0 {
		fmt.Fprintf(os.Stderr, "buildGT: CC=%s does not name a compiler; skipping the icu4c include-path probe\n", cc)
		return nil
	}
	if _, err := exec.LookPath(ccParts[0]); err != nil {
		fmt.Fprintf(os.Stderr, "buildGT: no C compiler at %q; skipping the icu4c include-path probe\n", ccParts[0])
		return nil
	}
	args := append(append([]string{}, cgoFlags...), "-fsyntax-only", "-x", "c", "-")
	probe := exec.Command(ccParts[0], args...)
	probe.Stdin = strings.NewReader("#include " + icu4cHeader + "\n")
	probeOut, err := probe.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := string(probeOut)
	if strings.Contains(msg, "file not found") || strings.Contains(msg, "No such file or directory") {
		return fmt.Errorf(`cgo include path cannot resolve icu4c's %s, so Dolt's go-icu-regex dependency will not compile:
  CGO_CPPFLAGS=%s
  CGO_CXXFLAGS=%s
  %s: %s
icu4c installs keg-only or into a prefix the compiler does not search, and
upgrading or reinstalling moves the prefix without updating $GOENV. Repair
it, then re-run — or prefer the rig's own gate, which derives the flags
itself:
  go env -w CGO_CPPFLAGS="-I<icu4c-prefix>/include" CGO_LDFLAGS="-L<icu4c-prefix>/lib"`,
			icu4cHeader, values["CGO_CPPFLAGS"], values["CGO_CXXFLAGS"],
			strings.Join(ccParts, " "), firstLine(probeOut))
	}
	return fmt.Errorf("buildGT: %s failed the icu4c include-path probe with an error it does not recognize, so the cgo flags cannot be verified:\n%s",
		strings.Join(ccParts, " "), strings.TrimSpace(msg))
}

// splitCgoFlags splits cgo flag variables on the same quoting rules the go
// tool applies, so a quoted path with spaces stays one word.
func splitCgoFlags(s string) []string {
	return shellSplit(s)
}

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

// hasIncludeDir reports whether cgo flags name at least one include
// directory, in either the joined (-I/path) or separated (-I /path)
// spelling. Both match on the prefix, so the bare forms need no special case.
func hasIncludeDir(cgoFlags []string) bool {
	for _, f := range cgoFlags {
		if strings.HasPrefix(f, "-I") || strings.HasPrefix(f, "-isystem") {
			return true
		}
	}
	return false
}

// firstLine trims compiler output to its first non-empty line, so a probe
// failure reads as one actionable sentence instead of a clang banner.
func firstLine(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "(no output)"
}
