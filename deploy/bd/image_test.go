package bd

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	dockerfileName = "Dockerfile.bd"

	// composeFile is the compose unit the gateway runs under, from this
	// package's directory: the file whose --verbs value names where the image
	// must carry its policy file.
	composeFile = "../data/docker-compose.yml"
)

// numericNonRootUser is a USER instruction naming a uid, or uid:gid, above zero.
var numericNonRootUser = regexp.MustCompile(`^USER ([1-9][0-9]*)(:[0-9]+)?$`)

// instructionLines returns the instruction lines in a Dockerfile's content,
// trimmed, with comments and blank lines dropped.
// Each line is in canonical form; see canonicalInstruction.
func instructionLines(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, canonicalInstruction(line))
	}
	return out
}

// readDockerfile returns the Dockerfile's raw content, comments included.
func readDockerfile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(dockerfileName)
	if err != nil {
		t.Fatalf("reading %s: %v", dockerfileName, err)
	}
	return string(raw)
}

// readInstructions returns the Dockerfile's instruction lines.
func readInstructions(t *testing.T) []string {
	return instructionLines(readDockerfile(t))
}

// shimInstructions returns the instruction lines that name the bdshim binary.
// The tracker program is deferred: the image runs the real bd as bd, and a
// bdshim copied or invoked here would put the shim back on PATH.
func shimInstructions(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, "bdshim") {
			out = append(out, l)
		}
	}
	return out
}

// composeVerbsPath returns the --verbs value the compose unit starts the
// gateway with: the path inside the image its policy file must reach.
func composeVerbsPath(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	var unit struct {
		Services struct {
			BD struct {
				Command []string `yaml:"command"`
			} `yaml:"bd"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &unit); err != nil {
		t.Fatalf("%s is not valid YAML: %v", composeFile, err)
	}
	cmd := unit.Services.BD.Command
	for i := 0; i+1 < len(cmd); i++ {
		if cmd[i] == "--verbs" {
			return cmd[i+1]
		}
	}
	t.Fatalf("%s: the bd service's command has no --verbs value", composeFile)
	return ""
}

func TestDockerfileBdShape(t *testing.T) {
	t.Parallel()
	lines := readInstructions(t)
	for _, tc := range []struct{ name, want string }{
		{"the base is the daemon image's ubuntu base", "FROM ubuntu:24.04"},
		{"the real bd is copied from the build context", "COPY --chmod=0755 bd.real /usr/local/bin/bd.real"},
		{"the real bd is bd on PATH", "RUN ln -sf /usr/local/bin/bd.real /usr/local/bin/bd"},
		{"the gateway is copied in", "COPY --chmod=0755 bdgw /usr/local/bin/bdgw"},
		{"the policy directory is traversable by the non-root user", "RUN mkdir -p /etc/bdgw && chmod 0755 /etc/bdgw"},
		{"the policy file is copied where the compose unit reads it", "COPY --chmod=0444 verbs.json " + composeVerbsPath(t)},
		{"the workspace parent is owned by the gateway user", "RUN mkdir -p /var/lib/bdgw && chown 65532:65532 /var/lib/bdgw"},
		{"the workspace root keeps its /ws name", "RUN ln -s /var/lib/bdgw/ws /ws"},
		{"the user is the gateway uid", "USER 65532:65532"},
		{"tini runs the gateway as the entrypoint", `ENTRYPOINT ["tini", "--", "/usr/local/bin/bdgw", "serve"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !contains(lines, tc.want) {
				t.Errorf("%s has no line %q", dockerfileName, tc.want)
			}
		})
	}
	t.Run("the policy directory is made before the policy is copied", func(t *testing.T) {
		t.Parallel()
		// A COPY that creates /etc/bdgw gives it the copied file's mode, 0444,
		// and the non-root user cannot traverse it.
		dirAt := indexOf(lines, "RUN mkdir -p /etc/bdgw && chmod 0755 /etc/bdgw")
		copyAt := indexOf(lines, "COPY --chmod=0444 verbs.json "+composeVerbsPath(t))
		if dirAt < 0 || copyAt < 0 {
			t.Fatalf("%s has no policy directory line (%d) or policy COPY line (%d), want both", dockerfileName, dirAt, copyAt)
		}
		if dirAt > copyAt {
			t.Errorf("%s copies the policy before it makes /etc/bdgw, want the directory made first", dockerfileName)
		}
	})
	t.Run("no instruction names the shim", func(t *testing.T) {
		t.Parallel()
		if got := shimInstructions(lines); len(got) != 0 {
			t.Errorf("%s has lines naming bdshim (%q), want the real bd on PATH", dockerfileName, got)
		}
	})
	t.Run("one stage", func(t *testing.T) {
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
	t.Run("nothing names the beads base image or its pin", func(t *testing.T) {
		t.Parallel()
		// The image is built from ubuntu:24.04 and the build context, so no
		// comment or instruction may name the beads repo's base or its pin file
		// as something this build reads.
		raw := readDockerfile(t)
		for _, ref := range []string{"beads-bd", "base-pin"} {
			if strings.Contains(raw, ref) {
				t.Errorf("%s mentions %q, want the image built from ubuntu:24.04 with the build context", dockerfileName, ref)
			}
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

// TestShimInstructions: a Dockerfile that copies or invokes bdshim has those
// lines reported, so a shim returning to the image is caught, and one that
// links the real bd as bd has none.
func TestShimInstructions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, dockerfile string
		want             []string
	}{
		{
			name:       "a shim copy",
			dockerfile: "FROM base\nCOPY --chmod=0755 bdshim /usr/local/bin/bd\n",
			want:       []string{"COPY --chmod=0755 bdshim /usr/local/bin/bd"},
		},
		{
			name:       "a shim invocation",
			dockerfile: "FROM base\nRUN /usr/local/bin/bdshim --version\n",
			want:       []string{"RUN /usr/local/bin/bdshim --version"},
		},
		{
			name:       "the real bd linked as bd",
			dockerfile: "FROM base\nRUN ln -sf /usr/local/bin/bd.real /usr/local/bin/bd\n",
		},
		{
			name:       "a comment naming the shim",
			dockerfile: "# the bdshim is deferred\nFROM base\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := shimInstructions(instructionLines(tc.dockerfile))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("shimInstructions(%q) = %q, want %q", tc.dockerfile, got, tc.want)
			}
		})
	}
}

// contains reports whether want is one of lines.
func contains(lines []string, want string) bool {
	return indexOf(lines, want) >= 0
}

// indexOf returns the position of want in lines, or -1 when lines has no such
// instruction. want is canonicalized here; lines come from readInstructions,
// already canonical.
func indexOf(lines []string, want string) int {
	want = canonicalInstruction(want)
	for i, l := range lines {
		if l == want {
			return i
		}
	}
	return -1
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
		"from   ubuntu:24.04",
		`entrypoint ["tini",   "--", "/usr/local/bin/bdgw", "serve"]`,
	} {
		if !contains(lines, want) {
			t.Errorf("contains(%q) = false, want true for the same instruction in another spelling", want)
		}
	}
}
