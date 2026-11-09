// Package shellparse parses one shell command into the flattened segments the
// command guards inspect, and names the construct when it cannot read the
// command.
//
// Parse reads a bash AST from mvdan.cc/sh/v3 and returns every executable
// command site in source order. A compound command contributes the segments of
// its bodies, not a segment of its own: a pipeline yields one segment per side,
// and a subshell, brace group, if, for, while, case or function body yields the
// segments inside it. A redirection written on a compound command applies to
// every segment inside it and is marked Inherited. A leading VAR=value prefix
// is not a command word: it is kept in Segment.Assigns, and Segment.Words
// starts at the command name.
//
// Word.Text is the word with quoting resolved: quotes are removed, backslash
// escapes applied and ANSI-C $'...' escapes decoded; an expansion stays as its
// source text. Word.Literal is true only when the shell would pass Text
// unchanged, which needs every part decoded and no expansion, tilde, glob or
// brace expansion in the word.
//
// The parser is mvdan.cc/sh/v3 v3.14.1 (released 2026-09-06, BSD-3-Clause);
// its go.mod is what raises golang.org/x/sys, golang.org/x/term and
// github.com/rogpeppe/go-internal to the versions go.mod lists.
package shellparse

import (
	"errors"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Script is one parsed command: its command sites in source order.
type Script struct {
	Segments []Segment

	// Subs holds the substitutions that run from a construct header (a for
	// loop's iteration list, a case word or pattern, an arithmetic or [[ ]]
	// expression) and so have no segment of their own.
	Subs []Script

	// Raw is the source text of a substitution's body as written: the text
	// between "$(" or "`" and the closing ")" or "`", or between "<(" or ">("
	// and ")". The body of a nested substitution keeps its escapes and quotes
	// as written. Parse's own result, the whole command, has no Raw.
	Raw string
}

// Segment is one command site: a simple command, a declaration or let clause,
// or a redirection-only command.
type Segment struct {
	// Words are the command and its arguments, quoting removed.
	Words []Word

	// Assigns are the VAR=value words that prefix the command.
	Assigns []Word

	// Redirs are the redirections other than here-documents.
	Redirs []Redirection

	// Heredocs are the here-document bodies the command reads.
	Heredocs []Heredoc

	// Subs are the substitutions in this segment's words, assignment values,
	// redirection targets and here-document bodies.
	Subs []Script

	// After is the operator that ends the segment: ";", "&&", "||", "|", "&",
	// or "" for the segment that ends the script. A newline reads as ";", the
	// way the shell's grammar treats it.
	After string

	// Shells holds the ids of the subshells enclosing the segment, outermost
	// first. One id belongs to one "(" group, so two segments share a prefix
	// only when they run in the same subshell. A brace group is not a
	// subshell and contributes no id.
	Shells []int

	// Conditional reports that the segment may not run, or runs in a copy of the
	// shell, because it sits in the body of an if, elif, else, while, until, for,
	// select, case or function, inside a brace group, subshell or coproc, after
	// "&&" or "||" but not as the first command of the list, in a pipeline of
	// more than one command, or in a background ("&") statement.
	Conditional bool
}

// Word is one shell word: Text has quoting resolved, Raw is the source text,
// and Literal reports that Text is the string the shell passes unchanged.
type Word struct {
	Text    string
	Raw     string
	Literal bool

	// Src is the byte offset of the word's first byte in the source Parse
	// read, or -1 when the parser gave it no position. It puts words back in
	// source order next to a redirection, whose operator may stand between
	// them.
	Src int

	// ProcSubs are the process substitutions written in the word, in order. A
	// word that is one of them ("<(cmd)", ">(cmd)") names a file the shell
	// fills by running it, so nothing has to cut the body out of the word
	// text; a word with text around one, or two side by side, is an ordinary
	// operand path.
	ProcSubs []ProcSubst
}

// ProcSubst is one process substitution: its source text and the command lines
// it runs.
type ProcSubst struct {
	Raw    string
	Script Script
}

// Redirection is one redirection: Op is the operator as written, N the file
// descriptor or {name} prefix, and Word the target.
type Redirection struct {
	Op   string
	N    string
	Word Word

	// Inherited is true when the redirection belongs to a compound command that
	// encloses the segment (a subshell, brace group, if, for, while, case or
	// function body), so it applies to this segment along with every other
	// segment inside that command. It is false when the redirection belongs to
	// the command itself.
	Inherited bool

	// Src is the byte offset of the redirection's target word, or -1 when the
	// parser gave it no position.
	Src int

	// OpSrc is the byte offset of the operator. With Src it tells a target
	// glued to its operator ("0<<<text") from one written after it ("<<< text"),
	// which is where a shell reads a bare here-string's word run from.
	OpSrc int
}

// Heredoc is one here-document: the delimiter as written, whether it was
// quoted, and the body with the shell's processing applied.
type Heredoc struct {
	Delimiter string
	Quoted    bool
	Body      string

	// N is the file descriptor the body is read on: "0", "3", "{name}", or ""
	// for the shell's default. Only the standard input descriptor feeds a
	// shell its script.
	N string
}

// UnparseableError reports a command Parse could not read, naming the
// construct in words ("unterminated here-document").
type UnparseableError struct {
	Construct string
}

func (e *UnparseableError) Error() string {
	return "shellparse: unreadable command: " + e.Construct
}

// Parse parses command into its segments. A command it cannot read returns an
// *UnparseableError rather than a partial result, so a caller that guards
// against a dangerous command can fail closed.
func Parse(command string) (Script, error) {
	// The parser reads a backslash at the end of a comment line as a line
	// continuation, which hides the next line from it. The blank gives it the
	// line break bash reads and keeps every offset, so the words are still
	// cut from command, the text the caller wrote (gt-4otugt).
	src, scanErr := blankCommentBackslashes(command)
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true))
	file, err := parser.Parse(strings.NewReader(src), "")
	if err != nil {
		return Script{}, unparseable(err)
	}
	// mvdan's own error names the construct, so it is reported first. A parse
	// that succeeds over a comment the walk did not reach is refused here.
	if scanErr != nil {
		return Script{}, scanErr
	}
	return flatten(command, file)
}

