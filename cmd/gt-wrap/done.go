package main

import (
	"errors"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

const (
	// gtTool is the tool whose done subcommand takes the worktree facts.
	gtTool = "gt"
	// doneSubcommand is the first argument that selects gt done.
	doneSubcommand = "done"
	// branchFlag and headFlag are the gt done flags that carry the worktree's facts.
	branchFlag = "--branch"
	headFlag   = "--head"
)

// withWorktreeFacts appends the branch and HEAD of the caller's worktree to a
// gt done call, since the pod cannot read the Mac's tree. Only gt done is changed.
// A flag the caller already passed is kept as given, and a caller who passed
// both leaves git unasked. A directory that git says is outside any repository,
// or a detached HEAD, adds nothing, and gt done refuses in the pod.
func withWorktreeFacts(name string, args []string, dir string, s surface) ([]string, error) {
	if name != gtTool || len(args) == 0 || args[0] != doneSubcommand {
		return args, nil
	}
	if hasFlag(args, branchFlag) && hasFlag(args, headFlag) {
		return args, nil
	}
	if s.git == nil {
		return nil, errors.New("gtwrap: no git runner is wired, so gt done cannot read the worktree's branch and HEAD")
	}
	facts, ok, err := gtwrap.ReadWorktreeFacts(s.git, dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return args, nil
	}
	out := slices.Clone(args)
	if !hasFlag(args, branchFlag) {
		out = append(out, branchFlag, facts.Branch)
	}
	if !hasFlag(args, headFlag) {
		out = append(out, headFlag, facts.Head)
	}
	return out, nil
}

// doneValueFlags are the gt done flags that take their value from the next
// argument, which can look like a flag ("--bead --branch" names a bead called
// --branch). Their values are skipped when looking for a flag.
var doneValueFlags = map[string]bool{
	"--status": true, "--issue": true, "--cleanup-status": true, "--target": true,
	"--bead": true, "--branch": true, "--head": true, "--finding": true,
}

// hasFlag reports whether args, a gt done call whose first element is the
// subcommand, carries flag in flag position, as --flag or --flag=value. The
// value of a flag that takes one is not in flag position, and nothing after
// "--" is a flag.
func hasFlag(args []string, flag string) bool {
	for i := 1; i < len(args); i++ {
		if args[i] == "--" {
			return false
		}
		name, _, inline := strings.Cut(args[i], "=")
		if name == flag {
			return true
		}
		if !inline && doneValueFlags[name] {
			i++
		}
	}
	return false
}
