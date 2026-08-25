package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"whatsapp-gpt-bot/types"
	"whatsapp-gpt-bot/utils"

	"github.com/rs/zerolog/log"
)

const (
	GEMINI_API_BASE = "https://generativelanguage.googleapis.com/v1/models/"
	OPENAI_API_BASE = "https://api.openai.org/v1/"
	GROQ_API_BASE   = "https://api.groq.com/openai/v1/"
	OPENROUTER_API_BASE = "https://openrouter.ai/api/v1/"
)

// Provider is an abstraction for AI model providers.
type Provider interface {
	// Name returns the provider name.
	Name() string
	// Generate returns (text, estimatedTokens, latency, error)
	Generate(ctx context.Context, prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error)
}

var client = NewAIClient()

// selectProvider chooses an implementation based on environment variables.
func selectProvider() Provider {
	// Explicit override first
	if p := os.Getenv("AI_PROVIDER"); p != "" {
		switch strings.ToLower(p) {
		case "local", "lmstudio":
			return &localProvider{}
		case "openrouter":
			return &openRouterProvider{}
		case "openai":
			return &openaiProvider{}
		case "gemini":
			return &geminiProvider{}
		case "groq":
			return &groqProvider{}
		}
	}
	// If AI_ENDPOINT is set and AI_PROVIDER is not specified, prefer local provider
	if os.Getenv("AI_ENDPOINT") != "" {
		return &localProvider{}
	}
	// Fallback to Groq if GROQ_API_KEY exists (fast and free)
	if os.Getenv("GROQ_API_KEY") != "" {
		return &groqProvider{}
	}
	// Fallback to OpenAI if OPENAI_API_KEY exists
	if os.Getenv("OPENAI_API_KEY") != "" {
		return &openaiProvider{}
	}
	// Fallback to Gemini if GEMINI_API_KEY exists
	if os.Getenv(types.GEMINI_API_KEY_ENV) != "" {
		return &geminiProvider{}
	}
	return nil
}

// whisperLocalProvider uses a local whisper.cpp server or CLI for transcription
type whisperLocalProvider struct{}

func (w *whisperLocalProvider) Name() string { return "whisper-local" }

func (w *whisperLocalProvider) Generate(ctx context.Context, prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error) {
	sttEndpoint := os.Getenv("STT_ENDPOINT")
	if sttEndpoint == "" {
		sttEndpoint = "http://localhost:9095/inference" // Default whisper.cpp server endpoint
	}

	start := time.Now()
	var transcription string

	if len(mediaData) > 0 && strings.HasPrefix(mimeType, "audio/") {
		log.Debug().Msg("Audio detected. Using local Whisper for transcription.")

		// Use multipart/form-data for whisper.cpp server
		headers := make(map[string]string)
		fields := map[string]string{
			"temperature": "0.0",
		}

		respBody, err := client.PostMultipart(ctx, sttEndpoint, headers, "audio.wav", mediaData, fields)
		if err != nil {
			return "", 0, time.Since(start), fmt.Errorf("local Whisper transcription failed: %w; body: %s", err, string(respBody))
		}

		var whisperResp struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(respBody, &whisperResp); err != nil {
			// Try parsing as alternative format if whisper.cpp server uses different schema
			var altResp map[string]interface{}
			if err2 := json.Unmarshal(respBody, &altResp); err2 == nil {
				if t, ok := altResp["text"].(string); ok {
					transcription = t
				}
			}
			if transcription == "" {
				return "", 0, time.Since(start), fmt.Errorf("failed to decode local Whisper response: %w; body: %s", err, string(respBody))
			}
		} else {
			transcription = whisperResp.Text
		}

		log.Debug().Str("text", transcription).Msg("Local Whisper transcription successful")
		prompt = fmt.Sprintf("%s\n\nUser's voice note content: \"%s\"", prompt, transcription)
	}

	// After transcription, delegate to the primary LLM provider
	p := selectProvider()
	if p == nil {
		return transcription, 0, time.Since(start), fmt.Errorf("no LLM provider available after local transcription")
	}

	return p.Generate(ctx, prompt, nil, "", timeout)
}

