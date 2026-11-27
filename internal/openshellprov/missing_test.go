package openshellprov

import (
	"errors"
	"io"
	"slices"
	"testing"
)

// listRunner answers every openshell call with listing or err and records argv.
type listRunner struct {
	listing string
	err     error
	argv    [][]string
}

func (l *listRunner) Run(name string, args []string, _ []string, _ io.Reader) ([]byte, []byte, error) {
	l.argv = append(l.argv, append([]string{name}, args...))
	return []byte(l.listing), nil, l.err
}

// One list call carrying -g answers every name, and the unlisted names come back
// in the order given.
func TestMissingSeatProvidersReportsUnlistedInOrder(t *testing.T) {
	t.Parallel()
	runner := &listRunner{listing: "NAME TYPE\nforgejo-seat-furiosa forgejo-polecat\n"}
	missing, err := (Client{Run: runner, Gateway: "main"}).MissingSeatProviders([]string{"furiosa", "nux", "slit"})
	if err != nil {
		t.Fatalf("MissingSeatProviders: %v", err)
	}
	if want := []string{"nux", "slit"}; !slices.Equal(missing, want) {
		t.Errorf("missing = %q, want %q", missing, want)
	}
	if want := []string{"openshell", "-g", "main", "provider", "list"}; len(runner.argv) != 1 || !slices.Equal(runner.argv[0], want) {
		t.Errorf("argv = %q, want only %q", runner.argv, want)
	}
}

// The whole name counts: a longer provider name does not stand for nux, and a
// name CreateSeatProviders refuses is missing even when its provider is listed.
func TestMissingSeatProvidersMatchesTheWholeName(t *testing.T) {
	t.Parallel()
	tests := []struct{ seat, listing string }{
		{"nux", "NAME TYPE\nforgejo-seat-nux2 forgejo-polecat\n"},
		{"a--b", "NAME TYPE\nforgejo-seat-a--b forgejo-polecat\n"},
	}
	for _, tc := range tests {
		t.Run(tc.seat, func(t *testing.T) {
			t.Parallel()
			missing, err := (Client{Run: &listRunner{listing: tc.listing}}).MissingSeatProviders([]string{tc.seat})
			if err != nil {
				t.Fatalf("MissingSeatProviders: %v", err)
			}
			if !slices.Equal(missing, []string{tc.seat}) {
				t.Errorf("missing = %q, want %q", missing, []string{tc.seat})
			}
		})
	}
}

// A list that fails names the command and returns no names.
func TestMissingSeatProvidersFailedListIsAnError(t *testing.T) {
	t.Parallel()
	missing, err := (Client{Run: &listRunner{err: errors.New("gateway down")}}).MissingSeatProviders([]string{"nux"})
	if err == nil {
		t.Fatal("MissingSeatProviders = nil error, want the failed list")
	}
	if missing != nil {
		t.Errorf("missing = %q, want nil", missing)
	}
}
