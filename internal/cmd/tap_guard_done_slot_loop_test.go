package cmd

import (
	"strings"
	"testing"
)

// The rule and its boundaries (gt-7dxw). A loop or watcher that runs `gt done`
// or `gt slot` is refused; a single `gt done`, a loop that has already closed
// before it, and a loop that never touches the gate all pass. The count
// assertion fails a version that blocks everything or nothing.
func TestMatchesDoneSlotLoop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// The incident: a slot-polling loop that re-ran gt done whenever the
		// slot read free (ruby, /tmp/gtpnkd-retry.sh, 2026-09-17).
		{"slot poll loop re-running gt done", `for i in $(seq 1 300); do if gt slot status | grep -q free; then gt done; fi; done`, true},
		{"while true gt done", "while true; do gt done; sleep 5; done", true},
		{"until slot free", "until gt slot status | grep -q free; do sleep 5; done", true},
		{"for range gt done", "for i in 1 2 3; do gt done --status DEFERRED; done", true},
		{"loop over rigs polling the slot", "for r in gastown beads; do gt slot status; done", true},
		{"while read re-running gt done", "while read -r x; do gt done; done < /tmp/beads", true},
		{"nested loop", "for i in 1 2; do for j in a b; do gt done; done; done", true},

		// Repeat wrappers, whose payload tokenizes as one quoted token.
		{"xargs gt done", "seq 1 300 | xargs -I{} bash -c 'gt done'", true},
		{"xargs gt slot", "seq 1 300 | xargs -I{} gt slot status", true},
		{"xargs with a separate-value flag", "seq 1 300 | xargs -n 1 gt done", true},
		{"xargs with two flags before the payload", "seq 1 300 | xargs -0 -I{} gt slot status", true},
		{"watch the slot", "watch gt slot status", true},
		{"watch with an interval", "watch -n 5 gt slot status", true},
		{"watch gt done", "watch gt done", true},
		{"watch a shell payload", "watch -n 5 bash -c 'gt done'", true},
		{"xargs a shell payload", "seq 1 300 | xargs -I{} sh -c 'gt done'", true},

		// A heredoc written to a file: how the incident's script was authored.
		{"heredoc-written retry script", "cat > /tmp/retry.sh <<'EOF'\nfor i in $(seq 1 300); do gt done; done\nEOF", true},
		{"heredoc-written script, redirect on the right", "cat <<'EOF' > /tmp/retry.sh\nwhile true; do gt done; sleep 5; done\nEOF", true},
		{"tee-written script", "tee /tmp/retry.sh <<'EOF'\nuntil gt slot status | grep -q free; do sleep 1; done\nEOF", true},
		{"heredoc-written script with a repeater", "cat > /tmp/retry.sh <<'EOF'\nseq 1 300 | xargs -I{} gt done\nEOF", true},

		// Allowed: `gt done` is the sanctioned path and must never trip this.
		{"gt done alone", "gt done", false},
		{"gt done with a target", "gt done --target main", false},
		{"gt slot status alone", "gt slot status", false},
		{"gt slot run wrapped suite", "gt slot run --role gastown/granite -- env GT_TEST_DOCKER=1 go test ./internal/beads/ -run TestOne", false},
		{"gt done after a closed loop, semicolon", `for f in a b; do gofmt -w "$f"; done; gt done`, false},
		{"gt done after a closed loop, &&", `for f in a b; do gofmt -w "$f"; done && gt done`, false},
		{"gt done before a loop", "gt done && for f in a b; do echo $f; done", false},
		{"loop with no gate command", "while read -r x; do bd close \"$x\"; done < list", false},
		{"for loop over files", `for f in $(git diff --name-only); do gofmt -w "$f"; done`, false},
		{"a quoted for is not a block", `grep -rn "for" --include="*.go" .`, false},
		{"seq with no gate command", "seq 1 5", false},
		{"xargs with no gate command", "seq 1 5 | xargs -I{} echo {}", false},

		// A repeat wrapper is judged on its OWN payload. The gate after the
		// pipeline runs once, and a pipeline that merely names the gate in an
		// argument is a search pattern, not an invocation (gt-7dxw review).
		{"xargs pipeline followed by the sanctioned gt done", `git diff --name-only | xargs gofmt -w && gt done`, false},
		{"xargs reading code that names the gate", `rg -l Foo | xargs grep -n "gt slot"`, false},
		{"xargs pipeline before a commit message naming the gate", `seq 1 5 | xargs gofmt -l; git commit -m "fix gt done retry message"`, false},
		{"xargs beside a mail body naming the gate", "seq 1 5 | xargs echo; gt mail send gastown/witness -s HELP --stdin <<'BODY'\ngt done failed, do not retry\nBODY", false},
		{"a closed loop followed by the sanctioned gt done", "while true; do echo hi; done && gt done", false},

		// Taking and releasing the slot is bounded work, so repeating those
		// subcommands is not a busy-wait (gt-7dxw review).
		{"sequential slot runs over packages", "for p in ./internal/a ./internal/b; do gt slot run --role gastown/pearl -- go test $p; done", false},
		{"slot reap over stale holders", "for h in a b; do gt slot reap --dry-run; done", false},

		// Prose must stay readable: a mail body or a notes file that merely
		// discusses the rule is not itself a retry loop.
		{"mail body mentioning gt done", "gt mail send gastown/witness -s \"HELP\" --stdin <<'BODY'\ngt done failed; do not retry\nBODY", false},
		{"notes file with prose", "cat > /tmp/notes.md <<'EOF'\nNever poll the slot. Run gt done once. For instance, the incident.\nEOF", false},
		{"notes file with prose starting a clause", "cat > /tmp/notes.md <<'EOF'\nDo not loop. while the gate is busy, wait; gt done is bounded.\nEOF", false},
		{"notes file with line-initial prose", "cat > /tmp/notes.md <<'EOF'\nWhile gt done waits for the slot, do not retry.\nEOF", false},
		{"notes file with a line-initial For", "cat > /tmp/notes.md <<'EOF'\nFor gt slot to free, wait; do not poll.\nEOF", false},
		{"a stderr redirect does not make the body a script", "gt mail send gastown/witness -s HELP --stdin <<'BODY' 2>/dev/null\ngt done failed; do not retry\nBODY", false},
		{"a commit-message heredoc is data", "cat <<'EOF' | git commit -F -\ngt done failed; do not retry\nEOF", false},
		{"a notes file whose prose names a loop", "cat > /tmp/notes.md <<'EOF'\nNever run a loop around gt done; the gate is what stalls.\nEOF", false},
	}
	blocked := 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := shellTokenize(tt.command)
			reason, _ := matchesDoneSlotLoop(tt.command, tokens, doneSlotLoopLower(tokens))
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("matchesDoneSlotLoop(%q) blocked=%v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
			if got {
				blocked++
			}
		})
	}
	if blocked != 20 {
		t.Errorf("blocked %d of %d cases, want exactly 20", blocked, len(tests))
	}
}

