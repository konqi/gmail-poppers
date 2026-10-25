package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/status"
)

func newServer(mode string) *Server {
	registry := status.NewRegistry()
	for _, name := range []string{"telekom", "gmx"} {
		registry.Register(&config.Source{Account: "me@gmail.com", Name: name, Interval: time.Minute, Timeout: time.Minute})
	}
	registry.Update("me@gmail.com/telekom", func(entry *status.Entry) { entry.LastRun = time.Now() })
	registry.Update("me@gmail.com/gmx", func(entry *status.Entry) { entry.LastRun = time.Now(); entry.Error = "imap login: bad" })
	return &Server{Status: registry, GatusMode: mode}
}

func request(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func TestStatusEndpoints(t *testing.T) {
	handler := newServer("pull").Handler()
	for _, test := range []struct {
		path    string
		code    int
		message string
	}{
		{"/status/me@gmail.com/telekom", 200, "ok"},
		{"/status/me@gmail.com/gmx", 503, "imap login: bad"},
	} {
		response := request(handler, http.MethodGet, test.path)
		var result statusJSON
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if response.Code != test.code || result.Message != test.message {
			t.Fatalf("%s: %d %+v", test.path, response.Code, result)
		}
	}
	if response := request(handler, "GET", "/status"); response.Code != 503 {
		t.Fatalf("all: %d", response.Code)
	}
	if response := request(handler, "GET", "/status/me@gmail.com/nope"); response.Code != 404 {
		t.Fatalf("unknown: %d", response.Code)
	}
}

func TestStatusOnlySurface(t *testing.T) {
	for _, mode := range []string{"pull", "push", "both"} {
		handler := newServer(mode).Handler()
		for _, path := range []string{"/", "/gatus", "/oauth/start?account=me@gmail.com", "/oauth/finish"} {
			for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
				if response := request(handler, method, path); response.Code != 404 {
					t.Errorf("%s %s in %s: %d", method, path, mode, response.Code)
				}
			}
		}
		for _, path := range []string{"/healthz", "/status", "/status/me@gmail.com/telekom"} {
			for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
				response := request(handler, method, path)
				if response.Code != 405 && response.Code != 404 {
					t.Errorf("accepted %s %s in %s", method, path, mode)
				}
			}
		}
		if response := request(handler, "GET", "/healthz"); response.Code != 200 {
			t.Fatalf("healthcheck in %s: %d", mode, response.Code)
		}
		if response := request(handler, "GET", "/status"); mode == "push" && response.Code != 404 {
			t.Fatalf("status in push mode: %d", response.Code)
		}
	}
}
