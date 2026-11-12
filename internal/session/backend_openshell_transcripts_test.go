package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// seatBase is the fixed clock the copy tests read the sandbox's transcript
// modification times from, so no test depends on the wall clock.
var seatBase = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// seatRemote is the sandbox path of a transcript under the test seat's project
// folder, as the sandbox listing names it.
func seatRemote(name string) string {
	return sandboxProjectsDir + "/" + seatFolder() + "/" + name
}

// listingLine is one line of the sandbox listing for the file at remote.
func listingLine(remote string, mtime time.Time, size int) string {
	return fmt.Sprintf("%d.%09d %d %s\n", mtime.Unix(), mtime.Nanosecond(), size, remote)
}

// copyingRunner is the CLI runner a test installs. The sandbox listing answers
// with listing. A download writes the body of the file it names, when copied
// names it, into the directory it was given, and writes nothing otherwise.
// Every call goes on to the fake, so its calls and failures are the fake's.
func copyingRunner(fake *sandbox.Fake, listing string, copied map[string]string) sandbox.Runner {
	run := fake.Runner()
	return func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		out, errOut, err := run(ctx, argv, stdin, env)
		switch {
		case slices.Contains(argv, "find"):
			return []byte(listing), errOut, err
		case slices.Contains(argv, "download"):
			if err != nil {
				return out, errOut, err
			}
			remote, localDir := argv[len(argv)-2], argv[len(argv)-1]
			if body, ok := copied[remote]; ok {
				if err := os.MkdirAll(localDir, 0o755); err != nil {
					return nil, nil, err
				}
				if err := os.WriteFile(filepath.Join(localDir, path.Base(remote)), []byte(body), 0o644); err != nil {
					return nil, nil, err
				}
			}
		}
		return out, errOut, err
	}
}

// seatFolder is the project folder Claude Code names for the test seat's sandbox
// clone: the folder a session run in /sandbox/gt/<rig>/polecats/<polecat>/<rig>
// writes to.
func seatFolder() string {
	return fmt.Sprintf("-sandbox-gt-%s-polecats-%s-%s", openshellTestRig, openshellTestPolecat, openshellTestRig)
}

// callIndex is the index of the first call whose argv holds verb, or -1.
func callIndex(calls [][]string, verb string) int {
	for i, c := range calls {
		if slices.Contains(c, verb) {
			return i
		}
	}
	return -1
}

// downloadedRemotes is the sandbox path each download in calls named, in order.
func downloadedRemotes(calls [][]string) []string {
	var remotes []string
	for _, c := range calls {
		if slices.Contains(c, "download") {
			remotes = append(remotes, c[len(c)-2])
		}
	}
	return remotes
}

func TestOpenShellOpsDeleteCopiesTheTranscriptsBeforeTheDelete(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	remote := seatRemote("ses-1.jsonl")
	b.cli.Run = copyingRunner(fake, listingLine(remote, seatBase, 4), map[string]string{remote: `{"type":"assistant"}`})
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	calls := fake.Calls()
	down, del := callIndex(calls, "download"), callIndex(calls, "delete")
	if down < 0 || del < 0 || down > del {
		t.Fatalf("openshell calls = %v, want a download before the delete", calls)
	}
	copyDir := SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)
	if got := calls[down][len(calls[down])-2:]; !reflect.DeepEqual(got, []string{remote, filepath.Join(copyDir, seatFolder())}) {
		t.Errorf("download = %v, want the transcript into the seat's copy folder", calls[down])
	}
	copied, _, err := CopiedSandboxTranscripts(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 || filepath.Base(copied[0]) != "ses-1.jsonl" {
		t.Fatalf("copied transcripts = %v, want ses-1.jsonl", copied)
	}
}

func TestOpenShellOpsDeleteFailedCopyLogsOneLineAndStillDeletes(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"download": errors.New("exit status 1")},
		Stderr: map[string]string{"download": "connection reset"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, listingLine(seatRemote("ses-1.jsonl"), seatBase, 4), nil)
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on past a failed copy", err)
	}
	if callIndex(fake.Calls(), "delete") < 0 {
		t.Fatalf("openshell calls = %v, want the sandbox deleted", fake.Calls())
	}
	_, ok, err := ReadSandboxRecord(b.townRoot, openshellTestSession)
	if err != nil {
		t.Fatalf("reading the record after Delete: %v", err)
	}
	if ok {
		t.Error("record survived Delete after a failed copy")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], openshellTestSession) || !strings.Contains(warnings[0], "copying its transcripts out failed") {
		t.Fatalf("warnings = %q, want one line naming the sandbox", warnings)
	}
	if _, err := os.Stat(SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)); !os.IsNotExist(err) {
		t.Errorf("partial copy left behind: stat err = %v", err)
	}
}

