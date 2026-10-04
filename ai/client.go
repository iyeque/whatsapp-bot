package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AIClient handles shared HTTP logic for AI providers
type AIClient struct {
	HTTPClient *http.Client
}

func NewAIClient() *AIClient {
	return &AIClient{
		HTTPClient: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}
}

// HTTPStatusError is a non-2xx response from an AI provider.
//
// It exists so callers can classify a failure structurally — by status code —
// instead of pattern-matching on an error string. The circuit breaker and the
// retry logic both depend on that distinction: a 400 is the caller's fault and
// must never trip a provider's breaker, while a 429 or 503 is a genuine provider
// fault worth counting.
type HTTPStatusError struct {
	StatusCode int
	URL        string
	Body       []byte
	// RetryAfter is the parsed Retry-After header, or 0 when absent.
	RetryAfter time.Duration
}

// Error preserves the historical "API returned status NNN" wording so existing
// logs and any remaining string-based checks keep working, while adding the
// status text and a bounded body excerpt to make failures diagnosable.
func (e *HTTPStatusError) Error() string {
	msg := fmt.Sprintf("API returned status %d", e.StatusCode)
	if text := http.StatusText(e.StatusCode); text != "" {
		msg += " (" + text + ")"
	}
	if snippet := bodySnippet(e.Body); snippet != "" {
		msg += ": " + snippet
	}
	return msg
}

// IsRetryable reports whether the failure is transient and worth retrying, or
// worth attributing to the provider rather than the request.
//
// 4xx responses other than 408 and 429 are the caller's fault: every provider
// would reject the same request, so they must not be counted against any one of
// them. 5xx and the two retryable 4xx codes indicate a provider-side problem.
func (e *HTTPStatusError) IsRetryable() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// IsClientError reports whether the request itself was rejected, meaning the
// same request would fail against every provider.
func (e *HTTPStatusError) IsClientError() bool {
	return e.StatusCode >= 400 && e.StatusCode < 500 && !e.IsRetryable()
}

// IsRateLimit reports whether this is a throttling response.
func (e *HTTPStatusError) IsRateLimit() bool {
	return e.StatusCode == http.StatusTooManyRequests
}

// statusError builds a typed error from a response, parsing Retry-After.
func statusError(resp *http.Response, url string, body []byte) *HTTPStatusError {
	return &HTTPStatusError{
		StatusCode: resp.StatusCode,
		URL:        url,
		Body:       body,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

// parseRetryAfter understands both forms of the header: delta-seconds and an
// HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// bodySnippet returns a whitespace-collapsed, length-bounded excerpt of a
// response body for inclusion in an error message.
func bodySnippet(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	const maxLen = 200
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}

// bodyIndicatesRateLimit inspects a response body for a throttling signal.
//
// This is needed because some providers (OpenRouter among them) return HTTP 200
// with a rate-limit error embedded in the JSON payload, so the status code alone
// is not enough to detect throttling.
func bodyIndicatesRateLimit(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	var parsed struct {
		Code    interface{} `json:"code"`
		Message string      `json:"message"`
		Error   *struct {
			Code    interface{} `json:"code"`
			Message string      `json:"message"`
		} `json:"error"`
		Metadata struct {
			Raw string `json:"raw"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		if numericCode(parsed.Code) == 429 {
			return true
		}
		if parsed.Error != nil {
			if numericCode(parsed.Error.Code) == 429 {
				return true
			}
			if hasRateLimitPhrase(parsed.Error.Message) {
				return true
			}
		}
		if hasRateLimitPhrase(parsed.Message) || hasRateLimitPhrase(parsed.Metadata.Raw) {
			return true
		}
	}

	// Last resort for non-JSON or unusual shapes.
	return hasRateLimitPhrase(string(body))
}

// numericCode extracts an integer from a JSON value that may decode as a number
// or a string.
func numericCode(v interface{}) int {
	switch c := v.(type) {
	case float64:
		return int(c)
	case string:
		n, _ := strconv.Atoi(c)
		return n
	}
	return 0
}

// hasRateLimitPhrase reports whether text carries a throttling signal.
func hasRateLimitPhrase(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, phrase := range []string{
		"rate limit", "rate_limit", "rate-limited", "ratelimit",
		"too many requests", "token_quota_exceeded", "quota exceeded",
		"tokens per minute", "requests per minute", "slow down",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// PostJSON sends a JSON POST request and returns the response body.
//
// On a non-2xx response it returns both the body and an *HTTPStatusError so the
// caller can inspect the status code directly.
func (c *AIClient) PostJSON(ctx context.Context, url string, headers map[string]string, body interface{}) ([]byte, error) {
	jsonData, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return bodyBytes, statusError(resp, url, bodyBytes)
	}

	return bodyBytes, nil
}

// PostMultipart sends a multipart/form-data POST request.
func (c *AIClient) PostMultipart(ctx context.Context, url string, headers map[string]string, fileName string, fileData []byte, fields map[string]string) ([]byte, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add file
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, fmt.Errorf("failed to create form file: %w", err)
	}
	if _, err := part.Write(fileData); err != nil {
		return nil, fmt.Errorf("failed to write file data: %w", err)
	}

	// Add fields
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			return nil, fmt.Errorf("failed to write field %s: %w", k, err)
		}
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, &body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return bodyBytes, statusError(resp, url, bodyBytes)
	}

	return bodyBytes, nil
}
