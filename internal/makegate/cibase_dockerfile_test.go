package makegate

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dockerInstruction is one instruction of a Dockerfile: its keyword in upper
// case, its argument text with continuation lines joined, and the line it
// starts on.
type dockerInstruction struct {
	line    int
	keyword string
	args    string
}

// parseDockerfile splits a Dockerfile into its instructions the way the builder
// reads them: comment and blank lines are dropped, even inside a continued
// instruction, and a trailing backslash joins the next line.
func parseDockerfile(text string) []dockerInstruction {
	var out []dockerInstruction
	var joined strings.Builder
	start := 0
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if joined.Len() == 0 {
			start = i + 1
		}
		if rest, ok := strings.CutSuffix(line, "\\"); ok {
			joined.WriteString(strings.TrimSpace(rest) + " ")
			continue
		}
		joined.WriteString(line)
		keyword, args, _ := strings.Cut(strings.TrimSpace(joined.String()), " ")
		out = append(out, dockerInstruction{line: start, keyword: strings.ToUpper(keyword), args: strings.TrimSpace(args)})
		joined.Reset()
	}
	return out
}

// checkCIBaseUsers reports why a ci-base Dockerfile does not default BASE_USER
// to ci, run every install step into /usr/local/bin as root, and end as the
// BASE_USER argument. It returns nil when all three hold.
func checkCIBaseUsers(text string) error {
	ins := parseDockerfile(text)
	if !hasDockerInstruction(ins, "ARG", "BASE_USER=ci") {
		return errors.New("no ARG BASE_USER=ci: the base user must default to ci")
	}
	user := ""
	installs := 0
	for _, in := range ins {
		switch in.keyword {
		case "USER":
			user = in.args
		case "RUN":
			if !strings.Contains(in.args, "/usr/local/bin") {
				continue
			}
			installs++
			if user != "root" {
				return fmt.Errorf("the install step at line %d has USER %q before it, want USER root", in.line, user)
			}
		}
	}
	if installs == 0 {
		return errors.New("no RUN installs into /usr/local/bin, so the root check has no install step to guard")
	}
	final := ""
	for _, in := range ins {
		if in.keyword == "USER" {
			final = in.args
		}
	}
	if final != "${BASE_USER}" {
		return fmt.Errorf("the last USER is %q, want ${BASE_USER} to restore the base user", final)
	}
	return nil
}

func hasDockerInstruction(ins []dockerInstruction, keyword, args string) bool {
	for _, in := range ins {
		if in.keyword == keyword && in.args == args {
			return true
		}
	}
	return false
}

// shippedJQSHA256 is the digest JQ_SHA256 pins in the shipped Dockerfile. The
// mutations below edit it, and the synthetic Dockerfiles reuse it.
const shippedJQSHA256 = "4dd2d8a0661df0b22f1bb9a1f9830f06b6f3b8f7d91211a1ef5d7c4f06a8b4a5"

// dockerArgDefault returns the default of the first ARG name=value instruction,
// or "" when there is none.
func dockerArgDefault(ins []dockerInstruction, name string) string {
	for _, in := range ins {
		if in.keyword != "ARG" {
			continue
		}
		if value, ok := strings.CutPrefix(in.args, name+"="); ok {
			return value
		}
	}
	return ""
}

// The four steps of the jq layer, each one `&&` segment of its RUN as the
// shipped Dockerfile writes it.
const (
	jqDownloadStep = `curl -fsSL -o /usr/local/bin/jq "https://github.com/jqlang/jq/releases/download/jq-${JQ_VERSION}/jq-linux-arm64"`
	jqDigestStep   = `echo "${JQ_SHA256}  /usr/local/bin/jq" | sha256sum -c -`
	jqChmodStep    = `chmod 0755 /usr/local/bin/jq`
	jqVersionStep  = `test "$(jq --version)" = "jq-${JQ_VERSION}"`
)