// flatten walks a parsed file whose source is src. It is separate from Parse so
// a test can pair a parsed file with source it did not read, which is the only
// way to reach nodeRaw's position guard.
func flatten(src string, file *syntax.File) (Script, error) {
	f := &flattener{src: src}
	var script Script
	f.stmts(file.Stmts, &script)
	if f.err != nil {
		return Script{}, f.err
	}
	return script, nil
}

// unparseable names the construct a parser error reports.
func unparseable(err error) error {
	return &UnparseableError{Construct: constructOf(err)}
}

// constructOf is the construct name for one parser error. The last return is a
// guard: a parser error of a type this version of mvdan does not report is
// still refused by name rather than passed to a caller as a bare message.
func constructOf(err error) string {
	var parseErr syntax.ParseError
	if errors.As(err, &parseErr) {
		return constructName(parseErr.Text)
	}
	var langErr syntax.LangError
	if errors.As(err, &langErr) {
		return "shell syntax outside bash"
	}
	return "unrecognized shell syntax"
}

// constructName turns the parser's message into the name of the construct. The
// phrases depend on mvdan's wording, so an unmatched message falls back to a
// generic phrase rather than echoing the parser text.
func constructName(msg string) string {
	switch {
	case strings.Contains(msg, "here-document"):
		return "unterminated here-document"
	case strings.Contains(msg, "without closing quote"):
		return "unterminated quote"
	case strings.Contains(msg, "without matching `$(`"):
		return "unterminated command substitution"
	case strings.Contains(msg, "without matching `<(`"), strings.Contains(msg, "without matching `>(`"):
		return "unterminated process substitution"
	case strings.Contains(msg, "without matching `${`"):
		return "unterminated parameter expansion"
	case strings.Contains(msg, "without matching `(`"), strings.Contains(msg, "encountered `)`"):
		return "unbalanced parenthesis"
	case strings.Contains(msg, "without matching `{`"):
		return "unbalanced brace"
	case strings.Contains(msg, "must end with `fi`"):
		return "unterminated if statement"
	case strings.Contains(msg, "must end with `esac`"):
		return "unterminated case statement"
	case strings.Contains(msg, "must end with `done`"):
		return "unterminated loop"
	case strings.Contains(msg, "must be followed by"):
		return "incomplete command"
	default:
		return "unrecognized shell syntax"
	}
}

// flattener walks a parsed bash file, reading source positions from src.
type flattener struct {
	src string

	// err is the first construct the walk could not read; flatten turns it into
	// the *UnparseableError Parse returns and drops the partial result.
	err error

	// shells is the stack of subshell ids the walk is inside, and nextShell the
	// id the next "(" group takes.
	shells    []int
	nextShell int

	// conditional counts the conditional constructs the walk is inside. A segment
	// added while it is above zero is marked Conditional.
	conditional int

	// quoted counts the double-quoted words and here-document bodies the walk
	// is inside, where a "<(" or ">(" is plain text to the shell.
	quoted int
}

// conditionally runs walk with the segments it adds marked Conditional.
func (f *flattener) conditionally(walk func()) {
	f.conditional++
	walk()
	f.conditional--
}

// segmentShells is the subshell stack a new segment carries, copied so a later
// sibling group cannot rewrite it.
func (f *flattener) segmentShells() []int {
	if len(f.shells) == 0 {
		return nil
	}
	return append([]int(nil), f.shells...)
}

// setAfter records op as the operator ending the last segment the walk has
// added since index from. A statement that added no segment leaves nothing to
// mark.
func setAfter(into *Script, from int, op string) {
	if from < len(into.Segments) {
		into.Segments[len(into.Segments)-1].After = op
	}
}

// fail records the first construct the walk could not read.
func (f *flattener) fail(construct string) {
	f.record(&UnparseableError{Construct: construct})
}

// record keeps the first failure the walk meets.
func (f *flattener) record(err error) {
	if f.err == nil {
		f.err = err
	}
}

