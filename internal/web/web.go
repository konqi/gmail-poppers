// Package web serves the status endpoints and the OAuth bootstrap UI.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/gatus"
	"gmail-poppers/internal/sink/gmailapi"
	"gmail-poppers/internal/status"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

type Server struct {
	Cfg       *config.Config
	Status    *status.Registry
	Auth      *gmailapi.Auth // nil if no OAuth client is configured
	Tokens    *gmailapi.TokenStore
	APISinks  map[string]*gmailapi.Sink
	GatusMode string
	DryRun    bool
	BasicAuth string // "user:password", empty disables
	Log       *slog.Logger

	mu      sync.Mutex
	pending map[string]pendingAuth
}

type pendingAuth struct {
	email    string
	verifier string
	expires  time.Time
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	if gatus.PullEnabled(s.GatusMode) {
		mux.HandleFunc("GET /status", s.handleStatusAll)
		mux.HandleFunc("GET /status/{account}/{source}", s.handleStatusOne)
	}
	mux.Handle("GET /{$}", s.protect(s.handleIndex))
	mux.Handle("GET /gatus", s.protect(s.handleGatus))
	mux.Handle("GET /oauth/start", s.protect(s.handleOAuthStart))
	mux.Handle("POST /oauth/finish", s.protect(s.handleOAuthFinish))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) protect(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.BasicAuth != "" {
			user, pass, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(s.BasicAuth)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="gmail-poppers"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	})
}

type statusJSON struct {
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

func (s *Server) toJSON(e status.Entry) statusJSON {
	ok, msg := e.Health(time.Now())
	return statusJSON{
		Account: e.Account, Source: e.Source, Healthy: ok, Message: msg, Summary: e.Summary, DryRun: s.DryRun,
		LastRun: e.LastRun, LastSuccess: e.LastSuccess, Duration: e.Duration.Round(time.Millisecond).String(),
		LastImported: e.LastImported, TotalImported: e.TotalImported, TotalSkipped: e.TotalSkipped,
	}
}

func (s *Server) handleStatusAll(w http.ResponseWriter, r *http.Request) {
	all := s.Status.All()
	out := make([]statusJSON, 0, len(all))
	healthy := true
	for _, e := range all {
		j := s.toJSON(e)
		healthy = healthy && j.Healthy
		out = append(out, j)
	}
	writeJSON(w, healthy, out)
}

func (s *Server) handleStatusOne(w http.ResponseWriter, r *http.Request) {
	e, ok := s.Status.Get(r.PathValue("account"), r.PathValue("source"))
	if !ok {
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	j := s.toJSON(e)
	writeJSON(w, j.Healthy, j)
}

func writeJSON(w http.ResponseWriter, healthy bool, v any) {
	w.Header().Set("Content-Type", "application/json")
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(v)
}

type accountView struct {
	Email      string
	Method     string
	Authorized bool
	Sources    []statusJSON
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	var accounts []accountView
	for _, a := range s.Cfg.AccountList() {
		v := accountView{Email: a.Email, Method: a.Method, Authorized: a.Method != config.MethodAPI || s.Tokens.Exists(a.Email)}
		for _, e := range s.Status.All() {
			if e.Account == a.Email {
				v.Sources = append(v.Sources, s.toJSON(e))
			}
		}
		accounts = append(accounts, v)
	}
	s.render(w, "index.html", map[string]any{
		"Accounts":   accounts,
		"DryRun":     s.DryRun,
		"OAuthReady": s.Auth != nil,
		"Notice":     r.URL.Query().Get("notice"),
	})
}

func (s *Server) handleGatus(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	s.render(w, "gatus.html", map[string]any{
		"Mode": s.GatusMode,
		"Push": gatus.RenderExternalEndpoints(s.Cfg),
		"Pull": gatus.RenderPullEndpoints(s.Cfg, base),
	})
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("account")
	acc, ok := s.Cfg.Accounts[email]
	if !ok || acc.Method != config.MethodAPI {
		http.Error(w, "unknown account or account does not use method api", http.StatusBadRequest)
		return
	}
	if s.Auth == nil {
		http.Error(w, "no Google OAuth client configured (GOOGLE_CLIENT_SECRET_FILE or GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET)", http.StatusServiceUnavailable)
		return
	}
	state := randomToken()
	authURL, verifier := s.Auth.AuthURL(state, email)
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]pendingAuth{}
	}
	now := time.Now()
	for k, p := range s.pending {
		if now.After(p.expires) {
			delete(s.pending, k)
		}
	}
	s.pending[state] = pendingAuth{email: email, verifier: verifier, expires: now.Add(15 * time.Minute)}
	s.mu.Unlock()

	s.render(w, "oauth.html", map[string]any{
		"Email":    email,
		"AuthURL":  authURL,
		"State":    state,
		"Redirect": gmailapi.RedirectURL,
	})
}

func (s *Server) handleOAuthFinish(w http.ResponseWriter, r *http.Request) {
	state := r.PostFormValue("state")
	s.mu.Lock()
	p, ok := s.pending[state]
	if ok {
		delete(s.pending, state)
	}
	s.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		s.renderError(w, "This authorization attempt has expired. Please start again.")
		return
	}

	u, err := url.Parse(strings.TrimSpace(r.PostFormValue("url")))
	if err != nil {
		s.renderError(w, "The pasted text is not a valid URL.")
		return
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		s.renderError(w, "Google returned an error: "+e)
		return
	}
	if q.Get("state") != state || q.Get("code") == "" {
		s.renderError(w, "The pasted URL does not belong to this authorization attempt. Please copy the complete URL from the browser's address bar.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Auth.Exchange(ctx, q.Get("code"), p.verifier, p.email); err != nil {
		s.Log.Warn("oauth authorization failed", "account", p.email, "error", err)
		s.renderError(w, err.Error())
		return
	}
	if sk := s.APISinks[p.email]; sk != nil {
		sk.Reset()
	}
	s.Log.Info("gmail account authorized", "account", p.email)
	http.Redirect(w, r, "/?notice="+url.QueryEscape(p.email+" authorized successfully."), http.StatusSeeOther)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		s.Log.Error("rendering template failed", "template", name, "error", err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, "error.html", map[string]any{"Message": msg})
}

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}