// checkCIBaseJQ reports why the jq layer of a ci-base Dockerfile does not keep
// its integrity control. The layer is the one RUN that writes
// /usr/local/bin/jq: it downloads jq, runs sha256sum -c against JQ_SHA256,
// chmods the binary 0755 and runs the version test, in that order. The RUN must
// be exactly those four `&&` segments, each the line the Dockerfile ships, so
// an extra segment or a `|| true` fallback anywhere in the layer fails the
// match. It returns nil when all of that holds and JQ_SHA256 is a 64-character
// hex digest.
func checkCIBaseJQ(text string) error {
	ins := parseDockerfile(text)
	digest := dockerArgDefault(ins, "JQ_SHA256")
	if len(digest) != 64 {
		return fmt.Errorf("ARG JQ_SHA256 must be a 64-character sha256, has %d characters", len(digest))
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return errors.New("ARG JQ_SHA256 is not hex")
	}
	var layer string
	writers := 0
	for _, in := range ins {
		if in.keyword == "RUN" && strings.Contains(in.args, "/usr/local/bin/jq") {
			layer = in.args
			writers++
		}
	}
	if writers != 1 {
		return fmt.Errorf("%d RUN steps write /usr/local/bin/jq, want exactly one jq layer", writers)
	}
	steps := strings.Split(layer, "&&")
	want := []struct{ name, line string }{
		{"download", jqDownloadStep},
		{"sha256sum check", jqDigestStep},
		{"chmod", jqChmodStep},
		{"version test", jqVersionStep},
	}
	if len(steps) != len(want) {
		return fmt.Errorf("the jq layer has %d && steps, want the %d it ships: download, sha256sum check, chmod, version test; an extra step or a || fallback is refused", len(steps), len(want))
	}
	for i, w := range want {
		if got := strings.TrimSpace(steps[i]); got != w.line {
			return fmt.Errorf("the jq layer step %d is %q, want the %s step %q; a || fallback or a reordered step is refused", i+1, got, w.name, w.line)
		}
	}
	return nil
}

// golangciLintModule marks the RUN that installs golangci-lint, and goCacheClean
// names the segment of that RUN that empties the Go caches the install wrote.
const (
	golangciLintModule = "cmd/golangci-lint@"
	goCacheClean       = "go clean -cache -modcache"
)

// isGoCacheClean reports whether one && segment of a RUN is a go clean of both
// the build cache and the module cache. The two flags may come in either order
// and with any spacing. Any other word refuses it, so a dry run (-n) or a
// fallback (|| true) is not a clean.
func isGoCacheClean(step string) bool {
	fields := strings.Fields(step)
	if len(fields) < 2 || fields[0] != "go" || fields[1] != "clean" {
		return false
	}
	cache, modcache := false, false
	for _, word := range fields[2:] {
		switch word {
		case "-cache":
			cache = true
		case "-modcache":
			modcache = true
		default:
			return false
		}
	}
	return cache && modcache
}

// checkCIBaseGoCaches reports why the golangci-lint layer of a ci-base
// Dockerfile leaves the Go module and build caches in the image. The layer is
// the one RUN that installs golangci-lint. Its && steps must include a go clean
// of both caches (goCacheClean, in any flag order) after the install step, so
// the caches the install writes under GOPATH and GOCACHE are removed in the
// same layer, before it closes. It returns nil when that holds.
func checkCIBaseGoCaches(text string) error {
	ins := parseDockerfile(text)
	var layer string
	installers := 0
	for _, in := range ins {
		if in.keyword == "RUN" && strings.Contains(in.args, golangciLintModule) {
			layer = in.args
			installers++
		}
	}
	if installers != 1 {
		return fmt.Errorf("%d RUN steps install golangci-lint, want exactly one lint layer", installers)
	}
	install, clean := -1, -1
	for i, step := range strings.Split(layer, "&&") {
		step = strings.TrimSpace(step)
		if strings.Contains(step, golangciLintModule) {
			install = i
		}
		if isGoCacheClean(step) {
			clean = i
		}
	}
	if clean < 0 {
		return fmt.Errorf("the lint layer has no %q step, so the Go module and build caches the install wrote stay in the image", goCacheClean)
	}
	if clean < install {
		return fmt.Errorf("the lint layer runs %q before the install, so the install writes its caches after the clean; the clean must come after it", goCacheClean)
	}
	return nil
}

