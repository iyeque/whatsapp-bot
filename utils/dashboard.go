package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type Dashboard struct {
	metrics      *Metrics
	mutex        sync.RWMutex
	startTime    time.Time
	updateTicker *time.Ticker
}

type DashboardStats struct {
	UptimeSeconds    int64   `json:"uptime_seconds"`
	RequestsPerMin   float64 `json:"requests_per_min"`
	AvgResponseTime  float64 `json:"avg_response_time_ms"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	ErrorRate        float64 `json:"error_rate"`
	ActiveSessions   int     `json:"active_sessions"`
	MemoryUsageMB    float64 `json:"memory_usage_mb"`
	GoroutineCount   int     `json:"goroutine_count"`
	SlowResponseRate float64 `json:"slow_response_rate"`
	LMStudio         struct {
		AvgLatencyMS     float64 `json:"avg_latency_ms"`
		MaxLatencyMS     float64 `json:"max_latency_ms"`
		MinLatencyMS     float64 `json:"min_latency_ms"`
		TokensPerRequest float64 `json:"tokens_per_request"`
		TotalRequests    int64   `json:"total_requests"`
	} `json:"lm_studio"`
	Memory struct {
		HeapAllocMB  float64 `json:"heap_alloc_mb"`
		HeapInUseMB  float64 `json:"heap_in_use_mb"`
		HeapObjects  uint64  `json:"heap_objects"`
		AvgGCPauseMS float64 `json:"avg_gc_pause_ms"`
		LastGCTime   string  `json:"last_gc_time"`
	} `json:"memory"`
	Timeouts struct {
		Count       int64   `json:"timeout_count"`
		Avoided     int64   `json:"timeouts_avoided"`
		AvgTimeout  float64 `json:"avg_timeout_ms"`
		TimeoutRate float64 `json:"timeout_rate"`
	} `json:"timeouts"`
}

var dashboard *Dashboard

func InitDashboard() {
	dashboard = &Dashboard{
		metrics:      GetMetrics(),
		startTime:    time.Now(),
		updateTicker: time.NewTicker(1 * time.Minute),
	}

	// Start monitoring server
	go startMonitoringServer()
}

func startMonitoringServer() {
	http.HandleFunc("/metrics", handleMetrics)
	http.HandleFunc("/dashboard", handleDashboard)

	fmt.Println("Dashboard available at http://localhost:8080/dashboard")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Printf("Failed to start dashboard server: %v\n", err)
	}
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	dashboard.mutex.RLock()
	defer dashboard.mutex.RUnlock()

	stats := calculateStats()
	json.NewEncoder(w).Encode(stats)
}

func calculateStats() DashboardStats {
	m := dashboard.metrics
	uptime := time.Since(dashboard.startTime)
	totalReqs := float64(m.TotalRequests)

	stats := DashboardStats{
		UptimeSeconds:   int64(uptime.Seconds()),
		RequestsPerMin:  totalReqs / uptime.Minutes(),
		AvgResponseTime: float64(m.AverageLatency) / float64(time.Millisecond),
		ActiveSessions:  int(m.ActiveSessions),
		MemoryUsageMB:   float64(m.MemoryUsage) / 1024 / 1024,
		GoroutineCount:  m.GoroutineCount,
	}

	if totalReqs > 0 {
		stats.CacheHitRate = float64(m.CacheHits) / totalReqs * 100
		stats.ErrorRate = float64(m.FailedRequests) / totalReqs * 100
		stats.SlowResponseRate = float64(m.SlowResponses) / totalReqs * 100
	}

	// Add LM Studio stats
	lmStats := getLMStudioStats()
	stats.LMStudio.AvgLatencyMS = lmStats["avg_latency_ms"].(float64)
	stats.LMStudio.MaxLatencyMS = lmStats["max_latency_ms"].(float64)
	stats.LMStudio.MinLatencyMS = lmStats["min_latency_ms"].(float64)
	stats.LMStudio.TokensPerRequest = lmStats["tokens_per_request"].(float64)
	stats.LMStudio.TotalRequests = lmStats["total_requests"].(int64)

	// Add memory stats
	memStats := GetMemoryStats()
	gcMutex.RLock()
	var avgGCPause float64
	if len(gcPauses) > 0 {
		total := time.Duration(0)
		for _, pause := range gcPauses {
			total += pause
		}
		avgGCPause = float64(total) / float64(len(gcPauses)) / float64(time.Millisecond)
	}
	gcMutex.RUnlock()

	stats.Memory = struct {
		HeapAllocMB  float64 `json:"heap_alloc_mb"`
		HeapInUseMB  float64 `json:"heap_in_use_mb"`
		HeapObjects  uint64  `json:"heap_objects"`
		AvgGCPauseMS float64 `json:"avg_gc_pause_ms"`
		LastGCTime   string  `json:"last_gc_time"`
	}{
		HeapAllocMB:  float64(memStats.HeapAlloc) / 1024 / 1024,
		HeapInUseMB:  float64(memStats.HeapInUse) / 1024 / 1024,
		HeapObjects:  memStats.HeapObjects,
		AvgGCPauseMS: avgGCPause,
		LastGCTime:   memStats.LastGCTime.Format(time.RFC3339),
	}

	// Add timeout stats
	tm := GetTimeoutMetrics()
	stats.Timeouts = struct {
		Count       int64   `json:"timeout_count"`
		Avoided     int64   `json:"timeouts_avoided"`
		AvgTimeout  float64 `json:"avg_timeout_ms"`
		TimeoutRate float64 `json:"timeout_rate"`
	}{
		Count:       atomic.LoadInt64(&tm.TimeoutCount),
		Avoided:     atomic.LoadInt64(&tm.TimeoutsAvoided),
		AvgTimeout:  float64(atomic.LoadInt64(&tm.AverageTimeout)) / float64(time.Millisecond),
		TimeoutRate: float64(atomic.LoadInt64(&tm.TimeoutCount)) / totalReqs * 100,
	}

	return stats
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	dashboard.mutex.RLock()
	defer dashboard.mutex.RUnlock()

	stats := calculateStats()

	html := fmt.Sprintf(`
		<html>
		<head>
			<title>WhatsApp Bot Dashboard</title>
			<meta http-equiv="refresh" content="5">
			<style>
				body { font-family: Arial; margin: 20px; background: #f5f5f5; }
				.metric { margin: 10px; padding: 15px; border-radius: 8px; background: white; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
				.warning { color: #f44336; }
				.good { color: #4caf50; }
				.neutral { color: #2196f3; }
				.title { font-size: 24px; margin-bottom: 20px; }
				.section { margin: 20px 0; padding: 20px; background: white; border-radius: 8px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
				.section-title { font-size: 18px; margin: 0 0 15px 0; color: #333; }
				.flex-container { display: flex; flex-wrap: wrap; gap: 10px; }
				.metric-card { flex: 1; min-width: 200px; }
				.highlight { font-weight: bold; }
			</style>
		</head>
		<body>
			<h1 class="title">WhatsApp Bot Performance Dashboard</h1>
			
			<div class="section">
				<h2 class="section-title">System Health</h2>
				<div class="flex-container">
					<div class="metric-card">
						<div class="metric">Uptime: %d seconds</div>
						<div class="metric">Memory Usage: %.2f MB</div>
						<div class="metric">Goroutines: %d</div>
					</div>
					<div class="metric-card">
						<div class="metric">Heap Allocated: %.2f MB</div>
						<div class="metric">Heap In Use: %.2f MB</div>
						<div class="metric">Heap Objects: %d</div>
					</div>
					<div class="metric-card">
						<div class="metric">Avg GC Pause: %.2f ms</div>
						<div class="metric">Last GC: %s</div>
					</div>
				</div>
			</div>

			<div class="section">
				<h2 class="section-title">Request Performance</h2>
				<div class="flex-container">
					<div class="metric-card">
						<div class="metric">Requests/min: %.2f</div>
						<div class="metric %s">Response Time: %.2f ms</div>
						<div class="metric">Cache Hit Rate: %.2f%%</div>
					</div>
					<div class="metric-card">
						<div class="metric %s">Error Rate: %.2f%%</div>
						<div class="metric">Active Sessions: %d</div>
					</div>
				</div>
			</div>

			<div class="section">
				<h2 class="section-title">Timeout Performance</h2>
				<div class="flex-container">
					<div class="metric-card">
						<div class="metric %s">Timeout Rate: %.2f%%</div>
						<div class="metric highlight neutral">Timeouts Prevented: %d</div>
					</div>
					<div class="metric-card">
						<div class="metric">Current Timeout: %.2f ms</div>
						<div class="metric">Total Timeouts: %d</div>
					</div>
				</div>
			</div>

			<div class="section">
				<h2 class="section-title">LM Studio Performance</h2>
				<div class="flex-container">
					<div class="metric-card">
						<div class="metric">Total AI Requests: %d</div>
						<div class="metric %s">Average Latency: %.2f ms</div>
					</div>
					<div class="metric-card">
						<div class="metric">Min Latency: %.2f ms</div>
						<div class="metric">Max Latency: %.2f ms</div>
					</div>
					<div class="metric-card">
						<div class="metric">Tokens/Request: %.1f</div>
						<div class="metric %s">Slow Response Rate: %.2f%%</div>
					</div>
				</div>
			</div>
		</body>
		</html>
	`, stats.UptimeSeconds,
		stats.MemoryUsageMB, stats.GoroutineCount,
		stats.Memory.HeapAllocMB, stats.Memory.HeapInUseMB, stats.Memory.HeapObjects,
		stats.Memory.AvgGCPauseMS, stats.Memory.LastGCTime,
		stats.RequestsPerMin,
		getLatencyClass(stats.AvgResponseTime), stats.AvgResponseTime,
		stats.CacheHitRate,
		getErrorClass(stats.ErrorRate), stats.ErrorRate,
		stats.ActiveSessions,
		getTimeoutClass(stats.Timeouts.TimeoutRate), stats.Timeouts.TimeoutRate,
		stats.Timeouts.Avoided,
		stats.Timeouts.AvgTimeout,
		stats.Timeouts.Count,
		stats.LMStudio.TotalRequests,
		getLatencyClass(stats.LMStudio.AvgLatencyMS), stats.LMStudio.AvgLatencyMS,
		stats.LMStudio.MinLatencyMS, stats.LMStudio.MaxLatencyMS,
		stats.LMStudio.TokensPerRequest,
		getSlowResponseClass(stats.SlowResponseRate), stats.SlowResponseRate)

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

func getLatencyClass(latency float64) string {
	if latency > 5000 {
		return "warning"
	}
	return "good"
}

func getErrorClass(rate float64) string {
	if rate > 5 {
		return "warning"
	}
	return "good"
}

func getSlowResponseClass(rate float64) string {
	if rate > 10 {
		return "warning"
	}
	return "good"
}

func getTimeoutClass(rate float64) string {
	if rate > 5 {
		return "warning"
	}
	return "good"
}
