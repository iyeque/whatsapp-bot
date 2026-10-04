package ai

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------- typed status error ----------

// TestHTTPStatusErrorClassification proves classification reads the status code
// directly rather than pattern-matching text.
func TestHTTPStatusErrorClassification(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
		wantClient    bool
		wantRateLimit bool
	}{
		{400, false, true, false},
		{401, false, true, false},
		{403, false, true, false},
		{404, false, true, false},
		{422, false, true, false},
		{408, true, false, false}, // request timeout is transient
		{429, true, false, true},  // rate limit
		{500, true, false, false},
		{502, true, false, false},
		{503, true, false, false},
		{504, true, false, false},
	}
	for _, c := range cases {
		e := &HTTPStatusError{StatusCode: c.status}
		if got := e.IsRetryable(); got != c.wantRetryable {
			t.Errorf("status %d: IsRetryable()=%v want %v", c.status, got, c.wantRetryable)
		}
		if got := e.IsClientError(); got != c.wantClient {
			t.Errorf("status %d: IsClientError()=%v want %v", c.status, got, c.wantClient)
		}
		if got := e.IsRateLimit(); got != c.wantRateLimit {
			t.Errorf("status %d: IsRateLimit()=%v want %v", c.status, got, c.wantRateLimit)
		}
	}
}

// TestStatusErrorPreservesMessageFormat guards the historical wording, since
// logs and any remaining string checks depend on it.
func TestStatusErrorPreservesMessageFormat(t *testing.T) {
	e := &HTTPStatusError{StatusCode: 503, Body: []byte(`{"error":"upstream down"}`)}
	msg := e.Error()
	if !strings.Contains(msg, "API returned status 503") {
		t.Fatalf("message must preserve the historical prefix, got: %s", msg)
	}
	if !strings.Contains(msg, "Service Unavailable") {
		t.Fatalf("message should include the status text, got: %s", msg)
	}
	if !strings.Contains(msg, "upstream down") {
		t.Fatalf("message should include a body excerpt, got: %s", msg)
	}
}

// TestIsProviderFaultUsesStatusCode is the core structural test: the same body
// text with different status codes must classify differently, proving the
// decision comes from the code and not the message.
func TestIsProviderFaultUsesStatusCode(t *testing.T) {
	body := []byte(`{"error":{"message":"429 is mentioned in this text but means nothing"}}`)

	clientErr := &HTTPStatusError{StatusCode: 400, Body: body}
	if isProviderFault(clientErr) {
		t.Fatal("HTTP 400 must be a client fault even if the body mentions 429")
	}

	rateErr := &HTTPStatusError{StatusCode: 429, Body: body}
	if !isProviderFault(rateErr) {
		t.Fatal("HTTP 429 must be a provider fault")
	}

	// Wrapped errors must still be recognised (errors.As unwraps).
	if isProviderFault(fmt.Errorf("openrouter: %w", clientErr)) {
		t.Fatal("wrapped HTTP 400 must still classify as a client fault")
	}
}

// TestIsProviderFaultFallbackCases covers non-HTTP errors, which have no status
// code and fall back to transport-level classification.
func TestIsProviderFaultFallbackCases(t *testing.T) {
	cases := []struct {
		err  error
		want bool
		why  string
	}{
		{errors.New("HTTP request failed: dial tcp: connection refused"), true, "transport"},
		{errors.New("HTTP request failed: no such host"), true, "dns"},
		{errors.New("HTTP request failed: context deadline exceeded"), true, "timeout"},
		{errors.New("HTTP request failed: EOF"), true, "dropped"},
		{fmt.Errorf("openrouter: unsupported media type: %w", errClientSide), false, "client sentinel"},
		{errClientSide, false, "sentinel"},
		{nil, false, "nil"},
	}
	for _, c := range cases {
		if got := isProviderFault(c.err); got != c.want {
			t.Errorf("isProviderFault(%v)=%v want %v (%s)", c.err, got, c.want, c.why)
		}
	}
}

