package ai

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------- claim 2: circuit breaker must not count client-side errors ----------

func TestIsProviderFaultClassification(t *testing.T) {
	cases := []struct {
		err       error
		want      bool
		rationale string
	}{
		// Provider faults — SHOULD count.
		{errors.New("API returned status 429"), true, "rate limit"},
		{errors.New("API returned status 503"), true, "service unavailable"},
		{errors.New("API returned status 500"), true, "internal server error"},
		{errors.New("API returned status 502"), true, "bad gateway"},
		{errors.New("API returned status 504"), true, "gateway timeout"},
		{errors.New("rate_limit_exceeded"), true, "openrouter rate limit"},
		{errors.New("HTTP request failed: context deadline exceeded"), true, "timeout"},
		{errors.New("HTTP request failed: dial tcp: connection refused"), true, "conn refused"},
		{errors.New("model overloaded"), true, "overload"},
		{errors.New("HTTP request failed: EOF"), true, "connection dropped"},

		// Client faults — must NOT count.
		{errors.New("API returned status 400"), false, "bad request"},
		{errors.New("API returned status 401"), false, "unauthorized"},
		{errors.New("API returned status 403"), false, "forbidden"},
		{errors.New("API returned status 404"), false, "not found"},
		{errors.New("API returned status 422"), false, "unprocessable"},
		{errors.New("invalid_request_error"), false, "malformed prompt"},
		{errors.New("context length exceeded"), false, "prompt too long"},
		{errors.New("maximum context window is 8192"), false, "prompt too long"},
		{errors.New("invalid api key"), false, "bad credential"},
		{errors.New("content_policy_violation"), false, "moderation"},
		{fmt.Errorf("openrouter: unsupported media type %q: %w", "audio/ogg", errClientSide), false, "unsupported media"},
		{fmt.Errorf("no media-capable provider configured: %w", errClientSide), false, "no media provider"},
		{errClientSide, false, "sentinel itself"},
		{nil, false, "nil error"},
	}

	for _, c := range cases {
		got := isProviderFault(c.err)
		if got != c.want {
			t.Errorf("isProviderFault(%v) = %v, want %v (%s)", c.err, got, c.want, c.rationale)
		}
	}
}

// TestClientErrorsDoNotTripBreaker is the regression test for the reported bug:
// repeated client-side errors must never open the breaker.
func TestClientErrorsDoNotTripBreaker(t *testing.T) {
	b := &breaker{failures: map[string]int{}, openUntil: map[string]time.Time{}}
	name := "test-provider-client"

	// Simulate many client-side failures (well past the threshold of 3).
	for i := 0; i < 10; i++ {
		err := errors.New("API returned status 400")
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
		err := errors.New("API returned status 503")
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

	// 2 provider faults (below threshold) interleaved with many client errors.
	for i := 0; i < 5; i++ {
		if isProviderFault(errors.New("API returned status 400")) {
			b.recordFailure(name)
		}
		if isProviderFault(errors.New("API returned status 503")) {
			b.recordFailure(name)
		}
	}
	// 5 provider faults were counted, so it SHOULD be tripped; the point is the
	// count reflects only the 503s, not all 10 errors.
	if b.failures[name] != 5 {
		t.Fatalf("expected exactly 5 counted failures (the 503s), got %d", b.failures[name])
	}
}

// ---------- claim 1: media must never be silently downgraded to text ----------

// TestMediaNeverFallsThroughToTextOnly is the regression test for the reported
// bug: a media request must not reach a text-only provider.
func TestMediaNeverFallsThroughToTextOnly(t *testing.T) {
	// Force a config where the text fallback would be reached if the guard were
	// missing: a provider that always fails, with no media-capable provider.
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
	// The failure must be classified client-side so it does not poison the breaker.
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