// Local OpenAI-compatible provider
type localProvider struct{}

func (l *localProvider) Name() string { return "local" }

func isRateLimitError(err error, body []byte) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), "429") {
		return true
	}
	var parsed map[string]interface{}
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr == nil {
		if code, ok := parsed["code"].(string); ok && code == "token_quota_exceeded" {
			return true
		}
		if msg, ok := parsed["message"].(string); ok && strings.Contains(msg, "Tokens per minute limit exceeded") {
			return true
		}
	}
	return false
}

func callLMStudio(ctx context.Context, prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error) {
	endpoint := os.Getenv("LMSTUDIO_ENDPOINT")
	modelName := os.Getenv("LMSTUDIO_MODEL")
	if endpoint == "" {
		return "", 0, 0, fmt.Errorf("LMSTUDIO_ENDPOINT not configured")
	}

	var body interface{}
	if strings.Contains(strings.ToLower(endpoint), "/chat") {
		content := []map[string]interface{}{
			{"type": "text", "text": prompt},
		}
		if len(mediaData) > 0 {
			if strings.HasPrefix(mimeType, "image/") {
				b64Img := base64.StdEncoding.EncodeToString(mediaData)
				content = append(content, map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]string{
						"url": fmt.Sprintf("data:%s;base64,%s", mimeType, b64Img),
					},
				})
			} else if strings.HasPrefix(mimeType, "audio/") {
				prompt = "[VOICE NOTE RECEIVED] " + prompt
				content[0]["text"] = prompt
			}
		}
		body = map[string]interface{}{
			"model":    modelName,
			"messages": []map[string]interface{}{{"role": "user", "content": content}},
			"stream":   false,
		}
	} else {
		body = map[string]interface{}{
			"model": modelName,
			"input": prompt,
		}
	}

	headers := make(map[string]string)
	start := time.Now()
	bodyBytes, err := client.PostJSON(ctx, endpoint, headers, body)
	latency := time.Since(start)
	if err != nil {
		return "", 0, latency, fmt.Errorf("LM Studio request failed: %w; body: %s", err, string(bodyBytes))
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return "", 0, latency, fmt.Errorf("failed to parse LM Studio response: %w; body: %s", err, string(bodyBytes))
	}

	var text string
	if choices, ok := parsed["choices"].([]interface{}); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := c0["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].(string); ok {
					text = content
				}
			}
			if text == "" {
				if t, ok := c0["text"].(string); ok {
					text = t
				}
			}
		}
	}
	if text == "" {
		if s, ok := parsed["response"].(string); ok {
			text = s
		}
	}

	tokens := 0
	if usage, ok := parsed["usage"].(map[string]interface{}); ok {
		if tt, ok := usage["total_tokens"].(float64); ok {
			tokens = int(tt)
		}
	}

	utils.RecordLMStudioMetrics(latency, tokens)
	if text == "" {
		return "", tokens, latency, fmt.Errorf("LM Studio produced no text; body: %s", string(bodyBytes))
	}
	return text, tokens, latency, nil
}

