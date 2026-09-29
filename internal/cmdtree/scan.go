package cmdtree

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Ref is one gt or bd invocation found in a file.
type Ref struct {
	File  string   // repo-relative, forward slashes
	Line  int      // physical line in File
	Bin   string   // "gt" or "bd"
	Words []string // leading command-like words after Bin, as written
}

// Token renders the invocation as "gt mq close".
func (r Ref) Token() string { return strings.TrimSpace(r.Bin + " " + strings.Join(r.Words, " ")) }

// invocation matches gt/bd as a command: at line start or after whitespace,
// a shell operator, a quote, or `$(`/`{`, and followed by a blank. That rules
// out ~/gt/... paths and gt-abc bead ids.
var invocation = regexp.MustCompile("(?:^|[\\s;&|(`$'\"{])(gt|bd)[ \\t]+")

// cmdWord is a token that can be a command word. Flags, placeholders,
// variables, quoted strings and paths all stop word collection.
var cmdWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func commandWords(rest string) []string {
	var words []string
	for _, tok := range strings.Fields(rest) {
		if strings.HasPrefix(tok, "#") {
			break
		}
		trimmed := strings.TrimRight(tok, ";)`\"',.:")
		if !cmdWord.MatchString(trimmed) {
			break
		}
		words = append(words, trimmed)
		if trimmed != tok {
			break
		}
	}
	return words
}

// scanShellLine returns the invocations on one line of shell-like text. A
// mention bounded by whitespace inside an open quote is text, not a command
// (log "Installing gt from ..."), unless a shell operator puts it in command
// position ("... && gt prime").
func scanShellLine(file string, line int, text string) []Ref {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
		return nil
	}
	return scanMatches(file, line, text, func(boundary byte, start int) bool {
		if !isSpace(boundary) || !insideQuote(text[:start]) {
			return true
		}
		return commandPosition(text[:start])
	})
}

// scanInlineSpan returns the invocations in one markdown code span. Spans are
// often prose (`Rogue bd check: clean`), so gt/bd counts only in command
// position: at the start of the span or after a shell operator, past any
// VAR=value assignments.
func scanInlineSpan(file string, line int, span string) []Ref {
	return scanMatches(file, line, span, func(_ byte, start int) bool {
		return commandPosition(span[:start])
	})
}

// scanMatches collects the invocations in text that keep accepts; boundary
// is the byte before gt/bd (0 at the start of text).
func scanMatches(file string, line int, text string, keep func(boundary byte, start int) bool) []Ref {
	var refs []Ref
	for _, m := range invocation.FindAllStringSubmatchIndex(text, -1) {
		var boundary byte
		if m[2] > m[0] {
			boundary = text[m[0]]
		}
		if !keep(boundary, m[2]) {
			continue
		}
		words := commandWords(text[m[1]:])
		if len(words) == 0 {
			continue
		}
		refs = append(refs, Ref{File: file, Line: line, Bin: text[m[2]:m[3]], Words: words})
	}
	return refs
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }

// insideQuote reports whether prefix ends inside an unclosed shell quote.
func insideQuote(prefix string) bool {
	var quote byte
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		switch {
		case c == '\\' && quote != '\'':
			i++
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
		case c == quote:
			quote = 0
		}
	}
	return quote != 0
}

var (
	trailingAssignments = regexp.MustCompile(`(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)+$`)
	commandPositionEnd  = regexp.MustCompile("(?:^|&&|\\|\\||\\||;|\\$\\(|\\(|\\{|`|(?:^|\\s)(?:then|do|else|exec|xargs|if|while|until|!))\\s*$")
)

// commandPosition reports whether a command starting right after prefix is
// in command position.
func commandPosition(prefix string) bool {
	prefix = trailingAssignments.ReplaceAllString(prefix, "")
	return commandPositionEnd.MatchString(prefix)
}

// ScanShell treats every non-comment line of text as shell. firstLine is the
// line number of text's first line.
func ScanShell(file, text string, firstLine int) []Ref {
	var refs []Ref
	for i, l := range strings.Split(text, "\n") {
		refs = append(refs, scanShellLine(file, firstLine+i, l)...)
	}
	return refs
}

