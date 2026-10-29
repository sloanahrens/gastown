package seattoken

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/intent"
)

// seatToken is the fake token a test reads back from the file it wrote.
const seatToken = "trk_seat-token-value"

// fakeIssuer records the actors it was asked for, and hands back token and err
// together when a case sets both: an issuer that fails after minting a token
// must not have that token written or printed.
type fakeIssuer struct {
	token  string
	err    error
	actors []string
}

func (f *fakeIssuer) Issue(_ context.Context, actor string) (string, error) {
	f.actors = append(f.actors, actor)
	return f.token, f.err
}

// testSeat is the rig-level seat every case uses unless it names its own.
func testSeat() intent.Seat {
	return intent.Seat{Rig: "gastown", Role: "polecats", Name: "cheedo"}
}

func TestEnsureCreatesThenReuses(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	issuer := &fakeIssuer{token: seatToken}

	path, err := store.Ensure(context.Background(), testSeat(), issuer)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := filepath.Join(store.townRoot, ".runtime", "seats", "gastown", "polecats.cheedo.token"); path != want {
		t.Fatalf("Ensure path = %q, want %q", path, want)
	}
	if got := permOf(t, filepath.Dir(path)); got != 0o700 {
		t.Errorf("token directory mode = %o, want 700", got)
	}
	if got := permOf(t, path); got != 0o600 {
		t.Errorf("token file mode = %o, want 600", got)
	}
	if got := stringOf(t, path); got != seatToken {
		t.Errorf("token file holds %q, want %q", got, seatToken)
	}
	if len(issuer.actors) != 1 || issuer.actors[0] != "gastown/polecats/cheedo" {
		t.Fatalf("issuer asked for %v, want one token for gastown/polecats/cheedo", issuer.actors)
	}

	again, err := store.Ensure(context.Background(), testSeat(), issuer)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if again != path {
		t.Errorf("second Ensure path = %q, want %q", again, path)
	}
	if len(issuer.actors) != 1 {
		t.Errorf("second Ensure asked for %v, want the file it already has reused with no issue", issuer.actors)
	}
}

func TestPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		seat intent.Seat
		want string
	}{
		{"rig-level seat with a name", intent.Seat{Rig: "gastown", Role: "polecats", Name: "cheedo"}, "gastown/polecats.cheedo.token"},
		{"rig-level singleton", intent.Seat{Rig: "gastown", Role: "witness"}, "gastown/witness.token"},
		{"town-level seat with a name", intent.Seat{Role: "dog", Name: "rex"}, "dog.rex.token"},
		{"town-level singleton", intent.Seat{Role: "deacon"}, "deacon.token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if got, want := New(root).Path(tc.seat), filepath.Join(root, ".runtime", "seats", tc.want); got != want {
				t.Errorf("Path() = %q, want %q", got, want)
			}
		})
	}
}

func TestEnsureIssuerFailureLeavesNoFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		issuer  *fakeIssuer
		wantErr string
	}{
		{"the issuer fails", &fakeIssuer{token: seatToken, err: errors.New("tracker: 403 forbidden")}, "tracker: 403 forbidden"},
		{"the issuer mints nothing", &fakeIssuer{token: "  "}, "empty token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := New(t.TempDir())
			path, err := store.Ensure(context.Background(), testSeat(), tc.issuer)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Ensure error = %v, want it to name %q", err, tc.wantErr)
			}
			if path != "" {
				t.Errorf("Ensure returned path %q with an error; a caller must not read it as a token file", path)
			}
			if strings.Contains(err.Error(), seatToken) {
				t.Errorf("Ensure error = %q, want no token in it", err)
			}
			if _, statErr := os.Stat(store.Path(testSeat())); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("token file stat = %v, want the failed issue to leave no file behind", statErr)
			}
		})
	}
}

// An interrupted run can leave an empty file; the seat still gets a token, and
// the issuer is asked exactly once.
func TestEnsureReclaimsEmptyFile(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path := store.Path(testSeat())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeIssuer{token: seatToken}

	got, err := store.Ensure(context.Background(), testSeat(), issuer)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got != path {
		t.Errorf("Ensure path = %q, want %q", got, path)
	}
	if value := stringOf(t, path); value != seatToken {
		t.Errorf("token file holds %q, want the empty file replaced with a token", value)
	}
	if len(issuer.actors) != 1 {
		t.Errorf("issuer asked %d times, want 1", len(issuer.actors))
	}
}

