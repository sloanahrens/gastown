package bdgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// The policy answers one question: may this caller run this bd call. It is the
// verb allowlist in deploy/bd/verbs.json, the class each verb belongs to, the
// flags refused even for an allowed verb, and (env.go) the environment the
// gateway forwards. The design is in docs/design/bd-container.md ("The verb
// allowlist", "Environment allowlist"); the gateway command (G6 in that
// design) will load the file and map the errors below to HTTP 403.
//
// The file is the authority, never a default: a verb it does not list is
// refused by name, and a token class or verb class the code does not know is
// refused too, so a typo in the file narrows the surface rather than widening
// it. The verb comes from argv read the way bd's own root parse reads it
// (scanArgs), so the class check applies to the verb bd will run.

// VerbClass is what running a bd verb may do to the store.
type VerbClass string

const (
	// ClassRead reads the store and writes no bead.
	ClassRead VerbClass = "read"
	// ClassWrite changes beads, which is what gt's daemon writes do.
	ClassWrite VerbClass = "write"
	// ClassAdmin changes the store's configuration. Only an operator token may
	// run these.
	ClassAdmin VerbClass = "admin"
)

// TokenClass is the class a gateway token authenticates its caller as. The
// token directory (G3) names each file `<class>.<caller>.token` with one of
// these values and the caller it speaks as.
type TokenClass string

const (
	// TokenRead is the dashboard and monitors: the read verbs only.
	TokenRead TokenClass = "read"
	// TokenDaemon is the daemon and the gt it wraps: read and write verbs.
	TokenDaemon TokenClass = "daemon"
	// TokenOperator is the Mac wrapper: everything daemon may, plus admin.
	TokenOperator TokenClass = "operator"
)

// The policy's named refusals. Each is a 403 at the HTTP layer, and the caller
// matches on the error rather than on message text.
var (
	// ErrVerbNotAllowed is an argv whose verb the file does not list.
	ErrVerbNotAllowed = errors.New("bdgateway: verb not allowlisted")
	// ErrClassDenied is a listed verb whose class the caller's token class may
	// not run.
	ErrClassDenied = errors.New("bdgateway: token class may not run this verb")
	// ErrFlagRefused is a flag the file refuses, on any verb or on this one.
	ErrFlagRefused = errors.New("bdgateway: flag refused")
	// ErrUnknownTokenClass is a token class the gateway does not know. It
	// denies rather than defaulting to a class.
	ErrUnknownTokenClass = errors.New("bdgateway: unknown token class")
	// ErrBadVerbsFile is a verbs file ParsePolicy refuses: malformed JSON, a
	// class or name outside its spelling, a duplicate, or an empty list.
	ErrBadVerbsFile = errors.New("bdgateway: bad verbs file")
	// ErrAmbiguousVerb is argv whose verb the policy cannot name the way bd's
	// own parse would, because a flag before the verb is one the policy cannot
	// place. It refuses rather than guess: a flag it read as boolean where bd
	// reads a value (or the reverse) would class-check one verb while bd runs
	// another.
	ErrAmbiguousVerb = errors.New("bdgateway: cannot tell which verb these arguments run")
)

// maxPathWords bounds a verb path. bd's deepest verb gt calls is three words
// ("mol wisp list"), and the resolver tries at most this many words.
const maxPathWords = 3

// flagKind is how bd's root command reads a flag's argument.
type flagKind int

const (
	// flagBool takes no value, so the word after it is a command word.
	flagBool flagKind = iota
	// flagValue takes the next argument as its value, so that argument is
	// never a command word.
	flagValue
)

// bdRootFlags is every flag bd's root command knows, by its spelling on a
// command line, with whether it consumes the next argument. bd dispatches on
// the words left after its root flags are removed, so the resolver has to
// remove exactly the words bd does: reading a value-taking flag as boolean (or
// the reverse) shifts the verb words and can class-check one verb while bd runs
// another.
//
// The long names are bd's persistent flags in
// internal/cmdtree/bd-command-tree.json and the value-taking set is the one the
// command-tree scan uses (cmdtree.BdGlobalValueFlagNames), which is the only
// description of bd's root flags this repository holds. -C is --directory's
// shorthand, the one short flag listed there; another short token is refused
// rather than guessed, because the tree records no shorthands.
// TestRootFlagsMatchCommandTree holds this map to both lists.
var bdRootFlags = map[string]flagKind{
	"--actor":            flagValue,
	"--cpu-profile":      flagValue,
	"--database":         flagValue,
	"--db":               flagValue,
	"-C":                 flagValue,
	"--directory":        flagValue,
	"--dolt-auto-commit": flagValue,
	"--format":           flagValue,
	"--machine":          flagValue,
	"--mem-profile":      flagValue,

	"--global":             flagBool,
	"--ignore-schema-skew": flagBool,
	"--json":               flagBool,
	"--no-color":           flagBool,
	"--quiet":              flagBool,
	"--readonly":           flagBool,
	"--sandbox":            flagBool,
	"--verbose":            flagBool,
}

