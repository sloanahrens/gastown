package storypoints

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestSectionText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        string
		wantErr     bool
	}{
		{
			name:        "both sections are headed and joined",
			description: "## Goal\nship it\n\n## Constraints\n- keep the API\n\n## Acceptance\n- [ ] it works\n",
			want:        "Constraints:\n- keep the API\n\nAcceptance:\n- [ ] it works",
		},
		{
			name:        "a missing Acceptance is left out",
			description: "## Constraints\n- keep the API\n",
			want:        "Constraints:\n- keep the API",
		},
		{
			name:        "a missing Constraints is left out",
			description: "## Goal\nship it\n\n## Acceptance\n- [ ] it works\n",
			want:        "Acceptance:\n- [ ] it works",
		},
		{
			name:        "an empty section counts as missing",
			description: "## Constraints\n\n## Acceptance\n- [ ] it works\n",
			want:        "Acceptance:\n- [ ] it works",
		},
		{
			name:        "neither section is an error",
			description: "## Goal\nship it\n",
			wantErr:     true,
		},
		{
			name:        "an empty description is an error",
			description: "",
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SectionText(&beads.Issue{Description: tt.description})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SectionText = %q, nil error; want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SectionText: %v", err)
			}
			if got != tt.want {
				t.Errorf("SectionText = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSectionTextCutsAtTheLimit(t *testing.T) {
	t.Parallel()
	// "Constraints:\n" is 13 characters, so a body of 6000-13 characters makes
	// the whole text exactly the limit.
	const prefix = "Constraints:\n"
	atLimit := strings.Repeat("a", embedTextLimit-len(prefix))
	tests := []struct {
		name string
		body string
		want int
	}{
		{"under the limit is kept whole", "short", len(prefix) + len("short")},
		{"exactly the limit is kept whole", atLimit, embedTextLimit},
		{"one over the limit is cut to the limit", atLimit + "a", embedTextLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SectionText(&beads.Issue{Description: "## Constraints\n" + tt.body + "\n"})
			if err != nil {
				t.Fatalf("SectionText: %v", err)
			}
			if n := utf8.RuneCountInString(got); n != tt.want {
				t.Errorf("SectionText has %d characters, want %d", n, tt.want)
			}
		})
	}
}

func TestSectionTextCutDoesNotSplitACharacter(t *testing.T) {
	t.Parallel()
	// A body of two-byte characters: a byte cut at the limit would split one.
	body := strings.Repeat("é", embedTextLimit+10)
	got, err := SectionText(&beads.Issue{Description: "## Constraints\n" + body + "\n"})
	if err != nil {
		t.Fatalf("SectionText: %v", err)
	}
	if !utf8.ValidString(got) {
		t.Errorf("SectionText cut left invalid UTF-8")
	}
	if n := utf8.RuneCountInString(got); n != embedTextLimit {
		t.Errorf("SectionText has %d characters, want %d", n, embedTextLimit)
	}
}

// embedTransport is an http.RoundTripper that answers from a function, so the
// embedder's client needs no socket and no waiting.
type embedTransport func(*http.Request) (*http.Response, error)

func (f embedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// embedReplying answers the one /api/embed request with status and reply. It
// checks the request's method, path and body; it reports with t.Errorf.
func embedReplying(t *testing.T, model, text string, status int, reply string) *http.Client {
	t.Helper()
	return &http.Client{Transport: embedTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/embed" {
			t.Errorf("request is %s %s, want POST /api/embed", r.Method, r.URL.Path)
		}
		var got struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		if got.Model != model || len(got.Input) != 1 || got.Input[0] != text {
			t.Errorf("request is model %q input %q, want model %q input [%q]", got.Model, got.Input, model, text)
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(reply)),
			Header:     make(http.Header),
		}, nil
	})}
}

