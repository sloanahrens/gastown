package storypoints

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// countingEmbedder counts its calls and answers vec or err.
type countingEmbedder struct {
	calls int
	vec   []float32
	err   error
}

func (e *countingEmbedder) Embed(context.Context, string) ([]float32, error) {
	e.calls++
	return e.vec, e.err
}

func openCache(t *testing.T, inner Embedder, path, model string) *CachedEmbedder {
	t.Helper()
	c, err := NewCachedEmbedder(inner, path, model)
	if err != nil {
		t.Fatalf("NewCachedEmbedder: %v", err)
	}
	return c
}

func TestEmbedCacheMissThenHit(t *testing.T) {
	t.Parallel()
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "a", "c.jsonl")
	inner := &countingEmbedder{vec: []float32{1, 2}}
	if got, err := openCache(t, inner, path, "m").Embed(ctx, "x"); err != nil || !slices.Equal(got, inner.vec) {
		t.Fatalf("miss = %v, %v; want %v", got, err, inner.vec)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat cache dir: %v", err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("cache dir mode = %o; want no group or other access", perm)
	}
	// The hourly retry builds a new instance on the same file.
	again := openCache(t, inner, path, "m")
	if got, err := again.Embed(ctx, "x"); err != nil || !slices.Equal(got, inner.vec) || inner.calls != 1 {
		t.Fatalf("hit = %v, %v after %d calls; want %v after 1", got, err, inner.calls, inner.vec)
	}
	if h, m := again.Stats(); h != 1 || m != 0 {
		t.Fatalf("Stats = %d, %d; want 1, 0", h, m)
	}
}

func TestEmbedCacheSkipsTornLine(t *testing.T) {
	t.Parallel()
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "c.jsonl")
	inner := &countingEmbedder{vec: []float32{1}}
	seed := openCache(t, inner, path, "m")
	if _, err := seed.Embed(ctx, "kept"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A complete line that is not JSON, then a last line the killed write never
	// terminated: its body parses, but no newline makes it an incomplete line.
	data = append(data, []byte("junk\n{\"k\":\""+seed.key("lost")+"\",\"v\":[9]}")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	again := openCache(t, inner, path, "m")
	if _, err := again.Embed(ctx, "kept"); err != nil || inner.calls != 1 {
		t.Fatalf("kept: %v after %d calls; want the stored line", err, inner.calls)
	}
	if _, err := again.Embed(ctx, "lost"); err != nil || inner.calls != 2 {
		t.Fatalf("lost: %v after %d calls; want the torn line skipped", err, inner.calls)
	}
}

func TestEmbedCacheInnerErrorNotCached(t *testing.T) {
	t.Parallel()
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "c.jsonl")
	inner := &countingEmbedder{err: errors.New("down")}
	c := openCache(t, inner, path, "m")
	for i := 1; i <= 2; i++ {
		if _, err := c.Embed(ctx, "x"); !errors.Is(err, inner.err) || inner.calls != i {
			t.Fatalf("Embed = %v after %d calls; want the inner error, uncached", err, inner.calls)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v; want no file", err)
	}
}

func TestEmbedCacheModelIsPartOfKey(t *testing.T) {
	t.Parallel()
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "c.jsonl")
	a, b := &countingEmbedder{vec: []float32{1}}, &countingEmbedder{vec: []float32{2}}
	if _, err := openCache(t, a, path, "a").Embed(ctx, "x"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, err := openCache(t, b, path, "b").Embed(ctx, "x"); err != nil || b.calls != 1 {
		t.Fatalf("model b: %v after %d calls; want its own miss", err, b.calls)
	}
	if _, err := openCache(t, a, path, "a").Embed(ctx, "x"); err != nil || a.calls != 1 {
		t.Fatalf("model a: %v after %d calls; want its own hit", err, a.calls)
	}
}

func TestEmbedCacheRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		inner Embedder
		path  string
		want  string
	}{
		{"nil inner", nil, filepath.Join(dir, "x"), "no inner embedder"},
		{"empty path", &countingEmbedder{}, "", "no file path"},
		{"file is a directory", &countingEmbedder{}, dir, "read embedding cache"},
		{"parent is a file", &countingEmbedder{}, filepath.Join(blocked, "c.jsonl"), "create embedding cache directory"},
	} {
		if _, err := NewCachedEmbedder(c.inner, c.path, "m"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: NewCachedEmbedder = %v, want an error naming %q", c.name, err, c.want)
		}
	}
}

