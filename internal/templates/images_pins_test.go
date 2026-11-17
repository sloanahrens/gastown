package templates_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// Image pin durability (gt-3sgdk9): a preflight that verifies every digest pin
// reads DigestPinnedImages, so the list must hold each package pin once, in a
// shape a pull accepts, and the scan that reads images.go must fail closed
// rather than let an unreadable pin read as absent.

// TestImagePinDurabilityListHoldsEachPinOnceSorted is a table over the package
// constants that pin an image by digest: the list holds each distinct value
// once, and the whole list is sorted.
func TestImagePinDurabilityListHoldsEachPinOnceSorted(t *testing.T) {
	t.Parallel()
	images := templates.DigestPinnedImages()

	tests := []struct {
		name string
		ref  string
	}{
		{"DindImage", templates.DindImage},
		{"SweepDindImage", templates.SweepDindImage},
		{"DoltServerImage", templates.DoltServerImage},
	}
	distinct := map[string]bool{}
	for _, tt := range tests {
		if got := countOf(images, tt.ref); got != 1 {
			t.Errorf("%s: DigestPinnedImages() holds %q %d times, want once", tt.name, tt.ref, got)
		}
		distinct[tt.ref] = true
	}
	if !slices.IsSorted(images) {
		t.Errorf("DigestPinnedImages() = %q, not sorted", images)
	}
	if len(images) != len(distinct) {
		t.Errorf("DigestPinnedImages() holds %d references, want the %d distinct package pins", len(images), len(distinct))
	}
}

func countOf(values []string, want string) int {
	n := 0
	for _, value := range values {
		if value == want {
			n++
		}
	}
	return n
}

// TestImagePinDurabilityReferencesAreWellFormed is a table over the references
// the list actually returns: each names a host, repository and digest, so a
// verifier can pull it by digest.
func TestImagePinDurabilityReferencesAreWellFormed(t *testing.T) {
	t.Parallel()
	images := templates.DigestPinnedImages()
	if len(images) == 0 {
		t.Fatal("DigestPinnedImages() is empty; no pin is verified")
	}

	tests := make([]struct {
		name string
		ref  string
	}, len(images))
	for i, ref := range images {
		tests[i].name = fmt.Sprintf("reference %d", i)
		tests[i].ref = ref
	}
	for _, tt := range tests {
		if problem := imageRefProblem(tt.ref); problem != "" {
			t.Errorf("%s: %q %s", tt.name, tt.ref, problem)
		}
	}
}

// TestImagePinDurabilityReferenceCheck is a table of accepted and refused
// shapes: the check refuses a reference without a digest or with a malformed
// host, repository or tag, so a pin drifting into one of those shapes fails the
// list test above. A tag beside the digest is accepted because DoltServerImage
// carries one; the digest is what docker pulls.
func TestImagePinDurabilityReferenceCheck(t *testing.T) {
	t.Parallel()
	digest := "@sha256:" + strings.Repeat("a", 64)

	tests := []struct {
		name string
		ref  string
		ok   bool
	}{
		{"host and repository with digest", "example.com/img" + digest, true},
		{"tag beside the digest", "example.com/img:2.3.2" + digest, true},
		{"port in the host", "example.com:5000/img" + digest, true},
		{"tag only", "docker:29.8.2", false},
		{"no host", "/img" + digest, false},
		{"no repository", "example.com" + digest, false},
		{"short digest", "example.com/img@sha256:" + strings.Repeat("a", 63), false},
		{"upper-case digest", "example.com/img@sha256:" + strings.Repeat("A", 64), false},
		{"upper-case repository", "example.com/IMG" + digest, false},
		{"empty tag", "example.com/img:" + digest, false},
		{"space in repository", "example.com/my img" + digest, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problem := imageRefProblem(tt.ref)
			if tt.ok && problem != "" {
				t.Errorf("%q refused: %s", tt.ref, problem)
			}
			if !tt.ok && problem == "" {
				t.Errorf("%q accepted, want refused", tt.ref)
			}
		})
	}
}