func TestOllamaEmbedderReturnsTheVector(t *testing.T) {
	t.Parallel()
	e := OllamaEmbedder{
		URL:     "http://ollama.test",
		Model:   "qwen3",
		Dim:     3,
		Timeout: 5 * time.Second,
		HTTP:    embedReplying(t, "qwen3", "Constraints:\nx", http.StatusOK, `{"model":"qwen3","embeddings":[[0.5,-1,2]]}`),
	}
	got, err := e.Embed(context.Background(), "Constraints:\nx")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := []float32{0.5, -1, 2}
	if len(got) != len(want) {
		t.Fatalf("Embed returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Embed[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestEmbedEndpointJoinsThePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		base string
	}{
		{"base URL without a trailing slash", "http://ollama.test"},
		{"base URL with a trailing slash", "http://ollama.test/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got string
			e := OllamaEmbedder{
				URL:     tt.base,
				Model:   "qwen3",
				Dim:     3,
				Timeout: 5 * time.Second,
				HTTP: &http.Client{Transport: embedTransport(func(r *http.Request) (*http.Response, error) {
					got = r.URL.Path
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"embeddings":[[0.5,-1,2]]}`)),
						Header:     make(http.Header),
					}, nil
				})},
			}
			if _, err := e.Embed(context.Background(), "x"); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if got != "/api/embed" {
				t.Errorf("request path = %q, want %q", got, "/api/embed")
			}
		})
	}
}

func TestOllamaEmbedderRefusesABadReply(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		reply  string
		dim    int
	}{
		{"a 500 status is an error", http.StatusInternalServerError, `{"embeddings":[[0.5,-1,2]]}`, 3},
		{"a wrong dimension is an error", http.StatusOK, `{"embeddings":[[0.5,-1]]}`, 3},
		{"no embeddings is an error", http.StatusOK, `{"embeddings":[]}`, 3},
		{"two vectors for one input is an error", http.StatusOK, `{"embeddings":[[1,2,3],[4,5,6]]}`, 3},
		{"a reply that is not JSON is an error", http.StatusOK, `not json`, 3},
		{"a zero dimension refuses every reply", http.StatusOK, `{"embeddings":[[0.5,-1,2]]}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := OllamaEmbedder{
				URL:     "http://ollama.test",
				Model:   "qwen3",
				Dim:     tt.dim,
				Timeout: 5 * time.Second,
				HTTP:    embedReplying(t, "qwen3", "x", tt.status, tt.reply),
			}
			if got, err := e.Embed(context.Background(), "x"); err == nil {
				t.Fatalf("Embed = %v, nil error; want an error", got)
			}
		})
	}
}

func TestOllamaEmbedderErrorDoesNotEchoTheReply(t *testing.T) {
	t.Parallel()
	const secret = "upstream-secret-token"
	e := OllamaEmbedder{
		URL:     "http://ollama.test",
		Model:   "qwen3",
		Dim:     3,
		Timeout: 5 * time.Second,
		HTTP:    embedReplying(t, "qwen3", "x", http.StatusInternalServerError, secret),
	}
	_, err := e.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("Embed on a 500 status returned nil error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error %q echoes the server's reply", err)
	}
}

func TestOllamaEmbedderTimeoutIsAnError(t *testing.T) {
	t.Parallel()
	// The transport never answers; it returns when the request's context does,
	// which the embedder's timeout ends. No socket, and the wait is the timeout.
	e := OllamaEmbedder{
		URL:     "http://ollama.test",
		Model:   "qwen3",
		Dim:     3,
		Timeout: 20 * time.Millisecond,
		HTTP: &http.Client{Transport: embedTransport(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})},
	}
	_, err := e.Embed(context.Background(), "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Embed error = %v, want context.DeadlineExceeded", err)
	}
}

func TestOllamaEmbedderRefusesABadURL(t *testing.T) {
	t.Parallel()
	// A control character makes the URL unparseable, so no request is built.
	e := OllamaEmbedder{
		URL:     "http://ollama.test\x7f",
		Model:   "qwen3",
		Dim:     3,
		Timeout: 5 * time.Second,
		HTTP: &http.Client{Transport: embedTransport(func(r *http.Request) (*http.Response, error) {
			t.Errorf("an unparseable URL sent a request to %s", r.URL.Path)
			return nil, errors.New("unexpected request")
		})},
	}
	_, err := e.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("Embed with an unparseable URL returned nil error; want an error")
	}
	// The message tells this refusal, made while the endpoint is built, from a
	// failure later, when a request is sent.
	if !strings.Contains(err.Error(), "build embed endpoint") {
		t.Errorf("error %q does not say the endpoint could not be built", err)
	}
}