// markdown tracks fenced-code state across lines. Fenced lines are shell;
// outside a fence only inline code spans are checked, since prose that
// mentions "the gt binary" is not an invocation.
type markdown struct{ inFence bool }

var inlineCode = regexp.MustCompile("`([^`]+)`")

func (m *markdown) line(file string, n int, text string) []Ref {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		m.inFence = !m.inFence
		return nil
	}
	if m.inFence {
		return scanShellLine(file, n, text)
	}
	var refs []Ref
	for _, span := range inlineCode.FindAllStringSubmatch(text, -1) {
		refs = append(refs, scanInlineSpan(file, n, span[1])...)
	}
	return refs
}

// ScanMarkdown checks fenced code blocks and inline code spans.
func ScanMarkdown(file, text string) []Ref {
	var md markdown
	var refs []Ref
	for i, l := range strings.Split(text, "\n") {
		refs = append(refs, md.line(file, i+1, l)...)
	}
	return refs
}

// tomlKey matches the start of a TOML key/value line, capturing the value's
// opening quotes; tomlTable matches a table header. Either one begins a new
// string, so fence state from the previous string is dropped.
var (
	tomlKey   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*\s*=\s*("""|'''|"|')?`)
	tomlTable = regexp.MustCompile(`^\[\[?[A-Za-z0-9_.-]+\]\]?\s*$`)
)

var tomlUnescaper = strings.NewReplacer(`\\`, `\`, `\n`, "\n", `\"`, `"`, `\t`, "\t")

// ScanTOMLMarkdown scans a formula: TOML whose string values are markdown.
// Single-line strings carry their newlines as \n escapes, so each physical
// line is un-escaped and its logical lines all report the physical line.
// Keys and tables are recognised only outside multiline strings, where a
// shell line such as fail=0 would otherwise read as one.
func ScanTOMLMarkdown(file, text string) []Ref {
	var md markdown
	var refs []Ref
	multi := "" // closing delimiter of the multiline string we are in
	for i, raw := range strings.Split(text, "\n") {
		if multi != "" {
			if strings.Contains(raw, multi) {
				multi = ""
			}
		} else {
			if tomlTable.MatchString(raw) {
				md.inFence = false
				continue
			}
			if m := tomlKey.FindStringSubmatchIndex(raw); m != nil {
				md.inFence = false
				open := ""
				if m[2] >= 0 {
					open = raw[m[2]:m[3]]
				}
				raw = raw[m[1]:]
				if (open == `"""` || open == "'''") && !strings.Contains(raw, open) {
					multi = open
				}
			}
		}
		for _, l := range strings.Split(tomlUnescaper.Replace(raw), "\n") {
			refs = append(refs, md.line(file, i+1, l)...)
		}
	}
	return refs
}

// goCallArgs returns the binary and the argument expressions that follow it
// for the call shapes that exec gt or bd, or "" when call is none of them.
func goCallArgs(call *ast.CallExpr) (string, []ast.Expr) {
	lit := func(i int) string {
		if i >= len(call.Args) {
			return ""
		}
		if b, ok := call.Args[i].(*ast.BasicLit); ok && b.Kind == token.STRING {
			s, err := strconv.Unquote(b.Value)
			if err == nil {
				return s
			}
		}
		return ""
	}
	after := func(i int) []ast.Expr {
		if i > len(call.Args) {
			return nil
		}
		return call.Args[i:]
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if fn.Name == "BdCmd" {
			return "bd", call.Args
		}
	case *ast.SelectorExpr:
		pkg, ok := fn.X.(*ast.Ident)
		if !ok {
			return "", nil
		}
		switch pkg.Name + "." + fn.Sel.Name {
		case "exec.Command":
			if b := lit(0); b == "gt" || b == "bd" {
				return b, after(1)
			}
		case "exec.CommandContext":
			if b := lit(1); b == "gt" || b == "bd" {
				return b, after(2)
			}
		case "beads.Command":
			return "bd", after(3)
		case "beads.CommandContext", "beads.CommandContextBounded":
			return "bd", after(4)
		case "beads.CommandWithEnv":
			return "bd", after(2)
		}
	}
	return "", nil
}

// ScanGo finds exec.Command("gt"|"bd", ...), BdCmd(...) and beads.Command*
// calls and takes their leading string-literal arguments as the words.
// Calls whose arguments are built at runtime (args...) yield nothing.
func ScanGo(file string, src []byte) ([]Ref, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		bin, args := goCallArgs(call)
		if bin == "" {
			return true
		}
		var words []string
		for _, a := range args {
			b, ok := a.(*ast.BasicLit)
			if !ok || b.Kind != token.STRING {
				break
			}
			s, err := strconv.Unquote(b.Value)
			if err != nil || !cmdWord.MatchString(s) {
				break
			}
			words = append(words, s)
		}
		if len(words) > 0 {
			refs = append(refs, Ref{File: file, Line: fset.Position(call.Pos()).Line, Bin: bin, Words: words})
		}
		return true
	})
	return refs, nil
}

// jsExec matches an exec-style call whose program is gt or bd and whose
// arguments are an array literal: pi.exec("gt", ["prime", "--hook"]).
var (
	jsExec   = regexp.MustCompile(`(?s)\bexec\w*\(\s*["'](gt|bd)["']\s*,\s*\[([^\]]*)\]`)
	jsString = regexp.MustCompile(`^["']([^"']*)["']$`)
)

// ScanJS finds exec("gt"|"bd", [...]) calls in JavaScript and TypeScript hook
// templates. Other strings in those files are log and error text that names
// gt in prose ("gt tap guard rejected this operation"), so they are not read.
func ScanJS(file, text string) []Ref {
	var refs []Ref
	for _, m := range jsExec.FindAllStringSubmatchIndex(text, -1) {
		var words []string
		for _, el := range strings.Split(text[m[4]:m[5]], ",") {
			lit := jsString.FindStringSubmatch(strings.TrimSpace(el))
			if lit == nil || !cmdWord.MatchString(lit[1]) {
				break
			}
			words = append(words, lit[1])
		}
		if len(words) == 0 {
			continue
		}
		line := 1 + strings.Count(text[:m[0]], "\n")
		refs = append(refs, Ref{File: file, Line: line, Bin: text[m[2]:m[3]], Words: words})
	}
	return refs
}

type scanFunc func(file string, src []byte) ([]Ref, error)

func textScan(fn func(file, text string) []Ref) scanFunc {
	return func(file string, src []byte) ([]Ref, error) { return fn(file, string(src)), nil }
}

var (
	scanShellFile = textScan(func(file, text string) []Ref { return ScanShell(file, text, 1) })
	scanMDFile    = textScan(ScanMarkdown)
	scanTOMLFile  = textScan(ScanTOMLMarkdown)
	scanJSFile    = textScan(ScanJS)
)

// scannerFor picks the scanner for a repo-relative path, or nil when the
// file is not one of the lint's inputs: formulas, templates, plugins, hook
// templates and scripts, role configs, and non-test Go under internal/ and cmd/.
func scannerFor(rel string) scanFunc {
	base := filepath.Base(rel)
	ext := filepath.Ext(rel)
	under := func(dir string) bool { return strings.HasPrefix(rel, dir) }
	switch {
	case strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.sh"):
		return nil
	case ext == ".go":
		if under("internal/") || under("cmd/") {
			return ScanGo
		}
	case under("internal/formula/formulas/"):
		if ext == ".toml" {
			return scanTOMLFile
		}
	case under("internal/hooks/templates/"):
		if ext == ".js" || ext == ".ts" {
			return scanJSFile
		}
		return scanShellFile
	case under("internal/templates/"), under("templates/"):
		if ext == ".md" || ext == ".tmpl" {
			return scanMDFile
		}
	case under("plugins/"):
		if base == "plugin.md" {
			return scanMDFile
		}
		if ext == ".sh" {
			return scanShellFile
		}
	case under("scripts/guards/"):
		if ext == ".sh" {
			return scanShellFile
		}
	case under("internal/config/roles/"):
		if ext == ".toml" {
			return scanShellFile
		}
	}
	return nil
}

// ScanRepo walks root and returns every invocation in the lint's inputs, in
// walk (lexical) order. Ref.File is relative to root.
func ScanRepo(root string) ([]Ref, error) {
	var refs []Ref
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		scan := scannerFor(rel)
		if scan == nil {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found, err := scan(rel, src)
		if err != nil {
			return err
		}
		refs = append(refs, found...)
		return nil
	})
	return refs, err
}
