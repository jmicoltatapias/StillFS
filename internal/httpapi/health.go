// Package httpapi contains the small set of real infrastructure endpoints.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// HealthChecker reports whether the configured metadata database is reachable.
type HealthChecker interface {
	HealthCheck(context.Context) error
}

type healthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database,omitempty"`
}

// NewHandler registers liveness and readiness checks; unknown routes remain 404.
func NewHandler(database HealthChecker, s3Handler ...http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := database.HealthCheck(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "not_ready", Database: "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Database: "ok"})
	})
	var console http.Handler
	if _, err := os.Stat("web/dist"); err == nil {
		console = http.StripPrefix("/console/", http.FileServer(http.Dir("web/dist")))
	}
	var api http.Handler
	if len(s3Handler) > 0 {
		api = s3Handler[0]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/live", "/health/ready":
			mux.ServeHTTP(w, r)
		case "/console":
			if console == nil {
				http.NotFound(w, r)
				return
			}
			http.Redirect(w, r, "/console/", http.StatusTemporaryRedirect)
		default:
			if console != nil && strings.HasPrefix(r.URL.Path, "/console/") {
				console.ServeHTTP(w, r)
				return
			}
			if api != nil {
				api.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, body healthResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
