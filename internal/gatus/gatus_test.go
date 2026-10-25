package gatus

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/status"
)

func TestKey(t *testing.T) {
	if got := Key("My.Mail@gmail.com", "web_de #1"); got != "my-mail@gmail-com_web-de--1" {
		t.Fatalf("key = %q", got)
	}
}

func TestPush(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization")
	}))
	defer srv.Close()
	p := &Pusher{BaseURL: srv.URL + "/", Token: "tok", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.push(context.Background(), status.Entry{Account: "me@gmail.com", Source: "telekom", Error: "pop3 login: bad", Duration: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/endpoints/me@gmail-com_telekom/external" {
		t.Errorf("path = %q", gotPath)
	}
	for _, want := range []string{"success=false", "error=pop3+login%3A+bad", "duration=1.5s"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestPushHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown endpoint", http.StatusNotFound)
	}))
	defer srv.Close()
	p := &Pusher{BaseURL: srv.URL, Token: "tok"}
	if err := p.push(context.Background(), status.Entry{Account: "a@b.c", Source: "x"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
}

func TestRender(t *testing.T) {
	cfg, err := config.Parse([]byte(`
accounts:
  me@gmail.com:
    sources:
      telekom: {type: pop3, server: s, user: u, password: p, interval: 5m}
`), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	push := RenderExternalEndpoints(cfg)
	for _, want := range []string{`name: "telekom"`, `group: "me@gmail.com"`, "interval: 25m0s", "${GMAIL_POPPERS_GATUS_TOKEN}"} {
		if !strings.Contains(push, want) {
			t.Errorf("push snippet missing %q:\n%s", want, push)
		}
	}
	pull := RenderPullEndpoints(cfg, "http://poppers:8080/")
	if !strings.Contains(pull, `url: "http://poppers:8080/status/me@gmail.com/telekom"`) {
		t.Errorf("pull snippet:\n%s", pull)
	}
}