func TestOpenShellOpsDeleteCopyDownloadsOnlyWhatTheCapKeeps(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.transcriptCap = 10
	// The seat started four hours before the copy. Its predecessor's transcript is
	// last written before that and is not the seat's, so it is neither downloaded
	// nor counted as cut. Of the seat's three 4-byte files, the cap keeps the two
	// newest.
	listing := listingLine(seatRemote("predecessor.jsonl"), seatBase.Add(-5*time.Hour), 4) +
		listingLine(seatRemote("old.jsonl"), seatBase.Add(-3*time.Hour), 4) +
		listingLine(seatRemote("middle.jsonl"), seatBase.Add(-2*time.Hour), 4) +
		listingLine(seatRemote("new.jsonl"), seatBase.Add(-time.Hour), 4)
	b.cli.Run = copyingRunner(fake, listing, map[string]string{
		seatRemote("predecessor.jsonl"): "pppp",
		seatRemote("old.jsonl"):         "aaaa",
		seatRemote("middle.jsonl"):      "bbbb",
		seatRemote("new.jsonl"):         "cccc",
	})
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeSeatRecord(t, b, seatBase.Add(-4*time.Hour))

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, want := downloadedRemotes(fake.Calls()), []string{seatRemote("new.jsonl"), seatRemote("middle.jsonl")}; !reflect.DeepEqual(got, want) {
		t.Errorf("downloads = %v, want only the newest that fit the cap, newest first: %v", got, want)
	}
	copyDir := SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)
	marker, err := os.ReadFile(filepath.Join(copyDir, sandboxTranscriptMarker))
	if err != nil {
		t.Fatalf("no record of the files the cap left out: %v", err)
	}
	if got := strings.TrimSpace(string(marker)); got != "1" {
		t.Errorf("cap record = %q, want the one file left out", got)
	}
	copied, skipped, err := CopiedSandboxTranscripts(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Errorf("copy reports %d files cut by the cap, want the one left out", skipped)
	}
	var names []string
	for _, p := range copied {
		names = append(names, filepath.Base(p))
	}
	slices.Sort(names)
	if want := []string{"middle.jsonl", "new.jsonl"}; !reflect.DeepEqual(names, want) {
		t.Errorf("kept transcripts = %v, want %v", names, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "stopped at the 10-byte cap") || !strings.Contains(warnings[0], "1 older files") {
		t.Fatalf("warnings = %q, want one line saying the copy stopped at the cap", warnings)
	}
}

func TestOpenShellOpsDeleteCopyKeepsEachTranscriptsSandboxMtime(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	// A subagent's transcript sits below its session's folder, so the copy must
	// recreate that layout as well as keep each file's mtime.
	top, sub := seatRemote("ses-1.jsonl"), seatRemote("ses-1/subagents/agent-a.jsonl")
	topAt, subAt := seatBase.Add(-2*time.Hour), seatBase.Add(-90*time.Minute)
	b.cli.Run = copyingRunner(fake, listingLine(top, topAt, 4)+listingLine(sub, subAt, 4), map[string]string{
		top: "tttt",
		sub: "ssss",
	})
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	copyDir := filepath.Join(SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat), seatFolder())
	for local, want := range map[string]time.Time{
		filepath.Join(copyDir, "ses-1.jsonl"):                         topAt,
		filepath.Join(copyDir, "ses-1", "subagents", "agent-a.jsonl"): subAt,
	} {
		info, err := os.Stat(local)
		if err != nil {
			t.Fatalf("copied transcript missing: %v", err)
		}
		if !info.ModTime().Equal(want) {
			t.Errorf("%s mtime = %v, want the sandbox's %v", filepath.Base(local), info.ModTime(), want)
		}
	}
}

