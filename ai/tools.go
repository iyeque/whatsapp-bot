package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------- tool-calling types ----------
//
// These mirror the OpenAI function-calling wire format, which OpenRouter, Groq,
// OpenAI and LM Studio all speak. The existing Provider interface returns plain
// text; tool calling needs the richer shape below, so it lives alongside rather
// than replacing it.

// ToolSpec describes one callable tool.
type ToolSpec struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object describing the tool's arguments.
	Parameters map[string]interface{}
}

// ToolCall is a model-requested invocation of a tool.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // raw JSON string as emitted by the model
}

// ToolMessage is one turn in a tool-calling conversation.
type ToolMessage struct {
	Role    string // "system", "user", "assistant", "tool"
	Content string
	// ToolCalls is set on assistant turns that request tools.
	ToolCalls []ToolCall
	// ToolCallID links a "tool" result message back to the request.
	ToolCallID string
}

// ToolChatResult is the outcome of a single model turn.
type ToolChatResult struct {
	Text      string
	ToolCalls []ToolCall
	Tokens    int
	Latency   time.Duration
	Provider  string
}

// ---------- endpoint selection ----------

// toolEndpoint is an OpenAI-compatible chat-completions endpoint.
type toolEndpoint struct {
	name  string
	url   string
	key   string
	model string
}

// toolCapableModels returns the lowercase substrings of models known to support
// function calling. The local LM Studio model is deliberately absent: measured
// behaviour is that it returns an empty tool_calls array rather than an error,
// so advertising tools to it would silently no-op. Operators can extend this
// with TOOL_CAPABLE_MODELS without a rebuild.
func toolCapableModels() []string {
	defaults := []string{
		"gpt-4", "gpt-3.5-turbo", "o1", "o3",
		"claude", "gemini", "llama-3.3", "llama-3.1-70b", "llama-4",
		"mixtral", "qwen", "deepseek", "mistral-large",
		"liquid/", "nemotron", "hermes", "command-r",
	}
	if extra := strings.TrimSpace(os.Getenv("TOOL_CAPABLE_MODELS")); extra != "" {
		for _, m := range strings.Split(extra, ",") {
			if t := strings.ToLower(strings.TrimSpace(m)); t != "" {
				defaults = append(defaults, t)
			}
		}
	}
	return defaults
}

// ModelSupportsTools reports whether the named model can call tools.
func ModelSupportsTools(model string) bool {
	if model == "" {
		return false
	}
	lower := strings.ToLower(model)
	for _, capable := range toolCapableModels() {
		if strings.Contains(lower, capable) {
			return true
		}
	}
	return false
}

// toolEndpoints returns candidate tool-capable endpoints in priority order.
// Endpoints whose model cannot call tools are excluded entirely, so the caller
// can treat an empty result as "tool calling unavailable".
func toolEndpoints() []toolEndpoint {
	var out []toolEndpoint

	add := func(name, url, key, model string) {
		if url == "" || model == "" || !ModelSupportsTools(model) {
			return
		}
		out = append(out, toolEndpoint{name: name, url: url, key: key, model: model})
	}

	// OpenRouter first when enabled — it is the configured primary.
	if strings.EqualFold(os.Getenv("OPENROUTER_ENABLED"), "true") {
		url := os.Getenv("OPENROUTER_ENDPOINT")
		if url == "" {
			url = OPENROUTER_API_BASE + "chat/completions"
		}
		key := firstNonEmpty(os.Getenv("OPENROUTER_API_KEY"), os.Getenv("OPENAI_API_KEY"), os.Getenv("AI_API_KEY"))
		add("openrouter", url, key, os.Getenv("OPENROUTER_MODEL"))
	}

	// Local LM Studio, only if the operator explicitly marked the model capable.
	if ep := os.Getenv("AI_ENDPOINT"); ep != "" {
		add("local", ep, firstNonEmpty(os.Getenv("AI_API_KEY"), "local"), os.Getenv("LMSTUDIO_MODEL"))
	}

	if k := os.Getenv("GROQ_API_KEY"); k != "" {
		add("groq", GROQ_API_BASE+"chat/completions", k, os.Getenv("GROQ_MODEL"))
	}
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		add("openai", OPENAI_API_BASE+"chat/completions", k, os.Getenv("OPENAI_MODEL"))
	}

	// Drop endpoints whose breaker is open.
	var filtered []toolEndpoint
	for _, ep := range out {
		if providerBreaker.allow(ep.name) {
			filtered = append(filtered, ep)
		}
	}
	return filtered
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ToolsAvailable reports whether any configured endpoint can call tools.
// The bot uses this to decide whether to advertise tools at all — advertising
// them to a model that cannot use them produces fabricated success messages.
func ToolsAvailable() bool {
	return len(toolEndpoints()) > 0
}

