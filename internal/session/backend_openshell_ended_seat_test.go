package session

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// TestOpenShellPrepareReplacesAnEndedSandboxHoldingTheName is the rework sling
// of a seat whose sandbox the hold kept up (gt-vgm971): the create finds the
// name held, the gateway reports the sandbox for get, the ended marker is set,
// and the spawn saves the old seat's transcripts, deletes the sandbox and
// creates again, in that order.
func TestOpenShellPrepareReplacesAnEndedSandboxHoldingTheName(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	old := replacedSeatRecord()
	if err := WriteSandboxRecord(town, old); err != nil {
		t.Fatal(err)
	}
	oldTranscript := seatRemote("ses-old.jsonl")
	gw := &replacedSeatGateway{held: true, marker: "present", listing: listingLine(oldTranscript, old.CreatedAt.Add(time.Minute), 15)}
	var recordsAtCreate []int
	gw.onCreate = func(int) {
		entries, err := os.ReadDir(SandboxRegistryDir(town))
		if err != nil {
			t.Errorf("reading the registry at a create: %v", err)
		}
		recordsAtCreate = append(recordsAtCreate, len(entries))
	}
	b, _ := openshellTestBackend(town, openshellTestProfile, &sandbox.Fake{})
	run := gw.run
	// A download writes the body of the transcript the copy asks for, as the
	// sandbox download command does.
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		out, errOut, err := run(ctx, argv, stdin, env)
		if err == nil && slices.Contains(argv, "download") {
			dir := argv[len(argv)-1]
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, path.Base(argv[len(argv)-2])), []byte("old transcript"), 0o644); err != nil {
				return nil, nil, err
			}
		}
		return out, errOut, err
	}

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// The old run's record is gone by the second create: a record left beside a
	// deleted sandbox would route commands to it until the patrol removed it.
	if !slices.Equal(recordsAtCreate, []int{1, 0}) {
		t.Errorf("records at each create = %v, want the old record removed before the second create", recordsAtCreate)
	}
	copied, _, err := CopiedSandboxTranscripts(town, openshellTestRig, openshellTestPolecat)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 || path.Base(copied[0]) != "ses-old.jsonl" {
		t.Errorf("copied transcripts = %v, want the old seat's transcript saved before the delete", copied)
	}

	want := []string{"create", "get", "exec", "exec", "download", "delete", "create"}
	got := verbsOf(gw.calls)
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("first calls = %q, want %q (the held sandbox must be saved and deleted before the second create)", got, want)
	}
	// The two execs are found by what they ask, not by their position, so a
	// call added before them does not move the assertion.
	markerAt := slices.IndexFunc(gw.calls, func(call []string) bool { return slices.Contains(call, endedMarkerScript) })
	listAt := slices.IndexFunc(gw.calls, func(call []string) bool { return slices.Contains(call, sandboxProjectsDir) })
	deleteAt := slices.IndexFunc(gw.calls, func(call []string) bool { return openshellTestVerb(call) == "delete" })
	if markerAt < 0 || listAt < 0 || deleteAt < 0 || markerAt > listAt || listAt > deleteAt {
		t.Errorf("calls = %q, want the ended marker question, then the transcript listing, before the delete", got)
	}

	rec, ok, err := ReadSandboxRecord(town, openshellTestSession)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if !rec.CreatedAt.After(old.CreatedAt) {
		t.Errorf("record created_at = %v, want a record written after the old run's %v", rec.CreatedAt, old.CreatedAt)
	}
	if rec.Branch != openshellTestBranch || rec.WorkDir != openshellTestWorkDir {
		t.Errorf("record = %+v, want the new run's branch and work dir", rec)
	}
	entries, err := os.ReadDir(SandboxRegistryDir(town))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("registry holds %d records, want one for the seat name", len(entries))
	}
}

// replacedSeatOpts is one create the gateway refuses over an existing sandbox.
type replacedSeatOpts struct {
	held               bool   // the gateway has a sandbox of the name
	record             bool   // write the old run's record first
	corruptRecord      bool   // write an unreadable record instead
	recordGateway      string // the record's gateway; "" is the profile's
	getErr             error  // the gateway cannot say whether the name is held
	marker             string // the ended marker answer: "present", "absent" or ""
	markerErr          error  // the ended marker exec fails
	listErr            error  // the transcript listing exec fails
	deleteErr          error  // the delete fails
	metricsFile        bool   // a file blocks the seat-end ledger the record removal writes
	transcriptsBlocked bool   // a file blocks the transcript copy's directory
	secondRefused      bool   // the create after the delete is refused too
}

