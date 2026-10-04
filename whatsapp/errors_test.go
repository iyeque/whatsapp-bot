package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"whatsapp-gpt-bot/ai"
)

// TestIsTimeoutErrorClassification proves timeouts are detected structurally,
// and that incidental text cannot produce a false positive.
func TestIsTimeoutErrorClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
		why  string
	}{
		{context.DeadlineExceeded, true, "context deadline"},
		{&ai.HTTPStatusError{StatusCode: http.StatusRequestTimeout}, true, "408"},
		{&ai.HTTPStatusError{StatusCode: http.StatusGatewayTimeout}, true, "504"},
		{errors.New("HTTP request failed: context deadline exceeded"), true, "transport timeout"},
		{errors.New("net/http: request canceled (Client.Timeout exceeded)"), true, "client timeout text"},

		// Must NOT match.
		{&ai.HTTPStatusError{StatusCode: http.StatusServiceUnavailable}, false, "503 is not a timeout"},
		{&ai.HTTPStatusError{StatusCode: http.StatusBadRequest}, false, "400"},
		{errors.New("invalid request"), false, "generic"},
		{nil, false, "nil"},
	}
	for _, c := range cases {
		if got := isTimeoutError(c.err); got != c.want {
			t.Errorf("isTimeoutError(%v)=%v want %v (%s)", c.err, got, c.want, c.why)
		}
	}
}

// TestIsOverloadedErrorClassification is the regression test for the bare "503"
// substring match: digits appearing incidentally must not be read as overload.
func TestIsOverloadedErrorClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
		why  string
	}{
		{&ai.HTTPStatusError{StatusCode: http.StatusServiceUnavailable}, true, "503"},
		{&ai.HTTPStatusError{StatusCode: http.StatusBadGateway}, true, "502"},
		{errors.New("model overloaded"), true, "text signal"},

		// The old implementation returned true for all of these.
		{&ai.HTTPStatusError{StatusCode: http.StatusBadRequest}, false, "400"},
		{errors.New("request contained 503 tokens"), false, "incidental digits"},
		{errors.New("conversation id 503-a91"), false, "incidental digits in id"},
		{errors.New("context length exceeded: 1503 tokens"), false, "digits inside a larger number"},
		{nil, false, "nil"},
	}
	for _, c := range cases {
		if got := isOverloadedError(c.err); got != c.want {
			t.Errorf("isOverloadedError(%v)=%v want %v (%s)", c.err, got, c.want, c.why)
		}
	}
}

// TestUserFacingErrorMessages proves the right message is chosen for each
// failure, since this is what the user actually sees.
func TestUserFacingErrorMessages(t *testing.T) {
	// Mirrors the selection logic in handleTextMessage's error path.
	choose := func(err error) string {
		switch {
		case isTimeoutError(err):
			return "timeout"
		case isOverloadedError(err):
			return "overloaded"
		default:
			return "generic"
		}
	}

	if got := choose(&ai.HTTPStatusError{StatusCode: 504}); got != "timeout" {
		t.Errorf("504 should read as timeout, got %s", got)
	}
	if got := choose(&ai.HTTPStatusError{StatusCode: 503}); got != "overloaded" {
		t.Errorf("503 should read as overloaded, got %s", got)
	}
	if got := choose(&ai.HTTPStatusError{StatusCode: 400}); got != "generic" {
		t.Errorf("400 should read as generic, got %s", got)
	}
	// A 400 mentioning 503 in its body must not be reported as overload.
	misleading := &ai.HTTPStatusError{
		StatusCode: 400,
		Body:       []byte(`{"error":"invalid_request: model 503-v2 does not exist"}`),
	}
	if got := choose(misleading); got != "generic" {
		t.Errorf("HTTP 400 must read as generic despite 503 in the body, got %s", got)
	}
}

// TestWrappedStatusErrorStillClassifies proves errors.As unwrapping works through
// the provider layers, which is how these errors actually arrive.
func TestWrappedStatusErrorStillClassifies(t *testing.T) {
	base := &ai.HTTPStatusError{StatusCode: http.StatusServiceUnavailable}
	wrapped := fmt.Errorf("OpenRouter request failed: %w; body: %s", base, "{}")
	if !isOverloadedError(wrapped) {
		t.Error("wrapped 503 should still classify as overloaded")
	}

	base400 := &ai.HTTPStatusError{StatusCode: http.StatusBadRequest}
	wrapped400 := fmt.Errorf("OpenRouter request failed: %w", base400)
	if isOverloadedError(wrapped400) || isTimeoutError(wrapped400) {
		t.Error("wrapped 400 should classify as neither overloaded nor timeout")
	}
}
