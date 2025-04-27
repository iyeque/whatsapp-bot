package utils

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type Metrics struct {
	TotalRequests  int64
	CacheHits      int64
	CacheMisses    int64
	AverageLatency int64
	FailedRequests int64
	SlowResponses  int64
	ResponseTimes  []time.Duration
	ActiveSessions int64
	MemoryUsage    uint64
	GoroutineCount int
}

var metrics = &Metrics{}

// Track active sessions
func IncrementActiveSessions() {
	atomic.AddInt64(&metrics.ActiveSessions, 1)
}

func DecrementActiveSessions() {
	atomic.AddInt64(&metrics.ActiveSessions, -1)
}

// Update memory metrics
func updateMemoryStats() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	metrics.MemoryUsage = m.Alloc
	metrics.GoroutineCount = runtime.NumGoroutine()
}

// Start memory stats collection
func init() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			updateMemoryStats()
		}
	}()
}

// Existing metric functions
func IncrementRequests() {
	atomic.AddInt64(&metrics.TotalRequests, 1)
}

func IncrementCacheHit() {
	atomic.AddInt64(&metrics.CacheHits, 1)
}

func IncrementCacheMiss() {
	atomic.AddInt64(&metrics.CacheMisses, 1)
}

func RecordLatency(duration time.Duration) {
	atomic.StoreInt64(&metrics.AverageLatency, int64(duration))
	if duration > 5*time.Second {
		atomic.AddInt64(&metrics.SlowResponses, 1)
	}
}

func IncrementFailedRequest() {
	atomic.AddInt64(&metrics.FailedRequests, 1)
}

func GetMetrics() *Metrics {
	updateMemoryStats() // Get latest memory stats
	return metrics
}

// Add LM Studio specific metrics
type LMStudioMetrics struct {
	RequestCount    int64
	TotalLatency    int64
	MaxLatency      int64
	MinLatency      int64
	TokensGenerated int64
}

var lmMetrics = &LMStudioMetrics{
	MinLatency: math.MaxInt64,
}

func RecordLMStudioMetrics(latency time.Duration, tokens int) {
	atomic.AddInt64(&lmMetrics.RequestCount, 1)
	atomic.AddInt64(&lmMetrics.TotalLatency, int64(latency))
	atomic.AddInt64(&lmMetrics.TokensGenerated, int64(tokens))

	// Update max latency
	for {
		current := atomic.LoadInt64(&lmMetrics.MaxLatency)
		if int64(latency) <= current {
			break
		}
		if atomic.CompareAndSwapInt64(&lmMetrics.MaxLatency, current, int64(latency)) {
			break
		}
	}

	// Update min latency
	for {
		current := atomic.LoadInt64(&lmMetrics.MinLatency)
		if int64(latency) >= current {
			break
		}
		if atomic.CompareAndSwapInt64(&lmMetrics.MinLatency, current, int64(latency)) {
			break
		}
	}
}

func GetLMStudioMetrics() *LMStudioMetrics {
	return lmMetrics
}

// Add LM Studio metrics to dashboard
func getLMStudioStats() map[string]interface{} {
	m := GetLMStudioMetrics()
	reqCount := atomic.LoadInt64(&m.RequestCount)

	var avgLatency float64
	if reqCount > 0 {
		avgLatency = float64(atomic.LoadInt64(&m.TotalLatency)) / float64(reqCount) / float64(time.Millisecond)
	}

	return map[string]interface{}{
		"total_requests":     reqCount,
		"avg_latency_ms":     avgLatency,
		"max_latency_ms":     float64(atomic.LoadInt64(&m.MaxLatency)) / float64(time.Millisecond),
		"min_latency_ms":     float64(atomic.LoadInt64(&m.MinLatency)) / float64(time.Millisecond),
		"tokens_generated":   atomic.LoadInt64(&m.TokensGenerated),
		"tokens_per_request": float64(atomic.LoadInt64(&m.TokensGenerated)) / float64(reqCount),
	}
}

type MemoryStats struct {
	HeapAlloc   uint64
	HeapInUse   uint64
	HeapObjects uint64
	GCPauses    []time.Duration
	LastGCTime  time.Time
}

func GetMemoryStats() MemoryStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	return MemoryStats{
		HeapAlloc:   ms.HeapAlloc,
		HeapInUse:   ms.HeapInuse,
		HeapObjects: ms.HeapObjects,
		LastGCTime:  time.Unix(0, int64(ms.LastGC)),
	}
}

// Track GC patterns
var gcPauses []time.Duration
var gcMutex sync.RWMutex

func init() {
	// Monitor GC patterns
	go func() {
		for {
			runtime.GC()
			gcMutex.Lock()
			stats := GetMemoryStats()
			if len(gcPauses) > 100 {
				gcPauses = gcPauses[1:]
			}
			gcPauses = append(gcPauses, time.Since(stats.LastGCTime))
			gcMutex.Unlock()
			time.Sleep(30 * time.Second)
		}
	}()
}

// Add timeout specific metrics
type TimeoutMetrics struct {
	TimeoutCount    int64
	AverageTimeout  int64
	TimeoutsAvoided int64
}

var timeoutMetrics = &TimeoutMetrics{}

func RecordTimeout(wasAvoided bool) {
	if wasAvoided {
		atomic.AddInt64(&timeoutMetrics.TimeoutsAvoided, 1)
	} else {
		atomic.AddInt64(&timeoutMetrics.TimeoutCount, 1)
	}
}

func UpdateAverageTimeout(timeout time.Duration) {
	atomic.StoreInt64(&timeoutMetrics.AverageTimeout, int64(timeout))
}

func GetTimeoutMetrics() *TimeoutMetrics {
	return timeoutMetrics
}