func TestOpenShellOpsDeleteDownloadThatWritesNothingFailsTheCopy(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	// The listing names a transcript the download never writes: the copy must fail
	// there, not hand the seat end a copy that is missing a file.
	b.cli.Run = copyingRunner(fake, listingLine(seatRemote("ses-1.jsonl"), seatBase, 4), nil)
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on past a copy that wrote nothing", err)
	}
	if callIndex(fake.Calls(), "delete") < 0 {
		t.Fatalf("openshell calls = %v, want the sandbox deleted", fake.Calls())
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "copying its transcripts out failed") {
		t.Fatalf("warnings = %q, want one line saying the copy failed", warnings)
	}
	if _, err := os.Stat(SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)); !os.IsNotExist(err) {
		t.Errorf("partial copy left behind: stat err = %v", err)
	}
}

func TestOpenShellOpsDeleteMalformedListingDownloadsNothing(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, "not a listing line\n", nil)
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on past a malformed listing", err)
	}
	if callIndex(fake.Calls(), "download") >= 0 {
		t.Fatalf("openshell calls = %v, want no download from a listing that does not parse", fake.Calls())
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "copying its transcripts out failed") {
		t.Fatalf("warnings = %q, want one line saying the copy failed", warnings)
	}
}

func TestOpenShellOpsDeleteRecordedCopiesBeforeItsDelete(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, listingLine(seatRemote("ses-1.jsonl"), seatBase, 4), map[string]string{
		seatRemote("ses-1.jsonl"): `{"type":"assistant"}`,
	})
	rec := SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}

	if err := b.deleteRecorded(rec); err != nil {
		t.Fatalf("deleteRecorded: %v", err)
	}
	calls := fake.Calls()
	down, del := callIndex(calls, "download"), callIndex(calls, "delete")
	if down < 0 || del < 0 || down > del {
		t.Fatalf("openshell calls = %v, want the copy before the delete on the record path", calls)
	}
	copied, _, err := CopiedSandboxTranscripts(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 {
		t.Fatalf("copied transcripts = %v, want the one transcript", copied)
	}
}

func TestOpenShellOpsDeleteRemovesCopiesOlderThanAWeek(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, "", nil)
	b.now = func() time.Time { return seatBase }
	writeOpsRecord(t, b)

	rigDir := filepath.Dir(SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, "x"))
	stale := filepath.Join(rigDir, "sandbox-retired")
	fresh := filepath.Join(rigDir, "sandbox-recent")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	eightDays := seatBase.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, eightDays, eightDays); err != nil {
		t.Fatal(err)
	}
	oneDay := seatBase.Add(-24 * time.Hour)
	if err := os.Chtimes(fresh, oneDay, oneDay); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("copy older than a week kept: stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("copy within a week removed: %v", err)
	}
}

