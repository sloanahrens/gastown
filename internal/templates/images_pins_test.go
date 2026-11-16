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

// Image pin durability (gt-3sgdk9): a caller that must verify every digest pin
// reads DigestPinnedImages, so the list has to hold each distinct pin once, in
// a well-formed reference, and nothing the package pins may stay out of it.

// TestImagePinDurabilityListIsDistinctAndSorted pins the list against the
// package's digest-pinned image constants: each distinct value appears once and
// the result is sorted.
func TestImagePinDurabilityListIsDistinctAndSorted(t *testing.T) {
	t.Parallel()
	want := []string{templates.DindImage, templates.SweepDindImage, templates.DoltServerImage}
	slices.Sort(want)
	want = slices.Compact(want)

	got := templates.DigestPinnedImages()
	if !slices.Equal(got, want) {
		t.Fatalf("DigestPinnedImages() = %q, want the sorted distinct pins %q", got, want)
	}
}

// TestImagePinDurabilityReferencesAreWellFormed: every reference the list
// returns names a host and repository and carries a sha256 digest of 64
// lower-case hex digits, so a verifier can pull it by digest.
func TestImagePinDurabilityReferencesAreWellFormed(t *testing.T) {
	t.Parallel()
	images := templates.DigestPinnedImages()
	if len(images) == 0 {
		t.Fatal("DigestPinnedImages() is empty; no pin is verified")
	}
	for _, ref := range images {
		if problem := imageRefProblem(ref); problem != "" {
			t.Errorf("%q %s", ref, problem)
		}
	}
}

// TestImagePinDurabilityReferenceCheckRejectsBadShapes proves the shape check
// is not vacuous: each malformed sample is refused, so a pin drifting into one
// of these shapes fails the list test above.
func TestImagePinDurabilityReferenceCheckRejectsBadShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  string
	}{
		{"tag only", "docker:29.8.2"},
		{"no repository", "@sha256:" + strings.Repeat("a", 64)},
		{"short digest", "example.com/img@sha256:" + strings.Repeat("a", 63)},
		{"upper-case hex", "example.com/img@sha256:" + strings.Repeat("A", 64)},
		{"empty host", "/img@sha256:" + strings.Repeat("a", 64)},
	}
	for _, tt := range tests {
		if imageRefProblem(tt.ref) == "" {
			t.Errorf("%s: %q accepted, want refused", tt.name, tt.ref)
		}
	}
}

var (
	imageDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	imageHostRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?$`)
)

// imageRefProblem returns why ref is not a host/repo@sha256:<64 hex> reference,
// or "" when it is one.
func imageRefProblem(ref string) string {
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return "carries no @sha256: digest"
	}
	if !imageDigestRE.MatchString(digest) {
		return "digest is not sha256: followed by 64 lower-case hex digits"
	}
	host, _, ok := strings.Cut(repo, "/")
	if !ok {
		return "names no host and repository"
	}
	if !imageHostRE.MatchString(host) {
		return "does not start with a host name"
	}
	return ""
}

// TestImagePinDurabilityListCoversEveryPinnedConstant reads every string
// constant declared in images.go, and fails when one carries a digest but the
// list omits it: a new pin must join the list, not sit beside it. A constant
// carrying "@sha256:" whose value cannot be evaluated fails too, so the scan
// never reads an unreadable pin as absent.
func TestImagePinDurabilityListCoversEveryPinnedConstant(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("images.go")
	if err != nil {
		t.Fatalf("reading images.go: %v", err)
	}
	pins, err := digestPinsInSource(src)
	if err != nil {
		t.Fatalf("scanning images.go for digest pins: %v", err)
	}
	if len(pins) == 0 {
		t.Fatal("no digest-pinned constant found in images.go; the scan is not reading the pins")
	}

	listed := map[string]bool{}
	for _, ref := range templates.DigestPinnedImages() {
		listed[ref] = true
		if !pins[ref] {
			t.Errorf("DigestPinnedImages() lists %q, which no constant in images.go names", ref)
		}
	}
	for ref := range pins {
		if !listed[ref] {
			t.Errorf("images.go pins %q by digest but DigestPinnedImages() omits it", ref)
		}
	}
}

// digestPinsInSource returns the value of every string constant declared in one
// source file whose value carries an "@sha256:" digest.
func digestPinsInSource(src []byte) (map[string]bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "images.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	exprs := map[string]ast.Expr{}
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range values.Names {
				if i >= len(values.Values) {
					continue
				}
				exprs[name.Name] = values.Values[i]
				names = append(names, name.Name)
			}
		}
	}

	pins := map[string]bool{}
	for _, name := range names {
		expr := exprs[name]
		value, ok := evalConstString(expr, exprs, map[string]bool{})
		if !ok {
			start := fset.Position(expr.Pos()).Offset
			end := fset.Position(expr.End()).Offset
			if strings.Contains(string(src[start:end]), "@sha256:") {
				return nil, fmt.Errorf("constant %s carries a digest whose value cannot be read", name)
			}
			continue
		}
		if strings.Contains(value, "@sha256:") {
			pins[value] = true
		}
	}
	return pins, nil
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
