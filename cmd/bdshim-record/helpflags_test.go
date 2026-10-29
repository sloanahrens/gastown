package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helpBD answers bd's help for the commands a test names, in cobra's layout.
type helpBD map[string]string

func (f helpBD) run(dir string, args []string) ([]byte, int, error) {
	if out, ok := f[strings.Join(args, " ")]; ok {
		return []byte(out), 0, nil
	}
	return []byte("bd: no such command"), 1, nil
}

// testHelp is a small bd: persistent flags, a value flag that is a boolean
// elsewhere, a flag declared on a command as well as globally, an optional
// value, and a command three words deep.
var testHelp = helpBD{
	"version": "bd version 1.2.2 (dev) schema<=66\n",
	"--help": `Issues chained together like beads.

Usage:
  bd [flags]
  bd [command]

Working With Issues:
  close             Close one or more issues
  create            Create a new issue
  epic              Epic commands
  list              List issues

Managed:
  gate              Manage gates
  mol               Molecule commands

Sync & Data:
  backup            Back up the database

Flags:
      --actor string              Actor name
      --db string                 Database path
  -C, --directory string          Change directory
      --json                      Output in JSON format
`,
	"create --help": `Create a new issue

Usage:
  bd create [title] [flags]

Aliases:
  create, new

Flags:
  -a, --assignee string    Assignee
      --deps strings       Dependencies
      --dry-run            Preview
  -e, --estimate int       Estimate in minutes
  -f, --file string        Read issues from a file
  -h, --help               help for create
  -l, --labels strings     Labels
      --title string       Title
      --actor string       Actor name

Global Flags:
      --db string   Database path
`,
	"close --help": `Usage:
  bd close [issue-id...] [flags]

Flags:
  -f, --force           Force
  -r, --reason string   Reason
`,
	"list --help": `Usage:
  bd list [flags]

Flags:
      --deps string[="scheduling"]   Dependency edges
  -l, --label string                 Label
  -n, --limit int                    Limit results
  -r, --reverse                      Reverse the sort
`,
	"backup --help": `Usage:
  bd backup [command]

Available Commands:
  init   Set up the database
`,
	"backup init --help": `Usage:
  bd backup init [flags]

Aliases:
  init, add

Flags:
      --remote string   Remote to copy from
`,
	"epic --help": `Usage:
  bd epic [command]

Available Commands:
  close-eligible Close epics where all children are complete
  status         Show epic completion status
`,
	"epic close-eligible --help": `Usage:
  bd epic close-eligible [flags]

Flags:
  -r, --reason string   Reason
`,
	"epic status --help": `Usage:
  bd epic status [flags]

Flags:
      --json   Output JSON
`,
	"gate --help": `Usage:
  bd gate [command]

Available Commands:
  check       Evaluate gates
  discover    Discover gates
  list        List gates

Flags:
  -h, --help   help for gate
`,
	"gate discover --help": `Usage:
  bd gate discover [flags]

Flags:
  -n, --dry-run   Report without writing
`,
	"gate check --help": `Usage:
  bd gate check [flags]

Flags:
  -e, --escalate   Escalate the gate
      --limit int  Limit results
`,
	"gate list --help": `Usage:
  bd gate list [flags]

Flags:
  -a, --all   Show closed gates
`,
	"mol --help": `Usage:
  bd mol [command]

Available Commands:
  wisp   Wisp commands
`,
	"mol wisp --help": `Usage:
  bd mol wisp [command]

Available Commands:
  gc   Garbage collect wisps
`,
	"mol wisp gc --help": `Usage:
  bd mol wisp gc [flags]

Flags:
      --age duration   Wisp age
`,
}

