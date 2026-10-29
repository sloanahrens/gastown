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

// A seat whose file cannot be created is refused, so a launch never goes on
// tokenless.
func TestEnsureRefusesAnUnwritableDirectory(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	dir := filepath.Dir(store.Path(testSeat()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	readOnly(t, dir)
	issuer := &fakeIssuer{token: seatToken}

	if _, err := store.Ensure(context.Background(), testSeat(), issuer); err == nil {
		t.Fatal("Ensure wrote a token into a directory that refuses writes")
	}
	if len(issuer.actors) != 0 {
		t.Errorf("issuer asked %d times, want none while the file cannot be created", len(issuer.actors))
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
func TestRemoveReportsAFileItCannotDelete(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path := store.Path(testSeat())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(seatToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readOnly(t, filepath.Dir(path))

	if err := store.Remove(testSeat()); err == nil {
		t.Fatal("Remove reported success with the token file still in place")
	}
}

// A symlink where the seat directory should be is refused: its target is not
// created, so a token is never written outside the town's runtime directory.
func TestEnsureRefusesADirectorySymlink(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	dir := filepath.Dir(store.Path(testSeat()))
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeIssuer{token: seatToken}

	if _, err := store.Ensure(context.Background(), testSeat(), issuer); err == nil {
		t.Fatal("Ensure made a token directory through a symlink")
	}
	if len(issuer.actors) != 0 {
		t.Errorf("issuer asked %d times, want none while the directory cannot be made", len(issuer.actors))
	}
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the symlink's target was created: %v", err)
	}
}

// A symlink at the token path is refused rather than followed: O_EXCL never
// creates through one, and the seat is not handed a file at its target.
func TestEnsureRefusesASymlink(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path := store.Path(testSeat())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	issuer := &fakeIssuer{token: seatToken}

	if _, err := store.Ensure(context.Background(), testSeat(), issuer); err == nil {
		t.Fatal("Ensure took over a symlink at the token path")
	}
	if len(issuer.actors) != 0 {
		t.Errorf("issuer asked %d times, want none over a symlink", len(issuer.actors))
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the symlink is gone: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file was written at the symlink's target: %v", err)
	}
}

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
		town    string
		seat    intent.Seat
		wantErr string
	}{
		{"no town root", "", testSeat(), "no town root"},
		{"no role", "root", intent.Seat{Rig: "gastown"}, "names no role"},
		{"a separator in the rig", "root", intent.Seat{Rig: "../etc", Role: "polecats"}, "path separator"},
		{"a separator in the name", "root", intent.Seat{Rig: "gastown", Role: "polecats", Name: "a/b"}, "path separator"},
		{"a rig that climbs out", "root", intent.Seat{Rig: "..", Role: "polecats"}, "not one directory name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := tc.town
			if town != "" {
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

// readOnly makes dir refuse new and removed entries, and puts its mode back
// before the test's cleanup walks the tree.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	before := permOf(t, dir)
	//testpolicy:allow no-exec-files — the target is the fixture directory: it keeps its search bit so the token file inside stays readable, and drops its write bit so creating and removing entries fails
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