// A token file that cannot be read is an error, not an absent one: Ensure must
// not issue over something it could not inspect.
func TestEnsureUnreadableFile(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	if err := os.MkdirAll(store.Path(testSeat()), 0o700); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeIssuer{token: seatToken}

	if _, err := store.Ensure(context.Background(), testSeat(), issuer); err == nil {
		t.Fatal("Ensure succeeded over a path it cannot read")
	}
	if len(issuer.actors) != 0 {
		t.Errorf("issuer asked %d times, want none while the file cannot be read", len(issuer.actors))
	}
}

// A seat whose directory is a file cannot get a token directory, so a launch
// never goes on tokenless and the issuer is never asked.
func TestEnsureRefusesASeatDirectoryThatIsAFile(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	dir := filepath.Dir(store.Path(testSeat()))
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeIssuer{token: seatToken}

	if _, err := store.Ensure(context.Background(), testSeat(), issuer); err == nil {
		t.Fatal("Ensure wrote a token into a seat directory that is a file")
	}
	if len(issuer.actors) != 0 {
		t.Errorf("issuer asked %d times, want none while the directory cannot be made", len(issuer.actors))
	}
}

// An empty file the store cannot reclaim stays where it is: Ensure reports the
// failure rather than issuing a token it has nowhere to put.
func TestEnsureReportsAnUnreclaimableEmptyFile(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path := store.Path(testSeat())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	readOnly(t, filepath.Dir(path))

	if _, err := store.Ensure(context.Background(), testSeat(), &fakeIssuer{token: seatToken}); err == nil {
		t.Fatal("Ensure succeeded over an empty token file it could not replace")
	}
}

// Remove reports a file it cannot delete, so a caller does not record a seat
// as tokenless while its token is still on disk.
func TestRemoveReportsAPathItCannotDelete(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path := store.Path(testSeat())
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "entry"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.Remove(testSeat()); err == nil {
		t.Fatal("Remove reported success with a non-empty directory still at the token path")
	}
}

// A symlink at any level of the seat's directories is refused, not followed: the
// target is a real directory that must stay empty, so a token written through
// the link would land outside the town's runtime directory.
func TestEnsureRefusesASymlinkedDirectory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		level int // index into seatDirs: the runtime directory, seats, or the rig
	}{
		{"the runtime directory", 0},
		{"the seats directory", 1},
		{"the rig directory", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := New(t.TempDir())
			dir := store.seatDirs(testSeat())[tc.level]
			target := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dir); err != nil {
				t.Fatal(err)
			}
			issuer := &fakeIssuer{token: seatToken}

			if _, err := store.Ensure(context.Background(), testSeat(), issuer); err == nil {
				t.Fatal("Ensure wrote a token through a symlinked directory")
			}
			if len(issuer.actors) != 0 {
				t.Errorf("issuer asked %d times, want none through a symlink", len(issuer.actors))
			}
			entries, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("the symlink's target holds %d entries, want it left empty", len(entries))
			}
		})
	}
}

// A symlink at the token path is refused rather than reused or followed: the
// file it points at keeps its content, and the issuer is never asked.
func TestEnsureRefusesASymlink(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path := store.Path(testSeat())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	writeFile(t, target, "trk_old-token\n")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeIssuer{token: seatToken}

	if _, err := store.Ensure(context.Background(), testSeat(), issuer); err == nil {
		t.Fatal("Ensure reused a symlink at the token path")
	}
	if len(issuer.actors) != 0 {
		t.Errorf("issuer asked %d times, want none over a symlink", len(issuer.actors))
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the symlink is gone: %v", err)
	}
	if got := stringOf(t, target); got != "trk_old-token" {
		t.Errorf("the symlink's target now holds %q, want it untouched", got)
	}
}

// Remove does not delete through a symlinked seat directory: the file the link
// reaches is the seat's own only by accident, and it stays.
func TestRemoveRefusesASymlinkedDirectory(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	dir := store.seatDirs(testSeat())[2]
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	reached := filepath.Join(target, filepath.Base(store.Path(testSeat())))
	writeFile(t, reached, seatToken+"\n")

	if err := store.Remove(testSeat()); err == nil {
		t.Fatal("Remove reported success through a symlinked seat directory")
	}
	if got := stringOf(t, reached); got != seatToken {
		t.Errorf("the file behind the symlink holds %q, want it untouched", got)
	}
}

