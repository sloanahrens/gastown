package shellparse

import "strings"

// blankCommentBackslashes returns command with each backslash that ends a "#"
// comment line replaced by a space.
//
// bash ends a comment at its newline, and a backslash inside a comment
// continues nothing. mvdan.cc/sh reads "\" + newline as a line continuation even
// inside a comment, so in `bash -c 'x' # n \` + newline + `ls` it joins the
// comment to `ls` and the parser never sees the command bash runs (gt-4otugt).
// The blank keeps every byte offset, so the positions Parse records still index
// the source it read. The error is non-nil when the walk stopped before the end
// of command while command has a backslash before a newline: the comments past
// the stop are not blanked, so Parse must refuse the command even though
// mvdan.cc/sh may read it. It is also non-nil when a comment on a heredoc
// operator line ends in a backslash before a line feed, which is not blanked
// either, and when a comment ends in a backslash before a carriage return and
// line feed (gt-s9ogqg).
func blankCommentBackslashes(command string) (string, error) {
	ends, err := commentLineBackslashes(command)
	if len(ends) == 0 {
		return command, err
	}
	b := []byte(command)
	for _, i := range ends {
		b[i] = ' '
	}
	return string(b), err
}

// maxCommentScanNest bounds how deeply substitutions nest in the walk. Past it
// the walk stops and the command is refused.
const maxCommentScanNest = 256

// commentScan walks a command the way bash lexes it and records the offset of
// each backslash that is the last byte of a comment line.
type commentScan struct {
	src  string
	ends []int

	// nest is how many code walks are open, the substitutions around the
	// current text. It is bounded by maxCommentScanNest.
	nest int

	// heredocs are the heredocs whose operator is read and whose body has not
	// started. The shell reads each body at the next newline, even one inside
	// a substitution the operator sits in, so the list is shared by every walk.
	heredocs []heredocEnd

	// heredocLine is set when a comment on a heredoc operator line ends in a
	// backslash before a bare line feed. Such a comment is not blanked, so Parse
	// refuses the command.
	heredocLine bool

	// crlfLine is set when a comment ends in a backslash before a carriage
	// return and line feed. Its backslash is blanked like one before a bare
	// line feed, unless the comment is on a heredoc operator line, where it is
	// not blanked. Parse refuses the command either way (gt-s9ogqg, gt-wflha1).
	crlfLine bool
}

// commentLineBackslashes returns the offsets of the backslashes that end a
// comment line in src. The walk stops at the first construct it cannot close,
// or at the nesting bound. A comment that ends in a backslash needs a backslash
// before a newline, so a walk that stops hides nothing when src has none: mvdan
// reads that text as bash does. Only then is the stop an error, returned with
// the offsets the walk found before it. A comment on a heredoc operator line
// that ends in a backslash before a bare line feed is an error too, whatever
// follows it, and so is a comment that ends in a backslash before a carriage
// return and line feed.
func commentLineBackslashes(src string) ([]int, error) {
	s := &commentScan{src: src}
	if _, ok := s.code(0, 0); !ok && hasBackslashLineEnd(src) {
		return s.ends, &UnparseableError{Construct: "construct too deeply nested or unclosed to read"}
	}
	if s.heredocLine {
		return s.ends, &UnparseableError{Construct: "comment ending in a backslash on a heredoc operator line"}
	}
	if s.crlfLine {
		return s.ends, &UnparseableError{Construct: "comment ending in a backslash before a CR LF line end"}
	}
	return s.ends, nil
}

// hasBackslashLineEnd reports whether src has a backslash before a line feed,
// with or without a carriage return between them.
func hasBackslashLineEnd(src string) bool {
	return strings.Contains(src, "\\\n") || strings.Contains(src, "\\\r\n")
}