// TestParseRetryAfter covers both header forms.
func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("30"); d != 30*time.Second {
		t.Errorf("delta-seconds: got %v want 30s", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("empty: got %v want 0", d)
	}
	if d := parseRetryAfter("garbage"); d != 0 {
		t.Errorf("garbage: got %v want 0", d)
	}
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d < 40*time.Second || d > 50*time.Second {
		t.Errorf("http-date: got %v want ~45s", d)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(past); d != 0 {
		t.Errorf("past date: got %v want 0", d)
	}
}

// ---------- circuit breaker must not count client-side errors ----------

// TestClientErrorsDoNotTripBreaker is the regression test for the reported bug:
// repeated client-side errors must never open the breaker.
func TestClientErrorsDoNotTripBreaker(t *testing.T) {
	b := &breaker{failures: map[string]int{}, openUntil: map[string]time.Time{}}
	name := "test-provider-client"

	// Many client-side failures (well past the threshold of 3).
	for i := 0; i < 10; i++ {
		err := &HTTPStatusError{StatusCode: 400, Body: []byte(`{"error":"invalid prompt"}`)}
		if isProviderFault(err) {
			b.recordFailure(name)
		}
	}
	if !b.allow(name) {
		t.Fatal("client-side errors must not trip the breaker")
	}
	if b.failures[name] != 0 {
		t.Fatalf("expected 0 recorded failures for client errors, got %d", b.failures[name])
	}

	// Provider faults at the threshold must trip it.
	for i := 0; i < breakerThreshold; i++ {
		err := &HTTPStatusError{StatusCode: 503}
		if isProviderFault(err) {
			b.recordFailure(name)
		}
	}
	if b.allow(name) {
		t.Fatal("provider faults at threshold should trip the breaker")
	}
}

// TestMixedErrorsOnlyProviderFaultsCount proves client errors do not advance the
// counter toward the threshold — the specific false-positive scenario reported.
func TestMixedErrorsOnlyProviderFaultsCount(t *testing.T) {
	b := &breaker{failures: map[string]int{}, openUntil: map[string]time.Time{}}
	name := "mixed"

	clientErr := &HTTPStatusError{StatusCode: 400}
	providerErr := &HTTPStatusError{StatusCode: 503}

	for i := 0; i < 5; i++ {
		if isProviderFault(clientErr) {
			b.recordFailure(name)
		}
		if isProviderFault(providerErr) {
			b.recordFailure(name)
		}
	}
	// 5 provider faults were counted; the point is the count reflects only the
	// 503s, not all 10 errors.
	if b.failures[name] != 5 {
		t.Fatalf("expected exactly 5 counted failures (the 503s), got %d", b.failures[name])
	}
}

// ---------- end-to-end through a real HTTP server ----------

// TestPostJSONReturnsTypedError proves the client attaches the real status code
// to the returned error.
func TestPostJSONReturnsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limit reached"}}`)
	}))
	defer srv.Close()

	c := NewAIClient()
	body, err := c.PostJSON(t.Context(), srv.URL, nil, map[string]string{"a": "b"})
	if err == nil {
		t.Fatal("expected an error for HTTP 429")
	}

	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error should be an *HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status: got %d want 429", statusErr.StatusCode)
	}
	if statusErr.RetryAfter != 12*time.Second {
		t.Errorf("Retry-After: got %v want 12s", statusErr.RetryAfter)
	}
	if len(body) == 0 {
		t.Error("body should still be returned alongside the error")
	}
	if !isProviderFault(err) {
		t.Error("HTTP 429 must classify as a provider fault")
	}
	if !isRateLimitFailure(err) {
		t.Error("HTTP 429 must be detected as a rate limit")
	}
}

// TestPostJSONSuccessIsNotAnError guards the happy path.
func TestPostJSONSuccessIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := NewAIClient()
	body, err := c.PostJSON(t.Context(), srv.URL, nil, map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("unexpected body: %s", body)
	}
}

// TestPostJSONClientErrorDoesNotTripBreaker is the end-to-end regression test:
// a real 400 response must not count toward the breaker.
func TestPostJSONClientErrorDoesNotTripBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"invalid prompt"}}`)
	}))
	defer srv.Close()

	b := &breaker{failures: map[string]int{}, openUntil: map[string]time.Time{}}
	c := NewAIClient()

	for i := 0; i < 10; i++ {
		_, err := c.PostJSON(t.Context(), srv.URL, nil, map[string]string{"a": "b"})
		if err == nil {
			t.Fatal("expected an error")
		}
		if isProviderFault(err) {
			b.recordFailure("provider-x")
		}
	}
	if !b.allow("provider-x") {
		t.Fatal("a real HTTP 400 must never trip the breaker")
	}
}

// TestPostMultipartReturnsTypedError proves the multipart path (used for
// Whisper transcription) is typed too.
func TestPostMultipartReturnsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"whisper down"}`)
	}))
	defer srv.Close()

	c := NewAIClient()
	_, err := c.PostMultipart(t.Context(), srv.URL, nil, "audio.wav", []byte("data"), map[string]string{"model": "whisper-1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error should be an *HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d want 503", statusErr.StatusCode)
	}
	if !isProviderFault(err) {
		t.Error("HTTP 503 must classify as a provider fault")
	}
}

