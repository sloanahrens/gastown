package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdshim"
	"github.com/steveyegge/gastown/internal/bdshim/flagrecord"
)

// helpBD answers bd's help for the arguments a test names, in cobra's layout.
type helpBD map[string]string

func (f helpBD) run(dir string, args []string) ([]byte, int, error) {
	if out, ok := f[strings.Join(args, " ")]; ok {
		return []byte(out), 0, nil
	}
	return []byte("bd: no such command"), 1, nil
}

// fakeHelp is a bd whose help the walker records: a persistent flag, a value
// flag with a shorthand, and an operand.
var fakeHelp = helpBD{
	"version": "bd version 1.2.2 (dev) schema<=66\n",
	"--help": `Usage:
  bd [flags]
  bd [command]

Working With Issues:
  close             Close one or more issues
  create            Create a new issue

Flags:
      --db string   Database path
`,
	"create --help": `Usage:
  bd create [title] [flags]

Flags:
      --title string   Title
`,
	"close --help": `Usage:
  bd close [id...] [flags]

Flags:
  -r, --reason string   Reason
`,
}

// recordHelpBD is the table fakeHelp's help records.
func recordHelpBD(t *testing.T) []byte {
	t.Helper()
	body, err := flagrecord.RecordFlagFile(fakeHelp.run)
	if err != nil {
		t.Fatalf("recording the fake bd's help: %v", err)
	}
	return body
}

func TestBdShimFlags_Metadata(t *testing.T) {
	t.Parallel()
	check := NewBdShimFlagsCheck()
	if check.Name() != "bd-shim-flags" {
		t.Errorf("Name() = %q, want %q", check.Name(), "bd-shim-flags")
	}
	if check.Category() != CategoryInfrastructure {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryInfrastructure)
	}
	if check.CanFix() {
		t.Error("CanFix() should return false: the recorded file and flags.go change together, by hand")
	}
}

// TestBdShimFlags_HoldsARecording: the table the check compares with is what
// bd's help records, so a difference it reports is drift and not a copy
// someone edited.
func TestBdShimFlags_HoldsARecording(t *testing.T) {
	t.Parallel()
	recorded := recordHelpBD(t)
	for _, want := range []string{
		"persistent\t--db\n",
		"operand\tclose\t[id...]\n",
		"value\tclose\t--reason -r\n",
		"value\tcreate\t--title\n",
	} {
		if !strings.Contains(string(recorded), want) {
			t.Errorf("the recording lacks %q:\n%s", want, recorded)
		}
	}
}

// TestBdShimFlags_MatchesBDsHelp: a bd whose help records the table the check
// holds passes, naming the version it compared.
func TestBdShimFlags_MatchesBDsHelp(t *testing.T) {
	t.Parallel()
	result := (&BdShimFlagsCheck{run: fakeHelp.run, recorded: recordHelpBD(t)}).Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v: %s %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "1.2.2") {
		t.Errorf("message %q lacks the version compared", result.Message)
	}
}

// TestBdShimFlags_ComparesAgainstTheEmbeddedTable: a check with no table
// injected compares bd with the recording compiled into gt, which is the table
// the operator's upgrade has to be measured against.
func TestBdShimFlags_ComparesAgainstTheEmbeddedTable(t *testing.T) {
	t.Parallel()
	recorded := recordHelpBD(t)
	want, err := flagrecord.Compare(bdshim.RecordedFlagFile, recorded)
	if err != nil {
		t.Fatalf("comparing the embedded table with the fake bd's help: %v", err)
	}
	if want == nil {
		t.Fatal("the fake bd records the embedded table, so this test has nothing to compare")
	}
	result := (&BdShimFlagsCheck{run: fakeHelp.run}).Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want a warning against the embedded table: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, want.String()) {
		t.Errorf("message %q lacks %q, the difference from the embedded table", result.Message, want.String())
	}
}

// TestBdShimFlags_WarnsOnANewValueFlag: a bd upgrade that makes a flag take a
// value is the drift the shim most needs to hear about, so the warning names
// the command and the flag and prints the refresh steps.
func TestBdShimFlags_WarnsOnANewValueFlag(t *testing.T) {
	t.Parallel()
	upgraded := helpBD{}
	for k, v := range fakeHelp {
		upgraded[k] = v
	}
	upgraded["create --help"] = strings.Replace(fakeHelp["create --help"], "      --title string   Title\n",
		"      --deps strings   Dependencies\n      --title string   Title\n", 1)
	upgraded["version"] = "bd version 1.2.3 (dev) schema<=66\n"

	result := (&BdShimFlagsCheck{run: upgraded.run, recorded: recordHelpBD(t)}).Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want a warning: %s %v", result.Status, result.Message, result.Details)
	}
	for _, want := range []string{"`bd create`", "--deps", "1.2.3"} {
		if !strings.Contains(result.Message+strings.Join(result.Details, "\n"), want) {
			t.Errorf("the report lacks %q: %s %v", want, result.Message, result.Details)
		}
	}
	for _, want := range []string{
		"go run ./cmd/bdshim-record -flags -check",
		"go run ./cmd/bdshim-record -flags",
		"internal/bdshim/flags.go",
		"bd 1.2.2",
	} {
		if !strings.Contains(strings.Join(result.Details, "\n"), want) {
			t.Errorf("the details lack %q: %v", want, result.Details)
		}
	}
	if !strings.Contains(result.FixHint, "go run ./cmd/bdshim-record -flags") {
		t.Errorf("FixHint = %q, want the refresh command", result.FixHint)
	}
}

