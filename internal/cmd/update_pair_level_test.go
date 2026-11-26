package cmd

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
)

const (
	// pairLevelCommit is the pair commit the pair-level rows read the bd pin at.
	pairLevelCommit = "c0ffee0c0ffee0c0ffee0c0ffee0c0ffee0c0ffe"
	// pairLevelBdPin is the bd commit a pair's image pins name.
	pairLevelBdPin = "bdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbdbd"
	// pairLevelSchema is the schema level a pair and its database agree on.
	pairLevelSchema = 67
)

// agreeingPairLevels is a pair-level read whose pin and database are at the same
// schema level, so a gate driven by another check accepts the pair.
func agreeingPairLevels() pairLevelReads {
	return pairLevelReads{
		PinLevel: func(context.Context, string, string) (string, int, error) {
			return pairLevelBdPin, pairLevelSchema, nil
		},
		Database: func(context.Context, string) (int, error) {
			return pairLevelSchema, nil
		},
	}
}

// pairLevelImages is the pair the pair-level rows hand the gate: a full commit
// and the daemon and polecat digests the registry derivation answers for it, so
// a row that clears the level check also clears the registry match.
func pairLevelImages() update.ImagePair {
	return update.ImagePair{
		Daemon:  pairMatchRef(imagevalues.DaemonImage, "dddddddddddd"),
		Polecat: pairMatchRef(imagevalues.PolecatImage, "eeeeeeeeeeee"),
		Commit:  pairLevelCommit,
	}
}

// TestPreRollGuardPairLevel holds the pre-swap gate to the pair's own bd schema
// level: the preflight's town checks run the image that is running now, so they
// pass for a pair whose pin the database has not caught up to. A pin at 67
// against a database at 66 refuses, naming the commit and both levels; equal
// levels pass. Every read the check cannot make, and every read it was not given,
// refuses too: a pair nothing compared is not a pair the check passed.
func TestPreRollGuardPairLevel(t *testing.T) {
	t.Parallel()
	pinAt := func(pin string, level int) pairLevelReads {
		return pairLevelReads{
			PinLevel: func(context.Context, string, string) (string, int, error) { return pin, level, nil },
			Database: func(context.Context, string) (int, error) { return level, nil },
		}
	}
	skewed := pairLevelReads{
		PinLevel: func(context.Context, string, string) (string, int, error) { return pairLevelBdPin, 67, nil },
		Database: func(context.Context, string) (int, error) { return 66, nil },
	}
	reverse := pairLevelReads{
		PinLevel: func(context.Context, string, string) (string, int, error) { return pairLevelBdPin, 66, nil },
		Database: func(context.Context, string) (int, error) { return 67, nil },
	}
	pinFails := pairLevelReads{
		PinLevel: func(context.Context, string, string) (string, int, error) {
			return "", 0, errors.New("no such path")
		},
		Database: func(context.Context, string) (int, error) { return pairLevelSchema, nil },
	}
	// levelFails reads the pin and then fails on the level it carries. The
	// database is at 0, so a swallowed level error compares 0 with 0 and passes.
	levelFails := pairLevelReads{
		PinLevel: func(context.Context, string, string) (string, int, error) {
			return pairLevelBdPin, 0, errors.New("bd carries no migrations")
		},
		Database: func(context.Context, string) (int, error) { return 0, nil },
	}
	databaseFails := pairLevelReads{
		PinLevel: func(context.Context, string, string) (string, int, error) {
			return pairLevelBdPin, pairLevelSchema, nil
		},
		Database: func(context.Context, string) (int, error) { return 0, errors.New("Dolt is not reachable") },
	}
	tests := []struct {
		name  string
		pair  update.ImagePair
		reads pairLevelReads
		// want is empty when the pair is accepted; otherwise every string must
		// appear in the refusal.
		want []string
	}{
		{
			name:  "a pin at 67 against a database at 66 refuses",
			pair:  pairLevelImages(),
			reads: skewed,
			want:  []string{pairLevelCommit, pairLevelBdPin, "schema level 67", "database is at 66"},
		},
		{
			name:  "a pin at 66 against a database at 67 refuses",
			pair:  pairLevelImages(),
			reads: reverse,
			want:  []string{pairLevelCommit, pairLevelBdPin, "schema level 66", "database is at 67"},
		},
		{
			name:  "equal levels pass",
			pair:  pairLevelImages(),
			reads: pinAt(pairLevelBdPin, pairLevelSchema),
		},
		{
			name:  "a pin that cannot be read refuses",
			pair:  pairLevelImages(),
			reads: pinFails,
			want:  []string{pairLevelCommit, "no such path"},
		},
		{
			name:  "a pin level that cannot be read refuses, even against a database at zero",
			pair:  pairLevelImages(),
			reads: levelFails,
			want:  []string{pairLevelCommit, "bd carries no migrations"},
		},
		{
			name:  "a database level that cannot be read refuses",
			pair:  pairLevelImages(),
			reads: databaseFails,
			want:  []string{"database's schema level", "Dolt is not reachable"},
		},
		{
			name:  "a pair-level read that is not wired refuses",
			pair:  pairLevelImages(),
			reads: pairLevelReads{Database: agreeingPairLevels().Database},
			want:  []string{"no read of the pair's bd pin level"},
		},
		{
			name:  "a database read that is not wired refuses",
			pair:  pairLevelImages(),
			reads: pairLevelReads{PinLevel: agreeingPairLevels().PinLevel},
			want:  []string{"no read of the database's schema level"},
		},
		{
			name:  "a pair that names no commit refuses",
			reads: agreeingPairLevels(),
			want:  []string{"names no commit"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gate := preSwapPreflight(t.TempDir(), preflightPassing, tt.reads, pairHeldDerive(tt.pair), &preflightRunningProbes{commit: preflightRunningCommit}, false, descendingAncestry, io.Discard)

			err := gate.check(context.Background(), tt.pair)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("PreSwap = %v, want the pair accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatal("PreSwap accepted a pair the pair-level check refused")
			}
			var pc update.PairCheckError
			if !errors.As(err, &pc) || pc.Check != update.CheckPreflight {
				t.Errorf("PreSwap error = %v, want a PairCheckError naming %q", err, update.CheckPreflight)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("PreSwap error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}

// TestPreRollGuardProductionWiresThePairLevel pins that the production seams
// carry both pair-level reads, and that they are the real ones: daemon.PairBdLevel
// and townDatabaseLevel, compared by function value because calling the database
// read would open the town's store. A stub or another function wired in their
// place would pass a bare non-nil check.
func TestPreRollGuardProductionWiresThePairLevel(t *testing.T) {
	t.Parallel()
	reads := productionUpdateSeams().PairLevel
	if reads.PinLevel == nil {
		t.Error("productionUpdateSeams wires no bd pin level read: every digest install would be refused")
	}
	if reads.Database == nil {
		t.Error("productionUpdateSeams wires no database level read: every digest install would be refused")
	}
	if got, want := reflect.ValueOf(reads.PinLevel).Pointer(), reflect.ValueOf(daemon.PairBdLevel).Pointer(); got != want {
		t.Error("productionUpdateSeams wires a bd pin level read other than daemon.PairBdLevel")
	}
	if got, want := reflect.ValueOf(reads.Database).Pointer(), reflect.ValueOf(townDatabaseLevel).Pointer(); got != want {
		t.Error("productionUpdateSeams wires a database level read other than townDatabaseLevel")
	}
}
