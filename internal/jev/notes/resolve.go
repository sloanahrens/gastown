package notes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The mechanical labels. A line with neither is unlabeled.
const (
	LabelStale   = "stale"
	LabelCurrent = "current"
)

// ErrNoBinary means a binary a text names is not installed, so its subcommands
// and flags cannot be checked.
var ErrNoBinary = errors.New("binary is not installed")

// Repo reads the rig's main tree from the bare clone the submission battery
// owns, and nothing else.
type Repo interface {
	// Sync fetches the clone's branches. A pass calls it once, before its first
	// read.
	Sync(ctx context.Context) error
	// Tree lists every file path at the head of main.
	Tree(ctx context.Context) ([]string, error)
	// HasKey reports whether a config key name appears as a quoted name in a
	// file at the head of main.
	HasKey(ctx context.Context, key string) (bool, error)
}

// Helper returns the --help text of an installed binary, for the subcommand
// path given. It runs the binary on the PATH and nothing else. A binary that
// is not installed is ErrNoBinary.
type Helper interface {
	Help(ctx context.Context, bin string, path ...string) (string, error)
}

// Token is one thing a text names that the label checked.
type Token struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Result is the mechanical label for one text, and what it rests on.
type Result struct {
	// Label is LabelStale, LabelCurrent or "".
	Label string
	// Checked counts the tokens that could be checked.
	Checked int
	// Missing are the paths, subcommands and flags that do not resolve.
	Missing []Token
	// Unresolved are config keys found nowhere in the tree. They do not make a
	// text stale, but they keep it from being labeled current.
	Unresolved []Token
}

// conventionalRoots are top-level directories a path is checked under even
// after the directory itself was removed from the tree.
var conventionalRoots = map[string]bool{
	"internal": true, "cmd": true, "docs": true, "pkg": true,
	"scripts": true, "templates": true, "formulas": true,
}

// Resolver labels texts against the clone's tree and the installed binaries'
// --help. It caches every read for the life of one pass.
type Resolver struct {
	Repo Repo
	Help Helper

	tree  map[string]bool
	roots map[string]bool
	help  map[string]helpDoc
	// noHelp holds the errNoHelp of each command whose help could not be had.
	noHelp map[string]error
	keys   map[string]bool
}

// helpDoc is a parsed --help text.
type helpDoc struct {
	text string
	subs map[string]bool
	// positional is true when the command's usage line takes arguments, so a
	// word after it may be an argument, not a subcommand.
	positional bool
}

// Resolve checks each token a text names. A path is missing from the main
// tree, a subcommand or flag is absent from its binary's --help. A path under
// a top-level directory neither in the tree nor conventional, a command of an
// uninstalled binary, and a flag of a command whose help lists no flags are not
// checked at all.
func (r *Resolver) Resolve(ctx context.Context, t Tokens) (Result, error) {
	var res Result
	if t.Empty() {
		return res, nil
	}
	if len(t.Paths) > 0 {
		if err := r.loadTree(ctx); err != nil {
			return res, err
		}
		for _, p := range t.Paths {
			root, _, _ := strings.Cut(p, "/")
			// A dot directory (.beads, .claude, .runtime) is per-workspace
			// state a note names by where it sits in some checkout, not a
			// path in the tree.
			if strings.HasPrefix(root, ".") || !r.roots[root] && !conventionalRoots[root] {
				continue
			}
			res.Checked++
			if !r.tree[p] {
				res.Missing = append(res.Missing, Token{Kind: KindPath, Text: p})
			}
		}
	}
	for _, c := range t.Commands {
		if err := r.resolveCommand(ctx, c, &res); err != nil {
			return res, err
		}
	}
	for _, k := range t.Keys {
		ok, err := r.hasKey(ctx, k)
		if err != nil {
			return res, err
		}
		res.Checked++
		if !ok {
			res.Unresolved = append(res.Unresolved, Token{Kind: KindConfigKey, Text: k})
		}
	}
	switch {
	case len(res.Missing) > 0:
		res.Label = LabelStale
	case res.Checked > 0 && len(res.Unresolved) == 0:
		res.Label = LabelCurrent
	}
	return res, nil
}

func (r *Resolver) loadTree(ctx context.Context) error {
	if r.tree != nil {
		return nil
	}
	files, err := r.Repo.Tree(ctx)
	if err != nil {
		return err
	}
	r.tree = make(map[string]bool, len(files)*2)
	r.roots = map[string]bool{}
	for _, f := range files {
		if f == "" {
			continue
		}
		r.tree[f] = true
		root, _, _ := strings.Cut(f, "/")
		r.roots[root] = true
		for i := 0; i < len(f); i++ {
			if f[i] == '/' {
				r.tree[f[:i]] = true
			}
		}
	}
	return nil
}