// ---------- the tool-capable request ----------

// MakeToolChat performs one model turn with tools available. It returns either
// text, one or more tool calls, or both. The caller is responsible for
// executing the calls and looping.
func MakeToolChat(messages []ToolMessage, tools []ToolSpec, timeout time.Duration) (ToolChatResult, error) {
	endpoints := toolEndpoints()
	if len(endpoints) == 0 {
		return ToolChatResult{}, fmt.Errorf("no tool-capable AI provider configured")
	}

	var lastErr error
	start := time.Now()
	for _, ep := range endpoints {
		res, err := callToolEndpoint(ep, messages, tools, timeout)
		if err == nil {
			providerBreaker.recordSuccess(ep.name)
			res.Provider = ep.name
			return res, nil
		}
		lastErr = err
		if isProviderFault(err) {
			providerBreaker.recordFailure(ep.name)
		} else {
			log.Warn().Err(err).Str("provider", ep.name).
				Msg("Tool-chat error classified as client-side; not counting toward circuit breaker")
		}
		log.Warn().Err(err).Str("provider", ep.name).Msg("Tool chat failed; trying next endpoint")
	}
	return ToolChatResult{}, fmt.Errorf("all tool-capable providers failed, last error: %w (elapsed %v)", lastErr, time.Since(start))
}

// callToolEndpoint issues one OpenAI-compatible chat-completions request with tools.
func callToolEndpoint(ep toolEndpoint, messages []ToolMessage, tools []ToolSpec, timeout time.Duration) (ToolChatResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Serialise messages, including assistant tool_calls and tool results.
	wireMsgs := make([]map[string]interface{}, 0, len(messages))
	for _, m := range messages {
		msg := map[string]interface{}{"role": m.Role}
		if m.Role == "tool" {
			msg["content"] = m.Content
			msg["tool_call_id"] = m.ToolCallID
		} else if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			if m.Content != "" {
				msg["content"] = m.Content
			} else {
				msg["content"] = nil
			}
			calls := make([]map[string]interface{}, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, map[string]interface{}{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      tc.Name,
						"arguments": tc.Arguments,
					},
				})
			}
			msg["tool_calls"] = calls
		} else {
			msg["content"] = m.Content
		}
		wireMsgs = append(wireMsgs, msg)
	}

	wireTools := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		params := t.Parameters
		if params == nil {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		wireTools = append(wireTools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}

	body := map[string]interface{}{
		"model":       ep.model,
		"messages":    wireMsgs,
		"tools":       wireTools,
		"tool_choice": "auto",
	}

	headers := map[string]string{}
	if ep.key != "" {
		headers["Authorization"] = "Bearer " + ep.key
	}

	start := time.Now()
	raw, err := client.PostJSON(ctx, ep.url, headers, body)
	latency := time.Since(start)
	if err != nil {
		return ToolChatResult{}, fmt.Errorf("%s: %w; body: %.300s", ep.name, err, string(raw))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content   interface{} `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ToolChatResult{}, fmt.Errorf("%s: failed to parse response: %w; body: %.300s", ep.name, err, string(raw))
	}
	if parsed.Error != nil {
		return ToolChatResult{}, fmt.Errorf("%s: api error %d: %s", ep.name, parsed.Error.Code, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return ToolChatResult{}, fmt.Errorf("%s: no choices in response", ep.name)
	}

	choice := parsed.Choices[0]
	res := ToolChatResult{Tokens: parsed.Usage.TotalTokens, Latency: latency}

	// content may be a string or null.
	if s, ok := choice.Message.Content.(string); ok {
		res.Text = strings.TrimSpace(s)
	}
	for _, tc := range choice.Message.ToolCalls {
		if tc.Function.Name == "" {
			continue
		}
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return res, nil
}
