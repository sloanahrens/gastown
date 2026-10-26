package presubmit

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// maxLine bounds one diff or cover-profile line. A generated file can hold a
// line longer than bufio's 64 KiB default, and a scanner that gives up on it
// would end the check early instead of judging the rest of the branch.
const maxLine = 1 << 20

// Finding is one added error branch that the branch's tests do not reach.
type Finding struct {
	Path string // repo-relative path in the branch's tree
	Line int    // line number in the branch's version of the file
	Text string // the added line, trimmed
}

// Result is what the check read and decided for one branch.
type Result struct {
	// Findings are the added error returns the profile records as never executed.
	Findings []Finding
	// Added is how many added error returns the diff holds in Go sources.
	Added int
	// Judged is how many of those the profile covers: a file the profile does not
	// mention was not built with coverage and is not judged. Added minus Judged is the
	// number the check could not see, so the caller prints both and a clean report is
	// never a report that judged nothing.
	Judged int
	// Quoted is how many added error returns sit in a path git quoted, which the check
	// does not read; they are counted here instead of being dropped without a word.
	Quoted int
	// Unjudged are the added error returns in root-module files the profile does not name. They
	// were not built with coverage, so nothing checked them, and they fail the check even when
	// other returns in the diff are judged (gt-b9j3o.5, overseer ruling).
	Unjudged []Finding
	// Excluded are the unjudged returns in files the build constraints of the target platform
	// exclude (GOOS, GOARCH, //go:build): no build can cover them there, so they are reported and
	// do not fail the check.
	Excluded []Finding
}

// UntestedErrorBranches reports the added Go error returns that the cover profile records as never
// executed.
//
// The rules: a return is the last line of a failure path, so an uncovered one is a failure path no
// test drives. The forms counted as an error return are the ones the diff text can name (err, an
// Err-named value, EOF, fmt.Errorf, errors.New, errors.Join); a call that returns an error without
// naming it, such as `return f.Close()`, is left unjudged. A profile file is matched by its full
// name, module plus the diff path, and nothing else, so a same-named file in another package never
// judges this one. Tests, testdata and directories the go tool skips are not sources. module is the
// module path the profile names files by (go test writes github.com/x/y/pkg/file.go).
func UntestedErrorBranches(diff, cover io.Reader, module string, nested, excluded func(path string) bool) (Result, error) {
	var res Result
	added, err := addedLines(diff)
	if err != nil {
		return res, err
	}
	blocks, err := parseCover(cover)
	if err != nil {
		return res, err
	}
	for _, line := range added {
		if !errorReturn(line.text) {
			continue
		}
		if line.path == "" {
			res.Quoted++
			continue
		}
		if !isGoSource(line.path) || (nested != nil && nested(line.path)) {
			continue
		}
		res.Added++
		f := Finding{Path: line.path, Line: line.line, Text: strings.TrimSpace(line.text)}
		judged, driven := driveStatus(blocks, module+"/"+line.path, line.line, returnColumn(line.text))
		if !judged {
			if excluded != nil && excluded(line.path) {
				res.Excluded = append(res.Excluded, f)
			} else {
				res.Unjudged = append(res.Unjudged, f)
			}
			continue
		}
		res.Judged++
		if !driven {
			res.Findings = append(res.Findings, f)
		}
	}
	return res, nil
}

// addedLine is one added line of a unified diff, with the line number git
// gives it in the branch's version of the file.
type addedLine struct {
	path string
	line int
	text string
}

