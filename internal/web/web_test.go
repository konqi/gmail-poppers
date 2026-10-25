package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/sink/gmailapi"
	"gmail-poppers/internal/status"
)

func newServer(t *testing.T, mode string) *Server {
	t.Helper()
	cfg, err := config.Parse([]byte(`
accounts:
  me@gmail.com:
    sources:
      telekom: {type: pop3, server: s, user: u, password: p}
      gmx: {type: imap, server: s, user: u, password: p}
`), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	reg := status.NewRegistry()
	for _, s := range cfg.Sources() {
		reg.Register(s)
	}
	reg.Update("me@gmail.com/telekom", func(e *status.Entry) { e.LastRun = time.Now() })
	reg.Update("me@gmail.com/gmx", func(e *status.Entry) { e.LastRun = time.Now(); e.Error = "imap login: bad" })

	tokens, _ := gmailapi.NewTokenStore(t.TempDir())
	oc, _ := gmailapi.LoadOAuthConfig("", "id", "secret")
	return &Server{
		Cfg: cfg, Status: reg, Tokens: tokens, Auth: gmailapi.NewAuth(oc, tokens),
		GatusMode: mode, BasicAuth: "admin:pw", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func do(h http.Handler, method, target string, body url.Values, auth bool) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if auth {
		r.SetBasicAuth("admin", "pw")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestStatusEndpoints(t *testing.T) {
	h := newServer(t, "pull").Handler()
	w := do(h, "GET", "/status/me@gmail.com/telekom", nil, false)
	var j statusJSON
	json.NewDecoder(w.Body).Decode(&j)
	if w.Code != 200 || !j.Healthy || j.Message != "ok" {
		t.Fatalf("telekom: %d %+v", w.Code, j)
	}
	w = do(h, "GET", "/status/me@gmail.com/gmx", nil, false)
	json.NewDecoder(w.Body).Decode(&j)
	if w.Code != 503 || j.Message != "imap login: bad" {
		t.Fatalf("gmx: %d %+v", w.Code, j)
	}
	if w := do(h, "GET", "/status", nil, false); w.Code != 503 {
		t.Fatalf("all: %d", w.Code)
	}
	if w := do(h, "GET", "/status/me@gmail.com/nope", nil, false); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
}

func TestStatusDisabledInPushMode(t *testing.T) {
	h := newServer(t, "push").Handler()
	if w := do(h, "GET", "/status", nil, false); w.Code != 404 {
		t.Fatalf("status in push mode: %d", w.Code)
	}
}

func TestBasicAuth(t *testing.T) {
	h := newServer(t, "pull").Handler()
	if w := do(h, "GET", "/", nil, false); w.Code != 401 {
		t.Fatalf("no auth: %d", w.Code)
	}
	w := do(h, "GET", "/", nil, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "imap login: bad") || !strings.Contains(w.Body.String(), "not authorized") {
		t.Fatalf("index: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/gatus", nil, true); w.Code != 200 || !strings.Contains(w.Body.String(), "external-endpoints:") {
		t.Fatalf("gatus: %d", w.Code)
	}
}

func TestOAuthFlowValidation(t *testing.T) {
	s := newServer(t, "pull")
	h := s.Handler()
	if w := do(h, "GET", "/oauth/start?account=other@gmail.com", nil, true); w.Code != 400 {
		t.Fatalf("unknown account: %d", w.Code)
	}
	w := do(h, "GET", "/oauth/start?account=me@gmail.com", nil, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "accounts.google.com") {
		t.Fatalf("start: %d", w.Code)
	}
	var state string
	for k := range s.pending {
		state = k
	}

	w = do(h, "POST", "/oauth/finish", url.Values{"state": {state}, "url": {gmailapi.RedirectURL + "?state=other&code=x"}}, true)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "does not belong") {
		t.Fatalf("mismatched state: %d %s", w.Code, w.Body.String())
	}
	// The pending state is consumed even on failure.
	w = do(h, "POST", "/oauth/finish", url.Values{"state": {state}, "url": {gmailapi.RedirectURL + "?state=" + state + "&code=x"}}, true)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("reused state: %d %s", w.Code, w.Body.String())
	}
}