// A launch that loses the publish race keeps the token the winner wrote and
// writes none of its own; the issuer's token is discarded, and no temporary
// file is left in the seat directory.
func TestEnsureRivalPublishedFirst(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path := store.Path(testSeat())
	const rival = "trk_rival-token-value"
	issuer := issuerFunc(func(string) (string, error) {
		writeFile(t, path, rival+"\n")
		return seatToken, nil
	})

	got, err := store.Ensure(context.Background(), testSeat(), issuer)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got != path {
		t.Errorf("Ensure path = %q, want %q", got, path)
	}
	if value := stringOf(t, path); value != rival {
		t.Errorf("token file holds %q, want the rival's token kept", value)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("seat directory holds %d entries, want only the token file", len(entries))
	}
}

// issuerFunc adapts a function to Issuer, for a case whose issue has a side
// effect on the store.
type issuerFunc func(actor string) (string, error)

func (f issuerFunc) Issue(_ context.Context, actor string) (string, error) { return f(actor) }

func TestRemove(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path, err := store.Ensure(context.Background(), testSeat(), &fakeIssuer{token: seatToken})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := store.Remove(testSeat()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat after Remove = %v, want the file gone", err)
	}
	if err := store.Remove(testSeat()); err != nil {
		t.Errorf("Remove with no file = %v, want success", err)
	}
}

func TestSeatRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		noTown  bool // the store is made with no town root
		seat    intent.Seat
		wantErr string
	}{
		{"no town root", true, testSeat(), "no town root"},
		{"no role", false, intent.Seat{Rig: "gastown"}, "names no role"},
		{"a separator in the rig", false, intent.Seat{Rig: "../etc", Role: "polecats"}, "path separator"},
		{"a separator in the name", false, intent.Seat{Rig: "gastown", Role: "polecats", Name: "a/b"}, "path separator"},
		{"a rig that climbs out", false, intent.Seat{Rig: "..", Role: "polecats"}, "not one directory name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := ""
			if !tc.noTown {
				town = t.TempDir()
			}
			store := New(town)
			if _, err := store.Ensure(context.Background(), tc.seat, &fakeIssuer{token: seatToken}); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Ensure error = %v, want it to name %q", err, tc.wantErr)
			}
			if err := store.Remove(tc.seat); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Remove error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

func TestEnsureWithoutIssuer(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	if _, err := store.Ensure(context.Background(), testSeat(), nil); err == nil {
		t.Fatal("Ensure with no issuer succeeded; nothing could have written a token")
	}
	if _, err := os.Stat(store.Path(testSeat())); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat = %v, want no file for a call with no issuer", err)
	}
}

// A town root that does not exist cannot hold a runtime directory: Ensure
// reports the failure and never asks the issuer.
func TestEnsureNeedsAnExistingTownRoot(t *testing.T) {
	t.Parallel()
	store := New(filepath.Join(t.TempDir(), "missing"))
	issuer := &fakeIssuer{token: seatToken}

	if _, err := store.Ensure(context.Background(), testSeat(), issuer); err == nil {
		t.Fatal("Ensure made a runtime directory under a town root that does not exist")
	}
	if len(issuer.actors) != 0 {
		t.Errorf("issuer asked %d times, want none while the directory cannot be made", len(issuer.actors))
	}
}

// A token file name the file system refuses is an error where it is read: the
// stat fails for a reason other than absence, so Ensure neither reuses nor
// replaces it.
func TestEnsureRefusesANameTheFilesystemRefuses(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	seat := intent.Seat{Rig: "gastown", Role: "polecats", Name: strings.Repeat("n", 300)}
	issuer := &fakeIssuer{token: seatToken}

	if _, err := store.Ensure(context.Background(), seat, issuer); err == nil {
		t.Fatal("Ensure succeeded with a token file name the file system refuses")
	}
	if len(issuer.actors) != 0 {
		t.Errorf("issuer asked %d times, want none over a name it cannot stat", len(issuer.actors))
	}
}

// Remove refuses a seat directory it cannot stat, rather than reporting the seat
// as having no token while one may still be on disk.
func TestRemoveRefusesADirectoryTheFilesystemRefuses(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	seat := intent.Seat{Rig: strings.Repeat("r", 300), Role: "polecats", Name: "cheedo"}
	if err := os.MkdirAll(store.seatDirs(seat)[1], 0o700); err != nil {
		t.Fatal(err)
	}

	if err := store.Remove(seat); err == nil {
		t.Fatal("Remove reported success for a seat directory it could not stat")
	}
}