// hunkHeaderRE reads the new-side start of a hunk header, "@@ -a,b +c,d @@".
var hunkHeaderRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// addedLines reads a unified diff: the file each hunk belongs to, and every
// line the hunk adds with its number in the new file. Context lines advance
// the number too, so a diff made with -U0 and one with context both read
// correctly; git's "No newline at end of file" marker names no line.
func addedLines(r io.Reader) ([]addedLine, error) {
	var out []addedLine
	sc := newScanner(r)
	file := ""
	inHunk := false
	next := 0
	for sc.Scan() {
		raw := strings.TrimSuffix(sc.Text(), "\r")
		if inHunk && raw != "" {
			switch raw[0] {
			case '+':
				out = append(out, addedLine{path: file, line: next, text: raw[1:]})
				next++
				continue
			case '-', '\\':
				continue
			case ' ':
				next++
				continue
			}
		}
		// A hunk body line always carries one of the prefixes above, so
		// anything else here is a header: the hunk is over.
		inHunk = false
		switch {
		case strings.HasPrefix(raw, "+++ "):
			file = diffPath(raw[len("+++ "):])
		case strings.HasPrefix(raw, "@@"):
			m := hunkHeaderRE.FindStringSubmatch(raw)
			if m == nil {
				return nil, fmt.Errorf("unreadable hunk header %q", raw)
			}
			start, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, fmt.Errorf("unreadable hunk header %q: %w", raw, err)
			}
			next, inHunk = start, true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// diffPath reads the path on a "+++ " line. A deletion names /dev/null, and a
// path git quoted reads as the empty string so it is never reported with a
// name no editor can find.
func diffPath(s string) string {
	s = strings.TrimSuffix(s, "\t")
	if s == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		return ""
	}
	return strings.TrimPrefix(s, "b/")
}

// isGoSource reports whether path is a Go source file a test can drive: not a
// test file, and not under a directory the go tool skips.
func isGoSource(p string) bool {
	if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
		return false
	}
	if base := path.Base(p); strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
		return false
	}
	for _, segment := range strings.Split(path.Dir(p), "/") {
		if segment == "." {
			continue
		}
		if segment == "testdata" || strings.HasPrefix(segment, "_") || strings.HasPrefix(segment, ".") {
			return false
		}
	}
	return true
}

// errorReturn reports whether an added line is a return statement that carries
// an error value in its expression list.
func errorReturn(line string) bool {
	s := strings.TrimSpace(line)
	rest, ok := strings.CutPrefix(s, "return")
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return false
	}
	for _, expr := range topLevelExprs(rest) {
		if errorExpr(expr) {
			return true
		}
	}
	return false
}

