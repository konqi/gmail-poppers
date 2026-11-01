package web

import (
	"encoding/json"
	"net/http"
	"time"

	"gmail-poppers/internal/gatus"
	"gmail-poppers/internal/status"
)

type Server struct {
	Status    *status.Registry
	GatusMode string
	DryRun    bool
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	if gatus.PullEnabled(s.GatusMode) {
		mux.HandleFunc("GET /status", s.handleStatusAll)
		mux.HandleFunc("GET /status/{account}/{source}", s.handleStatusOne)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

type statusJSON struct {
	Phase         string    `json:"phase,omitempty"`
	QueueDuration string    `json:"queue_duration"`
	Account       string    `json:"account"`
	Source        string    `json:"source"`
	Healthy       bool      `json:"healthy"`
	Message       string    `json:"message"`
	Summary       string    `json:"summary,omitempty"`
	DryRun        bool      `json:"dry_run"`
	LastRun       time.Time `json:"last_run,omitzero"`
	LastSuccess   time.Time `json:"last_success,omitzero"`
	Duration      string    `json:"duration"`
	LastImported  int       `json:"last_imported"`
	TotalImported int       `json:"total_imported"`
	TotalSkipped  int       `json:"total_skipped"`
}

func (s *Server) toJSON(entry status.Entry) statusJSON {
	ok, message := entry.Health(time.Now())
	return statusJSON{
		Phase: entry.Phase, QueueDuration: entry.QueueDuration.Round(time.Millisecond).String(),
		Account: entry.Account, Source: entry.Source, Healthy: ok, Message: message, Summary: entry.Summary, DryRun: s.DryRun,
		LastRun: entry.LastRun, LastSuccess: entry.LastSuccess, Duration: entry.Duration.Round(time.Millisecond).String(),
		LastImported: entry.LastImported, TotalImported: entry.TotalImported, TotalSkipped: entry.TotalSkipped,
	}
}

func (s *Server) handleStatusAll(w http.ResponseWriter, r *http.Request) {
	all := s.Status.All()
	out := make([]statusJSON, 0, len(all))
	healthy := true
	for _, entry := range all {
		result := s.toJSON(entry)
		healthy = healthy && result.Healthy
		out = append(out, result)
	}
	writeJSON(w, healthy, out)
}

func (s *Server) handleStatusOne(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.Status.Get(r.PathValue("account"), r.PathValue("source"))
	if !ok {
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	result := s.toJSON(entry)
	writeJSON(w, result.Healthy, result)
}

func writeJSON(w http.ResponseWriter, healthy bool, value any) {
	w.Header().Set("Content-Type", "application/json")
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(value)
}