func callOpenRouter(ctx context.Context, prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error) {
	endpoint := os.Getenv("OPENROUTER_ENDPOINT")
	modelName := os.Getenv("OPENROUTER_MODEL")
	if endpoint == "" {
		endpoint = OPENROUTER_API_BASE + "chat/completions"
	}
	if modelName == "" {
		modelName = "google/gemma-4-26b-a4b-it:free"
	}

	var body interface{}
	if len(mediaData) > 0 && strings.HasPrefix(mimeType, "image/") {
		b64Img := base64.StdEncoding.EncodeToString(mediaData)
		body = map[string]interface{}{
			"model": modelName,
			"messages": []map[string]interface{}{
				{"role": "user", "content": []map[string]interface{}{
					{"type": "text", "text": prompt},
					{"type": "image_url", "image_url": map[string]string{"url": fmt.Sprintf("data:%s;base64,%s", mimeType, b64Img)}},
				}},
			},
		}
	} else {
		body = map[string]interface{}{
			"model":    modelName,
			"messages": []map[string]interface{}{{"role": "user", "content": prompt}},
		}
	}

	headers := make(map[string]string)
	if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		headers["Authorization"] = "Bearer " + k
	} else if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		headers["Authorization"] = "Bearer " + k
	} else if k := os.Getenv("AI_API_KEY"); k != "" {
		headers["Authorization"] = "Bearer " + k
	}

	start := time.Now()
	bodyBytes, err := client.PostJSON(ctx, endpoint, headers, body)
	latency := time.Since(start)
	if err != nil {
		return "", 0, latency, fmt.Errorf("OpenRouter request failed: %w; body: %s", err, string(bodyBytes))
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return "", 0, latency, fmt.Errorf("failed to parse OpenRouter response: %w; body: %s", err, string(bodyBytes))
	}

	var text string
	if choices, ok := parsed["choices"].([]interface{}); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := c0["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].(string); ok {
					text = content
				}
			}
			if text == "" {
				if t, ok := c0["text"].(string); ok {
					text = t
				}
			}
		}
	}
	if text == "" {
		if s, ok := parsed["response"].(string); ok {
			text = s
		}
	}

	tokens := 0
	if usage, ok := parsed["usage"].(map[string]interface{}); ok {
		if tt, ok := usage["total_tokens"].(float64); ok {
			tokens = int(tt)
		}
	}

	utils.RecordLMStudioMetrics(latency, tokens)
	if text == "" {
		return "", tokens, latency, fmt.Errorf("OpenRouter produced no text; body: %s", string(bodyBytes))
	}
	return text, tokens, latency, nil
}

type openRouterProvider struct{}

func (o *openRouterProvider) Name() string { return "openrouter" }

func (o *openRouterProvider) Generate(ctx context.Context, prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error) {
	return callOpenRouter(ctx, prompt, mediaData, mimeType, timeout)
}

func (l *localProvider) Generate(ctx context.Context, prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error) {
	aiEndpoint := os.Getenv("AI_ENDPOINT")
	modelName := os.Getenv("MODEL_NAME")
	if aiEndpoint == "" {
		return "", 0, 0, fmt.Errorf("AI_ENDPOINT not configured for local provider")
	}

	var body interface{}
	if strings.Contains(strings.ToLower(aiEndpoint), "/chat") {
		content := []map[string]interface{}{
			{"type": "text", "text": prompt},
		}
		// Local providers typically only support image media in chat format
		if len(mediaData) > 0 {
			if strings.HasPrefix(mimeType, "image/") {
				b64Img := base64.StdEncoding.EncodeToString(mediaData)
				content = append(content, map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]string{
						"url": fmt.Sprintf("data:%s;base64,%s", mimeType, b64Img),
					},
				})
			} else if strings.HasPrefix(mimeType, "audio/") {
				// If it's audio, most local providers don't support it directly.
				// We append a note to the prompt so the user knows it was a voice note.
				prompt = "[VOICE NOTE RECEIVED] " + prompt
				content[0]["text"] = prompt
			}
		}

		body = map[string]interface{}{
			"model":    modelName,
			"messages": []map[string]interface{}{{"role": "user", "content": content}},
		}
	} else {
		body = map[string]interface{}{
			"model": modelName,
			"input": prompt,
		}
	}

	headers := make(map[string]string)
	if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		headers["Authorization"] = "Bearer " + k
	} else if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		headers["Authorization"] = "Bearer " + k
	} else if k := os.Getenv("AI_API_KEY"); k != "" {
		headers["Authorization"] = "Bearer " + k
	}

	start := time.Now()
	bodyBytes, err := client.PostJSON(ctx, aiEndpoint, headers, body)
	latency := time.Since(start)
	if err != nil {
		if os.Getenv("LMSTUDIO_ENABLED") == "true" {
			log.Warn().Err(err).Str("endpoint", aiEndpoint).Msg("Primary AI failed; falling back to LM Studio")
			if lmText, lmTokens, lmLatency, lmErr := callLMStudio(ctx, prompt, mediaData, mimeType, timeout); lmErr == nil {
				return lmText, lmTokens, lmLatency, nil
			} else {
				log.Warn().Err(lmErr).Msg("LM Studio fallback failed")
			}
		}
		if os.Getenv("OPENROUTER_ENABLED") == "true" {
			log.Warn().Err(err).Str("endpoint", aiEndpoint).Msg("Primary AI failed; falling back to OpenRouter")
			if orText, orTokens, orLatency, orErr := callOpenRouter(ctx, prompt, mediaData, mimeType, timeout); orErr == nil {
				return orText, orTokens, orLatency, nil
			} else {
				log.Warn().Err(orErr).Msg("OpenRouter fallback failed")
			}
		}
		return "", 0, latency, fmt.Errorf("local AI request failed: %w; body: %s", err, string(bodyBytes))
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return "", 0, latency, fmt.Errorf("failed to parse local AI response: %w; body: %s", err, string(bodyBytes))
	}

	// Extract text (simplified)
	var text string
	if choices, ok := parsed["choices"].([]interface{}); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := c0["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].(string); ok {
					text = content
				}
			}
			if text == "" {
				if t, ok := c0["text"].(string); ok {
					text = t
				}
			}
		}
	}
	if text == "" {
		if s, ok := parsed["response"].(string); ok {
			text = s
		}
	}

	tokens := 0
	if usage, ok := parsed["usage"].(map[string]interface{}); ok {
		if tt, ok := usage["total_tokens"].(float64); ok {
			tokens = int(tt)
		}
	}

	utils.RecordLMStudioMetrics(latency, tokens)

	if text == "" {
		return "", tokens, latency, fmt.Errorf("local AI produced no text; body: %s", string(bodyBytes))
	}
	return text, tokens, latency, nil
}

