package storypoints

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// embedTextLimit is the most characters of section text the embedder reads.
const embedTextLimit = 6000

// maxEmbedReply bounds the reply read from the embedder, so a runaway reply
// cannot exhaust memory.
const maxEmbedReply = 1 << 20

// Embedder turns a bead's section text into the vector the embedding ridge
// reads.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// SectionText is the text the embedding ridge reads: the Constraints and
// Acceptance sections of the bead's description, each headed, joined by a
// blank line, and cut at embedTextLimit characters. A section with no body is
// left out. A bead with neither section has no text to embed, and that is an
// error. The sections are split by specdispatch.Sections; specdispatch does
// not import this package, so the import creates no cycle.
func SectionText(issue *beads.Issue) (string, error) {
	sections := specdispatch.Sections(issue.Description)
	var parts []string
	for _, s := range []struct{ key, heading string }{
		{"constraints", "Constraints"},
		{"acceptance", "Acceptance"},
	} {
		if body := sections[s.key]; body != "" {
			parts = append(parts, s.heading+":\n"+body)
		}
	}
	if len(parts) == 0 {
		return "", errors.New("storypoints: bead has no Constraints or Acceptance text to embed")
	}
	text := strings.Join(parts, "\n\n")
	if r := []rune(text); len(r) > embedTextLimit {
		text = string(r[:embedTextLimit])
	}
	return text, nil
}

// OllamaEmbedder embeds text with an ollama server's /api/embed endpoint. The
// caller gives the URL and the model. Dim is the embedding dimension of the
// weights it will be paired with: a reply of any other length is refused, and
// a zero Dim refuses every reply. Timeout bounds one request, and a zero
// Timeout is refused before anything is sent. HTTP sends the request and must
// be set: a nil client is refused, so the process-wide default client and its
// proxy environment are never used.
type OllamaEmbedder struct {
	URL     string
	Model   string
	Dim     int
	Timeout time.Duration
	HTTP    *http.Client
}

// embedRequest is the body POSTed to /api/embed.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedReply is the part of the /api/embed reply this code reads.
type embedReply struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed returns the vector for text, which must be one embedding of the
// configured dimension.
func (e OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e.Timeout <= 0 {
		return nil, errors.New("storypoints: embedder has no timeout, so it will not send a request")
	}
	if e.HTTP == nil {
		return nil, errors.New("storypoints: embedder has no HTTP client, so it will not send a request")
	}
	// The request holds a model name and one string, so its JSON cannot fail to encode.
	body, _ := json.Marshal(embedRequest{Model: e.Model, Input: []string{text}})
	ctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("storypoints: build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("storypoints: embed request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The reply body is not put in the error: it is the server's output,
		// and it can hold anything.
		return nil, fmt.Errorf("storypoints: embedder answered status %d", resp.StatusCode)
	}
	var reply embedReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxEmbedReply)).Decode(&reply); err != nil {
		return nil, fmt.Errorf("storypoints: decode embed reply: %w", err)
	}
	if len(reply.Embeddings) != 1 {
		return nil, fmt.Errorf("storypoints: embedder returned %d vectors for one input", len(reply.Embeddings))
	}
	vec := reply.Embeddings[0]
	if len(vec) != e.Dim {
		return nil, fmt.Errorf("storypoints: embedding has %d numbers, want %d", len(vec), e.Dim)
	}
	return vec, nil
}

// EmbeddingDimension is the length of the vector EmbeddingMinutes expects: the
// weights' embedding dimension, 0 when w has no embedding ridge. A caller
// wiring an embedder sizes its reply check with it.
func (w Weights) EmbeddingDimension() int {
	return len(w.embedCoef)
}

// EmbeddingMinutes is the embedding ridge's predicted polecat minutes for vec:
// exp(intercept + dot(coef, vec)). It refuses a vector whose length is not the
// weights' embedding dimension, and a result that is not a finite number.
func (w Weights) EmbeddingMinutes(vec []float32) (float64, error) {
	if len(w.embedCoef) == 0 {
		return 0, ErrNoWeights
	}
	if len(vec) != len(w.embedCoef) {
		return 0, fmt.Errorf("storypoints: embedding has %d numbers, want %d", len(vec), len(w.embedCoef))
	}
	m := math.Exp(w.embedLog(vec))
	if math.IsNaN(m) || math.IsInf(m, 0) {
		return 0, errors.New("storypoints: embedding minutes are not a finite number")
	}
	return m, nil
}

// embedLog is the embedding ridge's log minutes for vec. The caller checks the
// length against the weights' dimension; the result may be non-finite.
func (w Weights) embedLog(vec []float32) float64 {
	z := w.embedIntercept
	for i, x := range vec {
		z += w.embedCoef[i] * float64(x)
	}
	return z
}