// Every answer short of a set marker over a recorded sandbox the gateway still
// has keeps the sandbox: the spawn fails, naming the reason, and the create runs
// no more than once more (gt-vgm971).
func TestOpenShellPrepareCreateOverAHeldSeatRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		opts        replacedSeatOpts
		wantErr     string
		wantDeletes int
		wantCreates int
	}{
		{
			name:        "no sandbox holds the name",
			opts:        replacedSeatOpts{},
			wantErr:     "creating sandbox",
			wantCreates: 1,
		},
		{
			name:        "the gateway cannot say whether the name is held",
			opts:        replacedSeatOpts{getErr: errors.New("gateway unreachable")},
			wantErr:     "asking the gateway whether an ended sandbox holds the name",
			wantCreates: 1,
		},
		{
			name:        "the held sandbox has no record",
			opts:        replacedSeatOpts{held: true},
			wantErr:     "no record of it, so its seat is unknown",
			wantCreates: 1,
		},
		{
			name:        "the held sandbox's record cannot be read",
			opts:        replacedSeatOpts{held: true, corruptRecord: true},
			wantErr:     "reading the record of the sandbox holding the name",
			wantCreates: 1,
		},
		{
			name:        "the record names another gateway",
			opts:        replacedSeatOpts{held: true, record: true, recordGateway: "elsewhere"},
			wantErr:     `names gateway "elsewhere"`,
			wantCreates: 1,
		},
		{
			name:        "the held sandbox has no ended marker",
			opts:        replacedSeatOpts{held: true, record: true, marker: "absent"},
			wantErr:     "creating sandbox",
			wantCreates: 1,
		},
		{
			name:        "the ended marker question fails",
			opts:        replacedSeatOpts{held: true, record: true, markerErr: errors.New("exec failed")},
			wantErr:     "asking the sandbox holding the name for its ended marker",
			wantCreates: 1,
		},
		{
			name:        "the transcripts cannot be copied",
			opts:        replacedSeatOpts{held: true, record: true, marker: "present", listErr: errors.New("exec failed")},
			wantErr:     "copying its transcripts out failed",
			wantCreates: 1,
		},
		{
			name:        "the ended sandbox cannot be deleted",
			opts:        replacedSeatOpts{held: true, record: true, marker: "present", deleteErr: errors.New("delete failed")},
			wantErr:     "deleting the ended sandbox holding the name",
			wantDeletes: 1,
			wantCreates: 1,
		},
		{
			name:        "the transcript copy has nowhere to write",
			opts:        replacedSeatOpts{held: true, record: true, marker: "present", transcriptsBlocked: true},
			wantErr:     "creating its transcript copy failed",
			wantCreates: 1,
		},
		{
			name:        "the record of the ended sandbox cannot be removed",
			opts:        replacedSeatOpts{held: true, record: true, marker: "present", metricsFile: true},
			wantErr:     "removing the record of the ended sandbox holding the name",
			wantDeletes: 1,
			wantCreates: 1,
		},
		{
			name:        "the create is refused again after the delete",
			opts:        replacedSeatOpts{held: true, record: true, marker: "present", secondRefused: true},
			wantErr:     "creating sandbox",
			wantDeletes: 1,
			wantCreates: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			if tt.opts.record {
				rec := replacedSeatRecord()
				if tt.opts.recordGateway != "" {
					rec.Gateway = tt.opts.recordGateway
				}
				if err := WriteSandboxRecord(town, rec); err != nil {
					t.Fatal(err)
				}
			}
			if tt.opts.corruptRecord {
				if err := os.MkdirAll(SandboxRegistryDir(town), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(sandboxRecordPath(town, openshellTestSession), []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.opts.metricsFile {
				// A file where the seat-end ledger's directory goes fails the
				// ledger write the record removal makes.
				if err := os.MkdirAll(filepath.Join(town, ".runtime"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(town, ".runtime", "metrics"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.opts.transcriptsBlocked {
				// A dangling symlink where the copy directory's parent goes: the
				// copy's clear finds nothing to remove and its creation cannot
				// follow the link.
				parent := filepath.Dir(SandboxTranscriptCopyDir(town, openshellTestRig, openshellTestPolecat))
				if err := os.MkdirAll(filepath.Dir(parent), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(town, "no-such-transcripts"), parent); err != nil {
					t.Fatal(err)
				}
			}
			gw := &replacedSeatGateway{
				held:          tt.opts.held,
				marker:        tt.opts.marker,
				getErr:        tt.opts.getErr,
				markerErr:     tt.opts.markerErr,
				listErr:       tt.opts.listErr,
				deleteErr:     tt.opts.deleteErr,
				secondRefused: tt.opts.secondRefused,
			}
			b, _ := openshellTestBackend(town, openshellTestProfile, &sandbox.Fake{})
			b.cli.Run = gw.run

			_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			verbs := verbsOf(gw.calls)
			if got := countOf(verbs, "delete"); got != tt.wantDeletes {
				t.Errorf("deletes = %d (%q), want %d", got, verbs, tt.wantDeletes)
			}
			if gw.creates != tt.wantCreates {
				t.Errorf("creates = %d, want %d", gw.creates, tt.wantCreates)
			}
		})
	}
}

// replacedSeatRecord is the record a finished seat leaves: the run whose sandbox
// the hold is keeping up.
func replacedSeatRecord() SandboxRecord {
	return SandboxRecord{
		Session:   openshellTestSession,
		Rig:       openshellTestRig,
		Polecat:   openshellTestPolecat,
		Gateway:   openshellTestProfile.Gateway,
		Profile:   openshellTestProfile.Name,
		Image:     openshellTestProfile.Image,
		CreatedAt: time.Now().Add(-time.Hour).UTC(),
		Branch:    "polecat/sandbox-ace/gt-old",
		WorkDir:   openshellTestWorkDir,
	}
}

// replacedSeatGateway is a sandbox.CLI runner for a spawn over a held seat: the
// first create is refused, get answers whether the name is held, delete answers
// as set, the ended marker and transcript-listing execs answer from the options,
// and every other call succeeds as a healthy spawn's does. It records every argv.
type replacedSeatGateway struct {
	held          bool
	marker        string
	listing       string // the transcript listing's answer
	getErr        error
	markerErr     error
	listErr       error
	deleteErr     error
	secondRefused bool
	onCreate      func(create int) // runs before a create is answered
	creates       int
	calls         [][]string
}

func (g *replacedSeatGateway) run(_ context.Context, argv []string, _ []byte, _ []string) ([]byte, []byte, error) {
	g.calls = append(g.calls, append([]string(nil), argv...))
	switch openshellTestVerb(argv) {
	case "create":
		g.creates++
		if g.onCreate != nil {
			g.onCreate(g.creates)
		}
		if g.creates == 1 || g.secondRefused {
			return nil, []byte(`Error:   × code: 'The entity that a client attempted to create already exists', message: "sandbox already exists"`), errors.New("exit status 1")
		}
		return nil, nil, nil
	case "get":
		if g.getErr != nil {
			return nil, nil, g.getErr
		}
		if !g.held {
			return nil, []byte(notFoundReplyText), errors.New("exit status 1")
		}
		return []byte(openshellReadyReply), nil, nil
	case "delete":
		return nil, nil, g.deleteErr
	case "exec":
		switch {
		case slices.Contains(argv, endedMarkerScript):
			return []byte(g.marker + "\n"), nil, g.markerErr
		case slices.Contains(argv, sandboxProjectsDir):
			return []byte(g.listing), nil, g.listErr
		}
		return []byte(openshellTestVersionReply), nil, nil
	}
	return nil, nil, nil
}

// notFoundReplyText is the gateway's missing-sandbox answer, as openshell prints
// it and CLI.Exists reads it.
const notFoundReplyText = `Error:   × code: 'Some requested entity was not found', message: "sandbox not found"`

// openshellTestVerb is the verb of one openshell argv.
func openshellTestVerb(argv []string) string {
	for i, a := range argv {
		if a == "sandbox" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func verbsOf(calls [][]string) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = openshellTestVerb(c)
	}
	return out
}

func countOf(values []string, want string) int {
	n := 0
	for _, v := range values {
		if v == want {
			n++
		}
	}
	return n
}