var imageRefRE = regexp.MustCompile(
	`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?@sha256:[0-9a-f]{64}$`)

// imageRefProblem returns why ref is not a host/repo[:tag]@sha256:<64 hex>
// reference, or "" when it is one.
func imageRefProblem(ref string) string {
	if imageRefRE.MatchString(ref) {
		return ""
	}
	return "is not a host/repo[:tag]@sha256:<64 lower-case hex> reference"
}

// TestImagePinDurabilityCoverageAlarm is a table over synthetic images.go
// sources: a digest pin the list omits raises a problem, a listed reference the
// source does not name raises one, and a constant whose value the scan cannot
// read is an error rather than a silent skip.
func TestImagePinDurabilityCoverageAlarm(t *testing.T) {
	t.Parallel()
	digest := "@sha256:" + strings.Repeat("a", 64)
	pinned := "example.com/img" + digest

	tests := []struct {
		name        string
		src         string
		listed      []string
		wantProblem string // substring of the alarm; "" when the scan is clean
		wantErr     string // substring of the error; "" when the scan succeeds
	}{
		{
			name:   "every pin is listed",
			src:    "package templates\nconst Pinned = \"example.com/img" + digest + "\"",
			listed: []string{pinned},
		},
		{
			name:        "a pin the list omits alarms",
			src:         "package templates\nconst Pinned = \"example.com/img" + digest + "\"",
			listed:      nil,
			wantProblem: "omits it",
		},
		{
			name:        "a listed pin the source does not name alarms",
			src:         "package templates\nconst Unrelated = \"ubuntu:24.04\"",
			listed:      []string{pinned},
			wantProblem: "no constant in images.go names",
		},
		{
			name:   "a pin built by concatenation is read",
			src:    "package templates\nconst (\nVersion = \"2.3.2\"\nPinned = \"dolthub/dolt-sql-server:\" + Version + \"" + digest + "\"\n)",
			listed: []string{"dolthub/dolt-sql-server:2.3.2" + digest},
		},
		{
			name:   "a pin repeated by omission is read",
			src:    "package templates\nconst (\nPinned = \"example.com/img" + digest + "\"\nAlias\n)",
			listed: []string{pinned},
		},
		{
			name:    "a constant the scan cannot follow fails closed",
			src:     "package templates\nconst Pinned = pinFromAnotherFile",
			wantErr: "cannot evaluate",
		},
		{
			name:    "a digest built by a call fails closed",
			src:     "package templates\nconst Pinned = pin(\"" + digest + "\")",
			wantErr: "cannot evaluate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems, err := imagePinProblemsIn([]byte(tt.src), tt.listed)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := strings.Join(problems, "; ")
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Errorf("problems = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantProblem) {
				t.Errorf("problems = %q, want one naming %q", got, tt.wantProblem)
			}
		})
	}
}