// hasKey checks a config key. A dotted key resolves when every part does.
func (r *Resolver) hasKey(ctx context.Context, key string) (bool, error) {
	for _, seg := range strings.Split(key, ".") {
		if ok, seen := r.keys[seg]; seen {
			if !ok {
				return false, nil
			}
			continue
		}
		ok, err := r.Repo.HasKey(ctx, seg)
		if err != nil {
			return false, err
		}
		if r.keys == nil {
			r.keys = map[string]bool{}
		}
		r.keys[seg] = ok
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// resolveCommand walks a command's words down the --help tree of its binary,
// then checks its flags against the help of the command it reached.
func (r *Resolver) resolveCommand(ctx context.Context, c Command, res *Result) error {
	path := []string{}
	doc, err := r.helpOf(ctx, c.Bin, path)
	if errors.Is(err, errNoHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, w := range c.Words {
		if len(doc.subs) == 0 || (i > 0 && doc.positional) {
			break
		}
		if !doc.subs[w] {
			res.Checked++
			res.Missing = append(res.Missing, Token{Kind: KindCommand, Text: c.Bin + " " + strings.Join(append(append([]string{}, path...), w), " ")})
			return nil
		}
		res.Checked++
		path = append(path, w)
		next, err := r.helpOf(ctx, c.Bin, path)
		if errors.Is(err, errNoHelp) {
			return nil
		}
		if err != nil {
			return err
		}
		// A binary that prints its own usage for every --help (hm does) gives
		// a subcommand no help of its own; what follows is not checked.
		if next.text == doc.text {
			return nil
		}
		doc = next
	}
	if !strings.Contains(doc.text, "--") {
		return nil
	}
	for _, f := range c.Flags {
		res.Checked++
		if !hasFlag(doc.text, f) {
			res.Missing = append(res.Missing, Token{Kind: KindFlag, Text: strings.TrimSpace(c.Bin + " " + strings.Join(path, " ") + " " + f)})
		}
	}
	return nil
}

// errNoHelp means a command's help cannot be had: the binary is not installed
// or its help would not run, which is no evidence either way.
var errNoHelp = errors.New("no help for the command")

// helpOf reads and parses the help of a command path. It returns errNoHelp when
// the help cannot be had, and the context's error when ctx was canceled.
func (r *Resolver) helpOf(ctx context.Context, bin string, path []string) (helpDoc, error) {
	key := bin + " " + strings.Join(path, " ")
	if doc, ok := r.help[key]; ok {
		return doc, nil
	}
	if err, ok := r.noHelp[key]; ok {
		return helpDoc{}, err
	}
	text, err := r.Help.Help(ctx, bin, path...)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return helpDoc{}, cerr
		}
		if r.noHelp == nil {
			r.noHelp = map[string]error{}
		}
		r.noHelp[key] = fmt.Errorf("%w: %s: %v", errNoHelp, key, err)
		return helpDoc{}, r.noHelp[key]
	}
	doc := parseHelp(text, len(path)+1)
	if r.help == nil {
		r.help = map[string]helpDoc{}
	}
	r.help[key] = doc
	return doc, nil
}

var (
	// subLineRe is a command section line: a name, two or more spaces, then
	// its description.
	subLineRe = regexp.MustCompile(`^\s+([a-z][a-z0-9:-]*)\s{2,}\S`)

	// inlineCommandsRe is a one-line command list: "commands: a b c".
	inlineCommandsRe = regexp.MustCompile(`(?m)^commands:\s+(.+)$`)
)

// nonCommandSections are help headings whose indented lines are not
// subcommands.
var nonCommandSections = map[string]bool{
	"usage": true, "aliases": true, "examples": true, "flags": true,
	"global flags": true, "additional help topics": true, "related commands": true,
}

// parseHelp reads the subcommands out of a cobra-style help text, and out of a
// one-line "commands: a b c" list. depth is the number of words in the command
// path including the binary, so the usage line's trailing arguments can be told
// from "[flags]" and "[command]".
func parseHelp(text string, depth int) helpDoc {
	doc := helpDoc{text: text, subs: map[string]bool{}}
	section := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			if strings.HasSuffix(strings.TrimSpace(line), ":") {
				section = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(line), ":"))
			} else {
				section = ""
			}
			continue
		}
		if section == "usage" {
			fields := strings.Fields(line)
			if len(fields) > depth {
				for _, f := range fields[depth:] {
					if f != "[flags]" && f != "[command]" && f != "[options]" {
						doc.positional = true
					}
				}
			}
			continue
		}
		if section == "" || nonCommandSections[section] {
			continue
		}
		if m := subLineRe.FindStringSubmatch(line); m != nil {
			doc.subs[m[1]] = true
		}
	}
	if m := inlineCommandsRe.FindStringSubmatch(text); m != nil {
		for _, w := range strings.Fields(m[1]) {
			doc.subs[strings.Trim(w, ",")] = true
		}
	}
	return doc
}

// hasFlag reports whether help names the long flag as a whole word.
func hasFlag(help, flag string) bool {
	for i := 0; ; {
		j := strings.Index(help[i:], flag)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(flag)
		before := j == 0 || !isFlagChar(help[j-1])
		after := end >= len(help) || !isFlagChar(help[end])
		if before && after {
			return true
		}
		i = j + 1
	}
}

func isFlagChar(b byte) bool {
	return b == '-' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