// rowsOf returns the recording's rows, without its comment heading, so a test
// compares what the shim reads and not the prose that explains it.
func rowsOf(t *testing.T, body []byte) string {
	t.Helper()
	var rows []string
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

// TestRecordFlagFile: the recording holds every command bd documents a value
// for, with the short and long spelling of each flag, a value flag that is a
// boolean elsewhere in the bool row, and the persistent set once.
func TestRecordFlagFile(t *testing.T) {
	t.Parallel()
	body, err := RecordFlagFile(testHelp.run)
	if err != nil {
		t.Fatalf("RecordFlagFile = %v, want no error", err)
	}
	if !strings.Contains(string(body), "# What bd 1.2.2 reads a value for") {
		t.Errorf("the heading does not name the bd whose help was recorded:\n%s", body)
	}
	want := strings.Join([]string{
		"persistent\t--actor --db --directory -C",
		"alias\tbackup add\tbackup init",
		"alias\tnew\tcreate",
		"value\tbackup init\t--remote",
		"value\tclose\t--reason -r",
		"value\tcreate\t--assignee --deps --estimate --file --labels --title -a -e -f -l",
		"value\tepic close-eligible\t--reason -r",
		"value\tgate check\t--limit",
		"value\tlist\t--label --limit -l -n",
		"value\tmol wisp gc\t--age",
		"bool\tclose\t-f",
		"bool\tgate check\t-e",
		"bool\tgate discover\t-n",
		"bool\tgate list\t-a",
		"bool\tlist\t--deps -r",
	}, "\n")
	if got := rowsOf(t, body); got != want {
		t.Errorf("RecordFlagFile rows =\n%s\nwant\n%s", got, want)
	}
}

// TestRecordFlagFileRefusesAHelpItCannotRead: a bd that does not answer a
// command's help fails the recording, so a partial file is never written.
func TestRecordFlagFileRefusesAHelpItCannotRead(t *testing.T) {
	t.Parallel()
	broken := helpBD{
		"version":      "bd version 1.2.2 (dev) schema<=66\n",
		"--help":       testHelp["--help"],
		"close --help": testHelp["close --help"],
	}
	if _, err := RecordFlagFile(broken.run); err == nil || !strings.Contains(err.Error(), "create --help exited 1") {
		t.Errorf("RecordFlagFile with a bd that refuses create = %v, want the command and its exit code named", err)
	}
}

// TestRecordFlagFileRefusesAnUnreadableVersion: the heading names the version,
// so a bd whose version cannot be read is not recorded.
func TestRecordFlagFileRefusesAnUnreadableVersion(t *testing.T) {
	t.Parallel()
	broken := helpBD{"version": "usage: bd version\n"}
	if _, err := RecordFlagFile(broken.run); err == nil || !strings.Contains(err.Error(), "bd version printed") {
		t.Errorf("RecordFlagFile with a bd that prints usage = %v, want the version line refused", err)
	}
}

// TestRecordFlagFileRefusesARunThatCannotStart: a bd that cannot be run at
// all is an error, not an empty recording.
func TestRecordFlagFileRefusesARunThatCannotStart(t *testing.T) {
	t.Parallel()
	broken := func(dir string, args []string) ([]byte, int, error) {
		return nil, 0, errors.New("no such file or directory")
	}
	if _, err := RecordFlagFile(broken); err == nil || !strings.Contains(err.Error(), "bd version: no such file or directory") {
		t.Errorf("RecordFlagFile with a bd that cannot start = %v, want the version run named", err)
	}
	failing := func(dir string, args []string) ([]byte, int, error) {
		if args[0] == "version" {
			return []byte("bd version 1.2.2 (dev) schema<=66\n"), 0, nil
		}
		return nil, 0, errors.New("bd was killed by a signal")
	}
	if _, err := RecordFlagFile(failing); err == nil || !strings.Contains(err.Error(), "bd --help: bd was killed by a signal") {
		t.Errorf("RecordFlagFile with a help run that cannot start = %v, want the command named", err)
	}
}

// TestRecordFlagFileRefusesAVersionThatExitsNonZero: bd answering its version
// with a failure is refused, so a heading never names a version bd did not give.
func TestRecordFlagFileRefusesAVersionThatExitsNonZero(t *testing.T) {
	t.Parallel()
	broken := func(dir string, args []string) ([]byte, int, error) {
		return []byte(""), 1, nil
	}
	if _, err := RecordFlagFile(broken); err == nil || !strings.Contains(err.Error(), "bd version exited 1") {
		t.Errorf("RecordFlagFile with a bd whose version exits 1 = %v, want the exit code named", err)
	}
}

// TestRecordFlagsCheck: -check reports whether the recorded file still matches
// bd's help and writes nothing, so a bd upgrade has a command that names it.
func TestRecordFlagsCheck(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bd-flags.txt")
	var stderr bytes.Buffer
	if code := recordFlags(options{out: path}, testHelp.run, &stderr); code != 0 {
		t.Fatalf("recordFlags = %d, want 0: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "wrote "+path) {
		t.Errorf("stderr = %q, want the file it wrote named", stderr.String())
	}

	stderr.Reset()
	if code := recordFlags(options{out: path, check: true}, testHelp.run, &stderr); code != 0 {
		t.Fatalf("recordFlags -check on its own output = %d, want 0: %s", code, stderr.String())
	}

	stale := []byte("value\tcreate\t--title\n")
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatalf("writing a stale recording: %v", err)
	}
	stderr.Reset()
	if code := recordFlags(options{out: path, check: true}, testHelp.run, &stderr); code != 1 {
		t.Fatalf("recordFlags -check on a stale file = %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "differs from bd's help") {
		t.Errorf("stderr = %q, want the drift named", stderr.String())
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, stale) {
		t.Error("-check rewrote the file, want it left untouched")
	}
}
