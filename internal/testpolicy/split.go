package testpolicy

import "strings"

// goTestBoolFlags are the go test flags (build and test) that take no
// separate value argument; every other flag go test knows takes one
// ("-timeout 20m"), unless it is written "-flag=value".
var goTestBoolFlags = map[string]bool{
	"a": true, "asan": true, "benchmem": true, "buildvcs": true, "c": true,
	"cover": true, "failfast": true, "fullpath": true, "i": true, "json": true,
	"linkshared": true, "modcacherw": true, "msan": true, "n": true,
	"race": true, "short": true, "trimpath": true, "v": true, "work": true,
	"x": true,
}

// goTestValueFlags are the go test flags (build and test) that take a value.
var goTestValueFlags = map[string]bool{
	"asmflags": true, "bench": true, "benchtime": true, "blockprofile": true,
	"blockprofilerate": true, "buildmode": true, "C": true, "compiler": true,
	"count": true, "covermode": true, "coverpkg": true, "coverprofile": true,
	"cpu": true, "cpuprofile": true, "exec": true, "fuzz": true,
	"fuzzminimizetime": true, "fuzztime": true, "gccgoflags": true,
	"gcflags": true, "installsuffix": true, "ldflags": true, "list": true,
	"memprofile": true, "memprofilerate": true, "mod": true, "modfile": true,
	"mutexprofile": true, "mutexprofilefraction": true, "o": true,
	"outputdir": true, "overlay": true, "p": true, "parallel": true,
	"pgo": true, "pkgdir": true, "run": true, "shuffle": true, "skip": true,
	"tags": true, "timeout": true, "toolexec": true, "trace": true, "vet": true,
}

// flagName returns the name of a flag argument ("-timeout=20m" and
// "--test.timeout" are both "timeout") and whether it carries its value.
func flagName(arg string) (name string, hasValue bool) {
	name = strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	name, _, hasValue = strings.Cut(name, "=")
	return strings.TrimPrefix(name, "test."), hasValue
}

// SplitTestArgs splits go test's arguments into the flags before the package
// list, the package list, and everything after it, the way go test reads
// them: the package list is the first run of non-flag arguments, a flag it
// knows that is not boolean consumes the next argument as its value, and
// "-args", "--" or a flag it does not know ends the part it parses (the rest
// goes to the test binary). The budget runner re-runs go test with the same
// before and after arguments around a narrower package list.
func SplitTestArgs(args []string) (before, pkgs, after []string) {
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" || arg == "-args" || arg == "--args" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if pkgs != nil {
				break // a second run of non-flags belongs to the test binary
			}
			for i < len(args) && (!strings.HasPrefix(args[i], "-") || args[i] == "-") {
				pkgs = append(pkgs, args[i])
				i++
			}
			continue
		}
		name, hasValue := flagName(arg)
		n := 1
		switch {
		case goTestBoolFlags[name] || hasValue:
		case goTestValueFlags[name]:
			n = 2
		default:
			// go test passes a flag it does not know to the test binary and
			// cannot tell whether it takes a value, so it stops looking for
			// packages there.
			return before, pkgs, append(after, args[i:]...)
		}
		if n > len(args)-i {
			n = len(args) - i
		}
		if pkgs == nil {
			before = append(before, args[i:i+n]...)
		} else {
			after = append(after, args[i:i+n]...)
		}
		i += n
	}
	if i < len(args) {
		after = append(after, args[i:]...)
	}
	return before, pkgs, after
}

// TagsArgs returns the -tags flags in go test's arguments, so go list
// resolves the package patterns under the same build tags go test uses.
func TagsArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			continue
		}
		name, hasValue := flagName(args[i])
		if name != "tags" {
			continue
		}
		out = append(out, args[i])
		if !hasValue && i+1 < len(args) {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

// PartitionPackages splits import paths into the packages the budget judges
// and the packages listed in exempt (unconverted.txt, keyed by path relative
// to module). The budget runner runs the judged ones through its exec
// wrapper, which turns off go test's result cache, and the exempt ones
// through plain go test, which keeps it.
func PartitionPackages(importPaths []string, module string, exempt map[string]bool) (judged, cached []string) {
	for _, p := range importPaths {
		rel, inModule := strings.CutPrefix(p, module+"/")
		if inModule && exempt[rel] {
			cached = append(cached, p)
		} else {
			judged = append(judged, p)
		}
	}
	return judged, cached
}
