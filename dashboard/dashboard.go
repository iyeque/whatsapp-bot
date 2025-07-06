package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"whatsapp-gpt-bot/utils"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	defaultPort = 8080
)

var (
	metrics = &Metrics{
		TotalRequests:    0,
		SuccessResponses: 0,
		ErrorResponses:   0,
		ResponseTimes:    make([]time.Duration, 0, 1000),
	}
	metricsMux sync.RWMutex
)

// Metrics holds the dashboard metrics data
type Metrics struct {
	TotalRequests    int
	SuccessResponses int
	ErrorResponses   int
	ResponseTimes    []time.Duration
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func NewResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Start initializes and starts the metrics dashboard server
func Start() error {
	port := defaultPort

	// Register metrics handlers
	http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/dashboard-metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		
		generalMetrics := utils.GetMetrics()
		lmMetrics := utils.GetLMStudioMetrics()
		timeoutMetrics := utils.GetTimeoutMetrics()
		memStats := utils.GetMemoryStats()

		response := map[string]interface{}{
			"general":   generalMetrics,
			"lm_studio": lmMetrics,
			"timeouts":  timeoutMetrics,
			"memory":    memStats,
			"timestamp": time.Now(),
		}

		json.NewEncoder(w).Encode(response)
	})

	// Serve the dashboard HTML file
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		html, err := os.ReadFile(filepath.Join("dashboard", "index.html"))
		if err != nil {
			http.Error(w, "Could not read dashboard file", http.StatusInternalServerError)
			return
		}
		w.Write(html)
	})

	// Start server in a goroutine
	go func() {
		addr := fmt.Sprintf(":%d", port)
		fmt.Printf("Starting metrics dashboard at http://localhost%s\n", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			fmt.Printf("Error starting metrics dashboard: %v\n", err)
		}
	}()

	return nil
}