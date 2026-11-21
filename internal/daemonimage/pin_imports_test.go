package daemonimage

import (
	"go/ast"
	"reflect"
	"testing"
)

// importsOf runs the production parser on a source fixture and fails on error.
func importsOf(t *testing.T, src string) []string {
	t.Helper()
	got, err := devopsImports(src)
	if err != nil {
		t.Fatalf("devopsImports: %v", err)
	}
	return got
}

func wantDirs(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("directories = %v, want %v", got, want)
	}
}

func TestDevopsImportsReadsSingleAndGroupedImports(t *testing.T) {
	t.Parallel()
	wantDirs(t, importsOf(t, `package cloudcheck
import "gt.local/devops/pkg/patrol"
import (
	"gt.local/devops/internal/ledgerdb"
	"gt.local/devops/pkg/patrol"
	"gt.local/devops/cmd/cloudcheck"
)`), []string{"pkg/patrol", "internal/ledgerdb", "cmd/cloudcheck"})
}

func TestDevopsImportsKeepsAliasedAndBlankImports(t *testing.T) {
	t.Parallel()
	wantDirs(t, importsOf(t, `package reporter
import (
	client "gt.local/devops/pkg/client"
	_ "gt.local/devops/internal/driver"
	. "gt.local/devops/pkg/patrol"
)`), []string{"pkg/client", "internal/driver", "pkg/patrol"})
}

func TestDevopsImportsDropsStdlibAndOtherModules(t *testing.T) {
	t.Parallel()
	wantDirs(t, importsOf(t, `package reporter
import (
	"fmt"
	"github.com/x/y"
	"gt.local/gastown/internal/daemonimage"
	"gt.local/devopsx/pkg/patrol"
	"gt.local/devops"
	"gt.local/devops/"
	"gt.local/devops/pkg/patrol"
)`), []string{"pkg/patrol"})
}

func TestDevopsImportsIgnoresImportsInComments(t *testing.T) {
	t.Parallel()
	wantDirs(t, importsOf(t, `package reporter
// import "gt.local/devops/from/line/comment"
import (
	// "gt.local/devops/from/inner/line"
	"gt.local/devops/real/pkg"
	/* "gt.local/devops/from/block/comment" */
)`), []string{"real/pkg"})
}

func TestDevopsImportsRefusesASourceThatDoesNotParse(t *testing.T) {
	t.Parallel()
	// A well-formed devops import precedes the unclosed block, so a parser that
	// swallows the error has a partial list to return.
	got, err := devopsImports(`package reporter
import (
	"gt.local/devops/pkg/patrol"
`)
	if err == nil {
		t.Fatalf("devopsImports accepted a source that does not parse and returned %v", got)
	}
	if got != nil {
		t.Errorf("directories = %v, want nil beside the error", got)
	}
}

func TestDevopsImportsRefusesABodySyntaxErrorAfterAGoodImportBlock(t *testing.T) {
	t.Parallel()
	// The import block is well formed and names a devops directory; the syntax
	// error sits in a function body, which only a whole-file parse reaches.
	got, err := devopsImports(`package reporter
import "gt.local/devops/pkg/patrol"
func f( {
`)
	if err == nil {
		t.Fatalf("devopsImports accepted a body syntax error and returned %v", got)
	}
	if got != nil {
		t.Errorf("directories = %v, want nil beside the error", got)
	}
}

// The parser admits only a well-formed string literal, so text cannot reach the
// decode refusal; a crafted tree is what does (gt-886zbx).
func TestDevopsDirsRefusesAnImportLiteralItCannotDecode(t *testing.T) {
	t.Parallel()
	f := &ast.File{Imports: []*ast.ImportSpec{{Path: &ast.BasicLit{Value: `"gt.local/devops/x`}}}}
	if got, err := devopsDirs(f); err == nil || got != nil {
		t.Fatalf("devopsDirs on an undecodable literal = %v, %v; want nil and an error", got, err)
	}
}
