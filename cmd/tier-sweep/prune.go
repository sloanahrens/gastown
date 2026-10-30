package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

// pruneCommand is the first argument that selects the prune subcommand. Every
// other command line is the sweep's, unchanged (gt-g6xz7).
const pruneCommand = "prune"

// pruneErrorExit is the code for a prune that could not read the directory or
// could not delete a directory it listed. An empty list is not this: a dry run
// that found nothing to remove succeeded. Failing open here would tell the
// operator a removal happened that did not.
const pruneErrorExit = 1

// pruneUsage is printed after a refusal, the one time a caller needs the flags.
const pruneUsage = "usage: " + tiersweep.Name + " prune [--older-than 48h] [--dir DIR] [--apply]"

// pruneOptions is one prune command line.
type pruneOptions struct {
	// dir is the directory to list, and an empty one takes TMPDIR as a sweep
	// takes it, so the two agree on where the log directories are.
	dir string
	// maxAge is the age a directory must exceed to be listed.
	maxAge time.Duration
	// apply deletes the listed directories. Without it the prune only lists
	// them: a directory goes once an operator has read its name (gt-g6xz7).
	apply bool
}

// runPrune lists the old tier-sweep directories under the chosen directory
// and, with --apply, deletes them. Without --apply it deletes nothing, so the
// operator reads one run's output before the overseer repeats it with --apply.
func runPrune(args []string, stdout, stderr io.Writer, d tiersweep.Deps) int {
	opts, err := parsePruneArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n%s\n", tiersweep.Name, err, pruneUsage)
		return tiersweep.UsageExit
	}
	host := d.Host
	// A host missing a call it needs is a refusal, not a fallback to os: the
	// fallback would list and delete on whatever machine the command runs on.
	if host.ReadDir == nil || host.Lstat == nil ||
		(opts.apply && host.RemoveAll == nil) || (opts.dir == "" && host.Env == nil) {
		fmt.Fprintf(stderr, "%s: prune: the host is missing a call it needs to list, describe and delete directories\n", tiersweep.Name)
		return pruneErrorExit
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	dir := opts.dir
	if dir == "" {
		dir = tempDir(host)
	}
	// One clock reading for the whole command: the list and the deletion after
	// it must judge an age the same way.
	at := now()
	candidates, err := tiersweep.PruneCandidates(host, dir, at, opts.maxAge)
	if err != nil {
		fmt.Fprintf(stderr, "%s: prune %s: %v\n", tiersweep.Name, dir, err)
		return pruneErrorExit
	}
	size := int64(0)
	for _, c := range candidates {
		size += c.Size
		fmt.Fprintf(stdout, "%s: prune %s age %s size %d bytes\n", tiersweep.Name, c.Path, c.Age, c.Size)
	}
	summary := fmt.Sprintf("prune %s, %d bytes, older than %s in %s",
		count(len(candidates)), size, opts.maxAge, dir)
	if !opts.apply {
		fmt.Fprintf(stdout, "%s: %s: nothing removed (--apply deletes them)\n", tiersweep.Name, summary)
		return 0
	}
	removed, err := tiersweep.Prune(host, dir, at, opts.maxAge)
	code := 0
	if err != nil {
		fmt.Fprintf(stderr, "%s: prune %s: %v\n", tiersweep.Name, dir, err)
		code = pruneErrorExit
	}
	// The list is what an operator approved, so a deletion of anything else is
	// a failure as much as a listed directory left behind: the count in the
	// total line is the listed ones that went, never len(removed).
	notRemoved, extra := missing(candidates, removed), strays(candidates, removed)
	for _, path := range notRemoved {
		fmt.Fprintf(stderr, "%s: prune %s: not removed\n", tiersweep.Name, path)
	}
	for _, path := range extra {
		fmt.Fprintf(stderr, "%s: prune %s: removed although the list above did not name it\n", tiersweep.Name, path)
	}
	if len(notRemoved) > 0 || len(extra) > 0 {
		code = pruneErrorExit
	}
	fmt.Fprintf(stdout, "%s: %s: removed %d of %d\n", tiersweep.Name, summary, len(candidates)-len(notRemoved), len(candidates))
	return code
}

// tempDir is the directory a command line that names none lists, resolved as a
// sweep resolves it.
func tempDir(host tiersweep.Host) string {
	if dir := host.Env("TMPDIR"); dir != "" {
		return dir
	}
	return tiersweep.DefaultTempDir
}

// missing is the listed paths Prune did not report as removed, in list order.
func missing(candidates []tiersweep.PruneEntry, removed []string) []string {
	gone := make(map[string]bool, len(removed))
	for _, path := range removed {
		gone[path] = true
	}
	var out []string
	for _, c := range candidates {
		if !gone[c.Path] {
			out = append(out, c.Path)
		}
	}
	return out
}

// strays is the removed paths the list did not name, in removal order: a
// directory that appeared between the listing and the deletion is removed by
// the sweep's own rule, and the operator hears about it.
func strays(candidates []tiersweep.PruneEntry, removed []string) []string {
	listed := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		listed[c.Path] = true
	}
	var out []string
	for _, path := range removed {
		if !listed[path] {
			out = append(out, path)
		}
	}
	return out
}

// count is n as a counted noun, so a one-line listing reads as one directory.
func count(n int) string {
	if n == 1 {
		return "1 directory"
	}
	return fmt.Sprintf("%d directories", n)
}

// parsePruneArgs reads the flags: --dir and --older-than take a value as
// "--flag value" or "--flag=value", --apply takes none. Any other argument is
// a refusal, so a typo cannot change what a supervised --apply deletes.
func parsePruneArgs(args []string) (pruneOptions, error) {
	opts := pruneOptions{maxAge: tiersweep.PruneMaxAge}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(a, "=")
		switch {
		case a == "--apply":
			opts.apply = true
		case name == "--dir" || name == "--older-than":
			if !hasValue {
				if i+1 == len(args) {
					return opts, fmt.Errorf("%s needs a value", name)
				}
				i++
				value = args[i]
			}
			var err error
			if name == "--dir" {
				err = opts.setDir(value)
			} else {
				err = opts.setMaxAge(value)
			}
			if err != nil {
				return opts, err
			}
		default:
			return opts, fmt.Errorf("unknown argument %s", a)
		}
	}
	return opts, nil
}

// setDir takes the directory to list. An empty value is a refusal: it would
// otherwise fall back to TMPDIR, which is not the directory the caller named.
func (o *pruneOptions) setDir(value string) error {
	if value == "" {
		return fmt.Errorf("--dir needs a directory")
	}
	o.dir = value
	return nil
}

// setMaxAge takes the threshold. Zero is a refusal as well as a negative age:
// it would make every tier-sweep directory a candidate, the running sweep's
// own among them.
func (o *pruneOptions) setMaxAge(value string) error {
	age, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("--older-than %s: want a duration such as 48h", value)
	}
	if age <= 0 {
		return fmt.Errorf("--older-than %s: want a positive age", value)
	}
	o.maxAge = age
	return nil
}
