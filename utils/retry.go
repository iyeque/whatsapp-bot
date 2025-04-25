package utils

import (
	"time"

	"github.com/cenkalti/backoff/v4"
)

// RetryConfig holds configuration for retry operations
type RetryConfig struct {
	InitialInterval time.Duration
	MaxInterval     time.Duration
	MaxElapsedTime  time.Duration
}

// DefaultRetryConfig returns a default retry configuration
func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		InitialInterval: 100 * time.Millisecond,
		MaxInterval:     2 * time.Second,
		MaxElapsedTime:  10 * time.Second,
	}
}

// WithRetry executes an operation with retry logic using exponential backoff
func WithRetry(operation func() error, config *RetryConfig) error {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = config.InitialInterval
	b.MaxInterval = config.MaxInterval
	b.MaxElapsedTime = config.MaxElapsedTime

	return backoff.Retry(operation, b)
}