package main

import "io"

// sweep runs the whole sweep of the swept commit's tree (gt-nqrvia): it reads
// the work tree out of args, builds the tree's cmd/tier-sweep into a temporary
// directory, runs it in the work tree with args, and removes the directory after
// the run. The build and the run both start from Environ with overrides added.
// It has the signature of Deps.Sweep. A refusal from treeRoot returns 1 and
// builds nothing. A build that does not exit 0 returns 1 and runs nothing; there
// is no fallback to the compiled-in sweep. Otherwise it returns the run's status.
func (t treeSweep) sweep(args []string, overrides map[string]string, stdout, stderr io.Writer) int {
	work, ok := treeRoot(args, stderr)
	if !ok {
		return 1
	}
	env := environWith(t.Environ, overrides)
	bin, remove, ok := t.build(work, env, stderr)
	if !ok {
		return 1
	}
	defer remove()
	return t.runBuilt(bin, work, args, env, stdout, stderr)
}