// Verb is one allowlisted verb: the path words that name it, the class that may
// run it, and flags refused in addition to the file's global list.
type Verb struct {
	// Path is the verb's words in bd's own spelling, one to maxPathWords long.
	Path []string
	// Class is what running the verb may do.
	Class VerbClass
	// FlagsRefused are flag names refused for this verb only, without dashes.
	FlagsRefused []string
}

// Name returns the verb path as one string, for logs and error messages.
func (v Verb) Name() string { return strings.Join(v.Path, " ") }

// Policy is a loaded verbs file: the allowlist, its classes and the refused
// flags. It is read-only after loading.
type Policy struct {
	verbs        map[string]Verb
	refusedFlags map[string]bool
}

// tokenClassRank orders token classes by the highest verb class they may run.
// rank 1 is read, 2 is write, 3 is admin.
func tokenClassRank(c TokenClass) (int, bool) {
	switch c {
	case TokenRead:
		return 1, true
	case TokenDaemon:
		return 2, true
	case TokenOperator:
		return 3, true
	}
	return 0, false
}

// verbClassRank orders verb classes the same way.
func verbClassRank(c VerbClass) (int, bool) {
	switch c {
	case ClassRead:
		return 1, true
	case ClassWrite:
		return 2, true
	case ClassAdmin:
		return 3, true
	}
	return 0, false
}

// Allowed reports whether a caller of class may run a verb of verbClass: read
// may run read verbs, daemon read and write, operator all three. An unknown
// class on either side is refused, never defaulted.
func Allowed(class TokenClass, verbClass VerbClass) bool {
	have, ok := tokenClassRank(class)
	if !ok {
		return false
	}
	need, ok := verbClassRank(verbClass)
	if !ok {
		return false
	}
	return have >= need
}

// policyDoc is the JSON document. Unknown fields are refused so a misspelled
// field (a "flags_refused" where "refused_flags" belongs, say) cannot load as
// an empty allowlist.
type policyDoc struct {
	Source       string    `json:"source"`
	RefusedFlags []string  `json:"refused_flags"`
	Verbs        []verbDoc `json:"verbs"`
}

// verbDoc is one verb row of the file.
type verbDoc struct {
	Path         string   `json:"path"`
	Class        string   `json:"class"`
	FlagsRefused []string `json:"flags_refused"`
}

var (
	// pathWord is one word of a verb path: bd's command words are lowercase
	// letters, digits and dashes.
	pathWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	// bareFlag is a flag name without its dashes. A single uppercase letter is
	// allowed because bd's -C shorthand is refused with its long form.
	bareFlag = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)
)

// LoadPolicyFile reads one verbs file, the repository's deploy/bd/verbs.json
// mounted into the gateway image.
func LoadPolicyFile(path string) (*Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("bdgateway: open verbs file: %w", err)
	}
	defer f.Close()
	p, err := ParsePolicy(f)
	if err != nil {
		return nil, fmt.Errorf("bdgateway: %s: %w", path, err)
	}
	return p, nil
}

