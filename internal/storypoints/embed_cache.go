package storypoints

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// CachedEmbedder serves a repeated embedding from a JSON-lines file and appends
// each new one as it arrives, so a run killed partway through the weekly
// retrain loses only the request in flight and the hourly retry resumes instead
// of restarting (gt-tibj01). A mutex guards the map and the append; the inner
// request runs outside it, so one slow request does not block the rest.
type CachedEmbedder struct {
	inner       Embedder
	path, model string

	// openFile, when set, is how the append opens the cache file; nil means
	// the host's file. A test sets it to drive the write and close failures a
	// real file cannot be made to produce.
	openFile func(name string) (io.WriteCloser, error)

	mu     sync.Mutex
	byKey  map[string][]float32
	hits   int
	misses int
	// needsNewline is set when the file ends mid-line, so the next append
	// starts on a fresh line and a reload keeps it (gt-al8gw8).
	needsNewline bool
}

// cachedEmbedding is one line of the cache file.
type cachedEmbedding struct {
	K string    `json:"k"`
	V []float32 `json:"v"`
}

// NewCachedEmbedder loads the cache at path and embeds misses with inner under
// model, creating the file's directory with mode 0700. A missing file is an
// empty cache. A file with unreadable lines keeps the readable ones; a read that
// fails any other way is an error, because a cache that cannot be read is not
// empty, it is unknown. An unterminated last line is a write a killed process
// did not finish, so it is dropped whole; the next append then starts on its own
// line, so a reload keeps it (gt-al8gw8).
func NewCachedEmbedder(inner Embedder, path, model string) (*CachedEmbedder, error) {
	if inner == nil {
		return nil, errors.New("storypoints: embedding cache has no inner embedder")
	}
	if path == "" {
		return nil, errors.New("storypoints: embedding cache has no file path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("storypoints: create embedding cache directory: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("storypoints: read embedding cache: %w", err)
	}
	c := &CachedEmbedder{inner: inner, path: path, model: model, byKey: make(map[string][]float32)}
	c.load(data)
	return c, nil
}

// load adds every newline-terminated line of data to the map. An unterminated
// last line is a write the killed process did not finish, so it is dropped
// whole and remembered: the next append starts with a newline, because otherwise
// it would land on that same physical line and a reload would drop both
// (gt-al8gw8). A complete line that does not decode is dropped and counted,
// because something other than this process wrote it.
func (c *CachedEmbedder) load(data []byte) {
	c.needsNewline = len(data) > 0 && data[len(data)-1] != '\n'
	bad := 0
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		var e cachedEmbedding
		if err := json.Unmarshal(data[:i], &e); err != nil || e.K == "" || e.V == nil {
			bad++
		} else {
			c.byKey[e.K] = e.V
		}
		data = data[i+1:]
	}
	if bad > 0 {
		log.Printf("storypoints: embedding cache %s: skipped %d unreadable lines", c.path, bad)
	}
}

// Embed returns the vector for text, from the cache when it is there. A miss
// asks inner and writes the answer before returning it; an inner error is
// returned as it came and nothing is written.
func (c *CachedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	key := c.key(text)
	c.mu.Lock()
	if vec, ok := c.byKey[key]; ok {
		c.hits++
		c.mu.Unlock()
		return slices.Clone(vec), nil
	}
	c.misses++
	c.mu.Unlock()

	vec, err := c.inner.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	if err := c.append(key, vec); err != nil {
		return nil, err
	}
	return slices.Clone(vec), nil
}

// append writes one line for key and records it. The line goes out in one call
// on a file opened O_APPEND, so a kill inside the write leaves at most the torn
// last line load drops (gt-tibj01).
func (c *CachedEmbedder) append(key string, vec []float32) error {
	line, err := json.Marshal(cachedEmbedding{K: key, V: vec})
	if err != nil {
		return fmt.Errorf("storypoints: encode embedding cache line: %w", err)
	}
	line = append(line, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.needsNewline {
		// The last write died before its newline; start this line on a fresh
		// one so a reload keeps it instead of dropping it with the torn tail.
		line = append([]byte{'\n'}, line...)
	}
	f, err := c.openCacheFile()
	if err != nil {
		return fmt.Errorf("storypoints: open embedding cache: %w", err)
	}
	n, werr := f.Write(line)
	cerr := f.Close()
	if n == len(line) {
		c.needsNewline = false
	} else if n > 0 {
		c.needsNewline = true // a partial line now ends the file
	}
	if werr != nil {
		return fmt.Errorf("storypoints: append embedding cache: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("storypoints: close embedding cache: %w", cerr)
	}
	c.byKey[key] = vec
	return nil
}

// openCacheFile opens the cache file for one append, through the test seam
// when one is set.
func (c *CachedEmbedder) openCacheFile() (io.WriteCloser, error) {
	if c.openFile != nil {
		return c.openFile(c.path)
	}
	return os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

// Stats returns how many embeddings came from the cache and how many asked the
// inner embedder; a miss whose request failed is counted, because it was asked.
func (c *CachedEmbedder) Stats() (hits, misses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// key is the cache key for text. The NUL keeps a model name ending in the start
// of a text from keying the same as a different model.
func (c *CachedEmbedder) key(text string) string {
	sum := sha256.Sum256([]byte(c.model + "\x00" + text))
	return hex.EncodeToString(sum[:])
}