// topLevelExprs splits a return statement's expression list on the commas that
// are not inside a call, literal or string, and drops a trailing // comment.
func topLevelExprs(s string) []string {
	var out []string
	start, depth := 0, 0
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\' && quote != '`':
				escaped = true
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				return append(out, s[start:i])
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// errorExpr reports whether one returned expression is an error value: the err
// variable, a name spelled like an error, EOF, or a constructor that builds
// one. A comparison such as err == nil returns a bool and is not one, so only
// an expression that is a bare name or selector chain is judged by its name; a
// call is known by the constructor it names and nothing else.
func errorExpr(expr string) bool {
	e := strings.TrimSpace(expr)
	if e == "" {
		return false
	}
	head := callHead(e)
	switch head {
	case "fmt.Errorf", "errors.New", "errors.Join":
		return true
	}
	if e != head {
		return false
	}
	last := head[strings.LastIndexByte(head, '.')+1:]
	switch {
	case last == "err", last == "EOF":
		return true
	case strings.HasSuffix(last, "Err"), strings.HasSuffix(last, "Error"):
		return true
	case strings.HasPrefix(last, "Err") && len(last) > 3 && isUpper(last[3]):
		return true
	}
	return false
}

// callHead returns the identifier or selector chain an expression starts with:
// "fmt.Errorf(" reads as "fmt.Errorf", "err == nil" as "err".
func callHead(e string) string {
	end := 0
	for end < len(e) {
		c := e[end]
		switch {
		case c == '_' || isLetter(c) || (end > 0 && c >= '0' && c <= '9'):
			end++
		case c == '.' && end > 0 && e[end-1] != '.':
			end++
		default:
			return e[:end]
		}
	}
	return e[:end]
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }

// coverBlock is one statement range of a cover profile: where it starts and ends (line and
// column) and how many times the instrumented code ran. go test writes a block for every
// instrumented statement, so a block count of zero is a statement no test executed, and a file
// with no blocks was not built with coverage.
type coverBlock struct {
	file      string
	startLine int
	startCol  int
	endLine   int
	endCol    int
	count     int
}

// returnColumn is the byte column of the return keyword in an added line, which is where the
// statement a coverage block covers begins (go cover columns count bytes from 1).
func returnColumn(text string) int {
	if i := strings.Index(text, "return"); i >= 0 {
		return i + 1
	}
	return 1
}

// parseCover reads a go test cover profile. A file without its "mode:" line is
// refused rather than read as empty, so a wrong path fails the check instead of
// silently judging nothing.
func parseCover(r io.Reader) ([]coverBlock, error) {
	sc := newScanner(r)
	var blocks []coverBlock
	sawMode := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "mode: ") {
			sawMode = true
			continue
		}
		b, err := parseCoverBlock(line)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sawMode {
		return nil, fmt.Errorf("cover profile has no mode: line")
	}
	return blocks, nil
}

// parseCoverBlock reads one profile line, "file:start.col,end.col stmts count".
func parseCoverBlock(line string) (coverBlock, error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return coverBlock{}, fmt.Errorf("unreadable cover profile line %q", line)
	}
	count, err := strconv.Atoi(fields[2])
	if err != nil {
		return coverBlock{}, fmt.Errorf("unreadable count in %q: %w", line, err)
	}
	colon := strings.LastIndexByte(fields[0], ':')
	if colon < 0 {
		return coverBlock{}, fmt.Errorf("unreadable cover profile line %q", line)
	}
	sl, sc, el, ec, err := parseCoverRange(fields[0][colon+1:])
	if err != nil {
		return coverBlock{}, fmt.Errorf("unreadable cover profile line %q: %w", line, err)
	}
	return coverBlock{file: fields[0][:colon], startLine: sl, startCol: sc, endLine: el, endCol: ec, count: count}, nil
}

// parseCoverRange reads "startLine.startCol,endLine.endCol" and keeps all four numbers.
func parseCoverRange(s string) (sl, sc, el, ec int, err error) {
	bounds := strings.SplitN(s, ",", 2)
	if len(bounds) != 2 {
		return 0, 0, 0, 0, fmt.Errorf("range %q has no comma", s)
	}
	sl, sc, err = posOf(bounds[0])
	if err != nil {
		return 0, 0, 0, 0, err
	}
	el, ec, err = posOf(bounds[1])
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return sl, sc, el, ec, nil
}

// posOf reads "line.col".
func posOf(s string) (int, int, error) {
	line, col, found := strings.Cut(s, ".")
	if !found {
		return 0, 0, fmt.Errorf("position %q has no column", s)
	}
	l, err := strconv.Atoi(line)
	if err != nil {
		return 0, 0, err
	}
	c, err := strconv.Atoi(col)
	if err != nil {
		return 0, 0, err
	}
	return l, c, nil
}

// driveStatus reports whether the profile names file at all and, if it does, whether one executed
// block covers the position (line, col). A block covers a position by its line.column range, so a
// return that shares a line with an executed block is not covered by that block. A file the profile
// does not name is unjudged: nothing built it with coverage.
func driveStatus(blocks []coverBlock, file string, line, col int) (judged, driven bool) {
	for _, b := range blocks {
		if b.file != file {
			continue
		}
		judged = true
		if b.count > 0 && within(b, line, col) {
			driven = true
		}
	}
	return judged, driven
}

// within reports whether the position (line, col) lies in the block's range, both ends included.
func within(b coverBlock, line, col int) bool {
	afterStart := line > b.startLine || (line == b.startLine && col >= b.startCol)
	beforeEnd := line < b.endLine || (line == b.endLine && col <= b.endCol)
	return afterStart && beforeEnd
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	return sc
}
