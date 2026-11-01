package gmailapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"gmail-poppers/internal/sink"
)

type fakeGmail struct {
	mu        sync.Mutex
	labels    []gmailLabel
	imported  []string
	importIDs [][]string
	query     string
	writes    int
	reads     int
	rejectAll bool
}

func (f *fakeGmail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer access" {
		http.Error(w, `{"error":{"message":"no auth"}}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/gmail/v1/users/me/labels":
		f.reads++
		json.NewEncoder(w).Encode(map[string]any{"labels": f.labels})
	case r.Method == "POST" && r.URL.Path == "/gmail/v1/users/me/labels":
		f.writes++
		var l gmailLabel
		json.NewDecoder(r.Body).Decode(&l)
		l.ID = "Label_" + l.Name
		f.labels = append(f.labels, l)
		json.NewEncoder(w).Encode(l)
	case r.Method == "POST" && r.URL.Path == "/upload/gmail/v1/users/me/messages/import":
		f.writes++
		if f.rejectAll {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":400,"message":"Invalid message"}}`))
			return
		}
		f.query = r.URL.RawQuery
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		meta, _ := mr.NextPart()
		var m struct {
			LabelIDs []string `json:"labelIds"`
		}
		json.NewDecoder(meta).Decode(&m)
		part, _ := mr.NextPart()
		raw, _ := io.ReadAll(part)
		f.imported = append(f.imported, string(raw))
		f.importIDs = append(f.importIDs, m.LabelIDs)
		w.Write([]byte(`{"id":"1"}`))
	default:
		http.NotFound(w, r)
	}
}

func newTestSink(t *testing.T, f *fakeGmail) *Sink {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	store, _ := NewTokenStore(t.TempDir())
	store.Save("me@gmail.com", &oauth2.Token{AccessToken: "access", RefreshToken: "r", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
	cfg, _ := LoadOAuthConfig("", "id", "secret")
	s := New("me@gmail.com", cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.baseURL = srv.URL
	return s
}

func TestCheck(t *testing.T) {
	server := &fakeGmail{}
	client := newTestSink(t, server)
	before, err := os.ReadFile(client.store.path(client.email))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(client.store.path(client.email))
	if err != nil {
		t.Fatal(err)
	}
	if server.reads != 1 || server.writes != 0 || string(before) != string(after) {
		t.Fatal("check must list labels without writing to Gmail or tokens")
	}
	client.email = "missing@gmail.com"
	if err := client.Check(context.Background()); err == nil {
		t.Fatal("missing token must fail")
	}
}

func TestCheckRefreshDoesNotSaveToken(t *testing.T) {
	server := &fakeGmail{}
	client := newTestSink(t, server)
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.FormValue("grant_type") != "refresh_token" || request.FormValue("refresh_token") != "r" {
			t.Error("unexpected refresh request")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenEndpoint.Close()
	client.cfg.Endpoint.TokenURL = tokenEndpoint.URL
	if err := client.store.Save(client.email, &oauth2.Token{AccessToken: "expired", RefreshToken: "r", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(client.store.path(client.email))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(client.store.path(client.email))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || server.reads != 1 || server.writes != 0 {
		t.Fatal("refresh during a check must not save tokens or write to Gmail")
	}
}

func TestImportCreatesLabelAndUploads(t *testing.T) {
	f := &fakeGmail{labels: []gmailLabel{{ID: "INBOX", Name: "INBOX"}}}
	s := newTestSink(t, f)
	sess, err := s.Open(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	raw := "Subject: hi\r\n\r\nbody\r\n"
	for i := 0; i < 2; i++ {
		if err := sess.Import(context.Background(), []byte(raw), "telekom"); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.imported) != 2 || f.imported[0] != raw {
		t.Fatalf("imported = %q", f.imported)
	}
	if got := strings.Join(f.importIDs[0], ","); got != "INBOX,UNREAD,Label_telekom" {
		t.Fatalf("label ids = %s", got)
	}
	if !strings.Contains(f.query, "internalDateSource=dateHeader") || !strings.Contains(f.query, "uploadType=multipart") {
		t.Fatalf("query = %s", f.query)
	}
	if len(f.labels) != 2 {
		t.Fatalf("label created %d times", len(f.labels)-1)
	}
}

func TestImportRejected(t *testing.T) {
	f := &fakeGmail{rejectAll: true}
	sess, err := newTestSink(t, f).Open(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	err = sess.Import(context.Background(), []byte("x"), "")
	if !sink.IsRejected(err) || !strings.Contains(err.Error(), "gmail api: 400 Bad Request: Invalid message") {
		t.Fatalf("err = %v", err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := &fakeGmail{}
	sess, err := newTestSink(t, f).Open(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Import(context.Background(), []byte("Subject: x\r\n\r\n"), "telekom"); err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 {
		t.Fatalf("dry run performed %d writes", f.writes)
	}
}

func TestOpenWithoutToken(t *testing.T) {
	store, _ := NewTokenStore(t.TempDir())
	cfg, _ := LoadOAuthConfig("", "id", "secret")
	_, err := New("me@gmail.com", cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil))).Open(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadOAuthConfigFromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "client_secret.json")
	os.WriteFile(p, []byte(`{"installed":{"client_id":"cid","client_secret":"cs","redirect_uris":["http://localhost"]}}`), 0o600)
	cfg, err := LoadOAuthConfig(p, "", "")
	if err != nil || cfg.ClientID != "cid" || cfg.ClientSecret != "cs" || cfg.RedirectURL != "" || cfg.Endpoint.TokenURL == "" {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
	os.WriteFile(p, []byte(`{"other":{}}`), 0o600)
	if _, err := LoadOAuthConfig(p, "", ""); err == nil {
		t.Fatal("expected error for unsupported client file")
	}
}