// stmts flattens every statement in list into the script accumulated so far.
func (f *flattener) stmts(list []*syntax.Stmt, into *Script) {
	for i, s := range list {
		if f.err != nil {
			return
		}
		before := len(into.Segments)
		f.stmt(s, into)
		if i < len(list)-1 {
			// A statement list is separated by ";" or a newline, which the
			// shell's grammar treats the same way, or by a background "&",
			// which the parser records on the statement rather than as an
			// operator between two of them.
			op := ";"
			if s.Background {
				op = "&"
			}
			setAfter(into, before, op)
		}
	}
}

// stmt flattens one statement and attaches its redirections. A redirection on a
// simple command belongs to that command's segment; a redirection on a compound
// command applies to every segment inside it, so each one carries a copy marked
// Inherited. When the statement produced no segment (a redirection-only
// command), the redirections get a segment of their own.
func (f *flattener) stmt(s *syntax.Stmt, into *Script) {
	if s.Background {
		// A background statement runs in a copy of the shell while the walk
		// moves on, so its segments are conditional.
		f.conditional++
		defer func() { f.conditional-- }()
	}
	before := len(into.Segments)
	if s.Cmd != nil {
		f.command(s.Cmd, into)
	}
	var redirs []Redirection
	var heredocs []Heredoc
	var subs []Script
	for _, r := range s.Redirs {
		f.redirect(r, &redirs, &heredocs, &subs)
	}
	if f.err != nil {
		return
	}
	inherited := compoundCommand(s.Cmd)
	for i := before; i < len(into.Segments); i++ {
		seg := &into.Segments[i]
		for _, r := range redirs {
			r.Inherited = inherited
			seg.Redirs = append(seg.Redirs, r)
		}
		seg.Heredocs = append(seg.Heredocs, heredocs...)
	}
	if len(into.Segments) > before {
		// A substitution in a redirection target runs once, with the first
		// segment the statement produced.
		into.Segments[before].Subs = append(into.Segments[before].Subs, subs...)
		return
	}
	if len(redirs) == 0 && len(heredocs) == 0 && len(subs) == 0 {
		return
	}
	into.Segments = append(into.Segments, Segment{Redirs: redirs, Heredocs: heredocs, Subs: subs, Shells: f.segmentShells(), Conditional: f.conditional > 0})
}

// compoundCommand reports whether c encloses other commands, so a redirection
// written on c applies to every segment inside it rather than to c alone.
func compoundCommand(c syntax.Command) bool {
	switch c.(type) {
	case *syntax.CallExpr, *syntax.DeclClause, *syntax.LetClause, *syntax.ArithmCmd, *syntax.TestClause:
		return false
	default:
		return true
	}
}

// command flattens one command node into the segments it executes.
func (f *flattener) command(c syntax.Command, into *Script) {
	switch x := c.(type) {
	case *syntax.CallExpr:
		var seg Segment
		for _, a := range x.Assigns {
			f.assign(a, &seg)
		}
		for _, arg := range x.Args {
			seg.Words = append(seg.Words, f.word(arg))
			f.wordSubs(arg, &seg.Subs)
		}
		if f.err != nil {
			return
		}
		seg.Shells = f.segmentShells()
		seg.Conditional = f.conditional > 0
		into.Segments = append(into.Segments, seg)

	case *syntax.BinaryCmd:
		before := len(into.Segments)
		if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
			// Every command of a pipeline runs in a subshell, so the left side is
			// conditional too; "&&" and "||" make only the right side conditional.
			f.conditionally(func() { f.stmt(x.X, into) })
		} else {
			f.stmt(x.X, into)
		}
		setAfter(into, before, x.Op.String())
		f.conditionally(func() { f.stmt(x.Y, into) })

	case *syntax.Subshell:
		f.shells = append(f.shells, f.nextShell)
		f.nextShell++
		f.conditionally(func() { f.stmts(x.Stmts, into) })
		f.shells = f.shells[:len(f.shells)-1]

	case *syntax.Block:
		f.conditionally(func() { f.stmts(x.Stmts, into) })

	case *syntax.IfClause:
		cond := len(into.Segments)
		f.stmts(x.Cond, into)
		setAfter(into, cond, ";")
		f.conditionally(func() {
			then := len(into.Segments)
			f.stmts(x.Then, into)
			if x.Else != nil {
				setAfter(into, then, ";")
				f.command(x.Else, into)
			}
		})

	case *syntax.ForClause:
		switch loop := x.Loop.(type) {
		case *syntax.WordIter:
			for _, item := range loop.Items {
				f.wordSubs(item, &into.Subs)
			}
		case *syntax.CStyleLoop:
			for _, expr := range []syntax.ArithmExpr{loop.Init, loop.Cond, loop.Post} {
				if expr != nil {
					f.arithmSubs(expr, &into.Subs)
				}
			}
		}
		f.conditionally(func() { f.stmts(x.Do, into) })

	case *syntax.WhileClause:
		cond := len(into.Segments)
		f.stmts(x.Cond, into)
		setAfter(into, cond, ";")
		f.conditionally(func() { f.stmts(x.Do, into) })

	case *syntax.CaseClause:
		f.wordSubs(x.Word, &into.Subs)
		for _, item := range x.Items {
			for _, pattern := range item.Patterns {
				f.wordSubs(pattern, &into.Subs)
			}
			f.conditionally(func() { f.stmts(item.Stmts, into) })
		}

	case *syntax.FuncDecl:
		if x.Body != nil {
			f.conditionally(func() { f.stmt(x.Body, into) })
		}

	case *syntax.TimeClause:
		if x.Stmt != nil {
			f.stmt(x.Stmt, into)
		}

	case *syntax.CoprocClause:
		f.wordSubs(x.Name, &into.Subs)
		if x.Stmt != nil {
			// A coproc runs in a copy of the shell, like a background statement.
			f.conditionally(func() { f.stmt(x.Stmt, into) })
		}

	case *syntax.DeclClause:
		name := "declare"
		if x.Variant != nil {
			name = x.Variant.Value
		}
		seg := Segment{Words: []Word{{Text: name, Raw: name, Literal: true}}}
		for _, a := range x.Args {
			f.assign(a, &seg)
		}
		if f.err != nil {
			return
		}
		seg.Shells = f.segmentShells()
		seg.Conditional = f.conditional > 0
		into.Segments = append(into.Segments, seg)

	case *syntax.LetClause:
		seg := Segment{Words: []Word{{Text: "let", Raw: "let", Literal: true}}}
		for _, expr := range x.Exprs {
			raw := f.raw(expr)
			if f.err != nil {
				return
			}
			// let evaluates each expression as arithmetic, so the source text is
			// not a literal argument.
			seg.Words = append(seg.Words, Word{Text: raw, Raw: raw})
			f.arithmSubs(expr, &seg.Subs)
		}
		seg.Shells = f.segmentShells()
		seg.Conditional = f.conditional > 0
		into.Segments = append(into.Segments, seg)

	case *syntax.TestDecl:
		f.wordSubs(x.Description, &into.Subs)
		if x.Body != nil {
			f.stmt(x.Body, into)
		}

	case *syntax.ArithmCmd:
		// (( )) runs no words; only its substitutions run.
		if x.X != nil {
			f.arithmSubs(x.X, &into.Subs)
		}

	case *syntax.TestClause:
		// [[ ]] runs no words of its own; only its substitutions run.
		if x.X != nil {
			f.testSubs(x.X, &into.Subs)
		}

	default:
		f.fail("unrecognized command construct")
	}
}

