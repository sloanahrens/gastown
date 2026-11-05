package buildstamp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// goProducer is a Go file that records a commit stamp or a build id from git.
// exempt names the functions that keep git's own abbreviation, with the reason:
// they stamp a build id that has to match another checkout's, not gt's stamp.
type goProducer struct {
	path   string // repo-relative
	exempt map[string]string
}

var goProducers = []goProducer{
	{path: "internal/daemonimage/build.go", exempt: map[string]string{
		"bdBuildID": "bd's build id, which must equal the id the host's bd reports (gt-glsx8); not gt's stamp",
	}},
	{path: "internal/deploysource/check.go"},
	{path: "internal/polecat/bdbuild/bdbuild.go", exempt: map[string]string{
		"buildID": "bd's build id, which must equal the id the host's bd reports (gt-glsx8); not gt's stamp",
	}},
}

const makefilePath = "Makefile" // repo-relative

var (
	makefileStampLenLine = regexp.MustCompile(`(?m)^STAMP_LEN[ \t]*:=[ \t]*([0-9]+)[ \t]*$`)
	goFuncName           = regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z_]\w*)`)
)

// makefileAbbreviates reports a Makefile line that asks git for an abbreviated
// commit id. Git's --short is refused with or without a length: --short=N is a
// minimum that grows when the prefix is ambiguous, so it is not the rule either.
// A comment is not code.
func makefileAbbreviates(line string) bool {
	return !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "rev-parse") && strings.Contains(line, "--short")
}

// makefileLine is one logical Makefile line. A line ending in a backslash
// continues on the next, and the logical line reports the number it starts on.
type makefileLine struct {
	no   int
	text string
}

// makefileLines joins the backslash-continued lines of mk, so a git request
// split across recipe lines is scanned as one line.
func makefileLines(mk string) []makefileLine {
	var lines []makefileLine
	var text strings.Builder
	first := 0
	for i, raw := range strings.Split(mk, "\n") {
		if text.Len() == 0 {
			first = i + 1
		}
		if cont, ok := strings.CutSuffix(raw, `\`); ok {
			text.WriteString(cont + " ")
			continue
		}
		text.WriteString(raw)
		lines = append(lines, makefileLine{no: first, text: text.String()})
		text.Reset()
	}
	if text.Len() > 0 {
		lines = append(lines, makefileLine{no: first, text: text.String()})
	}
	return lines
}

// makefileStampVars are the Makefile variables that record gt's commit stamp.
// HEAD_STAMP cuts the full sha to STAMP_LEN characters; the others take their
// value from it, so there is one length in the Makefile.
var makefileStampVars = []string{"HEAD_STAMP", "COMMIT", "POLE_TAG"}

// makefileProblems returns each way the Makefile breaks the stamp rule: a
// STAMP_LEN that is not Len, a stamp variable that does not take the stamp from
// HEAD_STAMP, or a logical line that asks git for an abbreviation.
func makefileProblems(mk string) []string {
	var problems []string
	if err := makefileStampLenErr(mk); err != nil {
		problems = append(problems, err.Error())
	}
	lines := makefileLines(mk)
	for _, name := range makefileStampVars {
		problems = append(problems, makefileStampVarProblems(lines, name)...)
	}
	for _, l := range lines {
		if makefileAbbreviates(l.text) {
			problems = append(problems, fmt.Sprintf("%s:%d: git's abbreviation is sized by the object count; take the first %d characters of the full sha: %s", makefilePath, l.no, Len, strings.TrimSpace(l.text)))
		}
	}
	return problems
}

// makefileStampVarProblems checks every line that defines the stamp variable
// name: HEAD_STAMP must cut the full sha to STAMP_LEN, and any other stamp
// variable must take HEAD_STAMP. A variable no line defines is a problem, so
// the guard never passes a stamp it did not see.
func makefileStampVarProblems(lines []makefileLine, name string) []string {
	def := regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `[ \t]*[:?]?=`)
	var problems []string
	defined := false
	for _, l := range lines {
		if !def.MatchString(l.text) {
			continue
		}
		defined = true
		if name == "HEAD_STAMP" {
			if !strings.Contains(l.text, "rev-parse") || !strings.Contains(l.text, "cut -c1-$(STAMP_LEN)") {
				problems = append(problems, fmt.Sprintf("%s:%d: HEAD_STAMP must cut the full sha to the first STAMP_LEN characters (cut -c1-$(STAMP_LEN))", makefilePath, l.no))
			}
		} else if !strings.Contains(l.text, "$(HEAD_STAMP)") {
			problems = append(problems, fmt.Sprintf("%s:%d: %s must take the stamp from $(HEAD_STAMP), not its own git request", makefilePath, l.no, name))
		}
	}
	if !defined {
		problems = append(problems, fmt.Sprintf("%s defines no %s; the stamp is defined once, from HEAD_STAMP", makefilePath, name))
	}
	return problems
}

// goAbbreviates reports a Go code line that passes a --short argument to git.
// A comment is not code.
func goAbbreviates(line string) bool {
	return !strings.HasPrefix(strings.TrimSpace(line), "//") && strings.Contains(line, `"--short`)
}

