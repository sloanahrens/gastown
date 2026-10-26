package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// WithWorkDir runs bd from dir instead of the wrapper's default directory.
// A pinned wrapper still targets its own beads directory. The formula verbs
// need it: bd searches formula directories from its cwd.
func WithWorkDir(dir string) Option {
	return func(f *beadsFields) { f.workDir = dir }
}

// Cook runs bd's formula engine on formula with vars ("key=value") and
// returns the step tree bd printed (bd cook). A failure's message is bd's
// own: the machine envelope's error message, else what bd wrote to stderr.
func (b *Beads) Cook(formula string, vars []string) ([]byte, error) {
	out, err := b.run(append([]string{"cook", formula}, varArgs(vars)...)...)
	if err != nil {
		return nil, withBDMessage(err)
	}
	return out, nil
}

// Bond spawns proto as an ephemeral molecule attached to beadID (bd mol
// bond --ephemeral) and returns bd's JSON answer, which names the spawned
// root. Failures read as Cook's do.
func (b *Beads) Bond(proto, beadID string, vars []string) ([]byte, error) {
	args := append([]string{"mol", "bond", proto, beadID, "--json", "--ephemeral"}, varArgs(vars)...)
	out, err := b.run(args...)
	if err != nil {
		return nil, withBDMessage(err)
	}
	return out, nil
}

// FormulaVerbs is the formula-verb seam gt formula prints through: the list
// and one formula, each as bd's JSON payload (FormulaList, FormulaShow) or as
// its prose (FormulaListText, FormulaShowText). It is not Client because a
// formula lives in files, not the issue store, so a tracker-backed Client has
// no answer (gt-p85pw.11).
type FormulaVerbs interface {
	FormulaList() ([]byte, error)
	FormulaListText() ([]byte, error)
	FormulaShow(formula string) ([]byte, error)
	FormulaShowText(formula string) ([]byte, error)
}

var _ FormulaVerbs = (*Beads)(nil)

// FormulaShow returns bd's JSON payload for formula. A name no formula has is
// an error, so a nil error means the formula exists: bd normally exits 1
// naming the name, and an empty or null answer is read as the same case.
//
// A failure keeps bd's own text rather than the envelope's message: bd puts
// the reason on stderr (`formula not found: ...` and its search paths) and the
// envelope only says "details are on stderr" (gt-p85pw.11).
func (b *Beads) FormulaShow(formula string) ([]byte, error) {
	out, err := b.run("formula", "show", "--", formula)
	if err != nil {
		return nil, err
	}
	return requireFormula(formula, out)
}

// FormulaShowText returns bd formula show's prose for formula, the form the
// operator's terminal wants; bd writes it only outside machine mode. A name no
// formula has is an error, as FormulaShow's is.
func (b *Beads) FormulaShowText(formula string) ([]byte, error) {
	out, err := b.runText("formula", "show", "--", formula)
	if err != nil {
		return nil, err
	}
	return requireFormula(formula, out)
}

// FormulaList returns bd formula list's JSON payload (bd formula list
// --json), the form FormulaShow answers one formula in. No formulas is bd's
// null, not an error.
func (b *Beads) FormulaList() ([]byte, error) {
	out, err := b.run("formula", "list", "--json")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FormulaListText returns bd formula list's prose, the form the operator's
// terminal wants; bd writes it only outside machine mode.
func (b *Beads) FormulaListText() ([]byte, error) {
	out, err := b.runText("formula", "list")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// requireFormula is out when bd printed a formula and an error when it printed
// none: the empty answer, or the JSON null, a bd that exits 0 for a name it
// did not find leaves. Both show forms pass through it, so every
// FormulaVerbs answer means "this formula exists" when the error is nil.
func requireFormula(formula string, out []byte) ([]byte, error) {
	if s := strings.TrimSpace(string(out)); s == "" || s == "null" {
		return nil, fmt.Errorf("formula %q: bd printed no formula", formula)
	}
	return out, nil
}

// Wisp instantiates formula with vars as an ephemeral wisp (bd mol wisp)
// and returns bd's JSON answer, which names the wisp's root. Failures read
// as Cook's do.
func (b *Beads) Wisp(formula string, vars []string) ([]byte, error) {
	args := append([]string{"mol", "wisp", formula}, varArgs(vars)...)
	out, err := b.run(append(args, "--json")...)
	if err != nil {
		return nil, withBDMessage(err)
	}
	return out, nil
}

// varArgs is vars as bd's repeated --var flags.
func varArgs(vars []string) []string {
	args := make([]string, 0, 2*len(vars))
	for _, v := range vars {
		args = append(args, "--var", v)
	}
	return args
}

// bdMessageError is a bd failure whose message is the one bd reported.
type bdMessageError struct {
	msg string
	err error
}

func (e *bdMessageError) Error() string { return e.msg }
func (e *bdMessageError) Unwrap() error { return e.err }

// withBDMessage is err reading as the message of the error envelope bd left
// on stdout (machine mode, or the legacy {"error": "..."} of --json); err
// itself when there is none.
func withBDMessage(err error) error {
	var ue *unavailableError
	if !errors.As(err, &ue) {
		return err
	}
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(ue.stdout), &payload) != nil || len(payload.Error) == 0 {
		return err
	}
	var typed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload.Error, &typed) == nil && typed.Message != "" {
		return &bdMessageError{msg: typed.Message, err: err}
	}
	var legacy string
	if json.Unmarshal(payload.Error, &legacy) == nil && legacy != "" {
		return &bdMessageError{msg: legacy, err: err}
	}
	return err
}