// assign appends one VAR=value word to seg and collects the substitutions in
// its value, index and array.
func (f *flattener) assign(a *syntax.Assign, seg *Segment) {
	if a == nil {
		f.fail("assignment without a name or value")
		return
	}
	name := ""
	if a.Name != nil {
		name = a.Name.Value
	}
	if a.Index != nil {
		name += "[" + f.raw(a.Index) + "]"
		f.arithmSubs(a.Index, &seg.Subs)
	}
	op := "="
	if a.Append {
		op = "+="
	}
	text := name
	literal := true
	switch {
	case a.Array != nil:
		elems := make([]string, 0, len(a.Array.Elems))
		for _, elem := range a.Array.Elems {
			if elem == nil {
				f.fail("array element without a value")
				return
			}
			if elem.Index != nil {
				f.arithmSubs(elem.Index, &seg.Subs)
			}
			if elem.Value != nil {
				elems = append(elems, f.word(elem.Value).Text)
				f.wordSubs(elem.Value, &seg.Subs)
			}
			if f.err != nil {
				return
			}
		}
		// An array assignment passes no single string to a command.
		text = name + op + "(" + strings.Join(elems, " ") + ")"
		literal = false
	case a.Value != nil:
		value := f.word(a.Value)
		text = name + op + value.Text
		literal = value.Literal
		f.wordSubs(a.Value, &seg.Subs)
	}
	if f.err != nil {
		return
	}
	seg.Assigns = append(seg.Assigns, Word{Text: text, Raw: f.raw(a), Literal: literal})
}

// redirect reads one redirection into its output slices.
func (f *flattener) redirect(r *syntax.Redirect, redirs *[]Redirection, heredocs *[]Heredoc, subs *[]Script) {
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		if r.Hdoc == nil {
			f.fail("here-document without a body")
			return
		}
		if r.Word == nil || len(r.Word.Parts) == 0 {
			f.fail("here-document without a delimiter")
			return
		}
		delimiter, _ := f.wordText(r.Word, quoteNone)
		if f.err != nil {
			return
		}
		raw := f.raw(r.Word)
		if f.err != nil {
			return
		}
		quoted := delimiter != raw
		body := f.heredocBody(r.Hdoc, quoted, r.Op == syntax.DashHdoc)
		if f.err != nil {
			return
		}
		n := ""
		if r.N != nil {
			n = r.N.Value
		}
		*heredocs = append(*heredocs, Heredoc{Delimiter: delimiter, Quoted: quoted, Body: body, N: n})
		// A here-document body is read like a double-quoted word: a "<(" in it
		// is text, not a process substitution.
		f.quoted++
		f.wordSubs(r.Hdoc, subs)
		f.quoted--
	default:
		target := f.word(r.Word)
		if f.err != nil {
			return
		}
		n := ""
		if r.N != nil {
			n = r.N.Value
		}
		*redirs = append(*redirs, Redirection{
			Op:    r.Op.String(),
			N:     n,
			Word:  target,
			Src:   target.Src,
			OpSrc: posOffset(r.OpPos),
		})
		f.wordSubs(r.Word, subs)
	}
}