func TestParseSandboxListingReadsEachLineOrRefusesIt(t *testing.T) {
	t.Parallel()
	valid := listingLine(seatRemote("s.jsonl"), seatBase, 12)
	tests := []struct {
		name    string
		out     string
		want    []string // rel paths, in listing order
		wantErr bool
	}{
		{name: "empty listing holds none", out: "", want: nil},
		{name: "one transcript", out: valid, want: []string{seatFolder() + "/s.jsonl"}},
		{name: "subagent transcript", out: listingLine(seatRemote("s/subagents/a.jsonl"), seatBase, 1), want: []string{seatFolder() + "/s/subagents/a.jsonl"}},
		{name: "short line", out: "1731369600.0 12\n", wantErr: true},
		{name: "mtime not a number", out: "later 12 " + seatRemote("s.jsonl") + "\n", wantErr: true},
		{name: "size not a count", out: "1731369600.0 big " + seatRemote("s.jsonl") + "\n", wantErr: true},
		{name: "path outside the projects dir", out: "1731369600.0 1 /etc/s.jsonl\n", wantErr: true},
		{name: "path steps out of the copy", out: "1731369600.0 1 " + sandboxProjectsDir + "/../s.jsonl\n", wantErr: true},
		{name: "not a transcript", out: "1731369600.0 1 " + sandboxProjectsDir + "/" + seatFolder() + "/s.txt\n", wantErr: true},
		{name: "no project folder", out: "1731369600.0 1 " + sandboxProjectsDir + "/s.jsonl\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := parseSandboxListing(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsed %q as %v, want a refusal", tt.out, files)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %q: %v", tt.out, err)
			}
			var got []string
			for _, f := range files {
				got = append(got, f.rel)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("rel paths = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSandboxListingKeepsTheFractionOfAnMtime(t *testing.T) {
	t.Parallel()
	files, err := parseSandboxListing("1731369600.250000000 7 " + seatRemote("s.jsonl") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !files[0].mtime.Equal(time.Unix(1731369600, 250000000)) || files[0].size != 7 {
		t.Fatalf("parsed = %+v, want the stamp to the nanosecond and size 7", files)
	}
}

func TestCopiedSandboxTranscriptsSkipsLinks(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := filepath.Join(SandboxTranscriptCopyDir(town, openshellTestRig, openshellTestPolecat), SandboxTranscriptFolder(openshellTestRig, openshellTestPolecat))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The copy comes from an untrusted sandbox, so a link to a file is not a transcript.
	if err := os.Symlink(filepath.Join(dir, "real.jsonl"), filepath.Join(dir, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	paths, _, err := CopiedSandboxTranscripts(town, openshellTestRig, openshellTestPolecat)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "real.jsonl" {
		t.Fatalf("listed %v, want only the regular file real.jsonl", paths)
	}
}

func TestCopiedSandboxTranscriptsReadsNothingWithoutACopyAndRefusesANonPlainSeat(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if paths, _, err := CopiedSandboxTranscripts(town, openshellTestRig, openshellTestPolecat); err != nil || len(paths) != 0 {
		t.Fatalf("no copy: paths %v, err %v, want none and no error", paths, err)
	}
	if _, _, err := CopiedSandboxTranscripts(town, openshellTestRig, "../escape"); err == nil {
		t.Fatal("a polecat name that steps out of the transcripts dir was read")
	}
}

func TestCopiedSandboxTranscriptsRefusesACopyItCannotRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// The seat's folder is a file, so the copy cannot be listed as a directory.
	copyDir := SandboxTranscriptCopyDir(town, openshellTestRig, openshellTestPolecat)
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, SandboxTranscriptFolder(openshellTestRig, openshellTestPolecat)), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CopiedSandboxTranscripts(town, openshellTestRig, openshellTestPolecat); err == nil {
		t.Fatal("an unreadable copy listed as holding none, want the read failure")
	}
}

func TestOpenShellOpsDeleteWarnsWhenTheTranscriptsDirCannotBeRead(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, "", nil)
	// The transcripts directory is a file, so neither the prune nor the copy can use it.
	copyDir := SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)
	transcripts := filepath.Dir(filepath.Dir(copyDir))
	if err := os.MkdirAll(filepath.Dir(transcripts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcripts, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on", err)
	}
	if callIndex(fake.Calls(), "delete") < 0 {
		t.Fatalf("openshell calls = %v, want the sandbox deleted", fake.Calls())
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "pruning old transcript copies failed") {
		t.Fatalf("warnings = %q, want the prune failure named", warnings)
	}
}

func TestOpenShellOpsDeleteFailedListingLogsOneLineAndStillDeletes(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"exec": errors.New("exit status 1")},
		Stderr: map[string]string{"exec": "gateway unreachable"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, listingLine(seatRemote("ses-1.jsonl"), seatBase, 4), map[string]string{
		seatRemote("ses-1.jsonl"): "tttt",
	})
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on past a failed listing", err)
	}
	if callIndex(fake.Calls(), "download") >= 0 {
		t.Fatalf("openshell calls = %v, want no download without a listing", fake.Calls())
	}
	if callIndex(fake.Calls(), "delete") < 0 {
		t.Fatalf("openshell calls = %v, want the sandbox deleted", fake.Calls())
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "copying its transcripts out failed") || !strings.Contains(warnings[0], "listing its transcripts") {
		t.Fatalf("warnings = %q, want one line saying the listing failed", warnings)
	}
}

func TestOpenShellOpsDeleteCopyThatCannotMakeItsFolderFailsTheCopy(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	// The session's own transcript is downloaded first, newest first. Then a plain
	// file takes the place of the folder its subagent's transcript must go in.
	top, sub := seatRemote("ses-1.jsonl"), seatRemote("ses-1/subagents/agent-a.jsonl")
	inner := copyingRunner(fake, listingLine(top, seatBase.Add(-time.Hour), 4)+listingLine(sub, seatBase.Add(-2*time.Hour), 4), map[string]string{
		top: "tttt",
		sub: "ssss",
	})
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		out, errOut, err := inner(ctx, argv, stdin, env)
		if slices.Contains(argv, "download") && err == nil {
			if werr := os.WriteFile(filepath.Join(argv[len(argv)-1], "ses-1"), []byte("x"), 0o644); werr != nil {
				t.Fatal(werr)
			}
		}
		return out, errOut, err
	}
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on past a copy that cannot make its folder", err)
	}
	if callIndex(fake.Calls(), "delete") < 0 {
		t.Fatalf("openshell calls = %v, want the sandbox deleted", fake.Calls())
	}
	// Only the session's own transcript is downloaded: the subagent's has no folder to go in.
	if got := downloadedRemotes(fake.Calls()); !reflect.DeepEqual(got, []string{top}) {
		t.Errorf("downloads = %v, want only %s before the folder fails", got, top)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "copying its transcripts out failed") {
		t.Fatalf("warnings = %q, want one line saying the copy failed", warnings)
	}
	if _, err := os.Stat(SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)); !os.IsNotExist(err) {
		t.Errorf("partial copy left behind: stat err = %v", err)
	}
}