// OpenAI provider
type openaiProvider struct{}

func (o *openaiProvider) Name() string { return "openai" }

func (o *openaiProvider) Generate(ctx context.Context, prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return "", 0, 0, fmt.Errorf("OPENAI_API_KEY environment variable not set")
	}

	headers := map[string]string{
		"Authorization": "Bearer " + apiKey,
	}

	start := time.Now()
	var transcription string
	var tokens int
	var latency time.Duration

	// Handle Audio via Whisper
	if len(mediaData) > 0 && strings.HasPrefix(mimeType, "audio/") {
		log.Debug().Msg("Audio detected. Using Whisper for transcription.")

		fileName := "audio.mp3"
		if strings.Contains(mimeType, "ogg") {
			fileName = "audio.ogg"
		} else if strings.Contains(mimeType, "wav") {
			fileName = "audio.wav"
		}

		fields := map[string]string{
			"model": "whisper-1",
		}

		respBody, err := client.PostMultipart(ctx, OPENAI_API_BASE+"audio/transcriptions", headers, fileName, mediaData, fields)
		if err != nil {
			return "", 0, time.Since(start), fmt.Errorf("Whisper transcription failed: %w; body: %s", err, string(respBody))
		}

		var whisperResp struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(respBody, &whisperResp); err != nil {
			return "", 0, time.Since(start), fmt.Errorf("failed to decode Whisper response: %w", err)
		}

		transcription = whisperResp.Text
		log.Debug().Str("text", transcription).Msg("Whisper transcription successful")

		// Use transcription as the new prompt or part of it
		prompt = fmt.Sprintf("%s\n\nUser's voice note content: \"%s\"", prompt, transcription)
		// We don't return here, we proceed to GPT for a response
	}

	// Handle Chat Completion (Text and/or Images)
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o" // Default to gpt-4o which supports vision
	}

	content := []map[string]interface{}{
		{"type": "text", "text": prompt},
	}

	// Handle Images
	if len(mediaData) > 0 && strings.HasPrefix(mimeType, "image/") {
		b64Img := base64.StdEncoding.EncodeToString(mediaData)
		content = append(content, map[string]interface{}{
			"type": "image_url",
			"image_url": map[string]string{
				"url": fmt.Sprintf("data:%s;base64,%s", mimeType, b64Img),
			},
		})
	}

	reqBody := map[string]interface{}{
		"model":    model,
		"messages": []map[string]interface{}{{"role": "user", "content": content}},
	}

	respBody, err := client.PostJSON(ctx, OPENAI_API_BASE+"chat/completions", headers, reqBody)
	latency = time.Since(start)
	if err != nil {
		return "", 0, latency, fmt.Errorf("OpenAI chat completion failed: %w; body: %s", err, string(respBody))
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", 0, latency, fmt.Errorf("failed to decode OpenAI response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", 0, latency, fmt.Errorf("no choices in OpenAI response")
	}

	text := chatResp.Choices[0].Message.Content
	tokens = chatResp.Usage.TotalTokens

	utils.RecordLMStudioMetrics(latency, tokens)
	return text, tokens, latency, nil
}

