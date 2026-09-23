package cmd

import (
	"strings"
)

// Loops and watchers around `gt done` / `gt slot` (gt-7dxw).
//
// The sanctioned path is to run `gt done` ONCE — it waits for the container
// gate itself and is bounded — so a scripted retry around it only holds the
// slot that every other agent is queued behind, one pass at a time. The rule
// the polecat reads is in the prime template, the mol-polecat-work formula,
// the /done body, and docs/reference.md; this guard refuses the loop before a
// shell ever starts it, for every role and every cwd, because the harm is not
// role-specific.
//
// What it blocks:
//
//  1. A `for` / `while` / `until` block that runs `gt done`, or a read-only
//     `gt slot` poll, inside a body that closes. Taking and releasing the slot
//     is bounded work, so `gt slot run` and `gt slot reap` may be repeated.
//  2. A repeat wrapper — `xargs` / `watch` — whose OWN payload runs the gate.
//     The gate is looked for in that wrapper's arguments, never in the command
//     line at large, so `… | xargs gofmt -w && gt done` is the ordinary
//     one-shot it reads as. `seq` needs no rule of its own: it emits the input
//     some other command repeats, and that command is matched here while the
//     loop that consumes it is matched by (1).
//  3. A heredoc body being WRITTEN TO A FILE (`cat > /tmp/retry.sh <<EOF`,
//     `tee retry.sh <<EOF`, `dd of=retry.sh <<EOF`) that itself contains (1)
//     or (2): the shape the incident's retry script was authored in.
//
// Deliberately not attempted, matching the policy tap_guard_polecat_paths.go
// states for itself: reading the CONTENTS of a script the command merely
// executes (`bash /tmp/retry.sh` is three ordinary tokens) and parsing
// arbitrary interpreter payloads. This guard is a seat belt, not a cage — the
// prime text and the formula carry the instruction, and the witness remains
// the backstop for the shapes it cannot see.
const (
	doneSlotLoopReason      = "Never poll the slot or script a gt done retry"
	doneSlotLoopAlternative = "Alternative: run `gt done` once, as its own command. It waits for the " +
		"container-gate slot itself, prints a progress line while it waits, and gives up with a " +
		"slot-acquire timeout if it cannot get one. Do not loop, watch, or script a retry around " +
		"`gt done` or `gt slot`: a polling loop holds the gate other agents are queued behind, one " +
		"pass at a time (gt-7dxw). If `gt done` fails on the test-verify slot cap or the run budget, " +
		"add a bead comment with the error and the verify log path, then `gt escalate -s medium` " +
		"asking the mayor for a one-shot `--skip-verify` ruling, and wait (gt-pnkd)."
)

// doneSlotLoopKeywords are the block-shaped shell loop openers. The deacon
// patrol-loop matchers (tap_guard_patrol_loop.go) cover only `for ... seq` and
// `while true` / `while :`, so `until` and the general shapes are matched
// here.
var doneSlotLoopKeywords = map[string]bool{"for": true, "while": true, "until": true}

// doneSlotLoopRepeaters are commands that run a payload repeatedly without a
// block keyword — the two spellings that can hide their payload inside a
// quoted argument, where no command-word walk can see it.
var doneSlotLoopRepeaters = map[string]bool{"xargs": true, "watch": true}

// doneSlotLoopWrapperValueFlags names the wrapper flags that take the next
// token as their value, so payload matching skips `-n 5` whole and still sees
// the command behind it. An attached value is a single token (`-I{}`, `-n5`,
// `--interval=2`) and needs no entry here.
var doneSlotLoopWrapperValueFlags = map[string]map[string]bool{
	"xargs": {
		"-I": true, "-n": true, "-P": true, "-s": true, "-L": true, "-a": true,
		"-d": true, "-E": true, "-e": true, "--arg-file": true, "--delimiter": true,
		"--eof": true, "--max-args": true, "--max-chars": true, "--max-lines": true,
		"--max-procs": true, "--replace": true,
	},
	"watch": {"-n": true, "--exec": true, "--interval": true},
}

