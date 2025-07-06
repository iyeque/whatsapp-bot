package whatsapp

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter holds the rate limiters for each user
type RateLimiter struct {
	visitors map[string]*rate.Limiter
	mutex    sync.Mutex
	limit    rate.Limit
	burst    int
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(limit rate.Limit, burst int) *RateLimiter {
	return &RateLimiter{
		visitors: make(map[string]*rate.Limiter),
		limit:    limit,
		burst:    burst,
	}
}

// Allow checks if a user is allowed to make a request
func (rl *RateLimiter) Allow(userID string) bool {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	limiter, exists := rl.visitors[userID]
	if !exists {
		limiter = rate.NewLimiter(rl.limit, rl.burst)
		rl.visitors[userID] = limiter
	}

	return limiter.Allow()
}

// StartCleanup starts a goroutine to clean up old rate limiters
func (rl *RateLimiter) StartCleanup() {
	go func() {
		for {
			time.Sleep(1 * time.Minute)
			rl.cleanupStaleVisitors()
		}
	}()
}

// cleanupStaleVisitors removes rate limiters for users who haven't been active
func (rl *RateLimiter) cleanupStaleVisitors() {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	// In a real-world scenario, you'd want to track the last access time
	// for each user and remove them if they haven't been active for a while.
	// For this example, we'll just keep all visitors.
}