// heredocBody is the text of a here-document body. A quoted delimiter makes
// bash read the body literally; an unquoted one decodes the backslash escapes
// bash keeps in a here-document. A <<- body has the leading tabs of every line
// stripped from the raw source before either rule runs, which is where bash's
// reader applies the dash. The parser leaves the leading tabs of the delimiter
// line at the body's end, and they are stripped the same way.
func (f *flattener) heredocBody(w *syntax.Word, quoted, dash bool) string {
	stripper := dashStripper{dash: dash, atLineStart: true}
	var b strings.Builder
	for _, part := range w.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			if quoted {
				f.fail("quoted here-document with an expansion")
				return ""
			}
			// An expansion stays as its source text, with the dash applied to
			// the lines it spans.
			b.WriteString(stripper.strip(f.raw(part)))
			if f.err != nil {
				return ""
			}
			continue
		}
		raw := stripper.strip(lit.Value)
		if quoted {
			b.WriteString(raw)
			continue
		}
		b.WriteString(decodeHeredoc(raw))
	}
	return b.String()
}

// dashStripper applies <<- to a body's raw source. bash strips the leading tabs
// of a line before it decodes anything, and a line joined to the previous one
// by a backslash-newline starts no new line: the tab run after a continuation
// stays. The delimiter line the parser leaves at the body end has its tabs
// stripped as the final line.
type dashStripper struct {
	dash        bool
	atLineStart bool
	backslashes int
}