func TestOpenShellOpsDeleteCapRecordThatCannotBeWrittenFailsTheCopy(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.transcriptCap = 4
	newer, older := seatRemote("new.jsonl"), seatRemote("old.jsonl")
	inner := copyingRunner(fake, listingLine(newer, seatBase.Add(-time.Hour), 4)+listingLine(older, seatBase.Add(-2*time.Hour), 4), map[string]string{
		newer: "nnnn",
		older: "oooo",
	})
	copyDir := SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		out, errOut, err := inner(ctx, argv, stdin, env)
		if slices.Contains(argv, "download") && err == nil {
			// The cap record is a file at the copy's root, so a directory there cannot be written over.
			if merr := os.MkdirAll(filepath.Join(copyDir, sandboxTranscriptMarker), 0o755); merr != nil {
				t.Fatal(merr)
			}
		}
		return out, errOut, err
	}
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on past an unwritable cap record", err)
	}
	if callIndex(fake.Calls(), "delete") < 0 {
		t.Fatalf("openshell calls = %v, want the sandbox deleted", fake.Calls())
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "copying its transcripts out failed") || !strings.Contains(warnings[0], "recording the transcripts the cap left out") {
		t.Fatalf("warnings = %q, want one line saying the cap record could not be written", warnings)
	}
	if _, err := os.Stat(copyDir); !os.IsNotExist(err) {
		t.Errorf("a copy with no cap record left behind: stat err = %v", err)
	}
}

func TestOpenShellOpsDeleteCopyRefusesASizeThatOverflowsTheCap(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.transcriptCap = 10
	// The listing reports the older transcript at the top of int64. Added to the
	// newer 4-byte one, it wraps below the cap, so the check must refuse it by
	// comparing against the room left, not by summing.
	small, huge := seatRemote("small.jsonl"), seatRemote("huge.jsonl")
	listing := listingLine(small, seatBase.Add(-time.Hour), 4) + listingLine(huge, seatBase.Add(-2*time.Hour), math.MaxInt64)
	b.cli.Run = copyingRunner(fake, listing, map[string]string{small: "ssss", huge: "hhhh"})
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeSeatRecord(t, b, seatBase.Add(-4*time.Hour))

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, want := downloadedRemotes(fake.Calls()), []string{small}; !reflect.DeepEqual(got, want) {
		t.Errorf("downloads = %v, want only the transcript that fits the cap: %v", got, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "1 older files") {
		t.Fatalf("warnings = %q, want one line saying the cap left one file out", warnings)
	}
}

func TestOpenShellOpsDeleteOfAGoneSandboxCopiesNothingAndWarnsNothing(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"exec": errors.New("exit status 1")},
		Stderr: map[string]string{"exec": "sandbox not found: " + openshellTestSession},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, "", nil)
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want a sandbox that is already gone to delete as success", err)
	}
	if callIndex(fake.Calls(), "download") >= 0 {
		t.Fatalf("openshell calls = %v, want no download from a sandbox that is gone", fake.Calls())
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none for a sandbox that is already gone", warnings)
	}
	if _, err := os.Stat(SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)); !os.IsNotExist(err) {
		t.Errorf("copy made for a sandbox that is gone: stat err = %v", err)
	}
}

// writeSeatRecord writes the test seat's record with the time its sandbox was
// created, which is when the seat started.
func writeSeatRecord(t *testing.T, b *openshellBackend, createdAt time.Time) {
	t.Helper()
	if err := WriteSandboxRecord(b.townRoot, SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat, CreatedAt: createdAt}); err != nil {
		t.Fatal(err)
	}
}