// TestImagePinDurabilityListCoversEveryPinnedConstant runs the scan on the real
// images.go: a digest pin added there without joining DigestPinnedImages, or a
// listed reference the file does not name, fails here.
func TestImagePinDurabilityListCoversEveryPinnedConstant(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("images.go")
	if err != nil {
		t.Fatalf("reading images.go: %v", err)
	}
	problems, err := imagePinProblemsIn(src, templates.DigestPinnedImages())
	if err != nil {
		t.Fatalf("scanning images.go: %v", err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

// imagePinProblemsIn returns why the source and listed disagree: a digest pin
// the source declares but listed omits, or a reference listed does not declare.
// An error means the source's constants could not all be read, never that they
// were absent.
func imagePinProblemsIn(src []byte, listed []string) ([]string, error) {
	pins, err := digestPinsInSource(src)
	if err != nil {
		return nil, err
	}

	var problems []string
	named := map[string]bool{}
	for _, ref := range listed {
		named[ref] = true
		if !pins[ref] {
			problems = append(problems, fmt.Sprintf("DigestPinnedImages() lists %q, which no constant in images.go names", ref))
		}
	}
	for ref := range pins {
		if !named[ref] {
			problems = append(problems, fmt.Sprintf("images.go pins %q by digest but DigestPinnedImages() omits it", ref))
		}
	}
	slices.Sort(problems)
	return problems, nil
}

// constDeclsInSource parses one source file's const declarations into the
// expression that gives each declared name its value, the names in declaration
// order, and which declarations mark their names as images. A declaration that
// inherits the previous expression list is followed; a name with no value to
// inherit is an error, never a skipped constant.
func constDeclsInSource(src []byte) (exprs map[string]ast.Expr, names []string, image map[string]bool, err error) {
	file, err := parser.ParseFile(token.NewFileSet(), "images.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, nil, err
	}

	exprs = map[string]ast.Expr{}
	image = map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		var inherited []ast.Expr
		for _, spec := range gen.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if len(values.Values) > 0 {
				inherited = values.Values
			}
			isImage := declaresAnImage(values.Names)
			for i, name := range values.Names {
				if i >= len(inherited) {
					return nil, nil, nil, fmt.Errorf("constant %s has no value the scan can follow", name.Name)
				}
				exprs[name.Name] = inherited[i]
				names = append(names, name.Name)
				if isImage {
					image[name.Name] = true
				}
			}
		}
	}
	return exprs, names, image, nil
}

// declaresAnImage reports whether every name in a const declaration marks its
// value as an image. A declaration that mixes the two is skipped whole; the
// caller's DaemonBaseImage guard then fails if the skipped declaration held the
// image it needed, so a mixed block cannot quietly pin nothing.
func declaresAnImage(names []*ast.Ident) bool {
	if len(names) == 0 {
		return false
	}
	for _, name := range names {
		if !strings.HasSuffix(name.Name, "Image") {
			return false
		}
	}
	return true
}

// digestPinsInSource returns the value of every string constant declared in one
// source file whose value carries an "@sha256:" digest. A constant whose value
// it cannot read fails the scan unless the value is a provably non-string
// literal, so an unreadable pin never reads as absent.
func digestPinsInSource(src []byte) (map[string]bool, error) {
	exprs, names, _, err := constDeclsInSource(src)
	if err != nil {
		return nil, err
	}

	pins := map[string]bool{}
	for _, name := range names {
		expr := exprs[name]
		value, ok := evalConstString(expr, exprs, map[string]bool{})
		if !ok {
			if isNonStringLiteral(expr) {
				continue
			}
			return nil, fmt.Errorf("constant %s has a value the scan cannot evaluate, so a digest pin in it could go unlisted", name)
		}
		if strings.Contains(value, "@sha256:") {
			pins[value] = true
		}
	}
	return pins, nil
}

// isNonStringLiteral reports whether expr is, through parentheses, a literal
// that cannot be a string constant.
func isNonStringLiteral(expr ast.Expr) bool {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = paren.X
	}
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind != token.STRING
}

// evalConstString evaluates a string constant expression built from literals,
// parentheses, "+" and references to other constants declared in the same file.
// ok is false for anything else.
func evalConstString(expr ast.Expr, exprs map[string]ast.Expr, seen map[string]bool) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return value, true
	case *ast.ParenExpr:
		return evalConstString(e.X, exprs, seen)
	case *ast.Ident:
		if seen[e.Name] {
			return "", false
		}
		inner, ok := exprs[e.Name]
		if !ok {
			return "", false
		}
		seen[e.Name] = true
		value, ok := evalConstString(inner, exprs, seen)
		delete(seen, e.Name)
		return value, ok
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := evalConstString(e.X, exprs, seen)
		if !ok {
			return "", false
		}
		right, ok := evalConstString(e.Y, exprs, seen)
		if !ok {
			return "", false
		}
		return left + right, true
	}
	return "", false
}
