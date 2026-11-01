package gmailapi

import (
	"context"
	"encoding/json"
	"errors"
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
	"testing/synctest"
	"time"

	"golang.org/x/oauth2"

	"gmail-poppers/internal/sink"
	"gmail-poppers/internal/source"
	"gmail-poppers/internal/state"
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

type quotaTransport func(*http.Request) (*http.Response, error)

func (transport quotaTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func quotaResponse(code int, reason, retryAfter string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Retry-After": {retryAfter}},
		Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"message":"synthetic failure","errors":[{"reason":%q}]}}`, reason)))}
}

func TestAPIErrorQuotaClassification(t *testing.T) {
	for _, test := range []struct {
		code   int
		reason string
		quota  bool
	}{
		{429, "", true},
		{403, "rateLimitExceeded", true},
		{403, "userRateLimitExceeded", true},
		{403, "quotaExceeded", true},
		{403, "insufficientPermissions", false},
		{403, "", false},
		{400, "quotaExceeded", false},
		{503, "rateLimitExceeded", false},
	} {
		t.Run(fmt.Sprintf("%d/%s", test.code, test.reason), func(t *testing.T) {
			response := quotaResponse(test.code, test.reason, "120")
			defer response.Body.Close()
			var failure *apiError
			if err := parseError(response); !errors.As(err, &failure) {
				t.Fatalf("error = %v, want apiError", err)
			}
			if failure.Code != test.code || failure.Reason != test.reason || failure.RetryAfter != "120" || failure.Message != "synthetic failure" || failure.quotaError() != test.quota {
				t.Fatalf("parsed error = %+v, quota = %v", failure, failure.quotaError())
			}
		})
	}
}

func TestQuotaRetriesDoNotRejectOrAdvanceState(t *testing.T) {
	for _, test := range []struct {
		code   int
		reason string
	}{{429, ""}, {403, "rateLimitExceeded"}, {403, "userRateLimitExceeded"}, {403, "quotaExceeded"}} {
		t.Run(fmt.Sprintf("%d/%s", test.code, test.reason), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				store, err := state.Open(dir, "synthetic-source")
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				files, err := filepath.Glob(filepath.Join(dir, "*.json"))
				if err != nil || len(files) != 1 {
					t.Fatalf("state files = %v, error = %v", files, err)
				}
				before, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				attempts := 0
				raw := []byte("Message-ID: <quota@example.test>\r\nSubject: quota\r\n\r\nbody")
				var firstBody string
				client := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
					attempts++
					body, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					if attempts == 1 {
						firstBody = string(body)
					} else if string(body) != firstBody {
						t.Error("retry did not replay the complete import body")
					}
					return quotaResponse(test.code, test.reason, ""), nil
				})}
				processor := &source.Processor{State: store,
					Sink: session{s: &Sink{email: "me@gmail.com", baseURL: "https://gmail.test"}, client: client},
					Log:  slog.New(slog.NewTextHandler(io.Discard, nil))}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				err = processor.Handle(ctx, "uid", raw)
				var failure *apiError
				if err == nil || sink.IsRejected(err) || !errors.As(err, &failure) || !failure.quotaError() {
					t.Fatalf("quota error became a permanent rejection: %v", err)
				}
				if attempts != 4 {
					t.Fatalf("attempts = %d, want 4", attempts)
				}
				if processor.Done("uid") || processor.Deletable("uid") || store.KnowsMessageID(source.MessageIDHash(raw)) || processor.Result.Imported != 0 || processor.Result.Skipped != 0 || len(processor.Result.Rejected) != 0 {
					t.Fatalf("quota failure advanced progress: %+v", processor.Result)
				}
				after, err := os.ReadFile(files[0])
				if err != nil || string(before) != string(after) {
					t.Fatalf("quota failure changed persisted state: %v", err)
				}
				if attempts := store.Fail("uid"); attempts != 1 {
					t.Fatalf("quota failure increased permanent rejection count to %d", attempts-1)
				}
			})
		})
	}
}

func TestNonQuotaFailuresAreNotRetried(t *testing.T) {
	transportError := errors.New("ambiguous transport failure")
	for _, test := range []struct {
		name   string
		code   int
		reason string
		err    error
	}{
		{"permissions", 403, "insufficientPermissions", nil},
		{"internal server error", 500, "", nil},
		{"service unavailable", 503, "rateLimitExceeded", nil},
		{"transport", 0, "", transportError},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			client := &http.Client{Transport: quotaTransport(func(*http.Request) (*http.Response, error) {
				attempts++
				if test.err != nil {
					return nil, test.err
				}
				return quotaResponse(test.code, test.reason, ""), nil
			})}
			ss := session{s: &Sink{baseURL: "https://gmail.test"}, client: client}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := ss.Import(ctx, []byte("Subject: x\r\n\r\nbody"), "")
			if err == nil || sink.IsRejected(err) || attempts != 1 {
				t.Fatalf("error = %v, attempts = %d", err, attempts)
			}
			if test.err != nil && !errors.Is(err, transportError) {
				t.Fatalf("transport error lost: %v", err)
			}
			if test.err == nil {
				var failure *apiError
				if !errors.As(err, &failure) || failure.Code != test.code || failure.Reason != test.reason {
					t.Fatalf("original non-quota API error lost: %v", err)
				}
			}
		})
	}
}

func TestQuotaBackoffHonorsContextAndRetryAfter(t *testing.T) {
	for _, mode := range []string{"cancel", "seconds", "date"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				attempts := 0
				client := &http.Client{Transport: quotaTransport(func(*http.Request) (*http.Response, error) {
					attempts++
					retryAfter := "3600"
					if mode == "date" {
						retryAfter = time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
					}
					if mode == "cancel" {
						cancel()
					}
					return quotaResponse(429, "", retryAfter), nil
				})}
				ss := session{s: &Sink{baseURL: "https://gmail.test"}, client: client}
				err := ss.Import(ctx, []byte("Subject: x\r\n\r\nbody"), "")
				want := context.DeadlineExceeded
				if mode == "cancel" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || attempts != 1 {
					t.Fatalf("error = %v, attempts = %d, want %v and 1", err, attempts, want)
				}
			})
		})
	}
}

func TestSessionBudgetChargesEveryAttempt(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		path   string
		units  int
	}{
		{"list labels", http.MethodGet, "/gmail/v1/users/me/labels", 1},
		{"create label", http.MethodPost, "/gmail/v1/users/me/labels", 5},
		{"import", http.MethodPost, "/upload/gmail/v1/users/me/messages/import", 25},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				budget, err := NewBudget(1, 1)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				if remaining := 25 - 2*test.units; remaining > 0 {
					if err := budget.Wait(ctx, "me@gmail.com", remaining); err != nil {
						t.Fatal(err)
					}
				}
				attempts := 0
				client := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
					attempts++
					if request.Method != test.method || request.URL.Path != test.path {
						t.Errorf("request = %s %s", request.Method, request.URL.Path)
					}
					if attempts == 1 {
						return quotaResponse(429, "", ""), nil
					}
					return quotaResponse(200, "", ""), nil
				})}
				ss := session{s: &Sink{email: "me@gmail.com", baseURL: "https://gmail.test", Budget: budget}, client: client}
				start := time.Now()
				if err := ss.do(ctx, test.method, test.path, "", strings.NewReader("synthetic request"), nil); err != nil {
					t.Fatal(err)
				}
				if attempts != 2 {
					t.Fatalf("attempts = %d, want 2", attempts)
				}
				if test.units == 25 && time.Since(start) < 25*time.Minute {
					t.Fatal("import retry bypassed its 25-unit budget")
				}
				deadline, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				if err := budget.Wait(deadline, "me@gmail.com", 1); err == nil {
					t.Fatal("request attempts did not consume their full quota costs")
				}
			})
		})
	}
}

func TestBudgetedCheckAndDryRunRemainReadOnly(t *testing.T) {
	for _, mode := range []string{"check", "dry run"} {
		t.Run(mode, func(t *testing.T) {
			server := &fakeGmail{}
			client := newTestSink(t, server)
			budget, err := NewBudget(1, 1)
			if err != nil {
				t.Fatal(err)
			}
			client.Budget = budget
			before, err := os.ReadFile(client.store.path(client.email))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := budget.Wait(ctx, client.email, 24); err != nil {
				t.Fatal(err)
			}
			if mode == "check" {
				err = client.Check(ctx)
			} else {
				var sess sink.Session
				sess, err = client.Open(ctx, true)
				if err != nil {
					t.Fatal(err)
				}
				defer sess.Close()
				err = sess.Import(ctx, []byte("Subject: x\r\n\r\nbody"), "missing label")
			}
			if err != nil {
				t.Fatalf("read-only operation attempted more than a 1-unit label read: %v", err)
			}
			if err := budget.Wait(ctx, client.email, 1); err == nil {
				t.Fatal("label read bypassed the budget")
			}
			after, err := os.ReadFile(client.store.path(client.email))
			if err != nil {
				t.Fatal(err)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if server.reads != 1 || server.writes != 0 || len(server.imported) != 0 || string(before) != string(after) {
				t.Fatal("budgeted read-only operation wrote Gmail messages, labels, or tokens")
			}
		})
	}
}
