package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPIProvider_Concurrent asserts the API provider declares itself
// safe to call concurrently — the signal the embedding pool gates on.
func TestAPIProvider_Concurrent(t *testing.T) {
	p := NewAPIProvider("http://localhost:11434", "")
	assert.True(t, p.Concurrent(), "the API provider must opt into concurrent embedding")
}

// TestParseRetryAfter covers the delta-seconds Retry-After parser.
func TestParseRetryAfter(t *testing.T) {
	assert.Equal(t, 12*time.Second, parseRetryAfter("12"))
	assert.Equal(t, time.Duration(0), parseRetryAfter(""))
	assert.Equal(t, time.Duration(0), parseRetryAfter("  "))
	assert.Equal(t, time.Duration(0), parseRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT"),
		"the HTTP-date form is not parsed — returns 0 so the caller uses a fixed backoff")
	assert.Equal(t, time.Duration(0), parseRetryAfter("-5"), "a negative hint is rejected")
}

// TestAPIProvider_HonorsRetryAfterOn429 asserts the provider retries
// once after an HTTP 429, honouring the Retry-After header, and
// succeeds when the retry returns 200.
func TestAPIProvider_HonorsRetryAfterOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// First call: rate-limited with a 1-second hint.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		// Retry: succeed with an OpenAI-shaped embedding response.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Data: []openAIEmbedding{{Embedding: []float32{1, 2, 3}, Index: 0}},
		})
	}))
	defer srv.Close()

	// srv.URL has no Ollama markers, so the provider uses OpenAI format.
	p := NewAPIProvider(srv.URL, "text-embedding-3-small")

	start := time.Now()
	vecs, err := p.EmbedBatch(context.Background(), []string{"hello"})
	require.NoError(t, err, "the embed must succeed after the post-429 retry")
	require.Len(t, vecs, 1)
	assert.Equal(t, []float32{1, 2, 3}, vecs[0])
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "exactly one retry after the 429")
	assert.GreaterOrEqual(t, time.Since(start), time.Second,
		"the provider must wait out the Retry-After hint before retrying")
}

// TestAPIProvider_429WithoutHintStillRetries asserts that a 429 with no
// Retry-After header still triggers exactly one retry (on a short fixed
// backoff) rather than failing immediately.
func TestAPIProvider_429WithoutHintStillRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Data: []openAIEmbedding{{Embedding: []float32{4, 5, 6}, Index: 0}},
		})
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "")
	vecs, err := p.EmbedBatch(context.Background(), []string{"x"})
	require.NoError(t, err)
	require.Len(t, vecs, 1)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

// TestAPIProvider_PersistentRateLimitFails asserts a server that keeps
// returning 429 eventually surfaces an error — the retry is bounded to
// one attempt, it is not an infinite loop.
func TestAPIProvider_PersistentRateLimitFails(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "")
	_, err := p.EmbedBatch(context.Background(), []string{"x"})
	require.Error(t, err, "a persistent 429 must eventually fail")
	assert.LessOrEqual(t, atomic.LoadInt32(&calls), int32(2),
		"the 429 retry is bounded to a single re-attempt")
}

// TestAPIProvider_SendsAuthorizationHeader asserts that an embeddings API
// key (GORTEX_EMBEDDINGS_API_KEY) is forwarded as an Authorization: Bearer
// header — the fix that lets gortex use authenticated backends like OpenAI.
func TestAPIProvider_SendsAuthorizationHeader(t *testing.T) {
	t.Setenv("GORTEX_EMBEDDINGS_API_KEY", "test-secret")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0}]}`))
	}))
	defer srv.Close()

	// A non-Ollama URL selects the OpenAI format (the /v1/embeddings path).
	p := NewAPIProvider(srv.URL, "text-embedding-3-small")
	_, err := p.EmbedBatch(context.Background(), []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-secret", gotAuth)
}

// TestAPIProvider_NoKeyOmitsAuthorizationHeader asserts that with no key
// configured, no Authorization header is sent (keyless Ollama stays keyless
// and a stray OPENAI_API_KEY does not leak to a non-OpenAI endpoint).
func TestAPIProvider_NoKeyOmitsAuthorizationHeader(t *testing.T) {
	t.Setenv("GORTEX_EMBEDDINGS_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1],"index":0}]}`))
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "text-embedding-3-small")
	_, err := p.EmbedBatch(context.Background(), []string{"hi"})
	require.NoError(t, err)
	assert.False(t, hadAuth, "no Authorization header without a configured key")
}

// TestAPIProvider_AccumulatesTokenUsage asserts the provider reads the
// OpenAI `usage.total_tokens` field off each embedding response and
// accumulates it across calls — the signal the indexer logs so the paid
// embedding pass reports its actual token spend (it otherwise vanishes).
func TestAPIProvider_AccumulatesTokenUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":7,"total_tokens":7}}`))
	}))
	defer srv.Close()

	p := NewAPIProvider(srv.URL, "text-embedding-3-small")
	assert.Equal(t, int64(0), p.TokensUsed(), "no tokens before any call")

	_, err := p.EmbedBatch(context.Background(), []string{"hello"})
	require.NoError(t, err)
	_, err = p.EmbedBatch(context.Background(), []string{"world"})
	require.NoError(t, err)

	assert.Equal(t, int64(14), p.TokensUsed(), "usage accumulates across batches")
}

// TestTruncateEmbedInputs asserts oversized inputs are head-truncated to the
// byte cap (so OpenAI's 8192-token limit can't abort the whole vector index)
// while normal inputs pass through untouched.
func TestTruncateEmbedInputs(t *testing.T) {
	short := "small symbol"
	long := string(make([]byte, maxEmbedInputBytes+500))

	out := truncateEmbedInputs([]string{short, long})
	assert.Equal(t, short, out[0], "short input untouched")
	assert.LessOrEqual(t, len(out[1]), maxEmbedInputBytes, "long input capped")

	in := []string{"a", "b"}
	assert.Equal(t, in, truncateEmbedInputs(in), "no oversize → same slice values")
}
