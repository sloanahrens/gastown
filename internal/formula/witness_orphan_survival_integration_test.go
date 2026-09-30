//go:build integration

package formula

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The three orphan-survival blocks parse as bash: one bash -n over all of
// them, each wrapped in its own function.
func TestIntegrationWitnessOrphanSurvivalBlocksParseAsBash(t *testing.T) {
	survives, reset, unknown := orphanSurvivalBlocks(t)
	var script strings.Builder
	for i, b := range []string{survives, reset, unknown} {
		fmt.Fprintf(&script, "block%d() {\n%s}\n", i, fillOrphanPlaceholders(b))
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the blocks are not valid bash: %v\n%s\n%s", err, out, script.String())
	}
}

// orphanStub is a temp PATH with bd and gt stubs. bd keeps the bead's labels
// in a file; every bd write and every gt call is logged.
type orphanStub struct {
	dir, labels, log string
}

func newOrphanStub(t *testing.T) *orphanStub {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell stubs")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	s := &orphanStub{dir: dir, labels: filepath.Join(dir, "labels"), log: filepath.Join(dir, "log")}
	if err := os.WriteFile(s.labels, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	bd := `#!/bin/bash
labels="` + s.labels + `"
log="` + s.log + `"
case "$1" in
show)
  printf '[{"id":"%s","labels":[' "$2"
  sep=""
  while IFS= read -r l; do [ -n "$l" ] && printf '%s"%s"' "$sep" "$l" && sep=","; done < "$labels"
  printf ']}]\n'
  ;;
update)
  echo "bd $*" >> "$log"
  shift 2
  for a in "$@"; do
    case "$a" in
    --if-assignee=*) [ -n "$RESET_FENCE_FAIL" ] && exit 13 ;;
    esac
  done
  while [ $# -gt 0 ]; do
    case "$1" in
    --add-label) grep -qx "$2" "$labels" || echo "$2" >> "$labels"; shift ;;
    --remove-label) grep -vx "$2" "$labels" > "$labels.tmp"; mv "$labels.tmp" "$labels"; shift ;;
    esac
    shift
  done
  ;;
*) echo "unexpected bd $*" >&2; exit 2 ;;
esac
`
	gt := `#!/bin/bash
echo "gt $*" >> "` + s.log + `"
cat > /dev/null
`
	for name, body := range map[string]string{"bd": bd, "gt": gt} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// fillOrphanPlaceholders writes literal IDs where the formula says to, as the
// witness does before running a block.
func fillOrphanPlaceholders(block string) string {
	return strings.NewReplacer(
		"<bead-id>", "gt-orph1",
		"<rig>", "gastown",
		"<name>", "basalt",
		"<branch>", "polecat/basalt/gt-orph1+abc",
	).Replace(block)
}

func (s *orphanStub) run(t *testing.T, block string, env ...string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", fillOrphanPlaceholders(block))
	cmd.Env = append([]string{"PATH=" + s.dir + ":/usr/bin:/bin"}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// A trailing `grep -q … && …` legitimately ends non-zero; only a
		// stub complaint is a failure.
		if strings.Contains(string(out), "unexpected") {
			t.Fatalf("block failed: %v\n%s", err, out)
		}
	}
}

func (s *orphanStub) state(t *testing.T) (labels []string, log string) {
	t.Helper()
	l, _ := os.ReadFile(s.labels)
	for _, x := range strings.Split(strings.TrimSpace(string(l)), "\n") {
		if x != "" {
			labels = append(labels, x)
		}
	}
	g, _ := os.ReadFile(s.log)
	return labels, string(g)
}

func (s *orphanStub) resetLog(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(s.log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The label protocol across patrol cycles: escalate once per run of unknown
// answers, clear the run on any definite answer, mail the mayor once per
// preserved-orphan run, and forget that on a successful reset.
func TestIntegrationWitnessOrphanSurvivalLabelProtocol(t *testing.T) {
	survives, reset, unknown := orphanSurvivalBlocks(t)
	s := newOrphanStub(t)
	count := func(log, sub string) int { return strings.Count(log, sub) }

	// Run 1 of "cannot tell": label, then escalate once, then stay quiet.
	s.run(t, unknown)
	s.run(t, unknown)
	s.run(t, unknown)
	s.run(t, unknown)
	labels, log := s.state(t)
	if got := count(log, "gt escalate"); got != 1 {
		t.Fatalf("escalations in one unknown run = %d, want 1\n%s", got, log)
	}
	if strings.Join(labels, ",") != "gt:survival-unknown,gt:survival-escalated" {
		t.Fatalf("labels after unknown run = %v", labels)
	}

	// A definite "survives" ends the run and mails the mayor once.
	s.resetLog(t)
	s.run(t, survives)
	s.run(t, survives)
	labels, log = s.state(t)
	if strings.Join(labels, ",") != "gt:preserved-orphan" {
		t.Fatalf("labels after exit 0 = %v", labels)
	}
	if got := count(log, "gt mail send mayor/"); got != 1 {
		t.Fatalf("mayor mails = %d, want 1\n%s", got, log)
	}
	if count(log, "--status=open") != 0 {
		t.Fatalf("exit 0 must never reset\n%s", log)
	}

	// A new unknown run escalates again.
	s.resetLog(t)
	s.run(t, unknown)
	s.run(t, unknown)
	s.run(t, unknown)
	_, log = s.state(t)
	if got := count(log, "gt escalate"); got != 1 {
		t.Fatalf("escalations in second unknown run = %d, want 1\n%s", got, log)
	}

	// A reset whose guard no longer holds changes nothing more.
	s.resetLog(t)
	s.run(t, reset, "RESET_FENCE_FAIL=1")
	labels, log = s.state(t)
	if strings.Join(labels, ",") != "gt:preserved-orphan" {
		t.Fatalf("labels after fenced reset = %v (survival labels cleared, preserved kept)", labels)
	}
	if count(log, "gt mail send deacon/") != 0 {
		t.Fatalf("fenced reset must not mail the deacon\n%s", log)
	}

	// A successful reset clears everything and tells the deacon.
	s.resetLog(t)
	s.run(t, reset)
	labels, log = s.state(t)
	if len(labels) != 0 {
		t.Fatalf("labels after reset = %v, want none", labels)
	}
	if count(log, "--if-assignee=gastown/polecats/basalt") != 1 || count(log, "gt mail send deacon/") != 1 {
		t.Fatalf("reset must be guarded and mail the deacon once\n%s", log)
	}

	// A later orphan run whose work survives mails the mayor again.
	s.resetLog(t)
	s.run(t, survives)
	_, log = s.state(t)
	if got := count(log, "gt mail send mayor/"); got != 1 {
		t.Fatalf("mayor mails in a later orphan run = %d, want 1\n%s", got, log)
	}

	// A quiet cycle writes nothing.
	s.resetLog(t)
	s.run(t, survives)
	if _, log = s.state(t); count(log, "bd update") != 0 {
		t.Fatalf("a repeated exit 0 wrote labels\n%s", log)
	}
}
