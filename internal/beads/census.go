package beads

import (
	"runtime"
	"sort"
	"strings"
	"sync"
)

// The census counts the bd subprocesses this package starts, by verb and by
// the caller that asked for one, because that call volume is what Dolt pays
// for (gt-y3bzk); gt-7h3vm logs it.
const (
	// censusModulePath is the prefix the compiler gives every frame name in
	// this module, so trimming it names the caller independently of the
	// checkout's directory.
	censusModulePath = "github.com/steveyegge/gastown"
	// censusPkgPath is this package. A frame at or beneath it is the seam
	// itself, never the caller the census wants to name.
	censusPkgPath = censusModulePath + "/internal/beads"
	// censusMaxFrames bounds the stack read. The seam sits a handful of frames
	// below its caller, and the walk stops at the first frame outside it.
	censusMaxFrames = 32
	// censusUnknown is the verb or caller the census could not determine.
	censusUnknown = "unknown"
)

// CensusRow is how often one verb was started from one calling function since
// the last reset.
type CensusRow struct {
	Verb   string
	Caller string
	Count  int
}

// censusKey is a struct rather than a joined string so a count lookup
// allocates nothing.
type censusKey struct {
	verb, caller string
}

var (
	censusMu     sync.Mutex
	censusCounts = make(map[censusKey]int)
	censusTotal  int
)

// censusRecord counts one bd subprocess about to start with args.
func censusRecord(args []string) {
	verb, caller := censusVerb(args), censusCaller()
	censusMu.Lock()
	censusCounts[censusKey{verb: verb, caller: caller}]++
	censusTotal++
	censusMu.Unlock()
}

// CensusSnapshotAndReset returns every verb and caller counted since the last
// reset, most frequent first, and zeroes the counters.
func CensusSnapshotAndReset() []CensusRow {
	censusMu.Lock()
	rows := make([]CensusRow, 0, len(censusCounts))
	for key, count := range censusCounts {
		rows = append(rows, CensusRow{Verb: key.verb, Caller: key.caller, Count: count})
	}
	clear(censusCounts)
	censusTotal = 0
	censusMu.Unlock()

	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Verb != b.Verb {
			return a.Verb < b.Verb
		}
		return a.Caller < b.Caller
	})
	return rows
}

// CensusTotal returns how many bd subprocesses started since the last reset.
func CensusTotal() int {
	censusMu.Lock()
	defer censusMu.Unlock()
	return censusTotal
}

// censusCaller names the function that started the call being counted.
func censusCaller() string {
	var pcs [censusMaxFrames]uintptr
	// Skip runtime.Callers itself and censusCaller, so the walk starts at
	// censusRecord's caller.
	n := runtime.Callers(2, pcs[:])

	frames := runtime.CallersFrames(pcs[:n])
	var names [censusMaxFrames]string
	i := 0
	for ; i < censusMaxFrames; i++ {
		frame, more := frames.Next()
		names[i] = frame.Function
		if !more {
			i++
			break
		}
	}
	return censusCallerFromFrames(names[:i])
}

// censusCallerFromFrames returns the first frame name outside this package as
// "package.Func", or "unknown" when every frame is inside it.
func censusCallerFromFrames(names []string) string {
	for _, name := range names {
		// runtime frames are the walk's own machinery, not a caller.
		if name == "" || strings.HasPrefix(name, "runtime.") {
			continue
		}
		if censusInBeads(name) {
			continue
		}
		return censusShortName(name)
	}
	return censusUnknown
}

// censusInBeads reports whether the frame name is this package or a package
// beneath it.
func censusInBeads(name string) bool {
	return strings.HasPrefix(name, censusPkgPath+".") ||
		strings.HasPrefix(name, censusPkgPath+"/")
}

// censusShortName trims a frame name to "package.Func": the module prefix, the
// package path and closure suffixes all go.
func censusShortName(name string) string {
	name = strings.TrimPrefix(name, censusModulePath+"/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return censusStripClosures(name)
}

// censusStripClosures removes the ".funcN" (and nested ".funcN.M") suffix the
// compiler appends to a closure frame, leaving the function that declared it.
func censusStripClosures(name string) string {
	for {
		i := strings.LastIndex(name, ".func")
		if i < 0 || !censusIsClosureSuffix(name[i+len(".func"):]) {
			return name
		}
		name = name[:i]
	}
}

// censusIsClosureSuffix reports whether s is what follows ".func" in a closure
// frame name: digits, with dots between nested closures.
func censusIsClosureSuffix(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && s[i] != '.' {
			return false
		}
	}
	return true
}

// censusTwoWordVerbs are bd's verbs whose next word is part of what the call
// does, so "mol wisp list" counts as "mol wisp" and never as bare "mol".
var censusTwoWordVerbs = map[string]bool{
	"comments": true,
	"config":   true,
	"dep":      true,
	"formula":  true,
	"gate":     true,
	"kv":       true,
	"label":    true,
	"mol":      true,
}

// censusValueFlags are bd's global flags that take their value in the next
// argument, listable with "bd --help". The value is never part of the verb.
var censusValueFlags = map[string]bool{
	"--actor":            true,
	"--database":         true,
	"--db":               true,
	"-C":                 true,
	"--directory":        true,
	"--dolt-auto-commit": true,
	"--mem-profile":      true,
}

// censusVerb returns the verb of args: the first non-flag argument, with the
// next one appended when the verb takes a second word.
func censusVerb(args []string) string {
	verb := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if censusValueFlags[arg] {
				i++
			}
			continue
		}
		if verb == "" {
			verb = arg
			if !censusTwoWordVerbs[arg] {
				break
			}
			continue
		}
		return verb + " " + arg
	}
	if verb == "" {
		return censusUnknown
	}
	return verb
}
