package ai

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestToolEndpointSelection verifies the capability gate: the local model must
// be excluded (it returns empty tool_calls), and a tool-capable model included.
func TestToolEndpointSelection(t *testing.T) {
	// Local model is not tool-capable by measurement.
	os.Setenv("LMSTUDIO_MODEL", "gemma-3-1b-it-glm-4.7-flash-heretic-uncensored-thinking_gguf")
	os.Setenv("AI_ENDPOINT", "http://127.0.0.1:1234/v1/chat/completions")
	if ModelSupportsTools("gemma-3-1b-it-glm-4.7-flash-heretic-uncensored-thinking_gguf") {
		t.Fatal("local gemma model must NOT be treated as tool-capable")
	}

	// A known-capable model is accepted.
	if !ModelSupportsTools("liquid/lfm-2.5-2.6b:free") {
		t.Fatal("liquid model should be tool-capable")
	}
	if !ModelSupportsTools("gpt-4o") {
		t.Fatal("gpt-4o should be tool-capable")
	}
	if ModelSupportsTools("") {
		t.Fatal("empty model must not be tool-capable")
	}

	// Operator override extends the list.
	os.Setenv("TOOL_CAPABLE_MODELS", "my-custom-model")
	defer os.Unsetenv("TOOL_CAPABLE_MODELS")
	if !ModelSupportsTools("my-custom-model-v2") {
		t.Fatal("TOOL_CAPABLE_MODELS override not honoured")
	}
}

// TestToolEndpointsExcludeLocal proves a config with only a non-tool local model
// yields no endpoints, so ToolsAvailable() is false and the bot will not
// advertise tools it cannot use.
func TestToolEndpointsExcludeLocal(t *testing.T) {
	for _, k := range []string{"OPENROUTER_ENABLED", "OPENROUTER_API_KEY", "GROQ_API_KEY", "OPENAI_API_KEY", "TOOL_CAPABLE_MODELS"} {
		os.Unsetenv(k)
	}
	os.Setenv("AI_ENDPOINT", "http://127.0.0.1:1234/v1/chat/completions")
	os.Setenv("LMSTUDIO_MODEL", "gemma-3-1b-it-glm-4.7-flash-heretic-uncensored-thinking_gguf")
	if ToolsAvailable() {
		t.Fatal("expected no tool-capable endpoints for local-only non-tool config")
	}

	// Enabling a capable model makes tools available.
	os.Setenv("OPENROUTER_ENABLED", "true")
	os.Setenv("OPENROUTER_MODEL", "liquid/lfm-2.5-2.6b:free")
	os.Setenv("OPENROUTER_API_KEY", "test-key")
	if !ToolsAvailable() {
		t.Fatal("expected tools available with a capable model configured")
	}
}

// loadDotEnv loads .env from the repo root into the process environment.
// Needed because `go test` does not inherit shell-exported vars.
func loadDotEnv(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile("../.env")
	if err != nil {
		t.Skipf("no .env file: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		os.Setenv(key, val)
	}
}

// TestLiveToolCall exercises the real endpoint end-to-end. Skipped unless
// OPENROUTER_API_KEY is set, so it never breaks an offline build.
func TestLiveToolCall(t *testing.T) {
	loadDotEnv(t)
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("OPENROUTER_API_KEY not set; skipping live tool call")
	}
	os.Setenv("OPENROUTER_ENABLED", "true")
	if os.Getenv("OPENROUTER_MODEL") == "" {
		os.Setenv("OPENROUTER_MODEL", "liquid/lfm-2.5-2.6b:free")
	}

	tools := []ToolSpec{{
		Name:        "schedule_meeting",
		Description: "Schedule a meeting with a named person at a given time.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"attendee":         map[string]interface{}{"type": "string"},
				"when":             map[string]interface{}{"type": "string"},
				"duration_minutes": map[string]interface{}{"type": "integer"},
			},
			"required": []string{"attendee", "when", "duration_minutes"},
		},
	}}

	msgs := []ToolMessage{
		{Role: "system", Content: "You are a scheduling assistant. Use the tools provided."},
		{Role: "user", Content: "Book a 30 minute call with Wilma on Thursday at 2pm."},
	}

	res, err := MakeToolChat(msgs, tools, 60*time.Second)
	if err != nil {
		t.Fatalf("MakeToolChat failed: %v", err)
	}
	t.Logf("provider=%s tokens=%d latency=%v", res.Provider, res.Tokens, res.Latency)
	if len(res.ToolCalls) == 0 {
		t.Fatalf("expected a tool call, got text=%q", res.Text)
	}
	tc := res.ToolCalls[0]
	t.Logf("tool=%s args=%s", tc.Name, tc.Arguments)
	if tc.Name != "schedule_meeting" {
		t.Fatalf("unexpected tool %q", tc.Name)
	}

	// Round-trip: feed a tool result back and expect a natural-language answer.
	msgs = append(msgs, ToolMessage{Role: "assistant", ToolCalls: res.ToolCalls})
	msgs = append(msgs, ToolMessage{
		Role:       "tool",
		ToolCallID: tc.ID,
		Content:    `{"status":"scheduled","attendee":"Wilma","when":"2026-10-08T14:00:00","invite_sent":true}`,
	})
	res2, err := MakeToolChat(msgs, tools, 60*time.Second)
	if err != nil {
		t.Fatalf("second turn failed: %v", err)
	}
	t.Logf("final text: %q", res2.Text)
	if res2.Text == "" {
		t.Fatal("expected a natural-language confirmation after the tool result")
	}
}