// doneSlotLoopSlotPollVerbs are the `gt slot` subcommands this rule polices
// inside a loop: the read-only poll, which only asks whether the slot is free.
// `gt slot run` and `gt slot reap` take and release the slot, so repeating
// them is a bounded sequence rather than a busy-wait (gt-7dxw review).
var doneSlotLoopSlotPollVerbs = map[string]bool{"status": true}

// doneSlotLoopMaxBodyDepth bounds the recursion into heredoc bodies written
// to files, so a pathological nesting cannot loop unboundedly.
const doneSlotLoopMaxBodyDepth = 2

// matchesDoneSlotLoop reports whether command contains a loop or watcher that
// runs `gt done` or `gt slot`. tokens and lowerTokens must be index-aligned
// (see shellTokenize), and raw is the shell text the tokens came from — the
// heredoc bodies a command writes to a file are only visible there.
func matchesDoneSlotLoop(raw string, tokens, lowerTokens []string) (reason, alternative string) {
	return matchesDoneSlotLoopDepth(raw, tokens, lowerTokens, 0)
}

func matchesDoneSlotLoopDepth(raw string, tokens, lowerTokens []string, depth int) (reason, alternative string) {
	if doneSlotLoopHasBlockingLoop(tokens, lowerTokens) {
		return doneSlotLoopReason, doneSlotLoopAlternative
	}
	if doneSlotLoopHasRepeater(tokens) {
		return doneSlotLoopReason, doneSlotLoopAlternative
	}
	if depth >= doneSlotLoopMaxBodyDepth {
		return "", ""
	}
	for _, body := range doneSlotLoopFileWrittenBodies(raw) {
		bodyTokens := shellTokenize(body)
		if r, alt := matchesDoneSlotLoopDepth(body, bodyTokens, doneSlotLoopLower(bodyTokens), depth+1); r != "" {
			return r, alt
		}
	}
	return "", ""
}

// doneSlotLoopHasBlockingLoop reports whether a gate invocation sits inside a
// block that CLOSES. Both landmarks are required to be a segment's COMMAND
// WORD — the loop keyword because `for` and `while` are ordinary English words
// elsewhere in a command line, the terminator because `done` is the second
// token of `gt done` itself, which is what keeps `do gt done; done` from
// closing its own loop at the `done` it invokes.
//
// The invocation itself is checked at EVERY index: inside `do gt done; done`
// the `gt` is an argument, so a walk that only looked at command words would
// never see the invocation it is guarding.
func doneSlotLoopHasBlockingLoop(tokens, lowerTokens []string) bool {
	commandWords := doneSlotLoopSegmentCommandWords(tokens)
	// depths[i] is the block depth entering token i, and depths[len] the depth
	// the walk ended at, so a shallower depth anywhere later is a block that
	// closed. A body only runs once its block closes, which is what separates
	// a script from prose: a line that opens with "While gt done waits…" never
	// closes, and a loop truncated at EOF runs nothing because the shell
	// rejects the whole command (gt-7dxw review).
	depths := make([]int, len(lowerTokens)+1)
	depth := 0
	for i := range lowerTokens {
		depths[i] = depth
		if !doneSlotLoopIsCommandWord(commandWords, i) {
			continue
		}
		switch {
		case doneSlotLoopKeywords[lowerTokens[i]]:
			depth++
		case lowerTokens[i] == "done" && depth > 0:
			depth--
		}
	}
	depths[len(lowerTokens)] = depth
	shallowest := make([]int, len(depths))
	shallowest[len(depths)-1] = depths[len(depths)-1]
	for i := len(depths) - 2; i >= 0; i-- {
		shallowest[i] = depths[i]
		if shallowest[i+1] < shallowest[i] {
			shallowest[i] = shallowest[i+1]
		}
	}
	for i := range lowerTokens {
		if depths[i] == 0 || !doneSlotLoopPoliteInvocationAt(lowerTokens, i) {
			continue
		}
		if shallowest[i+1] < depths[i] {
			return true
		}
	}
	return false
}

