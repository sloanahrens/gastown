package bd

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	dockerfileName = "Dockerfile.bd"
	basePinName    = "base-pin"
)

// basePinShape is a base reference pinned by digest: a registry host with an
// optional port, one or more path segments, and a sha256 digest of 64 lowercase
// hex characters. A colon may only introduce the port, so a tag is refused.
var basePinShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(:[0-9]+)?(/[a-z0-9][a-z0-9._-]*)+@sha256:[0-9a-f]{64}$`)

// numericNonRootUser is a USER instruction naming a uid, or uid:gid, above zero.
var numericNonRootUser = regexp.MustCompile(`^USER ([1-9][0-9]*)(:[0-9]+)?$`)

// readInstructions returns the Dockerfile's instruction lines, trimmed, with
// comments and blank lines dropped.
// Each line is in canonical form; see canonicalInstruction.
func readInstructions(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(dockerfileName)
	if err != nil {
		t.Fatalf("reading %s: %v", dockerfileName, err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = canonicalInstruction(line)
		out = append(out, line)
	}
	return out
}

// readBasePin returns the base reference in base-pin, trimmed.
func readBasePin(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(basePinName)
	if err != nil {
		t.Fatalf("reading %s: %v", basePinName, err)
	}
	return strings.TrimSpace(string(raw))
}

func TestDockerfileBdShape(t *testing.T) {
	t.Parallel()
	lines := readInstructions(t)
	pin := readBasePin(t)
	for _, tc := range []struct{ name, want string }{
		{"base is the pin by digest", "FROM " + pin},
		{"the real bd is checked in place", "RUN test -x /usr/local/bin/bd.real"},
		{"the shim is bd", "COPY --chmod=0755 bdshim /usr/local/bin/bd"},
		{"the gateway is copied in", "COPY --chmod=0755 bdgw /usr/local/bin/bdgw"},
		{"the gateway is the entrypoint", `ENTRYPOINT ["/usr/local/bin/bdgw", "serve"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !contains(lines, tc.want) {
				t.Errorf("%s has no line %q", dockerfileName, tc.want)
			}
		})
	}
	t.Run("one base, by digest", func(t *testing.T) {
		t.Parallel()
		var froms []string
		for _, l := range lines {
			if strings.HasPrefix(l, "FROM ") {
				froms = append(froms, l)
			}
		}
		if len(froms) != 1 {
			t.Errorf("%s has %d FROM lines, want exactly one: %q", dockerfileName, len(froms), froms)
		}
	})
	t.Run("the user is a non-root uid", func(t *testing.T) {
		t.Parallel()
		var users []string
		for _, l := range lines {
			if strings.HasPrefix(l, "USER ") {
				users = append(users, l)
			}
		}
		if len(users) != 1 || !numericNonRootUser.MatchString(users[0]) {
			t.Errorf("%s USER lines = %q, want one numeric uid above zero", dockerfileName, users)
		}
	})
}

// TestBasePinIsDigest: base-pin holds one reference pinned by digest, and a
// reference with a tag, or no digest at all, is refused.
func TestBasePinIsDigest(t *testing.T) {
	t.Parallel()
	pin := readBasePin(t)
	if !basePinShape.MatchString(pin) {
		t.Fatalf("%s holds %q, want a reference pinned by sha256 digest with no tag", basePinName, pin)
	}
	for _, tc := range []struct{ name, ref string }{
		{"a latest tag", "forgejo:3000/bot-registry/beads-bd:latest"},
		{"a commit tag", "forgejo:3000/bot-registry/beads-bd:b7e5e9c"},
		{"a tag and a digest", "forgejo:3000/bot-registry/beads-bd:latest@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		{"a short digest", "forgejo:3000/bot-registry/beads-bd@sha256:abc"},
		{"an uppercase digest", "forgejo:3000/bot-registry/beads-bd@sha256:ABCDEF0000000000000000000000000000000000000000000000000000000000"},
		{"no digest", "forgejo:3000/bot-registry/beads-bd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if basePinShape.MatchString(tc.ref) {
				t.Errorf("reference %q passed the digest check, want it refused", tc.ref)
			}
		})
	}
}

// contains reports whether want is one of lines.
func contains(lines []string, want string) bool {
	// want is canonicalized here; lines come from readInstructions, already canonical.
	want = canonicalInstruction(want)
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// canonicalInstruction returns an instruction line in one spelling: the keyword
// upper-cased, runs of whitespace collapsed to one space, and a JSON exec form
// re-encoded with no spaces. Two spellings of one instruction then compare equal.
func canonicalInstruction(line string) string {
	kw, rest := line, ""
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		kw, rest = line[:i], strings.TrimSpace(line[i:])
	}
	var args []string
	if json.Unmarshal([]byte(rest), &args) == nil {
		if enc, err := json.Marshal(args); err == nil {
			rest = string(enc)
		}
	} else {
		rest = strings.Join(strings.Fields(rest), " ")
	}
	if rest == "" {
		return strings.ToUpper(kw)
	}
	return strings.ToUpper(kw) + " " + rest
}

// TestCanonicalInstruction: one instruction, however it is spaced or cased, has
// one canonical form.
func TestCanonicalInstruction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, line, want string }{
		{"keyword case", "from forgejo:3000/x@sha256:0", "FROM forgejo:3000/x@sha256:0"},
		{"run of spaces", "COPY   --chmod=0755  bdgw   /usr/local/bin/bdgw", "COPY --chmod=0755 bdgw /usr/local/bin/bdgw"},
		{"tab separator", "USER\t65532:65532", "USER 65532:65532"},
		{"exec form spacing", `ENTRYPOINT ["/usr/local/bin/bdgw", "serve"]`, `ENTRYPOINT ["/usr/local/bin/bdgw","serve"]`},
		{"bare keyword", "run", "RUN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := canonicalInstruction(tc.line); got != tc.want {
				t.Errorf("canonicalInstruction(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// TestContainsIgnoresSpelling: a wanted instruction written with other spacing
// or keyword case is found in the Dockerfile.
func TestContainsIgnoresSpelling(t *testing.T) {
	t.Parallel()
	lines := readInstructions(t)
	for _, want := range []string{
		"from   " + readBasePin(t),
		`entrypoint ["/usr/local/bin/bdgw",   "serve"]`,
	} {
		if !contains(lines, want) {
			t.Errorf("contains(%q) = false, want true for the same instruction in another spelling", want)
		}
	}
}