// strip removes the leading tabs of every line in raw and carries the line
// state into the next part.
func (d *dashStripper) strip(raw string) string {
	if !d.dash {
		return raw
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '\t' && d.atLineStart {
			continue
		}
		if c == '\n' {
			d.atLineStart = d.backslashes%2 == 0
			d.backslashes = 0
		} else {
			d.atLineStart = false
			if c == '\\' {
				d.backslashes++
			} else {
				d.backslashes = 0
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// word builds the Word for w.
func (f *flattener) word(w *syntax.Word) Word {
	text, decoded := f.wordText(w, quoteNone)
	if f.err != nil {
		return Word{}
	}
	word := Word{
		Text:    text,
		Raw:     f.raw(w),
		Literal: literalWord(w.Parts, decoded),
		Src:     offsetOf(w),
	}
	word.ProcSubs = f.procSubsts(w)
	if f.err != nil {
		return Word{}
	}
	return word
}

// procSubsts flattens the process substitutions written in a word. A
// substitution inside double quotes is plain text to the shell, and mvdan does
// not give it a ProcSubst part, so it is not collected here either.
func (f *flattener) procSubsts(w *syntax.Word) []ProcSubst {
	if w == nil {
		return nil
	}
	var subs []ProcSubst
	f.partsProcSubsts(w.Parts, &subs)
	return subs
}

// partsProcSubsts is the walk behind procSubsts.
func (f *flattener) partsProcSubsts(parts []syntax.WordPart, dest *[]ProcSubst) {
	for _, part := range parts {
		switch x := part.(type) {
		case *syntax.DblQuoted:
			f.partsProcSubsts(x.Parts, dest)
		case *syntax.ProcSubst:
			raw := f.raw(part)
			if f.err != nil {
				return
			}
			var scripts []Script
			f.script(x, x.Stmts, &scripts)
			if f.err != nil {
				return
			}
			if len(scripts) == 1 {
				*dest = append(*dest, ProcSubst{Raw: raw, Script: scripts[0]})
			}
		}
	}
}

// offsetOf is the source byte offset a node starts at, or -1 when the parser
// gave it no position.
func offsetOf(n syntax.Node) int {
	if n == nil {
		return -1
	}
	return posOffset(n.Pos())
}

// posOffset is offsetOf for a position already in hand.
func posOffset(p syntax.Pos) int {
	if !p.IsValid() {
		return -1
	}
	return int(p.Offset())
}

// literalWord reports whether the shell passes the word's text unchanged: every
// part decoded, no expansion, and no unquoted tilde, glob or brace expansion.
// An ANSI-C escape decoded from a table is quoted output, so it counts as
// literal.
func literalWord(parts []syntax.WordPart, decoded bool) bool {
	if !decoded {
		return false
	}
	for i, part := range parts {
		switch x := part.(type) {
		case *syntax.Lit:
			if expandsUnquoted(x.Value) {
				return false
			}
			// A ~ expands only at the start of a word, and an escaped ~ never
			// reaches here as the first byte.
			if i == 0 && len(x.Value) > 0 && x.Value[0] == '~' {
				return false
			}
		case *syntax.SglQuoted:
			// Quoted text is passed as written after ANSI-C decoding.
		case *syntax.DblQuoted:
			if partsExpand(x.Parts) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// expandsUnquoted reports whether an unquoted literal holds a character the
// shell expands: a glob metacharacter (*, ?, a bracket expression) or a brace
// expansion. A backslash quotes the byte after it.
func expandsUnquoted(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '*', '?':
			return true
		case '[':
			// bash expands a bracket expression only when a closing ] follows.
			if strings.IndexByte(s[i+1:], ']') >= 0 {
				return true
			}
		case '{':
			if braceExpands(s[i:]) {
				return true
			}
		}
	}
	return false
}

// braceExpands reports whether s, which starts at an unquoted {, is a brace
// expansion: a matching } with a top-level , or .. between them.
func braceExpands(s string) bool {
	depth := 0
	comma, dotdot := false, false
	prev := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			i++
			prev = 0
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return comma || dotdot
			}
		case ',':
			if depth == 1 {
				comma = true
			}
		case '.':
			if depth == 1 && prev == '.' {
				dotdot = true
			}
		}
		prev = c
	}
	return false
}

// quoteContext is the quoting a Lit part is read in, which decides which
// backslashes the shell removes.
type quoteContext int

const (
	// quoteNone is an unquoted word: a backslash escapes the next byte.
	quoteNone quoteContext = iota

	// quoteDouble is inside double quotes: a backslash escapes only $, `, " and
	// \, and a backslash-newline is a line continuation.
	quoteDouble
)

// wordText is the word's text with quoting resolved, and reports whether every
// part could be decoded.
func (f *flattener) wordText(w *syntax.Word, ctx quoteContext) (string, bool) {
	if w == nil || len(w.Parts) == 0 {
		f.fail("word without source")
		return "", false
	}
	var b strings.Builder
	decoded := f.partsText(w.Parts, ctx, &b)
	if f.err != nil {
		return "", false
	}
	return b.String(), decoded
}

// partsText writes the text of each part: literals by their decoded value, and
// every expansion as its source text. It reports whether every part could be
// decoded; an ANSI-C escape outside bash's table leaves decoded false because
// its text is kept as written.
func (f *flattener) partsText(parts []syntax.WordPart, ctx quoteContext, b *strings.Builder) bool {
	decoded := true
	for _, part := range parts {
		switch x := part.(type) {
		case *syntax.Lit:
			if ctx == quoteNone {
				b.WriteString(decodeUnquoted(x.Value))
			} else {
				b.WriteString(decodeQuoted(x.Value))
			}
		case *syntax.SglQuoted:
			if !x.Dollar {
				b.WriteString(x.Value)
				continue
			}
			text, ok, err := decodeANSIC(x.Value)
			if err != nil {
				f.record(err)
				return false
			}
			b.WriteString(text)
			decoded = decoded && ok
		case *syntax.DblQuoted:
			if !f.partsText(x.Parts, quoteDouble, b) {
				decoded = false
			}
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst, *syntax.ExtGlob, *syntax.BraceExp:
			b.WriteString(f.raw(part))
		default:
			f.fail("unrecognized word construct")
			return false
		}
		if f.err != nil {
			return false
		}
	}
	return decoded
}

// wordSubs appends the substitutions in w to dest.
func (f *flattener) wordSubs(w *syntax.Word, dest *[]Script) {
	if w == nil {
		return
	}
	f.partsSubs(w.Parts, dest)
}

// partsSubs appends the substitutions in parts to dest, each as a nested
// Script.
func (f *flattener) partsSubs(parts []syntax.WordPart, dest *[]Script) {
	for _, part := range parts {
		switch x := part.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			f.quoted++
			f.partsSubs(x.Parts, dest)
			f.quoted--
		case *syntax.CmdSubst:
			f.script(x, x.Stmts, dest)
		case *syntax.ProcSubst:
			f.script(x, x.Stmts, dest)
		case *syntax.ParamExp:
			f.paramExpSubs(x, dest)
		case *syntax.ArithmExp:
			if x.X != nil {
				f.arithmSubs(x.X, dest)
			}
		case *syntax.BraceExp:
			for _, elem := range x.Elems {
				f.wordSubs(elem, dest)
			}
		case *syntax.ExtGlob:
			// The pattern is a literal; extended globbing runs no command.
		default:
			f.fail("unrecognized word construct")
		}
		if f.err != nil {
			return
		}
	}
}

// script flattens one substitution node's statements into a nested Script
// appended to dest, with the node's body text as its Raw.
func (f *flattener) script(node syntax.Node, stmts []*syntax.Stmt, dest *[]Script) {
	raw := f.substBody(node)
	if f.err != nil {
		return
	}
	// A substitution runs in a shell of its own, so the subshells enclosing
	// the word it is written in do not enclose its segments.
	outer, outerQuoted := f.shells, f.quoted
	f.shells, f.quoted = nil, 0
	script := Script{Raw: raw}
	f.stmts(stmts, &script)
	f.shells, f.quoted = outer, outerQuoted
	if f.err != nil {
		return
	}
	*dest = append(*dest, script)
}

// substBody is the source text between the delimiters of a substitution node:
// the text after the opening "$(", "`", "<(" or ">(" up to the closing ")" or
// "`". A node of another type, or a delimiter position outside src, is a
// failure, never empty text.
func (f *flattener) substBody(node syntax.Node) string {
	var open, closing syntax.Pos
	var openLen int
	var backquote bool
	switch x := node.(type) {
	case *syntax.CmdSubst:
		open, closing, openLen = x.Left, x.Right, 2
		if x.Backquotes {
			openLen, backquote = 1, true
		}
	case *syntax.ProcSubst:
		open, closing, openLen = x.OpPos, x.Rparen, 2
	default:
		f.record(&UnparseableError{Construct: "source position out of range"})
		return ""
	}
	if !open.IsValid() || !closing.IsValid() {
		f.record(&UnparseableError{Construct: "source position out of range"})
		return ""
	}
	s, e := int(open.Offset())+openLen, int(closing.Offset())
	// Parse cannot produce positions outside src, so this bounds check is defense
	// in depth and is reached only by a flattener built with bad positions.
	if s < 0 || e < s || e > len(f.src) {
		f.record(&UnparseableError{Construct: "source position out of range"})
		return ""
	}
	// A backquote nested in a backquote substitution closes with an escaped
	// backquote, and the closing position is that backquote, after the
	// backslash that escapes it. An odd run of backslashes before the position
	// makes the backquote escaped, so the body ends before the one escaping
	// backslash; an even run is body text and stays.
	if backquote && e > s && oddBackslashRun(f.src[:e]) {
		e--
	}
	return f.src[s:e]
}

// oddBackslashRun reports whether s ends in an odd number of backslashes.
func oddBackslashRun(s string) bool {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// paramExpSubs appends the substitutions inside a ${...} expression.
func (f *flattener) paramExpSubs(p *syntax.ParamExp, dest *[]Script) {
	if p.Index != nil {
		f.arithmSubs(p.Index, dest)
	}
	if p.Slice != nil {
		for _, expr := range []syntax.ArithmExpr{p.Slice.Offset, p.Slice.Length} {
			if expr != nil {
				f.arithmSubs(expr, dest)
			}
		}
	}
	if p.Repl != nil {
		for _, w := range []*syntax.Word{p.Repl.Orig, p.Repl.With} {
			f.expWordSubs(w, dest)
		}
	}
	if p.Exp != nil {
		f.expWordSubs(p.Exp.Word, dest)
	}
	if p.NestedParam != nil {
		f.partsSubs([]syntax.WordPart{p.NestedParam}, dest)
	}
}

// expWordSubs appends the substitutions in the word of a parameter expansion.
// mvdan reads such a word as literal text wherever a "<(" or ">(" stands in it,
// so "${v:-<(cmd)}" carries no ProcSubst part, while the shell runs the process
// substitution in an unquoted word. A word that holds one is parsed again on
// its own, behind ": " so it reads as command arguments, and the substitutions
// that parse finds are the ones reported, each once. Its command substitutions
// come from that parse too, not from the walk of the word's parts, so one that
// sits inside a literal "<(" is not reported twice. A quoted word, or one with
// no "<(", takes the ordinary walk. A word that does not parse is refused, never
// read as having none.
func (f *flattener) expWordSubs(w *syntax.Word, dest *[]Script) {
	if w == nil {
		return
	}
	if f.quoted > 0 {
		f.wordSubs(w, dest)
		return
	}
	text := f.raw(w)
	if f.err != nil {
		return
	}
	if !strings.Contains(text, "<(") && !strings.Contains(text, ">(") {
		f.wordSubs(w, dest)
		return
	}
	synthetic := ": " + text
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(synthetic), "")
	if err != nil {
		f.record(unparseable(err))
		return
	}
	g := &flattener{src: synthetic, conditional: f.conditional}
	var parsed Script
	g.stmts(file.Stmts, &parsed)
	if g.err != nil {
		f.record(g.err)
		return
	}
	for _, seg := range parsed.Segments {
		*dest = append(*dest, seg.Subs...)
	}
	*dest = append(*dest, parsed.Subs...)
}

// arithmSubs appends the substitutions inside an arithmetic expression. A nil
// expression is not arithmetic, so it falls to the default and fails closed.
func (f *flattener) arithmSubs(x syntax.ArithmExpr, dest *[]Script) {
	switch e := x.(type) {
	case *syntax.BinaryArithm:
		f.arithmSubs(e.X, dest)
		f.arithmSubs(e.Y, dest)
	case *syntax.UnaryArithm:
		f.arithmSubs(e.X, dest)
	case *syntax.ParenArithm:
		f.arithmSubs(e.X, dest)
	case *syntax.FlagsArithm:
		f.arithmSubs(e.X, dest)
	case *syntax.Word:
		f.wordSubs(e, dest)
	default:
		f.fail("unrecognized arithmetic expression")
	}
}

// testSubs appends the substitutions inside a [[ ]] expression.
func (f *flattener) testSubs(x syntax.TestExpr, dest *[]Script) {
	switch e := x.(type) {
	case *syntax.BinaryTest:
		f.testSubs(e.X, dest)
		f.testSubs(e.Y, dest)
	case *syntax.UnaryTest:
		f.testSubs(e.X, dest)
	case *syntax.ParenTest:
		f.testSubs(e.X, dest)
	case *syntax.Word:
		f.wordSubs(e, dest)
	default:
		f.fail("unrecognized test expression")
	}
}

// raw is the source text of n. A position outside src is a failure, never empty
// text, so a guard cannot read an unreadable node as an empty one.
func (f *flattener) raw(n syntax.Node) string {
	text, err := f.nodeRaw(n)
	if err != nil {
		f.record(err)
		return ""
	}
	return text
}

// nodeRaw is the source text n spans in src. A node without a position has no
// text to return, so it is refused rather than read as an empty one.
func (f *flattener) nodeRaw(n syntax.Node) (string, error) {
	if n == nil {
		return "", &UnparseableError{Construct: "source position out of range"}
	}
	start, end := n.Pos(), n.End()
	if !start.IsValid() || !end.IsValid() {
		return "", &UnparseableError{Construct: "source position out of range"}
	}
	s, e := int(start.Offset()), int(end.Offset())
	if s < 0 || e < s || e > len(f.src) {
		return "", &UnparseableError{Construct: "source position out of range"}
	}
	return f.src[s:e], nil
}

// partsExpand reports whether any part is an expansion, so a word built from
// them is not a literal.
func partsExpand(parts []syntax.WordPart) bool {
	for _, part := range parts {
		switch x := part.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			if partsExpand(x.Parts) {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// decodeUnquoted resolves an unquoted literal: a backslash removes itself and
// keeps the next byte. The lexer has already removed a backslash-newline pair.
func decodeUnquoted(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// decodeHeredoc resolves the backslash escapes of an unquoted here-document
// body: a backslash escapes $, `, \ and a newline, and any other backslash is
// kept as written, including one before " or '.
func decodeHeredoc(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$', '`', '\\':
			b.WriteByte(s[i+1])
			i++
		case '\n':
			i++
		default:
			b.WriteByte('\\')
		}
	}
	return b.String()
}

// decodeQuoted resolves the backslash escapes a double-quoted string applies: a
// backslash escapes $, `, " and \, and backslash-newline is removed. Any other
// backslash stays as written.
func decodeQuoted(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$', '`', '"', '\\':
			b.WriteByte(s[i+1])
			i++
		case '\n':
			i++
		default:
			b.WriteByte('\\')
		}
	}
	return b.String()
}

// decodeANSIC decodes the content of a $'...' word by bash's escape table. It
// keeps an escape bash leaves alone as written and reports decoded false, so a
// caller never treats undecoded text as literal. An escape that decodes to a
// NUL byte is an error: bash truncates the string there, so the text the shell
// passes cannot be recovered from the source.
func decodeANSIC(s string) (string, bool, error) {
	var b strings.Builder
	decoded := true
	nul := false
	put := func(v byte) {
		if v == 0 {
			nul = true
		}
		b.WriteByte(v)
	}
	putRune := func(r rune) {
		if r == 0 {
			nul = true
		}
		b.WriteRune(r)
	}
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			put(s[i])
			i++
			continue
		}
		if i+1 >= len(s) {
			put('\\')
			i++
			decoded = false
			continue
		}
		c := s[i+1]
		switch c {
		case 'a':
			put(0x07)
			i += 2
		case 'b':
			put(0x08)
			i += 2
		case 'e', 'E':
			put(0x1b)
			i += 2
		case 'f':
			put(0x0c)
			i += 2
		case 'n':
			put('\n')
			i += 2
		case 'r':
			put('\r')
			i += 2
		case 't':
			put('\t')
			i += 2
		case 'v':
			put(0x0b)
			i += 2
		case '\\', '\'', '"', '?':
			put(c)
			i += 2
		case 'c':
			if i+2 >= len(s) {
				b.WriteString(`\c`)
				i += 2
				decoded = false
				continue
			}
			r := s[i+2]
			if r == '?' {
				put(0x7f)
			} else {
				put(r & 0x1f)
			}
			i += 3
		case '0', '1', '2', '3', '4', '5', '6', '7':
			value, n := 0, 0
			for n < 3 && i+1+n < len(s) && s[i+1+n] >= '0' && s[i+1+n] <= '7' {
				value = value*8 + int(s[i+1+n]-'0')
				n++
			}
			put(byte(value))
			i += 1 + n
		case 'x':
			value, n := 0, 0
			for n < 2 && i+2+n < len(s) && isHex(s[i+2+n]) {
				value = value*16 + hexValue(s[i+2+n])
				n++
			}
			if n == 0 {
				b.WriteString(`\x`)
				i += 2
				decoded = false
				continue
			}
			put(byte(value))
			i += 2 + n
		case 'u', 'U':
			width := 4
			if c == 'U' {
				width = 8
			}
			value, n := 0, 0
			for n < width && i+2+n < len(s) && isHex(s[i+2+n]) {
				value = value*16 + hexValue(s[i+2+n])
				n++
			}
			if n == 0 || value > 0x10ffff || (value >= 0xd800 && value <= 0xdfff) {
				b.WriteByte('\\')
				b.WriteByte(c)
				i += 2
				decoded = false
				continue
			}
			putRune(rune(value))
			i += 2 + n
		default:
			b.WriteByte('\\')
			b.WriteByte(c)
			i += 2
			decoded = false
		}
	}
	if nul {
		return "", false, &UnparseableError{Construct: "ANSI-C escape that decodes to a NUL byte"}
	}
	return b.String(), decoded, nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}