// readCIBaseDockerfile reads deploy/ci-base/Dockerfile from the checkout. This
// package sits two directories below the repo root.
func readCIBaseDockerfile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "ci-base", "Dockerfile"))
	if err != nil {
		t.Fatalf("read the ci-base Dockerfile: %v", err)
	}
	return string(b)
}

// mutate replaces old with new in text, and fails the test unless old occurs
// exactly once, so a mutation that matches nothing cannot pass for a check.
func mutate(t *testing.T, text, old, new string) string {
	t.Helper()
	if n := strings.Count(text, old); n != 1 {
		t.Fatalf("%q occurs %d times in the shipped Dockerfile, want once", old, n)
	}
	return strings.Replace(text, old, new, 1)
}

// TestCIBaseDockerfileUsers checks the user steps the image build depends on:
// every install step runs as root, and the image ends as BASE_USER, which
// defaults to ci. Each mutation removes or breaks one of them, and the check
// must refuse it. Mutation: drop the USER root line and the installs run as ci,
// the permission-denied failure the image build hit; put a USER back before the
// second install and only that one runs unprivileged; drop the restoring line
// and the image ends as root.
func TestCIBaseDockerfileUsers(t *testing.T) {
	t.Parallel()
	shipped := readCIBaseDockerfile(t)
	tests := []struct {
		name     string
		old, new string // a line of the shipped file to replace; empty keeps the file
		text     string // a whole Dockerfile to check instead
		wantErr  bool
	}{
		{name: "shipped file"},
		{name: "USER root line removed", old: "USER root\n", wantErr: true},
		{name: "second install runs as another user", old: "ARG JQ_VERSION=1.7.1\n", new: "ARG JQ_VERSION=1.7.1\nUSER ${BASE_USER}\n", wantErr: true},
		{name: "second install as another user", text: "FROM base\nARG BASE_USER=ci\nUSER root\nRUN install -o /usr/local/bin/a\nUSER ci\nRUN install -o /usr/local/bin/b\nUSER ${BASE_USER}\n", wantErr: true},
		{name: "restoring USER line removed", old: "USER ${BASE_USER}\n", wantErr: true},
		{name: "restore names ci, not the argument", old: "USER ${BASE_USER}\n", new: "USER ci\n", wantErr: true},
		{name: "BASE_USER argument removed", old: "ARG BASE_USER=ci\n", wantErr: true},
		{name: "no install step", text: "FROM base\nARG BASE_USER=ci\nUSER root\nUSER ${BASE_USER}\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text
			switch {
			case tc.old != "":
				text = mutate(t, shipped, tc.old, tc.new)
			case tc.text == "":
				text = shipped
			}
			err := checkCIBaseUsers(text)
			if tc.wantErr && err == nil {
				t.Fatalf("checkCIBaseUsers accepted the Dockerfile, want a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkCIBaseUsers refused the Dockerfile: %v", err)
			}
		})
	}
}

// TestParseDockerfileJoinsContinuations pins how a continued instruction is
// joined, since the jq layer is matched one `&&` segment at a time on that join.
func TestParseDockerfileJoinsContinuations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, text, wantArgs string
	}{
		{name: "space before the backslash joins to one space", text: "RUN a \\\n  && b\n", wantArgs: "a && b"},
		{name: "comment line inside a continuation is dropped", text: "RUN a \\\n# note\n  && b\n", wantArgs: "a && b"},
		{name: "blank line inside a continuation is dropped", text: "RUN a \\\n\n  && b\n", wantArgs: "a && b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ins := parseDockerfile(tc.text)
			if len(ins) != 1 {
				t.Fatalf("parseDockerfile found %d instructions, want 1", len(ins))
			}
			if ins[0].args != tc.wantArgs {
				t.Fatalf("parseDockerfile args = %q, want %q", ins[0].args, tc.wantArgs)
			}
		})
	}
}