func TestOllamaEmbedderRefusesToRunUnset(t *testing.T) {
	t.Parallel()
	// A transport that is reached fails the test: the refusal must come first.
	reached := embedTransport(func(r *http.Request) (*http.Response, error) {
		t.Errorf("an unset embedder sent a request to %s", r.URL.Path)
		return nil, errors.New("unexpected request")
	})
	tests := []struct {
		name    string
		timeout time.Duration
		client  *http.Client
	}{
		{"a zero timeout sends nothing", 0, &http.Client{Transport: reached}},
		{"a nil client sends nothing", 5 * time.Second, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := OllamaEmbedder{URL: "http://ollama.test", Model: "qwen3", Dim: 3, Timeout: tt.timeout, HTTP: tt.client}
			if _, err := e.Embed(context.Background(), "x"); err == nil {
				t.Fatal("Embed returned nil error; want an error")
			}
		})
	}
}

// synthEmbeddingWeights returns the embedded weights with an embedding block
// of the given coefficients and intercept.
func synthEmbeddingWeights(t *testing.T, coef []float64, intercept float64) Weights {
	t.Helper()
	data := weightsWith(t, func(f *weightsFile) {
		f.Embedding = embeddingBlock{Model: "synthetic", Dim: len(coef), Coef: coef, Intercept: intercept}
	})
	return loadWeightsFrom(t, data)
}

func TestEmbeddingMinutesIsExpOfTheDotProduct(t *testing.T) {
	t.Parallel()
	// coef . vec = 0.5*1 + -1*2 + 2*0.5 = -0.5; intercept 0.25; exp(-0.25).
	w := synthEmbeddingWeights(t, []float64{0.5, -1, 2}, 0.25)
	got, err := w.EmbeddingMinutes([]float32{1, 2, 0.5})
	if err != nil {
		t.Fatalf("EmbeddingMinutes: %v", err)
	}
	const want = 0.7788007830714049
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("EmbeddingMinutes = %v, want %v", got, want)
	}
}

func TestEmbeddingMinutesRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		weights func(t *testing.T) Weights
		vec     []float32
	}{
		{
			name:    "a vector shorter than the weights is refused",
			weights: func(t *testing.T) Weights { return synthEmbeddingWeights(t, []float64{0.5, -1, 2}, 0) },
			vec:     []float32{1, 2},
		},
		{
			name:    "a vector longer than the weights is refused",
			weights: func(t *testing.T) Weights { return synthEmbeddingWeights(t, []float64{0.5, -1, 2}, 0) },
			vec:     []float32{1, 2, 3, 4},
		},
		{
			name:    "a NaN in the vector is refused",
			weights: func(t *testing.T) Weights { return synthEmbeddingWeights(t, []float64{0.5, -1, 2}, 0) },
			vec:     []float32{float32(math.NaN()), 2, 3},
		},
		{
			name:    "a result that overflows to infinity is refused",
			weights: func(t *testing.T) Weights { return synthEmbeddingWeights(t, []float64{2, 0, 0}, 0) },
			vec:     []float32{3.4e38, 0, 0},
		},
		{
			name:    "zero-value weights have no embedding model",
			weights: func(t *testing.T) Weights { return Weights{} },
			vec:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.weights(t).EmbeddingMinutes(tt.vec)
			if err == nil {
				t.Fatalf("EmbeddingMinutes = %v, nil error; want an error", got)
			}
		})
	}
}

func TestEmbeddingMinutesUnderflowIsAnError(t *testing.T) {
	t.Parallel()
	// exp(-1000) underflows to zero, which is not a prediction.
	w := synthEmbeddingWeights(t, []float64{-1000}, 0)
	got, err := w.EmbeddingMinutes([]float32{1})
	if err == nil {
		t.Fatalf("EmbeddingMinutes = %v, nil error; want an error", got)
	}
	if !strings.Contains(err.Error(), "underflow") {
		t.Errorf("error %q does not say the result underflowed", err)
	}
}

func TestEmbeddingMinutesZeroWeightsIsErrNoWeights(t *testing.T) {
	t.Parallel()
	if _, err := (Weights{}).EmbeddingMinutes(nil); !errors.Is(err, ErrNoWeights) {
		t.Errorf("EmbeddingMinutes on zero Weights = %v, want ErrNoWeights", err)
	}
}