// abbreviationHits returns "file:line: text" for each line of src that hit
// reports, except lines inside an exempt function. A top-level "}" ends the
// function, so an exemption never covers the code after it.
func abbreviationHits(file, src string, hit func(string) bool, exempt map[string]string) []string {
	var hits []string
	fn := ""
	for i, line := range strings.Split(src, "\n") {
		if m := goFuncName.FindStringSubmatch(line); m != nil {
			fn = m[1]
		} else if line == "}" {
			fn = ""
		}
		if !hit(line) {
			continue
		}
		if _, ok := exempt[fn]; ok {
			continue
		}
		hits = append(hits, fmt.Sprintf("%s:%d: %s", file, i+1, strings.TrimSpace(line)))
	}
	return hits
}

// readRepoFile reads a file under the repository root, two levels up from this
// package. An unreadable producer is an error, never an empty file the scan
// passes as clean.
func readRepoFile(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	return string(b), err
}

// makefileStampLenErr is nil when the Makefile defines STAMP_LEN as Len, the one
// length every stamp producer in the Makefile takes its characters from.
func makefileStampLenErr(mk string) error {
	m := makefileStampLenLine.FindStringSubmatch(mk)
	if m == nil {
		return fmt.Errorf("%s has no STAMP_LEN := <n> line; the stamp length must be defined once there", makefilePath)
	}
	if got, err := strconv.Atoi(m[1]); err != nil || got != Len {
		return fmt.Errorf("%s STAMP_LEN = %s, want buildstamp.Len = %d", makefilePath, m[1], Len)
	}
	return nil
}

// TestProducersUseOneStampRule: each producer the test lists takes the first Len
// characters of the full sha, so an installed gt, a polecat image tag and a
// daemon image stamp can never differ. The Makefile's stamp variables all take
// their characters from HEAD_STAMP, which cuts to STAMP_LEN (the same Len), and
// no listed producer asks git for an abbreviation, whose length git sizes by the
// object count (gt-ian66).
func TestProducersUseOneStampRule(t *testing.T) {
	t.Parallel()
	mk, err := readRepoFile(makefilePath)
	if err != nil {
		t.Fatalf("cannot read stamp producer %s: %v", makefilePath, err)
	}
	for _, problem := range makefileProblems(mk) {
		t.Error(problem)
	}
	for _, p := range goProducers {
		src, err := readRepoFile(p.path)
		if err != nil {
			t.Fatalf("cannot read stamp producer %s: %v", p.path, err)
		}
		for _, hit := range abbreviationHits(p.path, src, goAbbreviates, p.exempt) {
			t.Errorf("stamp producer %s: git's abbreviation is sized by the object count; use buildstamp.Short on the full sha", hit)
		}
	}
}

