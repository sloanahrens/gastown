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
// it.

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
// token directory (G3) names each file <class>.token with one of these values.
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
)

// maxPathWords bounds a verb path. bd's deepest verb gt calls is three words
// ("mol wisp list"), and the resolver tries at most this many words.
const maxPathWords = 3

// leadingValueFlags are bd's persistent flags that take the next argument as
// their value, so the verb scan does not read that value as the verb. They are
// the value-taking persistent flags of bd 1.2.2 in
// internal/cmdtree/bd-command-tree.json; TestLeadingValueFlagsMatchCommandTree
// holds the two lists equal, so a bd upgrade that changes one fails the gate.
var leadingValueFlags = map[string]bool{
	"--actor":            true,
	"--cpu-profile":      true,
	"--database":         true,
	"--db":               true,
	"-C":                 true,
	"--directory":        true,
	"--dolt-auto-commit": true,
	"--format":           true,
	"--machine":          true,
	"--mem-profile":      true,
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
// argv is bd's arguments, never "bd" and never a shell string. Leading bd
// persistent flags are read past, and nothing after "--" is a flag.
func (p *Policy) Authorize(class TokenClass, argv []string) (Verb, error) {
	name := p.verbName(argv)
	v, ok := p.verbs[name]
	if !ok {
		return Verb{}, fmt.Errorf("%w: %q", ErrVerbNotAllowed, name)
	}
	if !Allowed(class, v.Class) {
		return Verb{}, fmt.Errorf("%w: %s token may not run %s (%s)", ErrClassDenied, class, v.Name(), v.Class)
	}
	if flag, ok := p.refusedFlag(v, argv); ok {
		return Verb{}, fmt.Errorf("%w: --%s on %s", ErrFlagRefused, flag, v.Name())
	}
	return v, nil
}

// verbName returns the listed verb path argv names, or the first word to name
// in a refusal when nothing matches. The longest listed prefix wins, so "mol
// wisp list" resolves to its read row while "mol wisp <formula>" falls back to
// the write row for "mol wisp", and "comments gt-abc" resolves to "comments"
// because "comments gt-abc" is not listed.
func (p *Policy) verbName(argv []string) string {
	words := nonFlagWords(argv, maxPathWords)
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

// nonFlagWords returns up to limit words of argv that are not flags, skipping
// the value of a bd persistent flag that takes one. It stops at "--": nothing
// after it is a flag or, in bd's parse, a command word.
func nonFlagWords(argv []string, limit int) []string {
	words := make([]string, 0, limit)
	for i := 0; i < len(argv) && len(words) < limit; i++ {
		arg := argv[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "-") {
			if leadingValueFlags[arg] {
				i++ // the next argument is this flag's value
			}
			continue
		}
		words = append(words, arg)
	}
	return words
}

// refusedFlag returns the first flag on argv that is refused for v or for every
// verb, or false when none is. Nothing after "--" is a flag. The scan does not
// know which flags take free text, so a value spelled like a refused flag (a
// title of "--db", say) is refused too; the call is retryable with the value
// attached (--title=--db), and refusing is the safe side.
func (p *Policy) refusedFlag(v Verb, argv []string) (string, bool) {
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			break
		}
		for _, name := range flagNames(arg) {
			if p.refusedFlags[name] || slices.Contains(v.FlagsRefused, name) {
				return name, true
			}
		}
	}
	return "", false
}

// flagNames returns the flag names a dash-prefixed token asks for, without
// their dashes or value: "--db=beads" and "--db" both name "db". A single dash
// starts shorthands, which pflag may group, so every leading letter is a name:
// "-C/town" names "C" and "-qC" names "q" and "C". A bare "-" is stdin, not a
// flag, and a token with no letter is not one either.
func flagNames(arg string) []string {
	if len(arg) < 2 || arg[0] != '-' {
		return nil
	}
	if strings.HasPrefix(arg, "--") {
		name := arg[2:]
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		return []string{name}
	}
	// The letters run until the first non-letter, which starts an attached
	// value, or an "=".
	var names []string
	for i := 1; i < len(arg); i++ {
		c := arg[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') {
			break
		}
		names = append(names, string(c))
	}
	return names
}
