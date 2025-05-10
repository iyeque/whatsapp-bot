package dashboard

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
	"whatsapp-gpt-bot/utils"
)

var (
	dashboardPort = 8080
)

func Start() {
	go startServer()
}

func startServer() {
	http.HandleFunc("/metrics", handleMetrics)
	http.HandleFunc("/stats", handleStats)

	addr := fmt.Sprintf(":%d", dashboardPort)
	log.Printf("[Dashboard] Starting metrics server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Printf("[Dashboard Error] Failed to start metrics server: %v", err)
	}
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	metrics := utils.GetMetrics()
	lmMetrics := utils.GetLMStudioMetrics()
	timeoutMetrics := utils.GetTimeoutMetrics()
	memStats := utils.GetMemoryStats()

	response := map[string]interface{}{
		"general":   metrics,
		"lm_studio": lmMetrics,
		"timeouts":  timeoutMetrics,
		"memory":    memStats,
		"timestamp": time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	stats := utils.GetLMStudioMetrics()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}