// Groq provider (fast and free tier available)
type groqProvider struct{}

func (g *groqProvider) Name() string { return "groq" }

func (g *groqProvider) Generate(ctx context.Context, prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error) {
	apiKey := os.Getenv("GROQ_API_KEY")
	if apiKey == "" {
		return "", 0, 0, fmt.Errorf("GROQ_API_KEY environment variable not set")
	}

	headers := map[string]string{
		"Authorization": "Bearer " + apiKey,
	}

	start := time.Now()
	var transcription string
	var tokens int
	var latency time.Duration

	// Handle Audio via Groq Whisper
	if len(mediaData) > 0 && strings.HasPrefix(mimeType, "audio/") {
		log.Debug().Msg("Audio detected. Using Groq Whisper for transcription.")

		fileName := "audio.mp3"
		if strings.Contains(mimeType, "ogg") {
			fileName = "audio.ogg"
		} else if strings.Contains(mimeType, "wav") {
			fileName = "audio.wav"
		}

		fields := map[string]string{
			"model": "whisper-large-v3",
		}

		respBody, err := client.PostMultipart(ctx, GROQ_API_BASE+"audio/transcriptions", headers, fileName, mediaData, fields)
		if err != nil {
			return "", 0, time.Since(start), fmt.Errorf("Groq Whisper failed: %w; body: %s", err, string(respBody))
		}

		var whisperResp struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(respBody, &whisperResp); err != nil {
			return "", 0, time.Since(start), fmt.Errorf("failed to decode Groq Whisper response: %w", err)
		}

		transcription = whisperResp.Text
		log.Debug().Str("text", transcription).Msg("Groq Whisper transcription successful")
		prompt = fmt.Sprintf("%s\n\nUser's voice note content: \"%s\"", prompt, transcription)
	}

	// Handle Chat Completion (Text and/or Images)
	model := os.Getenv("GROQ_MODEL")
	if model == "" {
		// Use a capable vision model as default if images are present
		if len(mediaData) > 0 && strings.HasPrefix(mimeType, "image/") {
			model = "llama-3.2-11b-vision-preview"
		} else {
			model = "llama-3.3-70b-specdec" // Default powerful text model
		}
	}

	content := []map[string]interface{}{
		{"type": "text", "text": prompt},
	}

	// Handle Images for Groq Vision models
	if len(mediaData) > 0 && strings.HasPrefix(mimeType, "image/") {
		b64Img := base64.StdEncoding.EncodeToString(mediaData)
		content = append(content, map[string]interface{}{
			"type": "image_url",
			"image_url": map[string]string{
				"url": fmt.Sprintf("data:%s;base64,%s", mimeType, b64Img),
			},
		})
	}

	reqBody := map[string]interface{}{
		"model":    model,
		"messages": []map[string]interface{}{{"role": "user", "content": content}},
	}

	respBody, err := client.PostJSON(ctx, GROQ_API_BASE+"chat/completions", headers, reqBody)
	latency = time.Since(start)
	if err != nil {
		return "", 0, latency, fmt.Errorf("Groq chat completion failed: %w; body: %s", err, string(respBody))
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", 0, latency, fmt.Errorf("failed to decode Groq response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", 0, latency, fmt.Errorf("no choices in Groq response")
	}

	text := chatResp.Choices[0].Message.Content
	tokens = chatResp.Usage.TotalTokens

	utils.RecordLMStudioMetrics(latency, tokens)
	return text, tokens, latency, nil
}