// The wiring: evaluateDangerousCommand is what runTapGuardDangerous calls, and
// it must reach the rule for every role and every cwd (the incident ran from a
// polecat sandbox, but a scripted retry is never the sanctioned path for
// anyone).
func TestDoneSlotLoopReachesGuard(t *testing.T) {
	const incident = `for i in $(seq 1 300); do if gt slot status | grep -q free; then gt done; fi; done`
	reason, alt := evaluateDangerousCommand(incident, 0, "")
	if reason != doneSlotLoopReason {
		t.Fatalf("evaluateDangerousCommand(%q) reason = %q, want %q", incident, reason, doneSlotLoopReason)
	}
	if alt == "" {
		t.Error("no alternative line: the refusal must name the sanctioned path")
	}
	// A single `gt done` is the sanctioned path and must stay untouched.
	if reason, _ := evaluateDangerousCommand("gt done", 0, ""); reason != "" {
		t.Errorf("evaluateDangerousCommand(\"gt done\") blocked: %q", reason)
	}
	// Nested payloads are judged the same way: the loop lives inside a
	// quoted -c argument, a command substitution, or an xargs payload, and
	// none of those is visible to the matcher on the top-level tokens alone.
	for _, nested := range []string{
		`bash -c "while true; do gt done; done"`,
		`bash -c "for i in $(seq 1 5); do gt done; done"`,
		"echo $(for i in 1 2; do gt slot status; done)",
		`seq 1 300 | xargs -I{} bash -c 'gt done'`,
	} {
		if reason, _ := evaluateDangerousCommand(nested, 0, ""); reason != doneSlotLoopReason {
			t.Errorf("evaluateDangerousCommand(%q) reason = %q, want %q", nested, reason, doneSlotLoopReason)
		}
	}
}