// jqLayerDockerfile wraps one RUN of the jq layer in the ARGs the layer reads,
// so a row can reorder or drop its steps.
func jqLayerDockerfile(run string) string {
	return "FROM base\nARG JQ_VERSION=1.7.1\nARG JQ_SHA256=" + shippedJQSHA256 + "\nRUN " + run + "\n"
}

// TestCIBaseDockerfileJQDigest checks the jq layer, which installs the binary
// every landing gate runs. Its sha256 check is the only integrity control on
// that binary, so each mutation drops or weakens the download, the check, the
// chmod, the version test or the pinned digest, and the check must refuse it.
func TestCIBaseDockerfileJQDigest(t *testing.T) {
	t.Parallel()
	shipped := readCIBaseDockerfile(t)
	tests := []struct {
		name     string
		old, new string // a line of the shipped file to replace; empty keeps the file
		text     string // a whole Dockerfile to check instead
		wantErr  bool
	}{
		{name: "shipped file"},
		{name: "sha256sum line removed", old: " && echo \"${JQ_SHA256}  /usr/local/bin/jq\" | sha256sum -c - \\\n", wantErr: true},
		{name: "sha256sum line ends in || true", old: "sha256sum -c - \\\n", new: "sha256sum -c - || true \\\n", wantErr: true},
		{name: "sha256sum checks JQ_VERSION, not JQ_SHA256", old: "\"${JQ_SHA256}  /usr/local/bin/jq\"", new: "\"${JQ_VERSION}  /usr/local/bin/jq\"", wantErr: true},
		{name: "sha256sum checks a literal digest", old: "\"${JQ_SHA256}  /usr/local/bin/jq\"", new: "\"" + strings.Repeat("0", 64) + "  /usr/local/bin/jq\"", wantErr: true},
		{name: "chmod removed", old: " && chmod 0755 /usr/local/bin/jq \\\n", wantErr: true},
		{name: "chmod leaves jq non-executable", old: "chmod 0755", new: "chmod 0644", wantErr: true},
		{name: "version test removed", old: " \\\n && test \"$(jq --version)\" = \"jq-${JQ_VERSION}\"", wantErr: true},
		{name: "download dropped", text: jqLayerDockerfile(jqDigestStep + " && " + jqChmodStep + " && " + jqVersionStep), wantErr: true},
		{name: "chmod runs before the digest check", text: jqLayerDockerfile(jqDownloadStep + " && " + jqChmodStep + " && " + jqDigestStep + " && " + jqVersionStep), wantErr: true},
		{name: "extra || true segment between sha256sum and chmod", text: jqLayerDockerfile(jqDownloadStep + " && " + jqDigestStep + " && true || true && " + jqChmodStep + " && " + jqVersionStep), wantErr: true},
		{name: "extra segment between download and sha256sum", text: jqLayerDockerfile(jqDownloadStep + " && true && " + jqDigestStep + " && " + jqChmodStep + " && " + jqVersionStep), wantErr: true},
		{name: "extra segment after the version test", text: jqLayerDockerfile(jqDownloadStep + " && " + jqDigestStep + " && " + jqChmodStep + " && " + jqVersionStep + " && true"), wantErr: true},
		{name: "four steps in order, no extra segment", text: jqLayerDockerfile(jqDownloadStep + " && " + jqDigestStep + " && " + jqChmodStep + " && " + jqVersionStep)},
		{name: "no jq layer", text: "FROM base\nARG JQ_SHA256=" + shippedJQSHA256 + "\nRUN true\n", wantErr: true},
		{name: "unchecked RUN writes jq before the layer", old: "ARG JQ_VERSION=1.7.1\n", new: "ARG JQ_VERSION=1.7.1\nRUN curl -fsSL -o /usr/local/bin/jq https://example.com/jq\n", wantErr: true},
		{name: "JQ_SHA256 ARG removed", old: "ARG JQ_SHA256=" + shippedJQSHA256 + "\n", wantErr: true},
		{name: "JQ_SHA256 has 62 hex digits", old: "JQ_SHA256=" + shippedJQSHA256, new: "JQ_SHA256=" + shippedJQSHA256[:62], wantErr: true},
		{name: "JQ_SHA256 has a non-hex character", old: "JQ_SHA256=" + shippedJQSHA256, new: "JQ_SHA256=" + shippedJQSHA256[:63] + "z", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text
			switch {
			case tc.old != "":
				text = mutate(t, shipped, tc.old, tc.new)
			case tc.text == "":
				text = shipped
			}
			err := checkCIBaseJQ(text)
			if tc.wantErr && err == nil {
				t.Fatalf("checkCIBaseJQ accepted the Dockerfile, want a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkCIBaseJQ refused the Dockerfile: %v", err)
			}
		})
	}
}

