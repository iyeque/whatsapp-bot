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

	// Text request: try every available provider in order, so one rate-limited or
	// down provider no longer fails the whole request.
	if len(mediaData) == 0 {
		return generateWithFallback(ctx, prompt, timeout)
	}

	// Media request that exhausted every media-capable provider. Do NOT fall
	// through to a text-only request: the media payload would be silently
	// dropped and the model would answer as if the user had sent no image or
	// voice note, inventing a response to content it never received. Surface the
	// failure instead so the caller can tell the user.
	if err == nil {
		err = fmt.Errorf("no media-capable provider configured: %w", errClientSide)
	}
	log.Error().Err(err).Str("mime", mimeType).Int("bytes", len(mediaData)).
		Msg("All media-capable providers failed; refusing text-only fallback")
	return "", 0, latency, fmt.Errorf("media processing failed (%s, %d bytes): %w", mimeType, len(mediaData), err)
}

// TrippedProviders reports providers currently skipped by the circuit breaker,
// so the bot layer can escalate a sustained outage to the operator.
func TrippedProviders() []string {
	return providerBreaker.trippedProviders()
}
