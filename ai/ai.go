package ai

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// MakeAIRequest is a thin wrapper that selects an AI provider and delegates the request.
func MakeAIRequest(prompt string, mediaData []byte, mimeType string, timeout time.Duration) (string, int, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var p Provider
	var res string
	var tokens int
	var latency time.Duration
	var err error

	// Smart Routing: Handle Media
	if len(mediaData) > 0 {
		// Prefer local STT if configured
		if strings.HasPrefix(mimeType, "audio/") && os.Getenv("STT_PROVIDER") == "LOCAL" {
			p = &whisperLocalProvider{}
			log.Debug().Msg("Local audio detected. Attempting local Whisper for transcription.")
			return p.Generate(ctx, prompt, mediaData, mimeType, timeout)
		}

		if os.Getenv("GROQ_API_KEY") != "" {
			p = &groqProvider{}
			log.Debug().Msg("Media detected. Attempting Groq for multi-modal processing.")
			res, tokens, latency, err = p.Generate(ctx, prompt, mediaData, mimeType, timeout)
			if err == nil {
				return res, tokens, latency, nil
			}
			log.Warn().Err(err).Msg("Groq media request failed, trying next provider")
		}

		if os.Getenv("OPENAI_API_KEY") != "" {
			p = &openaiProvider{}
			log.Debug().Msg("Media detected. Attempting OpenAI for multi-modal processing.")
			res, tokens, latency, err = p.Generate(ctx, prompt, mediaData, mimeType, timeout)
			if err == nil {
				return res, tokens, latency, nil
			}
			log.Warn().Err(err).Msg("OpenAI media request failed, trying fallback")
		}

		if os.Getenv("GEMINI_API_KEY") != "" {
			p = &geminiProvider{}
			log.Debug().Msg("Media detected. Attempting Gemini for multi-modal processing.")
			res, tokens, latency, err = p.Generate(ctx, prompt, mediaData, mimeType, timeout)
			if err == nil {
				return res, tokens, latency, nil
			}
			log.Warn().Err(err).Msg("Gemini media request failed, trying next provider")
		}

		if os.Getenv("OPENROUTER_ENABLED") == "true" {
			p = &openRouterProvider{}
			log.Debug().Msg("Media detected. Attempting OpenRouter for multi-modal processing.")
			res, tokens, latency, err = p.Generate(ctx, prompt, mediaData, mimeType, timeout)
			if err == nil {
				return res, tokens, latency, nil
			}
			log.Warn().Err(err).Msg("OpenRouter media request failed, trying next provider")
		}
	}

	// Use the selected provider (Gemini or Local)
	p = selectProvider()
	if p == nil {
		return "", 0, 0, fmt.Errorf("no AI provider available")
	}

	log.Debug().
		Str("provider", p.Name()).
		Int("media_len", len(mediaData)).
		Str("mime_type", mimeType).
		Msg("Making AI request")

	return p.Generate(ctx, prompt, mediaData, mimeType, timeout)
}