// lintInstallStep is the install segment of the lint RUN as the shipped
// Dockerfile writes it, so the synthetic rows match the real step.
const lintInstallStep = `GOBIN=/usr/local/bin GOFLAGS= go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"`

// TestCIBaseDockerfileCleansGoCaches checks that the golangci-lint install
// leaves no Go module or build cache in the image. The install writes both under
// GOPATH and GOCACHE, which the base sets to /home/ci, so a cleanup of /root
// removes nothing. The clean must be a step of the same RUN, after the install.
func TestCIBaseDockerfileCleansGoCaches(t *testing.T) {
	t.Parallel()
	shipped := readCIBaseDockerfile(t)
	tests := []struct {
		name     string
		old, new string // a fragment of the shipped file to replace; empty keeps the file
		text     string // a whole Dockerfile to check instead
		wantErr  bool
	}{
		{name: "shipped file"},
		{name: "clean removed from the lint layer", old: " && " + goCacheClean, wantErr: true},
		{name: "build cache only, module cache kept", old: goCacheClean, new: "go clean -cache", wantErr: true},
		{name: "module cache only, build cache kept", old: goCacheClean, new: "go clean -modcache", wantErr: true},
		{name: "not a go clean", old: goCacheClean, new: "rm -rf /home/ci/go -cache -modcache", wantErr: true},
		{name: "clean may fail silently", old: goCacheClean, new: goCacheClean + " || true", wantErr: true},
		{name: "clean with the flags in the other order", old: goCacheClean, new: "go clean -modcache -cache"},
		{name: "clean with extra spacing between words", old: goCacheClean, new: "go  clean\t-cache   -modcache"},
		{name: "clean is a dry run", old: goCacheClean, new: "go clean -n -cache -modcache", wantErr: true},
		{name: "clean in the install layer", text: "FROM base\nRUN " + lintInstallStep + " && " + goCacheClean + "\n"},
		{name: "clean in a separate RUN after the layer closed", text: "FROM base\nRUN " + lintInstallStep + "\nRUN " + goCacheClean + "\n", wantErr: true},
		{name: "clean before the install", text: "FROM base\nRUN " + goCacheClean + " && " + lintInstallStep + "\n", wantErr: true},
		{name: "no lint layer", text: "FROM base\nRUN true\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text
			switch {
			case tc.old != "":
				text = mutate(t, shipped, tc.old, tc.new)
			case tc.text == "":
				text = shipped
			}
			err := checkCIBaseGoCaches(text)
			if tc.wantErr && err == nil {
				t.Fatalf("checkCIBaseGoCaches accepted the Dockerfile, want a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkCIBaseGoCaches refused the Dockerfile: %v", err)
			}
		})
	}
}