// A token file that exists but cannot be read is an error, and the issuer is
// not asked to replace it.
func TestEnsureUnreadableTokenFile(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path := store.Path(testSeat())
	writeFile(t, path, seatToken+"\n")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeIssuer{token: seatToken}

	if _, err := store.Ensure(context.Background(), testSeat(), issuer); err == nil {
		t.Fatal("Ensure read a token file it cannot read")
	}
	if len(issuer.actors) != 0 {
		t.Errorf("issuer asked %d times, want none over a file it cannot read", len(issuer.actors))
	}
}

// A seat directory the process cannot write to refuses the temporary file, so
// no token file appears at the path.
func TestEnsureRefusesAReadOnlySeatDirectory(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path := store.Path(testSeat())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	readOnly(t, filepath.Dir(path))

	if _, err := store.Ensure(context.Background(), testSeat(), &fakeIssuer{token: seatToken}); err == nil {
		t.Fatal("Ensure published a token into a directory that refuses writes")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("token file stat = %v, want no file after a refused publish", err)
	}
}

// failingFiles is a tokenFiles whose temporary file or link fails as set.
type failingFiles struct {
	writeErr, closeErr, linkErr error
}

func (f failingFiles) CreateTemp(string, string) (tempFile, error) {
	return failingTemp{writeErr: f.writeErr, closeErr: f.closeErr}, nil
}

func (f failingFiles) Link(string, string) error { return f.linkErr }

// failingTemp is a temporary file whose write or close fails as set.
type failingTemp struct {
	writeErr, closeErr error
}

func (f failingTemp) WriteString(s string) (int, error) { return len(s), f.writeErr }
func (f failingTemp) Close() error                      { return f.closeErr }
func (f failingTemp) Name() string                      { return "" }

// A failure while publishing refuses the launch: no token file is left at the
// path, and the error does not carry the token.
func TestEnsurePublishFailureLeavesNoFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		files   failingFiles
		wantErr string
	}{
		{"the write fails", failingFiles{writeErr: errors.New("write fault")}, "write fault"},
		{"the close fails", failingFiles{closeErr: errors.New("close fault")}, "close fault"},
		{"the link fails", failingFiles{linkErr: errors.New("link not permitted")}, "link not permitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := New(t.TempDir())
			store.files = tc.files

			path, err := store.Ensure(context.Background(), testSeat(), &fakeIssuer{token: seatToken})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Ensure error = %v, want it to name %q", err, tc.wantErr)
			}
			if path != "" {
				t.Errorf("Ensure returned path %q with an error", path)
			}
			if strings.Contains(err.Error(), seatToken) {
				t.Errorf("Ensure error = %q, want no token in it", err)
			}
			if _, statErr := os.Stat(store.Path(testSeat())); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("token file stat = %v, want no file after a failed publish", statErr)
			}
		})
	}
}

// A rival launch that wins the publish race but leaves nothing usable at the
// path makes this launch refuse: a file with no token, or a non-file, is not a
// token to hand a seat.
func TestEnsureRivalLeavesNoToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		rival func(path string) error
	}{
		{"an empty file", func(path string) error { return os.WriteFile(path, nil, 0o600) }},
		{"a directory", func(path string) error { return os.Mkdir(path, 0o700) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := New(t.TempDir())
			path := store.Path(testSeat())
			issuer := issuerFunc(func(string) (string, error) {
				if err := tc.rival(path); err != nil {
					return "", err
				}
				return seatToken, nil
			})

			_, err := store.Ensure(context.Background(), testSeat(), issuer)
			if err == nil {
				t.Fatal("Ensure used the path a rival left without a token")
			}
			if strings.Contains(err.Error(), seatToken) {
				t.Errorf("Ensure error = %q, want no token in it", err)
			}
		})
	}
}

// readOnly makes dir refuse new and removed entries, and puts its mode back
// before the test's cleanup walks the tree.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	before := permOf(t, dir)
	//testpolicy:allow no-exec-files — a directory must keep its search bit to be entered, so the 0500 mode is the fixture's read-only state: the write bit is what it drops
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, before) })
}

func permOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func stringOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}
