package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"whatsapp-gpt-bot/utils"
	"whatsapp-gpt-bot/whatsapp"

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
	am         *whatsapp.AccountManager
)

// Metrics holds the dashboard metrics data
type Metrics struct {
	TotalRequests    int
	SuccessResponses int
	ErrorResponses   int
	ResponseTimes    []time.Duration
}

// apiKeyMiddleware ensures only requests with the correct X-API-Key header can access the API.
func apiKeyMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := os.Getenv("DASHBOARD_API_KEY")
		if apiKey == "" {
			fmt.Println("[Dashboard] WARNING: DASHBOARD_API_KEY is not set in .env")
			next.ServeHTTP(w, r)
			return
		}

		requestKey := r.Header.Get("X-API-Key")
		if requestKey != apiKey {
			fmt.Printf("[Dashboard] Unauthorized access attempt to %s from %s\n", r.URL.Path, r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	}
}

// Start initializes and starts the metrics dashboard server
func Start(manager *whatsapp.AccountManager) error {
	am = manager
	port := defaultPort

	// Use a dedicated ServeMux to avoid shadowing and catch-all issues
	mux := http.NewServeMux()

	// 1. Metrics Endpoint (Prometheus)
	mux.Handle("/metrics", promhttp.Handler())

	// 2. JSON Metrics Endpoint (Dashboard)
	mux.HandleFunc("/dashboard-metrics", apiKeyMiddleware(func(w http.ResponseWriter, r *http.Request) {
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
	}))

	// 3. API Endpoints for Management
	mux.HandleFunc("/api/tasks", apiKeyMiddleware(handleTasks))
	mux.HandleFunc("/api/bot/resume", apiKeyMiddleware(handleResume))
	mux.HandleFunc("/api/logs", apiKeyMiddleware(handleLogs))

	// 4. Serve the dashboard HTML file (Only for exact root path)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			fmt.Printf("[Dashboard] 404 Not Found: %s\n", r.URL.Path)
			http.NotFound(w, r)
			return
		}
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
		fmt.Printf("[Dashboard] Starting Nerve Center at http://localhost%s\n", addr)
		server := &http.Server{
			Addr:    addr,
			Handler: mux,
		}
		if err := server.ListenAndServe(); err != nil {
			fmt.Printf("[Dashboard] Error: %v\n", err)
		}
	}()

	return nil
}

func handleTasks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodGet {
		rows, err := am.SqlDB.Query("SELECT id, chat_id, target_jid, cron_expr, instruction, is_dynamic, created_at FROM scheduled_tasks")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		tasks := []map[string]interface{}{}
		for rows.Next() {
			var id int
			var chatID, targetJID, cronExpr, instruction string
			var isDynamic bool
			var createdAt time.Time
			if err := rows.Scan(&id, &chatID, &targetJID, &cronExpr, &instruction, &isDynamic, &createdAt); err == nil {
				tasks = append(tasks, map[string]interface{}{
					"id":          id,
					"chat_id":     chatID,
					"target_jid":  targetJID,
					"cron_expr":   cronExpr,
					"instruction": instruction,
					"is_dynamic":  isDynamic,
					"created_at":  createdAt,
				})
			}
		}
		json.NewEncoder(w).Encode(tasks)
		return
	}

	if r.Method == http.MethodDelete {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "Missing task ID", http.StatusBadRequest)
			return
		}

		_, err := am.SqlDB.Exec("DELETE FROM scheduled_tasks WHERE id = ?", id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bots := am.ListBots()
	for _, b := range bots {
		b.ResumeAutopilot()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "autopilot resumed across all bots"})
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	logPath := filepath.Join("logs", "whatsapp-bot.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		http.Error(w, "Could not read logs", http.StatusInternalServerError)
		return
	}

	lines := strings.Split(string(data), "\n")
	start := len(lines) - 50
	if start < 0 {
		start = 0
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(strings.Join(lines[start:], "\n")))
}
