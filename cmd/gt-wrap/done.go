package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

const (
	// gtTool is the tool named gt: its done call takes the worktree facts, and
	// its host command set runs on the Mac.
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
// both leaves git unasked. A directory that git says is outside any repository
// adds nothing and says nothing, and gt done refuses in the pod. A detached HEAD
// adds nothing either, and says so on stderr, since the pod's refusal would
// otherwise leave the caller to guess why no facts came with the call.
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
		if facts.Detached {
			fmt.Fprintf(s.stderr, "gt-wrap: gt done: %s is on a detached HEAD, so no --branch and --head were added; run git switch -c crew/<agent>/<bead> there, or pass --bead <id> --branch <b> --head <sha> yourself\n", dir)
		}
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