// commentEndBackslash returns the offset of the backslash that ends the comment
// whose text runs from p to nl, and whether a carriage return sits before the
// line feed. nl is the offset of the line feed, or len(src) when there is none.
// It returns -1 when no backslash ends the comment.
//
// A carriage return before the line feed is not a line end for bash: bash ends
// the comment at the line feed and reads the backslash as no continuation, while
// mvdan.cc/sh reads the carriage return and line feed as text in the comment. So
// the scan takes the backslash exactly as it takes one before a bare line feed,
// and the command is refused rather than read one way (gt-s9ogqg). No other line
// ending is taken this way.
func commentEndBackslash(src string, p, nl int) (int, bool) {
	if b := nl - 1; b > p && src[b] == '\\' {
		return b, false
	}
	if nl < len(src) && nl-2 > p && src[nl-1] == '\r' && src[nl-2] == '\\' {
		return nl - 2, true
	}
	return -1, false
}

// code walks shell text from p up to closer and returns the offset after it,
// and whether closer was found. A stop anywhere below returns false up through
// every caller, so the top walk (closer 0) reports whether it read the whole
// command. closer 0 is the top of the command, ')' ends a
// "$(", '`' a backquote and '}' a "${". A heredoc whose operator is read here
// has its body skipped at the next newline, as the shell does.
func (s *commentScan) code(p int, closer byte) (int, bool) {
	src := s.src
	if s.nest == maxCommentScanNest {
		return len(src), false
	}
	s.nest++
	defer func() { s.nest-- }()
	depth := 0
	// A "#" starts a comment only at the start of a word, and "${" opens a
	// name, where "#" is a length or a pattern operator, not a comment.
	wordStart := closer != '}'
	for p < len(src) {
		c := src[p]
		switch {
		case c == '\\':
			if p+1 < len(src) && src[p+1] == '\n' {
				// A continued line joins the next one: the word goes on.
				p += 2
				continue
			}
			p += 2
			wordStart = false
		case c == '\'':
			end := strings.IndexByte(src[p+1:], '\'')
			if end < 0 {
				return len(src), false
			}
			p += end + 2
			wordStart = false
		case c == '"':
			next, ok := s.dquote(p + 1)
			if !ok {
				return next, false
			}
			p, wordStart = next, false
		case c == '`':
			if closer == '`' {
				return p + 1, true
			}
			next, ok := s.code(p+1, '`')
			if !ok {
				return next, false
			}
			p, wordStart = next, false
		case c == '$' && p+1 < len(src) && src[p+1] == '\'':
			q := p + 2
			for q < len(src) && src[q] != '\'' {
				if src[q] == '\\' {
					q++
				}
				q++
			}
			if q >= len(src) {
				return len(src), false
			}
			p, wordStart = q+1, false
		case c == '$':
			next, ok, opened := s.expansion(p)
			if !opened {
				p, wordStart = p+1, false
				continue
			}
			if !ok {
				return next, false
			}
			p, wordStart = next, false
		case c == '#' && wordStart:
			nl := strings.IndexByte(src[p:], '\n')
			if nl < 0 {
				nl = len(src)
			} else {
				nl += p
			}
			// A backslash that ends the comment is blanked, unless the comment
			// is on a heredoc operator line. Blanking that one would mean
			// reading the heredoc body by hand, which gt-wnblap decided against,
			// so the scan records it and Parse refuses the command. The pending
			// heredoc belongs to the body on the next line, not to this comment
			// (gt-4otugt). A carriage return before the line feed is recorded
			// as the CR LF reason, not the heredoc one (gt-wflha1).
			if b, crlf := commentEndBackslash(src, p, nl); b >= 0 {
				if len(s.heredocs) == 0 {
					s.ends = append(s.ends, b)
				} else if !crlf {
					s.heredocLine = true
				}
				s.crlfLine = s.crlfLine || crlf
			}
			p = nl
		case c == '\n':
			p++
			wordStart = true
			for _, h := range s.heredocs {
				p = skipHeredocBody(src, p, h)
			}
			s.heredocs = nil
		case c == '(' && wordStart && p+1 < len(src) && src[p+1] == '(' && arithEnd(src, p+2) >= 0:
			// An arithmetic command "((...))" on one line: a shift in it is not
			// a heredoc, and no comment can sit inside it.
			p = arithEnd(src, p+2)
			wordStart = true
		case c == '<' && p+2 < len(src) && src[p+1] == '<' && src[p+2] == '<':
			// A here-string reads no body; its word follows as an ordinary word.
			p += 3
			wordStart = true
		case c == '<' && p+1 < len(src) && src[p+1] == '<':
			var h heredocEnd
			p += 2
			if p < len(src) && src[p] == '-' {
				h.dash = true
				p++
			}
			for p < len(src) && (src[p] == ' ' || src[p] == '\t') {
				p++
			}
			var delim strings.Builder
			for p < len(src) && !isShellMeta(src[p]) {
				switch src[p] {
				case '\\':
					if p+1 < len(src) {
						delim.WriteByte(src[p+1])
					}
					p += 2
				case '\'', '"':
					end := strings.IndexByte(src[p+1:], src[p])
					if end < 0 {
						return len(src), false
					}
					delim.WriteString(src[p+1 : p+1+end])
					p += end + 2
				default:
					delim.WriteByte(src[p])
					p++
				}
			}
			h.delim = delim.String()
			s.heredocs = append(s.heredocs, h)
			wordStart = false
		case closer == ')' && c == ')', closer == '}' && c == '}':
			if depth == 0 {
				return p + 1, true
			}
			depth--
			p++
			wordStart = isShellMeta(c)
		default:
			if closer == ')' && c == '(' || closer == '}' && c == '{' {
				depth++
			}
			wordStart = isShellMeta(c)
			p++
		}
	}
	return len(src), closer == 0
}

