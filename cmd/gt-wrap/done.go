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
// A flag the caller already passed is kept as given. A directory that is not a
// worktree, or a detached HEAD, adds nothing, and gt done refuses in the pod.
func withWorktreeFacts(name string, args []string, dir string, s surface) ([]string, error) {
	if name != gtTool || len(args) == 0 || args[0] != doneSubcommand {
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

// hasFlag reports whether args carries flag, as --flag or --flag=value.
func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