// ParsePolicy reads a verbs file. It refuses a document it cannot trust
// completely: malformed JSON, trailing data, an empty verb list or flag list, a
// path or flag name outside its spelling, an unknown class, or a duplicate.
func ParsePolicy(r io.Reader) (*Policy, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var doc policyDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadVerbsFile, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after the document", ErrBadVerbsFile)
	}
	if len(doc.Verbs) == 0 {
		return nil, fmt.Errorf("%w: no verbs", ErrBadVerbsFile)
	}
	if len(doc.RefusedFlags) == 0 {
		// A file that refuses no flag silently permits --db and --sandbox, so an
		// empty list is a configuration mistake, not a policy.
		return nil, fmt.Errorf("%w: no refused flags", ErrBadVerbsFile)
	}

	p := &Policy{
		verbs:        make(map[string]Verb, len(doc.Verbs)),
		refusedFlags: make(map[string]bool, len(doc.RefusedFlags)),
	}
	for _, name := range doc.RefusedFlags {
		if !bareFlag.MatchString(name) {
			return nil, fmt.Errorf("%w: refused flag %q is not a name without dashes", ErrBadVerbsFile, name)
		}
		if p.refusedFlags[name] {
			return nil, fmt.Errorf("%w: refused flag %q listed twice", ErrBadVerbsFile, name)
		}
		p.refusedFlags[name] = true
	}
	for _, row := range doc.Verbs {
		path := strings.Fields(row.Path)
		if len(path) == 0 || len(path) > maxPathWords {
			return nil, fmt.Errorf("%w: verb path %q is not one to %d words", ErrBadVerbsFile, row.Path, maxPathWords)
		}
		for _, word := range path {
			if !pathWord.MatchString(word) {
				return nil, fmt.Errorf("%w: verb path %q has a word that is not a command word", ErrBadVerbsFile, row.Path)
			}
		}
		class, err := parseVerbClass(row.Class)
		if err != nil {
			return nil, fmt.Errorf("%w: verb %q: %v", ErrBadVerbsFile, row.Path, err)
		}
		name := strings.Join(path, " ")
		if _, ok := p.verbs[name]; ok {
			return nil, fmt.Errorf("%w: verb %q listed twice", ErrBadVerbsFile, name)
		}
		v := Verb{Path: path, Class: class, FlagsRefused: append([]string(nil), row.FlagsRefused...)}
		for _, flag := range v.FlagsRefused {
			if !bareFlag.MatchString(flag) {
				return nil, fmt.Errorf("%w: verb %q: refused flag %q is not a name without dashes", ErrBadVerbsFile, name, flag)
			}
		}
		p.verbs[name] = v
	}
	return p, nil
}

// parseVerbClass reads a verb class name. The empty string is not read as a
// class: a row without one is refused.
func parseVerbClass(s string) (VerbClass, error) {
	class := VerbClass(s)
	if _, ok := verbClassRank(class); !ok {
		return "", fmt.Errorf("unknown class %q (want read, write or admin)", s)
	}
	return class, nil
}

// Authorize checks one call: argv names a listed verb, class may run that
// verb's class, and no flag on argv is refused for the verb or for every verb.
// It returns the matched verb so the caller can log its path and class.
//
// argv is bd's arguments, never "bd" and never a shell string. One scan
// (scanArgs) reads both the verb words and the flags, so the class check and
// the flag check cannot disagree about which tokens are flags.
func (p *Policy) Authorize(class TokenClass, argv []string) (Verb, error) {
	scan, err := p.scanArgs(argv)
	if err != nil {
		return Verb{}, err
	}
	name := p.verbName(scan.words)
	v, ok := p.verbs[name]
	if !ok {
		return Verb{}, fmt.Errorf("%w: %q", ErrVerbNotAllowed, name)
	}
	if !Allowed(class, v.Class) {
		return Verb{}, fmt.Errorf("%w: %s token may not run %s (%s)", ErrClassDenied, class, v.Name(), v.Class)
	}
	if flag, ok := p.refusedFlag(v, scan.flags); ok {
		return Verb{}, fmt.Errorf("%w: %s on %s", ErrFlagRefused, flag.spelling, v.Name())
	}
	return v, nil
}

// scanResult is what one scanArgs pass read from argv. words are the command
// words, flags the flags in option position, and operands every positional
// argument the command words do not cover, including those after "--". The
// call log reads operands for the bead ids among them (gt-itjuwu).
type scanResult struct {
	words    []string
	flags    []flagToken
	operands []string
}

// scanArgs reads argv once the way bd's root parse does, returning the command
// words, the flags in option position and the operands, or ErrAmbiguousVerb
// when a token cannot be placed. One scan decides all three, because a token
// one check reads as a value the other must not read as a flag: in "--format --
// --db=/x show", --format takes "--" as its value and --db is a flag bd acts
// on.
//
// The rules are cobra's stripFlags over the root flag set. "--" in option
// position ends the options: the rest of argv is positional, never a flag, and
// names no command word. A known value-taking flag takes the next argument as
// its value, even when that argument is "--" or starts with "-". An attached
// value ("--db=beads", "-Cdir") is the token's own, so it consumes nothing more.
// A known boolean flag consumes nothing. Once no listed verb path extends the
// words read, a later word cannot change the verb, but every later flag still
// counts for the refusal.
//
// A flag outside bdRootFlags is a command flag whose arity the tree does not
// record. Before the verb is settled, or with a following token that starts
// with "-", its value cannot be told from a flag, so the call is refused.
func (p *Policy) scanArgs(argv []string) (scanResult, error) {
	var res scanResult
	res.words = make([]string, 0, maxPathWords)
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			// The rest is positional, never a flag and never a command word.
			res.operands = append(res.operands, argv[i+1:]...)
			break
		}
		if arg == "" || arg == "-" {
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			if p.extends(res.words) {
				res.words = append(res.words, arg)
			} else {
				res.operands = append(res.operands, arg)
			}
			continue
		}
		res.flags = append(res.flags, flagNames(arg)...)
		kind, known, attached := rootFlag(arg)
		switch {
		case attached:
			// The value is glued to the token; it consumes nothing after it.
		case known && kind == flagValue:
			i++ // the value is the next argument, whatever it looks like
		case known:
			// A boolean root flag: the next argument is not its value.
		case i+1 < len(argv) && strings.HasPrefix(argv[i+1], "-"):
			return scanResult{}, ambiguousFlag(arg)
		case p.extends(res.words):
			return scanResult{}, ambiguousFlag(arg)
		}
	}
	return res, nil
}

