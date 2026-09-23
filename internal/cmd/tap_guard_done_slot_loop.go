package cmd

import (
	"strings"
)

// Loops and watchers around `gt done` / `gt slot` (gt-7dxw).
//
// The sanctioned path is to run `gt done` ONCE — it waits for the container
// gate itself and is bounded — so a scripted retry around it only holds the
// slot that every other agent is queued behind, one pass at a time (the
// improvised zsh slot-polling loop of gt-pnkd, killed by the mayor on
// 2026-09-17). The rule the polecat reads is in the prime template, the
// mol-polecat-work formula, and the /done body; this guard refuses the loop
// before a shell ever starts it, for every role and every cwd, because the
// harm is not role-specific.
//
// What it blocks:
//
//  1. A `for` / `while` / `until` block whose body invokes `gt done` or
//     `gt slot`. The block is tracked to its matching `done`, so a loop that
//     has already closed before `gt done` — `for f in a b; do gofmt -w $f;
//     done && gt done` — is allowed, and only a `gt done` still inside the
//     body is refused.
//  2. A repeat wrapper — `xargs`, `watch`, or `seq` — combined with `gt done`
//     or `gt slot` anywhere in the same command. These iterate without a
//     block, and `xargs`/`watch` hide their payload inside a quoted argument,
//     so the payload is matched as raw text rather than as tokens.
//  3. A heredoc body being WRITTEN TO A FILE (`cat > /tmp/retry.sh <<EOF`,
//     `tee retry.sh <<EOF`, or any reader line carrying a `>` redirect) that
//     itself contains (1) or (2): the shape the incident's retry script was
//     authored in.
//
// Deliberately not attempted, matching the policy tap_guard_polecat_paths.go
// states for itself: reading the CONTENTS of a script the command merely
// executes (`bash /tmp/retry.sh` is three ordinary tokens) and parsing
// arbitrary interpreter payloads. This guard is a seat belt, not a cage — the
// prime text and the formula carry the instruction, and the witness remains
// the backstop for the shapes it cannot see.
const (
	doneSlotLoopReason = "Never poll the slot or script a gt done retry"
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

// doneSlotLoopRepeaters are commands that iterate without a block keyword.
// seq is included for the piped forms (`seq 1 300 | ...`) and for a bare
// `seq` used as a counter beside the command; xargs and watch are the two
// spellings the incident itself used.
var doneSlotLoopRepeaters = map[string]bool{"xargs": true, "watch": true, "seq": true}

// doneSlotLoopRawPairs are the raw-text spellings the repeater check looks
// for, because a repeater's payload is normally one quoted token by the time
// the command is tokenized (`xargs -I{} sh -c 'gt done'`).
var doneSlotLoopRawPairs = []string{"gt done", "gt slot"}

// doneSlotLoopMaxBodyDepth bounds the recursion into heredoc bodies written
// to files, so a pathological nesting cannot loop unboundedly.
const doneSlotLoopMaxBodyDepth = 2

// matchesDoneSlotLoop reports whether command contains a loop or watcher that
// runs `gt done` or `gt slot`. tokens and lowerTokens must be index-aligned
// (see shellTokenize), and raw is the shell text the tokens came from — the
// heredoc bodies and the repeater payloads are only visible there.
func matchesDoneSlotLoop(raw string, tokens, lowerTokens []string) (reason, alternative string) {
	return matchesDoneSlotLoopDepth(raw, tokens, lowerTokens, 0)
}

func matchesDoneSlotLoopDepth(raw string, tokens, lowerTokens []string, depth int) (reason, alternative string) {
	if doneSlotLoopHasBlockingLoop(tokens, lowerTokens) {
		return doneSlotLoopReason, doneSlotLoopAlternative
	}
	if doneSlotLoopHasRepeater(tokens, raw) {
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

// doneSlotLoopHasBlockingLoop walks the token stream tracking block depth and
// reports whether a `gt done` / `gt slot` invocation sits inside an open
// for/while/until block.
//
// Both landmarks are required to be a segment's COMMAND WORD — the loop
// keyword because `for` and `while` are ordinary English words elsewhere in a
// command line ("grep -rn for --include=*.go" must not open a block, and
// prose in a heredoc-written file must not either), and the terminator
// because `done` is the second token of `gt done` itself. Requiring command
// position for `done` is what keeps `do gt done; done` from closing its own
// loop at the `done` it invokes.
func doneSlotLoopHasBlockingLoop(tokens, lowerTokens []string) bool {
	commandWords := doneSlotLoopSegmentCommandWords(tokens)
	depth := 0
	// The pair check runs at every index, the depth bookkeeping only at a
	// segment's command word: inside `do gt done; done` the `gt` is an
	// argument (the segment's command word is `do`), so a walk that only
	// looked at command words would never see the invocation it is guarding.
	for i := range lowerTokens {
		if depth > 0 && doneSlotLoopPairAt(lowerTokens, i) {
			return true
		}
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
	return false
}

// doneSlotLoopHasRepeater reports whether a repeat wrapper appears alongside
// `gt done` / `gt slot`. Both halves are matched loosely on purpose: the
// wrapper is the segment's command word, and the payload is raw text, because
// `xargs -I{} bash -c 'gt done'` tokenizes its payload into a single token
// that no token-pair check can see.
func doneSlotLoopHasRepeater(tokens []string, raw string) bool {
	if !doneSlotLoopMentionsRawPair(raw) {
		return false
	}
	for _, seg := range splitShellSegments(tokens) {
		word, _ := segmentCommandWord(seg)
		if doneSlotLoopRepeaters[strings.ToLower(word)] {
			return true
		}
	}
	return false
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
// body to a file rather than passing it to a command that consumes text: a
// redirect anywhere on the reader line, or a reader that is one of the
// commands whose whole job is copying text. `gt mail send --stdin <<BODY` and
// `bd update --notes "$(cat <<EOF ... EOF)"` are the false positives this
// keeps out — a mail body that merely discusses a loop must not be refused.
func doneSlotLoopWritesFile(reader string) bool {
	if strings.Contains(reader, ">") {
		return true
	}
	word, _ := segmentCommandWord(shellTokenize(reader))
	switch strings.ToLower(word) {
	case "cat", "tee", "dd", "printf", "echo":
		return true
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

// doneSlotLoopPairAt reports whether the token at i invokes `gt done` or
// `gt slot`.
func doneSlotLoopPairAt(lowerTokens []string, i int) bool {
	if lowerTokens[i] != "gt" || i+1 >= len(lowerTokens) {
		return false
	}
	switch lowerTokens[i+1] {
	case "done", "slot":
		return true
	}
	return false
}

// doneSlotLoopMentionsRawPair reports whether the raw command text names
// `gt done` or `gt slot` anywhere, quoted or not.
func doneSlotLoopMentionsRawPair(raw string) bool {
	for _, pair := range doneSlotLoopRawPairs {
		if strings.Contains(raw, pair) {
			return true
		}
	}
	return false
}

// doneSlotLoopLower lowercases tokens for the case-insensitive checks above.
func doneSlotLoopLower(tokens []string) []string {
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	return lower
}
