package tiersweep

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// shardText is shard.txt: the packages the integration tier runs as K shards of
// one test binary, one "./package K" per line (gt-rw118).
//
//go:embed shard.txt
var shardText string

// ParseShards reads shard.txt: one "./package K" per line, a # starts a comment,
// and blank lines are dropped. The package is the name the runner prints, so it
// keeps its ./ and a nested module's path is module-relative. A line that is not
// that shape, a count below one, or a package named twice is an error: a shard
// plan that cannot be read runs nothing, so the tier fails closed.
func ParseShards(text string) (map[string]int, error) {
	shards := make(map[string]int)
	for i, line := range strings.Split(text, "\n") {
		if j := strings.Index(line, "#"); j >= 0 {
			line = line[:j]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "./") {
			return nil, fmt.Errorf("shard.txt line %d: want a ./package name and a shard count", i+1)
		}
		k, err := strconv.Atoi(fields[1])
		if err != nil || k < 1 {
			return nil, fmt.Errorf("shard.txt line %d: shard count %q is not a positive integer", i+1, fields[1])
		}
		if _, dup := shards[fields[0]]; dup {
			return nil, fmt.Errorf("shard.txt line %d: package %s is named twice", i+1, fields[0])
		}
		shards[fields[0]] = k
	}
	return shards, nil
}

// SeedShards is the shard plan shard.txt names.
func SeedShards() (map[string]int, error) { return ParseShards(shardText) }

// shardGroup is one package run as K shards: what its build and list printed,
// the tests the test binary lists, and the shards the list was partitioned into.
// prepErr is set when the build or the list failed, and then there are no
// children: the package is RED on that step's word alone.
type shardGroup struct {
	k        int
	listed   []string
	prep     string
	prepErr  error
	children []*task
}

// shardPart is one shard's place in its package and the tests it runs.
type shardPart struct {
	index, of int
	tests     []string
}

// ListedTests reads a test binary's -test.list output: its top-level Test names,
// in the order listed. Benchmarks and examples are not a shard's work, and a line
// that is not a Test name is ignored.
func ListedTests(text string) []string {
	var names []string
	for _, line := range strings.Split(text, "\n") {
		if name := strings.TrimSpace(line); isTestName(name) {
			names = append(names, name)
		}
	}
	return names
}

// isTestName reports whether name is a top-level test by the testing package's
// rule: the word Test, then nothing, or a rune that is not lower case.
func isTestName(name string) bool {
	if !strings.HasPrefix(name, "Test") {
		return false
	}
	rest := name[len("Test"):]
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || !unicode.IsLower(r)
}

// shardBinName is the test binary a package's shards run, beside its log.
func shardBinName(tier, name string) string {
	return strings.TrimSuffix(LogName(tier, name), ".log") + ".test"
}

// shardArgv is one shard's command: the same -test.timeout and -test.count go
// test gives its binary, the -test.paniconexit0 it adds, and -test.v=test2json so
// the shard's terminal events name the tests that ran.
func shardArgv(prefix []string, bin string, tests []string, timeout string) []string {
	return append(append([]string{}, prefix...), bin,
		"-test.run", RunPattern(tests), "-test.count=1", "-test.paniconexit0",
		"-test.timeout", timeout, "-test.v=test2json")
}

// testWalls is the previous run's wall for each of names that has one, in
// seconds. A name with none is left out, and Partition gives it the median.
func (r *Runner) testWalls(tier, name string, names []string) map[string]float64 {
	walls := make(map[string]float64)
	for _, n := range names {
		if w, ok := r.TestWalls[TestWallKey(tier, name, n)]; ok {
			walls[n] = w
		}
	}
	return walls
}