// failingFile is the file the append opens when a test drives a write or
// close failure.
type failingFile struct{ writeErr, closeErr error }

func (f failingFile) Write([]byte) (int, error) { return 0, f.writeErr }
func (f failingFile) Close() error              { return f.closeErr }

// shortFile writes only the first n bytes of a line to f and then reports a
// short write, the way a full disk leaves a torn line behind.
type shortFile struct {
	f *os.File
	n int
}

func (s shortFile) Write(p []byte) (int, error) {
	n, err := s.f.Write(p[:s.n])
	if err != nil {
		return n, err
	}
	return n, io.ErrShortWrite
}

func (s shortFile) Close() error { return s.f.Close() }

// TestEmbedCacheAppendFailure drives every append failure through the openFile
// seam. Each row also proves the retry leaves a line a reload keeps: for a
// short write that means the retry starts on a fresh line.
func TestEmbedCacheAppendFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		open func(path string) (io.WriteCloser, error)
		want string
	}{
		{"open fails", func(string) (io.WriteCloser, error) { return nil, errors.New("open failed") }, "open embedding cache"},
		{"write fails", func(string) (io.WriteCloser, error) { return failingFile{writeErr: errors.New("disk full")}, nil }, "append embedding cache"},
		{"close fails", func(string) (io.WriteCloser, error) { return failingFile{closeErr: errors.New("close failed")}, nil }, "close embedding cache"},
		{"short write", func(path string) (io.WriteCloser, error) {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			if err != nil {
				return nil, err
			}
			return shortFile{f: f, n: 10}, nil
		}, "append embedding cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, path := context.Background(), filepath.Join(t.TempDir(), "c.jsonl")
			inner := &countingEmbedder{vec: []float32{1}}
			c := openCache(t, inner, path, "m")
			c.openFile = tc.open
			if _, err := c.Embed(ctx, "x"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Embed = %v, want an error naming %q", err, tc.want)
			}
			c.openFile = nil
			if _, err := c.Embed(ctx, "x"); err != nil || inner.calls != 2 {
				t.Fatalf("retry = %v after %d calls; want a fresh miss", err, inner.calls)
			}
			again := openCache(t, inner, path, "m")
			if _, err := again.Embed(ctx, "x"); err != nil || inner.calls != 2 {
				t.Fatalf("reload = %v after %d calls; want the retried line", err, inner.calls)
			}
		})
	}
}

// TestEmbedCacheAppendAfterTornLine is the killed-run case: the retry appends
// one entry on top of a torn tail, and a later run must keep it.
func TestEmbedCacheAppendAfterTornLine(t *testing.T) {
	t.Parallel()
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "c.jsonl")
	inner := &countingEmbedder{vec: []float32{1}}
	seed := openCache(t, inner, path, "m")
	if _, err := seed.Embed(ctx, "kept"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A write the kill tore: no terminating newline.
	data = append(data, []byte(`{"k":"`+seed.key("lost")+`","v":[9`)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := openCache(t, inner, path, "m")
	if _, err := resumed.Embed(ctx, "next"); err != nil || inner.calls != 2 {
		t.Fatalf("resume = %v after %d calls; want one fresh miss", err, inner.calls)
	}
	again := openCache(t, inner, path, "m")
	for _, text := range []string{"kept", "next"} {
		if _, err := again.Embed(ctx, text); err != nil || inner.calls != 2 {
			t.Fatalf("%q = %v after %d calls; want the stored line", text, err, inner.calls)
		}
	}
	if h, m := again.Stats(); h != 2 || m != 0 {
		t.Fatalf("Stats = %d, %d; want 2, 0", h, m)
	}
}

func TestEmbedCacheNonFiniteVectorNotCached(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "c.jsonl")
	c := openCache(t, &countingEmbedder{vec: []float32{float32(math.NaN())}}, path, "m")
	if _, err := c.Embed(context.Background(), "x"); err == nil {
		t.Fatal("NaN vector = nil error, want the encode failure")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v; want no file", err)
	}
}