// dquote walks the text of a double-quoted string that starts at p, after the
// opening quote, and returns the offset after the closing quote.
func (s *commentScan) dquote(p int) (int, bool) {
	src := s.src
	for p < len(src) {
		switch src[p] {
		case '\\':
			p += 2
		case '"':
			return p + 1, true
		case '$':
			next, ok, opened := s.expansion(p)
			if !opened {
				p++
				continue
			}
			if !ok {
				return next, false
			}
			p = next
		case '`':
			next, ok := s.code(p+1, '`')
			if !ok {
				return next, false
			}
			p = next
		default:
			p++
		}
	}
	return len(src), false
}

// expansion steps over the "$(", "$((" or "${" that opens at p. opened reports
// whether one does; when it does, ok reports whether it was closed and next is
// the offset after it.
func (s *commentScan) expansion(p int) (next int, ok, opened bool) {
	src := s.src
	switch {
	case strings.HasPrefix(src[p:], "$((") && arithEnd(src, p+3) >= 0:
		// Arithmetic that closes on its line is walked by its parentheses alone:
		// no comment can sit in it, and "<<" in it is a shift, not a heredoc.
		return arithEnd(src, p+3), true, true
	case strings.HasPrefix(src[p:], "$("):
		next, ok = s.code(p+2, ')')
		return next, ok, true
	case strings.HasPrefix(src[p:], "${"):
		next, ok = s.code(p+2, '}')
		return next, ok, true
	}
	return p, true, false
}

// arithEnd returns the offset after the "))" that closes arithmetic whose text
// starts at p, the text after its "((", or -1 when no such close is on the same
// line. Arithmetic that runs past its line is walked as ordinary shell text, so
// a comment in it is still seen.
func arithEnd(src string, p int) int {
	depth := 2
	for q := p; q < len(src) && src[q] != '\n'; q++ {
		switch src[q] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return q + 1
			}
		}
	}
	return -1
}

// heredocEnd is a heredoc whose body follows the line its operator is on: the
// delimiter after quote removal, and whether "<<-" strips leading tabs.
type heredocEnd struct {
	delim string
	dash  bool
}

// skipHeredocBody returns the offset after the body of h that starts at p: the
// lines up to one that is the delimiter, or the end of the text when none is.
func skipHeredocBody(src string, p int, h heredocEnd) int {
	for p < len(src) {
		line, next := src[p:], len(src)
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line, next = line[:nl], p+nl+1
		}
		line = strings.TrimSuffix(line, "\r")
		if h.dash {
			line = strings.TrimLeft(line, "\t")
		}
		if line == h.delim {
			return next
		}
		p = next
	}
	return len(src)
}

// isShellMeta reports whether c ends a word in shell text: a blank, a newline or
// an operator character. A "#" after one of these starts a comment.
func isShellMeta(c byte) bool {
	return strings.IndexByte(" \t\r\n;&|()<>", c) >= 0
}