// doneSlotLoopHasRepeater reports whether a repeat wrapper runs the gate in its
// own payload. Only the wrapper's arguments are inspected, because that is what
// the wrapper repeats: `git diff --name-only | xargs gofmt -w && gt done` runs
// `gt done` once, after the pipeline has finished, and a command that merely
// reads or writes the words ("rg -l Foo | xargs grep -n \"gt slot\"") runs the
// gate nowhere (gt-7dxw review).
func doneSlotLoopHasRepeater(tokens []string) bool {
	for _, segment := range splitShellSegments(tokens) {
		word, args := segmentCommandWord(segment)
		word = strings.ToLower(word)
		if !doneSlotLoopRepeaters[word] {
			continue
		}
		if doneSlotLoopExecutesGate(doneSlotLoopWrapperPayload(word, args)) {
			return true
		}
	}
	return false
}

// doneSlotLoopWrapperPayload strips the wrapper's own flags and returns the
// command it repeats.
func doneSlotLoopWrapperPayload(wrapper string, args []string) []string {
	valueFlags := doneSlotLoopWrapperValueFlags[wrapper]
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		flag := args[i]
		i++
		if !strings.Contains(flag, "=") && valueFlags[flag] && i < len(args) && !strings.HasPrefix(args[i], "-") {
			i++ // the flag's separate-value form
		}
	}
	return args[i:]
}

// doneSlotLoopExecutesGate reports whether a command line — a command word plus
// its arguments — runs the gate. A quoted payload arrives as ONE token
// (`xargs -I{} bash -c 'gt done'` tokenizes its payload to `gt done`), so a
// token that still holds several words is split again and judged as the command
// line it is. Launchers are followed the same way nestedCommands follows them.
func doneSlotLoopExecutesGate(tokens []string) bool {
	if len(tokens) == 1 && strings.ContainsAny(tokens[0], " \t") {
		return doneSlotLoopExecutesGate(shellTokenize(tokens[0]))
	}
	for _, segment := range splitShellSegments(tokens) {
		word, args := segmentCommandWord(doneSlotLoopLower(segment))
		switch {
		case word == "gt":
			if doneSlotLoopPoliteGtArgs(args) {
				return true
			}
		case doneSlotLoopRepeaters[word]:
			if doneSlotLoopExecutesGate(doneSlotLoopWrapperPayload(word, args)) {
				return true
			}
		case shellInvokers[word]:
			if payload, ok := doneSlotLoopShellPayload(args); ok &&
				doneSlotLoopExecutesGate(shellTokenize(payload)) {
				return true
			}
		case word == "eval":
			if doneSlotLoopExecutesGate(shellTokenize(strings.Join(args, " "))) {
				return true
			}
		}
	}
	return false
}

