package store

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	base36        = "0123456789abcdefghijklmnopqrstuvwxyz"
	issueIDChars  = 5
	wispIDChars   = 6
	unbiasedBelow = 252 // 7 * 36: bytes at or above this would favor the low digits
)

// randomBase36 returns n base-36 characters from r, uniform: a byte at or
// above 252 is skipped rather than reduced modulo 36. A source that runs dry
// is an error, never a shorter id.
func randomBase36(r io.Reader, n int) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for sb.Len() < n {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("store: random id: %w", err)
		}
		if buf[0] >= unbiasedBelow {
			continue
		}
		sb.WriteByte(base36[buf[0]%36])
	}
	return sb.String(), nil
}

// newID is a fresh issue id: the rig prefix, a dash and five base-36 characters.
func newID(r io.Reader, rig string) (string, error) {
	s, err := randomBase36(r, issueIDChars)
	if err != nil {
		return "", err
	}
	return rig + "-" + s, nil
}

// newWispID is a fresh wisp id: the rig prefix, "-wisp-" and six characters.
func newWispID(r io.Reader, rig string) (string, error) {
	s, err := randomBase36(r, wispIDChars)
	if err != nil {
		return "", err
	}
	return rig + "-wisp-" + s, nil
}

// rigOfID is the rig prefix of an id: the text before its first dash, or ""
// when there is none or it is empty.
func rigOfID(id string) string {
	rig, _, ok := strings.Cut(id, "-")
	if !ok {
		return ""
	}
	return rig
}

// nextChildID reserves the next child number under parent in tx and returns
// "<parent>.<n>". The counter row is created on first use; concurrent callers
// serialize on it and get distinct numbers.
func nextChildID(ctx context.Context, tx pgx.Tx, parent string) (string, error) {
	var n int
	err := tx.QueryRow(ctx, `INSERT INTO tracker.child_counters (parent_id, last_child) VALUES ($1, 1)
ON CONFLICT (parent_id) DO UPDATE SET last_child = tracker.child_counters.last_child + 1
RETURNING last_child`, parent).Scan(&n)
	if err != nil {
		return "", fmt.Errorf("store: next child id of %s: %w", parent, err)
	}
	return fmt.Sprintf("%s.%d", parent, n), nil
}