// TestBodyIndicatesRateLimit covers the HTTP-200-with-error-body case.
func TestBodyIndicatesRateLimit(t *testing.T) {
	yes := [][]byte{
		[]byte(`{"code":429}`),
		[]byte(`{"code":"429"}`),
		[]byte(`{"error":{"code":429,"message":"slow down"}}`),
		[]byte(`{"error":{"message":"rate_limit_exceeded"}}`),
		[]byte(`{"message":"Tokens per minute limit exceeded"}`),
		[]byte(`{"code":"token_quota_exceeded"}`),
		[]byte(`{"metadata":{"raw":"provider returned rate-limited"}}`),
	}
	for _, b := range yes {
		if !bodyIndicatesRateLimit(b) {
			t.Errorf("should detect rate limit in: %s", b)
		}
	}

	no := [][]byte{
		nil,
		[]byte(``),
		[]byte(`{"choices":[{"message":{"content":"hello"}}]}`),
		[]byte(`{"error":{"code":400,"message":"bad request"}}`),
	}
	for _, b := range no {
		if bodyIndicatesRateLimit(b) {
			t.Errorf("should NOT detect rate limit in: %s", b)
		}
	}
}

// ---------- media must never be silently downgraded to text ----------

// TestMediaNeverFallsThroughToTextOnly is the regression test for the reported
// bug: a media request must not reach a text-only provider.
func TestMediaNeverFallsThroughToTextOnly(t *testing.T) {
	os.Setenv("AI_PROVIDER", "local")
	os.Setenv("AI_ENDPOINT", "http://127.0.0.1:1/chat/completions") // nothing listens
	os.Setenv("LMSTUDIO_ENABLED", "false")
	os.Setenv("GROQ_API_KEY", "")
	os.Setenv("OPENAI_API_KEY", "")
	os.Setenv("GEMINI_API_KEY", "")
	os.Setenv("OPENROUTER_ENABLED", "false")
	os.Setenv("STT_PROVIDER", "NONE")
	defer func() {
		for _, k := range []string{"AI_PROVIDER", "AI_ENDPOINT", "LMSTUDIO_ENABLED", "STT_PROVIDER"} {
			os.Unsetenv(k)
		}
	}()

	_, _, _, err := MakeAIRequest("describe this", []byte("fake-image-bytes"), "image/jpeg", 2*time.Second)
	if err == nil {
		t.Fatal("media request with no capable provider must fail, not silently succeed")
	}
	if !strings.Contains(err.Error(), "media processing failed") {
		t.Fatalf("error should identify a media failure, got: %v", err)
	}
	// The failure must not poison the breaker.
	if isProviderFault(err) {
		t.Fatalf("media-unavailable error should not count as a provider fault: %v", err)
	}
	t.Logf("media failure surfaced correctly: %v", err)
}

// TestEmptyMediaStillUsesTextFallback proves the guard did not break plain text.
func TestEmptyMediaStillUsesTextFallback(t *testing.T) {
	os.Setenv("AI_PROVIDER", "local")
	os.Setenv("AI_ENDPOINT", "http://127.0.0.1:1/chat/completions")
	os.Setenv("LMSTUDIO_ENABLED", "false")
	os.Setenv("OPENROUTER_ENABLED", "false")
	defer func() {
		os.Unsetenv("AI_PROVIDER")
		os.Unsetenv("AI_ENDPOINT")
		os.Unsetenv("LMSTUDIO_ENABLED")
		os.Unsetenv("OPENROUTER_ENABLED")
	}()

	_, _, _, err := MakeAIRequest("hello", nil, "", 2*time.Second)
	// It will fail (nothing is listening) but must fail via the TEXT path, i.e.
	// "all providers failed" — not the media guard.
	if err == nil {
		t.Skip("provider unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "media processing failed") {
		t.Fatalf("text request must not hit the media guard: %v", err)
	}
	t.Logf("text path error (expected): %v", err)
}
