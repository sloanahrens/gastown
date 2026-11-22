package beads

import (
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
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
// the last reset, and the wall time those calls spent in bd.
type CensusRow struct {
	Verb   string
	Caller string
	Count  int
	// Ms is the wall milliseconds the counted subprocesses took, summed.
	Ms int64
}

// censusKey is a struct rather than a joined string so a count lookup
// allocates nothing.
type censusKey struct {
	verb, caller string
}

// censusStat is a row's count and time while it accumulates: the count is
// taken as the subprocess starts, the time as it ends.
type censusStat struct {
	count int
	ms    int64
}

var (
	censusMu     sync.Mutex
	censusCounts = make(map[censusKey]censusStat)
	censusTotal  int
)

// censusRecord counts one bd subprocess about to start with args and returns
// the row key censusRecordEnd adds its time under.
func censusRecord(args []string) censusKey {
	verb, caller := censusVerb(args), censusCaller()
	key := censusKey{verb: verb, caller: caller}
	censusMu.Lock()
	stat := censusCounts[key]
	stat.count++
	censusCounts[key] = stat
	censusTotal++
	censusMu.Unlock()
	return key
}

// censusRecordEnd adds d, the wall time the counted subprocess spent, to the
// row censusRecord returned. A snapshot taken in between already reported the
// call's count and cleared its row, so the time is dropped rather than carried
// into a row that would name time for no calls.
func censusRecordEnd(key censusKey, d time.Duration) {
	ms := d.Milliseconds()
	if ms <= 0 {
		return
	}
	censusMu.Lock()
	if stat, ok := censusCounts[key]; ok {
		stat.ms += ms
		censusCounts[key] = stat
	}
	censusMu.Unlock()
}

// CensusSnapshotAndReset returns every verb and caller counted since the last
// reset, most frequent first, and zeroes the counters.
func CensusSnapshotAndReset() []CensusRow {
	censusMu.Lock()
	rows := make([]CensusRow, 0, len(censusCounts))
	for key, stat := range censusCounts {
		rows = append(rows, CensusRow{Verb: key.verb, Caller: key.caller, Count: stat.count, Ms: stat.ms})
	}
	clear(censusCounts)
	censusTotal = 0
	censusMu.Unlock()

	SortCensusRows(rows)
	return rows
}

// SortCensusRows orders rows as the census logs them, most frequent first, then
// by verb and caller, so rows gathered from more than one sink line up as one
// ranking.
func SortCensusRows(rows []CensusRow) {
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

// censusWrapperFrames are frame prefixes of pass-through wrappers: methods
// that forward a call to the store beneath them, choosing only how to retry it
// and never whether the call is made. The census names the function that
// decided to read, so it skips these frames the way it skips this package's own
// seam (gt-krmal). Each prefix includes the module path, matching the frame
// names the compiler writes.
var censusWrapperFrames = []string{
	// landworker.RetryBeads retries a serialization failure and forwards every
	// other call unchanged, so every landing worker read named the wrapper
	// instead of the step that asked for it (gt-krmal).
	censusModulePath + "/internal/landworker.RetryBeads.",
	// The same wrapper's methods when declared on the pointer, which the frame
	// name spells differently.
	censusModulePath + "/internal/landworker.(*RetryBeads).",
}

// censusCallerFromFrames returns the first frame name outside this package and
// outside its pass-through wrappers as "package.Func", or "unknown" when every
// frame is one of those.
func censusCallerFromFrames(names []string) string {
	for i, name := range names {
		// runtime frames are the walk's own machinery, not a caller.
		if name == "" || strings.HasPrefix(name, "runtime.") {
			continue
		}
		if censusInBeads(name) {
			continue
		}
		// A wrapper stands in for the caller only when no frame above it can
		// name one; a stack that ends at the wrapper keeps its name.
		if censusInWrapper(name) && censusNamesCaller(names[i+1:]) {
			continue
		}
		return censusShortName(name)
	}
	return censusUnknown
}

// censusInWrapper reports whether the frame name is a pass-through wrapper the
// census looks through.
func censusInWrapper(name string) bool {
	for _, prefix := range censusWrapperFrames {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// censusNamesCaller reports whether names holds a frame that could still name
// the caller: not the runtime's machinery, and not this package's seam. The
// walk stops at censusMaxFrames, so a deciding frame past it is not in names
// and the wrapper keeps its name, the same fallback as a stack that ends at the
// wrapper.
func censusNamesCaller(names []string) bool {
	for _, name := range names {
		if name == "" || strings.HasPrefix(name, "runtime.") || censusInBeads(name) {
			continue
		}
		return true
	}
	return false
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