// TestBdShimFlags_WarnsOnADroppedRow: a command bd no longer documents is
// drift too, and the report names the command.
func TestBdShimFlags_WarnsOnADroppedRow(t *testing.T) {
	t.Parallel()
	shrunk := helpBD{}
	for k, v := range fakeHelp {
		shrunk[k] = v
	}
	delete(shrunk, "close --help")
	shrunk["--help"] = strings.Replace(fakeHelp["--help"], "  close             Close one or more issues\n", "", 1)

	result := (&BdShimFlagsCheck{run: shrunk.run, recorded: recordHelpBD(t)}).Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want a warning: %s %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "`bd close`") {
		t.Errorf("message %q lacks the command bd dropped", result.Message)
	}
}

// TestBdShimFlags_ReportsAVersionOnlyChange: the heading names the bd the
// table was recorded from, so an upgrade that moves no flag is still worth
// re-recording and the report says which version is installed.
func TestBdShimFlags_ReportsAVersionOnlyChange(t *testing.T) {
	t.Parallel()
	newer := helpBD{}
	for k, v := range fakeHelp {
		newer[k] = v
	}
	newer["version"] = "bd version 1.2.3 (dev) schema<=66\n"

	result := (&BdShimFlagsCheck{run: newer.run, recorded: recordHelpBD(t)}).Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want a warning: %s %v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"bd 1.2.2", "bd 1.2.3"} {
		if !strings.Contains(details, want) {
			t.Errorf("details lack %q: %v", want, result.Details)
		}
	}
}

// TestBdShimFlags_SkipsWithoutBD: no bd on PATH is a report, not an error and
// not a pass.
func TestBdShimFlags_SkipsWithoutBD(t *testing.T) {
	t.Parallel()
	check := NewBdShimFlagsCheck()
	check.lookPath = func(string) (string, error) {
		return "", errors.New("exec: \"bd\": executable file not found in $PATH")
	}
	result := check.Run(&CheckContext{})
	if result.Status != StatusSkipped {
		t.Fatalf("status = %v, want skipped: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "not on PATH") {
		t.Errorf("message %q does not say bd is missing", result.Message)
	}
}

// TestBdShimFlags_SkipsWhenTheHelpCannotBeRead: a bd that will not answer its
// help leaves the table unverified, which is skipped rather than passed.
func TestBdShimFlags_SkipsWhenTheHelpCannotBeRead(t *testing.T) {
	t.Parallel()
	failing := func(dir string, args []string) ([]byte, int, error) {
		if args[0] == "version" {
			return []byte("bd version 1.2.2 (dev) schema<=66\n"), 0, nil
		}
		return nil, 0, errors.New("bd was killed by a signal")
	}
	result := (&BdShimFlagsCheck{run: failing, recorded: recordHelpBD(t)}).Run(&CheckContext{})
	if result.Status != StatusSkipped {
		t.Fatalf("status = %v, want skipped: %s", result.Status, result.Message)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "killed by a signal") {
		t.Errorf("details lack the run's error: %v", result.Details)
	}
}

// TestBdShimFlags_ReadsOnlyBDsHelp: the check asks bd for its version and its
// help and nothing else, so it cannot refresh the table behind the operator's
// back.
func TestBdShimFlags_ReadsOnlyBDsHelp(t *testing.T) {
	t.Parallel()
	var calls [][]string
	recording := func(dir string, args []string) ([]byte, int, error) {
		calls = append(calls, append([]string{}, args...))
		return fakeHelp.run(dir, args)
	}
	result := (&BdShimFlagsCheck{run: recording, recorded: recordHelpBD(t)}).Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v: %s %v", result.Status, result.Message, result.Details)
	}
	if len(calls) == 0 {
		t.Fatal("the check ran no bd command")
	}
	for _, args := range calls {
		if len(args) == 1 && args[0] == "version" {
			continue
		}
		if args[len(args)-1] == "--help" {
			continue
		}
		t.Errorf("the check ran bd %v, want only version and --help", args)
	}
}

// TestBdShimFlags_SkipsAFileItCannotRead: a recorded table with a row this
// build does not know is unverified, never a pass or a guessed difference.
func TestBdShimFlags_SkipsAFileItCannotRead(t *testing.T) {
	t.Parallel()
	result := (&BdShimFlagsCheck{run: fakeHelp.run, recorded: []byte("flags\tcreate\t--title\n")}).Run(&CheckContext{})
	if result.Status != StatusSkipped {
		t.Fatalf("status = %v, want skipped: %s %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "could not be compared") {
		t.Errorf("message %q does not say the comparison could not run", result.Message)
	}
}
