package presubmit

import (
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"path"
	"strings"
)

// IntegrationRefsNote says what the check cannot see (gt-9p2wh7).
const IntegrationRefsNote = "a tagged test in another directory is not checked"

// IntegrationRef is a changed declaration a tagged test in its directory names.
type IntegrationRef struct{ Test, Name string }

// IntegrationRefsResult is what the check judged on one branch.
type IntegrationRefsResult struct {
	Declarations, TaggedFiles int
	Refs                      []IntegrationRef
	Note                      string
}

// IntegrationRefs lists the changed declarations an integration-tagged test in the
// same directory names (gt-9p2wh7); the gate compiles that test but never runs it.
func IntegrationRefs(run Runner, base string) (IntegrationRefsResult, error) {
	res := IntegrationRefsResult{Note: IntegrationRefsNote}
	decls, err := ChangedDecls(run, base)
	if err != nil {
		return res, err
	}
	res.Declarations = decls.Declarations
	out, err := run([]string{"grep", "-l", "-z", "-E", "^//go:build .*integration", "HEAD", "--", "*_test.go"})
	if err != nil {
		var e interface{ ExitCode() int }
		if !errors.As(err, &e) || e.ExitCode() != 1 {
			return res, fmt.Errorf("git grep for tagged tests: %w", err)
		}
		out = nil
	}
	for _, p := range strings.Split(string(out), "\x00") {
		names, ok := decls.Names[path.Dir(p)]
		if p == "" || !ok {
			continue
		}
		src, err := run([]string{"show", "HEAD:" + p})
		if err != nil {
			return res, fmt.Errorf("reading %s at HEAD: %w", p, err)
		}
		if !IntegrationTagged(src) {
			continue
		}
		res.TaggedFiles++
		used := usedIdents(src)
		for _, name := range names {
			if used[name] {
				res.Refs = append(res.Refs, IntegrationRef{Test: p, Name: name})
			}
		}
	}
	return res, nil
}

// usedIdents returns the identifiers src names, comments and strings aside.
func usedIdents(src []byte) map[string]bool {
	used := map[string]bool{}
	var s scanner.Scanner
	s.Init(token.NewFileSet().AddFile("", -1, len(src)), src, func(token.Position, string) {}, 0)
	for {
		if _, tok, lit := s.Scan(); tok == token.EOF {
			return used
		} else if tok == token.IDENT {
			used[lit] = true
		}
	}
}