// TestMakefileStampLenIsLen: the Makefile's length is the Go side's Len, and a
// Makefile that defines no length, or a different one, fails the guard.
func TestMakefileStampLenIsLen(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mk      string
		wantErr bool
	}{
		{name: "defined as Len", mk: "STAMP_LEN := 9\n"},
		{name: "shorter than Len", mk: "STAMP_LEN := 8\n", wantErr: true},
		{name: "longer than Len", mk: "STAMP_LEN := 10\n", wantErr: true},
		{name: "not defined", mk: "COMMIT := x\n", wantErr: true},
		{name: "defined only when unset", mk: "STAMP_LEN ?= 9\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := makefileStampLenErr(tc.mk)
			if (err != nil) != tc.wantErr {
				t.Errorf("makefileStampLenErr = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

// goodMakefile carries the stamp lines the way the real Makefile writes them.
const goodMakefile = "STAMP_LEN := 9\n" +
	"HEAD_STAMP := $(shell git rev-parse --verify --quiet HEAD 2>/dev/null | cut -c1-$(STAMP_LEN))\n" +
	"COMMIT := $(or $(HEAD_STAMP),unknown)\n" +
	"POLE_TAG ?= $(or $(HEAD_STAMP),dev)\n" +
	"POLE_COMMIT := $(shell git rev-parse HEAD 2>/dev/null)\n"

// TestMakefileProblems: the Makefile guard passes the stamp lines as written and
// names the line of each way they break the rule.
func TestMakefileProblems(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mk   string
		want string // a substring of the problems; "" means none
	}{
		{name: "stamp lines as written pass", mk: goodMakefile},
		{
			name: "COMMIT cut to a literal length",
			mk:   strings.Replace(goodMakefile, "COMMIT := $(or $(HEAD_STAMP),unknown)", "COMMIT := $(shell git rev-parse HEAD | cut -c1-8)", 1),
			want: "Makefile:3: COMMIT must take the stamp from $(HEAD_STAMP)",
		},
		{
			name: "POLE_TAG takes the full sha",
			mk:   strings.Replace(goodMakefile, "POLE_TAG ?= $(or $(HEAD_STAMP),dev)", "POLE_TAG ?= $(shell git rev-parse HEAD)", 1),
			want: "Makefile:4: POLE_TAG must take the stamp from $(HEAD_STAMP)",
		},
		{
			name: "HEAD_STAMP cut to a literal length",
			mk:   strings.Replace(goodMakefile, "cut -c1-$(STAMP_LEN))", "cut -c1-8)", 1),
			want: "Makefile:2: HEAD_STAMP must cut the full sha to the first STAMP_LEN characters",
		},
		{
			name: "COMMIT not defined",
			mk:   strings.Replace(goodMakefile, "COMMIT := $(or $(HEAD_STAMP),unknown)\n", "", 1),
			want: "Makefile defines no COMMIT",
		},
		{
			name: "continued recipe line asks git for --short",
			mk:   goodMakefile + "check:\n\t@x=$$(git rev-parse \\\n\t\t--short HEAD); echo $$x\n",
			want: "Makefile:7: git's abbreviation is sized by the object count",
		},
		{
			name: "comment naming --short is not code",
			mk:   goodMakefile + "# do not pass rev-parse --short here\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := strings.Join(makefileProblems(tc.mk), "\n")
			if tc.want == "" && got != "" {
				t.Errorf("makefileProblems = %q, want none", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("makefileProblems = %q, want it to name %q", got, tc.want)
			}
		})
	}
}

// TestUnreadableProducerFails: a producer the guard cannot read is an error,
// so the guard cannot pass on a file it never saw.
func TestUnreadableProducerFails(t *testing.T) {
	t.Parallel()
	if src, err := readRepoFile("internal/buildstamp/no-such-producer.go"); err == nil {
		t.Errorf("readRepoFile of a missing producer = %q, want an error", src)
	}
}

// TestStampGuardFlagsAbbreviation: the scan refuses each way a producer can ask
// git for an abbreviation, and passes the 9-character forms, comments and the
// exempt build ids.
func TestStampGuardFlagsAbbreviation(t *testing.T) {
	t.Parallel()
	const bdID = "func bdBuildID(git Git) (string, error) {\n\tout, err := git(dir, \"rev-parse\", \"--short\", commit)\n}\n"
	exemptBdID := map[string]string{"bdBuildID": "bd build id"}
	cases := []struct {
		name   string
		hit    func(string) bool
		src    string
		exempt map[string]string
		want   int
	}{
		{
			name: "makefile COMMIT with git's auto length",
			hit:  makefileAbbreviates,
			src:  `COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")`,
			want: 1,
		},
		{
			name: "makefile with an explicit length",
			hit:  makefileAbbreviates,
			src:  `POLE_TAG ?= $(shell git rev-parse --short=9 HEAD)`,
			want: 1,
		},
		{
			name: "makefile cut to nine characters",
			hit:  makefileAbbreviates,
			src:  `HEAD_STAMP := $(shell git rev-parse HEAD 2>/dev/null | cut -c1-$(STAMP_LEN))`,
			want: 0,
		},
		{
			name: "makefile symbolic-ref --short is not a commit id",
			hit:  makefileAbbreviates,
			src:  `BRANCH := $(shell git symbolic-ref --short -q HEAD 2>/dev/null)`,
			want: 0,
		},
		{
			name: "go argument list gains --short",
			hit:  goAbbreviates,
			src:  "func ShortHead() (string, error) {\n\tout, err := g.output(\"rev-parse\", \"--short\", \"HEAD\")\n}\n",
			want: 1,
		},
		{
			name: "go --short on its own argument line",
			hit:  goAbbreviates,
			src:  "func ShortHead() (string, error) {\n\tout, err := g.output(\n\t\t\"rev-parse\",\n\t\t\"--short\",\n\t\t\"HEAD\")\n}\n",
			want: 1,
		},
		{
			name: "go comment naming --short is not code",
			hit:  goAbbreviates,
			src:  "// do not pass \"--short\" to rev-parse\nfunc ShortHead() {}\n",
			want: 0,
		},
		{
			name:   "go exempt build id",
			hit:    goAbbreviates,
			src:    bdID,
			exempt: exemptBdID,
			want:   0,
		},
		{
			name: "go build id not exempt",
			hit:  goAbbreviates,
			src:  bdID,
			want: 1,
		},
		{
			name:   "exemption ends at the function's closing brace",
			hit:    goAbbreviates,
			src:    bdID + "func other() {\n\tx := \"--short\"\n}\n",
			exempt: exemptBdID,
			want:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits := abbreviationHits("producer.go", tc.src, tc.hit, tc.exempt)
			if len(hits) != tc.want {
				t.Errorf("got %d hits %q, want %d", len(hits), hits, tc.want)
			}
		})
	}
}