// ambiguousFlag is the refusal for a flag the scan cannot place.
func ambiguousFlag(arg string) error {
	return fmt.Errorf("%w: %q is not a bd root flag the policy can place", ErrAmbiguousVerb, arg)
}

// extends reports whether a listed verb path has words as a strict prefix, so
// a word after them can still name a longer verb.
func (p *Policy) extends(words []string) bool {
	if len(words) >= maxPathWords {
		return false
	}
	for _, v := range p.verbs {
		if len(v.Path) > len(words) && slices.Equal(v.Path[:len(words)], words) {
			return true
		}
	}
	return false
}

// rootFlag classifies one dash-prefixed argument. known is false for a flag
// outside bdRootFlags; attached reports that the token carries its value
// already ("--db=beads", "-Cdir").
func rootFlag(arg string) (kind flagKind, known, attached bool) {
	if strings.HasPrefix(arg, "--") {
		name, _, attached := strings.Cut(arg, "=")
		kind, ok := bdRootFlags[name]
		if !ok {
			// The flag is unknown, but a token holding "=" carries its value
			// whatever the flag's arity.
			return 0, false, attached
		}
		return kind, true, attached
	}
	switch {
	case arg == "-C":
		return flagValue, true, false
	case strings.HasPrefix(arg, "-C"):
		return flagValue, true, true
	}
	return 0, false, false
}

// verbName returns the listed verb path words name, or the first word to name
// in a refusal when nothing matches. The longest listed prefix wins, so "mol
// wisp list" resolves to its read row while "mol wisp <formula>" falls back to
// the write row for "mol wisp", and "comments gt-abc" resolves to "comments"
// because "comments gt-abc" is not listed.
func (p *Policy) verbName(words []string) string {
	for n := len(words); n >= 1; n-- {
		name := strings.Join(words[:n], " ")
		if _, ok := p.verbs[name]; ok {
			return name
		}
	}
	if len(words) == 0 {
		return ""
	}
	return words[0]
}

// refusedFlag returns the first flag in flags that is refused for v or for
// every verb, with its spelling as written, or false when none is.
func (p *Policy) refusedFlag(v Verb, flags []flagToken) (flagToken, bool) {
	for _, flag := range flags {
		if p.refusedFlags[flag.name] || slices.Contains(v.FlagsRefused, flag.name) {
			return flag, true
		}
	}
	return flagToken{}, false
}

// flagToken is one flag a dash-prefixed argument asks for: its name without
// dashes, and the spelling it appeared as, so a refusal names -C rather than
// --C.
type flagToken struct {
	name     string
	spelling string
	// clustered marks a shorthand that follows another in the same token:
	// pflag groups boolean shorthands, so "-qv" names both, but when the first
	// takes a value the rest of the token is that value, so the letters of
	// "-nfoo" are foo. The policy checks every clustered name so a refused flag
	// is never missed; the call log keeps only the first, so a value never
	// reaches a line as flag names (gt-itjuwu).
	clustered bool
}

// flagNames returns the flags a dash-prefixed token in option position asks
// for. A long flag names itself up to "=": "--db=beads" and "--db" both name
// "db" as --db. A single dash starts shorthands, which pflag may group, so
// each letter is a name until one that takes a value ends the flags, the rest
// of the token being that value: "-qC" names q and C, "-Cdir" names C only.
// Every name after the first in the token is marked clustered.
func flagNames(arg string) []flagToken {
	if len(arg) < 2 || arg[0] != '-' {
		return nil
	}
	if strings.HasPrefix(arg, "--") {
		name := arg[2:]
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		return []flagToken{{name: name, spelling: "--" + name}}
	}
	var tokens []flagToken
	for i := 1; i < len(arg); i++ {
		c := arg[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') {
			break
		}
		name := string(c)
		tokens = append(tokens, flagToken{name: name, spelling: "-" + name, clustered: len(tokens) > 0})
		if bdRootFlags["-"+name] == flagValue {
			break // the rest of the token is this shorthand's value
		}
	}
	return tokens
}
