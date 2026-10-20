package targets

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"strconv"
	"strings"
)

// maxScanLine is the longest ranked or history line read. A path or a commit
// subject is far shorter; the buffer only keeps a corrupt input from stopping
// the read as "token too long".
const maxScanLine = 1 << 20

// ParseRanked reads the ranked slice: one tab-separated row per file, in rank
// order, holding the tier, the last review date, the risk score, the size in
// bytes and the path. Rows a caller ranks beyond MaxFiles are kept; Run caps
// them. A row that does not hold the script's five fields is an error, because
// a rank the pass cannot read is a slice it must not guess at.
func ParseRanked(r io.Reader) ([]Row, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxScanLine)
	var rows []Row
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 5 {
			return nil, fmt.Errorf("ranked row %q: want 5 tab-separated fields, got %d", line, len(parts))
		}
		tier, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("ranked row %q: tier is not a number", line)
		}
		score, err := strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return nil, fmt.Errorf("ranked row %q: risk score is not a number", line)
		}
		size, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil, fmt.Errorf("ranked row %q: bytes are not a number", line)
		}
		path := parts[4]
		if err := checkPath(path); err != nil {
			return nil, fmt.Errorf("ranked row %q: %w", line, err)
		}
		rows = append(rows, Row{Tier: tier, LastReview: parts[1], RiskScore: score, Bytes: size, Path: path})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the ranked slice: %w", err)
	}
	return rows, nil
}

// ParseHistory reads `git log --name-only --format=@%s` output into the
// subjects that touched each path, newest first. Every other line is a touched
// path; a path before the first subject belongs to no commit and is ignored.
// The read is tolerant: history that cannot be attributed to a commit names no
// file, which costs a question its context but never stops the run.
func ParseHistory(r io.Reader) History {
	hist := History{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxScanLine)
	subject := ""
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "@"); ok {
			subject = rest
			continue
		}
		if subject == "" {
			continue
		}
		hist[line] = append(hist[line], subject)
	}
	return hist
}

// checkPath refuses a path the source must not read: a ranked row can only
// name a file inside the repository.
func checkPath(path string) error {
	if path == "" {
		return fmt.Errorf("the path is empty")
	}
	if strings.HasPrefix(path, "-") || strings.HasPrefix(path, "/") {
		return fmt.Errorf("path %q must be relative", path)
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return fmt.Errorf("path %q may not leave the repository", path)
		}
	}
	return nil
}

// exportedNames returns the file's exported function, method and type names in
// source order, at most maxIdentifiers. Text that is not parseable Go has no
// names: the file still gets scored, from its path and history.
func exportedNames(text string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "", text, parser.SkipObjectResolution)
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, maxIdentifiers)
	seen := make(map[string]bool, maxIdentifiers)
	add := func(name string) {
		if !ast.IsExported(name) || seen[name] || len(out) >= maxIdentifiers {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			add(d.Name.Name)
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, s := range d.Specs {
				if ts, ok := s.(*ast.TypeSpec); ok {
					add(ts.Name.Name)
				}
			}
		}
	}
	return out
}
