package beads

import (
	"bytes"
	"encoding/json"
	"errors"
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

// FormulaShow returns what bd formula show prints for formula. bd can exit
// 0 for a formula it did not find, printing nothing (or a JSON null), so a
// caller checks the output too.
func (b *Beads) FormulaShow(formula string) ([]byte, error) {
	out, err := b.run("formula", "show", formula)
	if err != nil {
		return nil, withBDMessage(err)
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