// OpenAI provider
type geminiProvider struct{}

func (g *geminiProvider) Name() string { return "gemini" }

func (g *geminiProvider) Generate(ctx context.Context, prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error) {
	// Construction of the Gemini content parts
	var parts []map[string]interface{}

	// Text part
	parts = append(parts, map[string]interface{}{
		"text": prompt,
	})

	// Media part
	if len(mediaData) > 0 {
		cleanMime := mimeType
		if idx := strings.Index(cleanMime, ";"); idx != -1 {
			cleanMime = strings.TrimSpace(cleanMime[:idx])
		}

		// Map common aliases
		if cleanMime == "audio/mp3" {
			cleanMime = "audio/mpeg"
		}

		b64Data := base64.StdEncoding.EncodeToString(mediaData)

		parts = append(parts, map[string]interface{}{
			"inline_data": map[string]interface{}{
				"mime_type": cleanMime,
				"data":      b64Data,
			},
		})

		log.Debug().
			Str("mime", cleanMime).
			Int("data_len", len(b64Data)).
			Msg("Constructed inline_data part")
	}

	reqBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role":  "user",
				"parts": parts,
			},
		},
	}

	geminiAPIKey := os.Getenv(types.GEMINI_API_KEY_ENV)
	if geminiAPIKey == "" {
		return "", 0, 0, fmt.Errorf("GEMINI_API_KEY environment variable not set")
	}

	// Try latest and stable model names
	// Order: gemini-1.5-flash (stable), gemini-1.5-pro (stable), gemini-1.5-flash-8b, gemini-2.0-flash (experimental)
	candidates := []string{"gemini-1.5-flash", "gemini-1.5-pro", "gemini-1.5-flash-8b", "gemini-2.0-flash"}
	if custom := os.Getenv("GEMINI_MODEL"); custom != "" {
		candidates = append([]string{custom}, candidates...)
	}

	var lastErr error
	for _, m := range candidates {
		apiURL := GEMINI_API_BASE + m + ":generateContent?key=" + geminiAPIKey

		log.Debug().Str("model", m).Str("url_base", GEMINI_API_BASE).Msg("Attempting Gemini request")

		start := time.Now()
		var bodyBytes []byte
		var err error

		// Retry with backoff on 429 (Rate Limit)
		for retry := 0; retry < 3; retry++ {
			bodyBytes, err = client.PostJSON(ctx, apiURL, nil, reqBody)
			if err == nil {
				break
			}

			// If it's a 429, wait and retry
			if strings.Contains(err.Error(), "429") {
				backoff := time.Duration(1<<retry) * 2 * time.Second
				log.Warn().Str("model", m).Msgf("Gemini rate limited (429). Retrying in %v...", backoff)
				time.Sleep(backoff)
				continue
			}
			break // For other errors, don't retry the same model
		}

		latency := time.Since(start)

		if err != nil {
			lastErr = fmt.Errorf("Gemini %s failed: %w; body: %s", m, err, string(bodyBytes))
			log.Warn().Err(err).Str("model", m).Msg("Gemini candidate failed")
			continue
		}

		var geminiResp struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(bodyBytes, &geminiResp); err != nil {
			return "", 0, latency, fmt.Errorf("failed to decode response: %w; body: %s", err, string(bodyBytes))
		}

		if len(geminiResp.Candidates) > 0 && len(geminiResp.Candidates[0].Content.Parts) > 0 {
			text := geminiResp.Candidates[0].Content.Parts[0].Text
			tokens := len(strings.Fields(text))
			utils.RecordLMStudioMetrics(latency, tokens)
			return text, tokens, latency, nil
		}
		return "", 0, latency, fmt.Errorf("no content in response")
	}
	return "", 0, 0, lastErr
}