// prepareGroup builds the package's test binary once, lists the tests it holds
// and partitions them into the group's shards, balanced by the last run's walls
// (gt-rw118). A step that fails leaves the group with prepErr and no shards, and
// its log names the step and shows what the step printed.
func (r *Runner) prepareGroup(tier, dir string, prefix, args []string, t *task) {
	g := t.group
	var out bytes.Buffer
	fail := func(err error) {
		fmt.Fprintf(&out, "FAIL shard setup %s: %v\n", t.name, err)
		g.prepErr = err
		g.prep = out.String()
	}
	bin, err := filepath.Abs(filepath.Join(r.LogDir, shardBinName(tier, t.name)))
	if err != nil {
		fail(err)
		return
	}
	build := append(append(append([]string{}, prefix...), "go", "test", "-c", "-o", bin), args...)
	if code := r.Exec(dir, r.Env, append(build, t.pkg), &out); code != 0 {
		fail(fmt.Errorf("build the test binary: exit %d", code))
		return
	}
	// The binary runs in its package's directory, as go test runs it there.
	pkgDir := filepath.Join(dir, strings.TrimPrefix(t.pkg, "./"))
	var listing bytes.Buffer
	list := append(append([]string{}, prefix...), bin, "-test.list", ".")
	if code := r.Exec(pkgDir, r.Env, list, &listing); code != 0 {
		out.Write(listing.Bytes())
		fail(fmt.Errorf("list the tests: exit %d", code))
		return
	}
	names := ListedTests(listing.String())
	if len(names) == 0 {
		fail(fmt.Errorf("the test binary lists no tests"))
		return
	}
	g.listed = names
	parts := Partition(names, r.testWalls(tier, t.name, names), g.k)
	for i, part := range parts {
		g.children = append(g.children, &task{
			pkg:   t.pkg,
			name:  t.name,
			shard: &shardPart{index: i + 1, of: len(parts), tests: part},
			cwd:   pkgDir,
			argv:  shardArgv(prefix, bin, part, r.Timeout),
		})
	}
	g.prep = out.String()
}

// runShard runs one shard's command and keeps its plain text for the group's
// log. The shard's exit code, wall and terminal events are its result.
func (r *Runner) runShard(t *task, begin func()) packageResult {
	if begin != nil {
		begin()
	}
	var out bytes.Buffer
	start := r.now()
	code, tests, err := r.execJSON(t.cwd, t.argv, &out)
	t.text = out.String()
	return packageResult{ran: true, code: code, start: start, tests: tests, logErr: err}
}

// finishGroup decides a sharded package's verdict and writes its one log: what
// the build and the list printed, then each shard's text in shard order. The
// package is RED when a step failed, a shard failed, or a listed test appears in
// no shard's output: a shard that drops a test cannot pass, and the missing name
// is in the log (gt-rw118).
func (r *Runner) finishGroup(tier string, t *task) {
	g := t.group
	var text strings.Builder
	text.WriteString(g.prep)
	failed := g.prepErr != nil
	ran := make(map[string]bool)
	for _, c := range g.children {
		text.WriteString(c.text)
		if !c.result.passed() {
			failed = true
		}
		for _, tt := range c.result.tests {
			ran[tt.Test] = true
		}
	}
	var missing []string
	for _, n := range g.listed {
		if !ran[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		failed = true
		fmt.Fprintf(&text, "FAIL shard coverage: %d listed test(s) did not run: %s\n", len(missing), strings.Join(missing, " "))
	}
	t.result = r.writeGroupLog(tier, t.name, text.String(), failed)
	r.stderrMu.Lock()
	defer r.stderrMu.Unlock()
	if t.result.createErr != nil {
		fmt.Fprintf(r.Stderr, "    create log %s: %v\n", t.result.log, t.result.createErr)
		return
	}
	r.reportLog(tier, t)
}

// writeGroupLog creates the package's log and writes the merged text into it. A
// log that cannot be created or written leaves the package not passed.
func (r *Runner) writeGroupLog(tier, name, text string, failed bool) packageResult {
	log := filepath.Join(r.LogDir, LogName(tier, name))
	f, err := r.createLog(log)
	if err != nil {
		return packageResult{log: log, createErr: err}
	}
	_, writeErr := io.WriteString(f, text)
	res := packageResult{ran: true, log: log, closeErr: f.Close()}
	if writeErr != nil {
		res.logErr = fmt.Errorf("write merged log: %w", writeErr)
	}
	if failed {
		res.code = 1
	}
	return res
}

// reportShard writes a shard's line to timings.jsonl and its slow tests. A
// shard that failed is named by the package's log, so it prints nothing more
// unless its own output could not be read. The caller holds stderrMu.
func (r *Runner) reportShard(tier string, t *task) {
	res := t.result
	r.addShardTiming(tier, t.name, res, t.shard)
	r.addSlowTests(tier, t.name, res.tests)
	if res.logErr != nil {
		fmt.Fprintf(r.Stderr, "    shard %d/%d: %v\n", t.shard.index, t.shard.of, res.logErr)
	}
}

// addShardTiming records one shard's line: the shard's own wall, not the
// package's, so the timings file has one line for each of the package's shards.
func (r *Runner) addShardTiming(tier, name string, res packageResult, s *shardPart) {
	if r.Timings == nil {
		return
	}
	r.Timings.Add(Timing{
		Kind:    kindShard,
		Tier:    tier,
		Package: name,
		WallMS:  r.now().Sub(res.start).Milliseconds(),
		Exit:    res.code,
		Start:   res.start.Format(time.RFC3339Nano),
		Shard:   fmt.Sprintf("%d/%d", s.index, s.of),
	})
}
