package ai

import "time"

type ClientOption func(*Client)

type RetryConfig struct {
	MaxAttempts     int
	InitialInterval time.Duration
	MaxInterval     time.Duration
}
