package utils

import (
	"time"
	"errors"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func CreateTextMessage(text string) *waProto.Message {
	return &waProto.Message{
		Conversation: proto.String(text),
	}
}

type RetryConfig struct {
	MaxAttempts     int
	InitialInterval time.Duration
	MaxInterval     time.Duration
	MaxElapsedTime  time.Duration
}

func WithRetry(f func() error, config *RetryConfig) error {
	var lastErr error
	startTime := time.Now()

	for attempt := 0; attempt < config.MaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff(attempt, *config))
		}

		if config.MaxElapsedTime > 0 && time.Since(startTime) > config.MaxElapsedTime {
			if lastErr != nil {
				return lastErr
			}
			return errors.New("retry timeout exceeded")
		}

		if err := f(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func backoff(attempt int, config RetryConfig) time.Duration {
	interval := config.InitialInterval * time.Duration(1<<uint(attempt))
	if interval > config.MaxInterval {
		interval = config.MaxInterval
	}
	return interval
}