// Through the real hook entry point, asserting the text a polecat actually
// receives on stderr: the block banner, the reason, and the alternative that
// names the one-shot escalation path (gt-7dxw, and the gt-pnkd ruling the
// incident produced).
func TestRunTapGuardDangerous_DoneSlotLoopRefusalText(t *testing.T) {
	t.Setenv("GT_POLECAT", "ruby")
	t.Setenv("GT_ROLE", "gastown/polecats/ruby")
	t.Setenv("GT_POLECAT_PATH", "/tmp/polecats/ruby")
	t.Chdir(t.TempDir())

	command := `for i in $(seq 1 300); do gt done; done`
	input := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(command) + `}}`

	var err error
	stderr := captureStderr(t, func() {
		withStdin(t, input, func() { err = runTapGuardDangerous(tapGuardDangerousCmd, nil) })
	})
	if err == nil {
		t.Fatal("the slot-polling gt done loop was allowed through the hook")
	}
	for _, want := range []string{
		"DANGEROUS COMMAND BLOCKED",
		doneSlotLoopReason,
		"run `gt done` once",
		"gt escalate -s medium",
		"--skip-verify",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal text is missing %q:\n%s", want, stderr)
		}
	}
}

// A single `gt done` must survive the whole hook path in a polecat session:
// the rule exists to stop the loop around it, not the command itself.
func TestRunTapGuardDangerous_PlainGtDoneAllowed(t *testing.T) {
	t.Setenv("GT_POLECAT", "ruby")
	t.Setenv("GT_ROLE", "gastown/polecats/ruby")
	t.Setenv("GT_POLECAT_PATH", "/tmp/polecats/ruby")
	t.Chdir(t.TempDir())

	input := `{"tool_name":"Bash","tool_input":{"command":"gt done"}}`
	var err error
	stderr := captureStderr(t, func() {
		withStdin(t, input, func() { err = runTapGuardDangerous(tapGuardDangerousCmd, nil) })
	})
	if err != nil {
		t.Errorf("plain `gt done` was blocked: err=%v stderr=%s", err, stderr)
	}
}

// The heredoc-written script path, through the hook: this is the incident's
// own authoring shape, and the body is invisible to every other rule because
// stripHeredocBodies removes it before the tokens are built.
func TestRunTapGuardDangerous_HeredocWrittenRetryScriptBlocked(t *testing.T) {
	t.Setenv("GT_POLECAT", "ruby")
	t.Setenv("GT_POLECAT_PATH", "/tmp/polecats/ruby")
	t.Chdir(t.TempDir())

	command := "cat > /tmp/retry.sh <<'EOF'\nfor i in $(seq 1 300); do gt done; done\nEOF"
	input := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(command) + `}}`
	var err error
	stderr := captureStderr(t, func() {
		withStdin(t, input, func() { err = runTapGuardDangerous(tapGuardDangerousCmd, nil) })
	})
	if err == nil {
		t.Fatalf("writing a retry script was allowed through the hook: %s", stderr)
	}
	if !strings.Contains(stderr, doneSlotLoopReason) {
		t.Errorf("refusal text is missing the reason:\n%s", stderr)
	}
}