// doneSlotLoopShellPayload returns the command an invoker's `-c` carries.
func doneSlotLoopShellPayload(args []string) (string, bool) {
	for i, arg := range args {
		if arg == "-c" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// doneSlotLoopPoliteInvocationAt reports whether the tokens at i invoke the
// gate in the shape this rule polices: `gt done`, or a read-only `gt slot`
// poll.
func doneSlotLoopPoliteInvocationAt(lowerTokens []string, i int) bool {
	if lowerTokens[i] != "gt" {
		return false
	}
	return doneSlotLoopPoliteGtArgs(lowerTokens[i+1:])
}

// doneSlotLoopPoliteGtArgs reports whether a `gt` command line's arguments name
// a policed invocation.
func doneSlotLoopPoliteGtArgs(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "done":
		return true
	case "slot":
		return doneSlotLoopSlotPollArgs(args[1:])
	}
	return false
}

// doneSlotLoopSlotPollArgs reports whether a `gt slot` command line's arguments
// name the read-only status poll. A bare `gt slot` reports status too, as does
// one followed by a flag or by the end of its segment.
func doneSlotLoopSlotPollArgs(args []string) bool {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || shellCommandSeparators[args[0]] {
		return true
	}
	return doneSlotLoopSlotPollVerbs[args[0]]
}

// doneSlotLoopFileWrittenBodies returns the heredoc bodies the command writes
// to a file. A body fed to a shell on stdin (`bash <<EOF`) is not included
// here: evaluateDangerousCommand already recurses into those as commands of
// their own (shellFedHeredocBodies).
func doneSlotLoopFileWrittenBodies(raw string) []string {
	var bodies []string
	for _, span := range scanHeredocs(raw) {
		if !doneSlotLoopWritesFile(span.reader) {
			continue
		}
		if strings.TrimSpace(span.body) == "" {
			continue
		}
		bodies = append(bodies, span.body)
	}
	return bodies
}

// doneSlotLoopWritesFile reports whether a heredoc's reader line writes its
// body to a file rather than handing it to a command as data: a stdout
// redirect, or a `tee`/`dd` naming a destination. A stderr redirect writes
// none of the body, and a reader that only copies text (`cat`, `echo`,
// `printf`) reaches a file through nothing else — reading either as a written
// script turned `gt mail send --stdin <<EOF 2>/dev/null` and `cat <<EOF | git
// commit -F -` into retry scripts (gt-7dxw review).
func doneSlotLoopWritesFile(reader string) bool {
	if doneSlotLoopRedirectsStdout(reader) {
		return true
	}
	word, args := segmentCommandWord(shellTokenize(reader))
	switch strings.ToLower(word) {
	case "tee":
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				return true // `tee FILE` writes the body to FILE
			}
		}
	case "dd":
		for _, arg := range args {
			if strings.HasPrefix(arg, "of=") {
				return true
			}
		}
	}
	return false
}

// doneSlotLoopRedirectsStdout reports whether the reader line redirects stdout
// — `> file`, `>> file`, `1> file`, `&> file` — which is the only way the body
// lands in a file. Scanned on the raw text so a redirect glued to a word
// (`cat>/tmp/retry.sh`) counts. A numbered descriptor that is not stdout
// (`2>`, `2>&1`) redirects something else and writes none of the body.
func doneSlotLoopRedirectsStdout(reader string) bool {
	for i := 0; i < len(reader); i++ {
		if reader[i] != '>' {
			continue
		}
		switch {
		case i == 0, reader[i-1] == '1':
			return true
		case reader[i-1] == '&':
			// `&> file` writes the body; `2>&1` duplicates a descriptor.
			if !(i > 1 && reader[i-2] >= '0' && reader[i-2] <= '9') {
				return true
			}
		case reader[i-1] >= '0' && reader[i-1] <= '9':
			continue
		default:
			return true
		}
	}
	return false
}

// doneSlotLoopSegmentCommandWords returns the token index that begins each
// shell segment's command, skipping leading VAR=value assignments and the
// launchers that carry the real command behind them (env VAR=x cmd, time cmd,
// ...). Mirrors segmentCommandWord, which returns the word rather than its
// index.
func doneSlotLoopSegmentCommandWords(tokens []string) map[int]bool {
	words := make(map[int]bool)
	for i := 0; i < len(tokens); {
		if shellCommandSeparators[tokens[i]] {
			i++
			continue
		}
		j := i
		for j < len(tokens) && !shellCommandSeparators[tokens[j]] {
			if isEnvAssignment(tokens[j]) {
				j++
				continue
			}
			switch tokens[j] {
			case "!", "time", "sudo", "command", "nohup", "env", "nice", "stdbuf":
				j++
				continue
			}
			break
		}
		if j < len(tokens) && !shellCommandSeparators[tokens[j]] {
			words[j] = true
		}
		for i < len(tokens) && !shellCommandSeparators[tokens[i]] {
			i++
		}
	}
	return words
}

// doneSlotLoopIsCommandWord reports whether tokens[i] begins its segment.
func doneSlotLoopIsCommandWord(commandWords map[int]bool, i int) bool {
	return commandWords[i]
}

// doneSlotLoopLower lowercases tokens for the case-insensitive checks above.
func doneSlotLoopLower(tokens []string) []string {
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	return lower
}
