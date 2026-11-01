package gmailapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"gmail-poppers/internal/sink"
)

const defaultBaseURL = "https://gmail.googleapis.com"

// Sink imports messages into one Gmail account. It is safe for concurrent use.
type Sink struct {
	email   string
	cfg     *oauth2.Config
	store   *TokenStore
	log     *slog.Logger
	baseURL string

	mu     sync.Mutex
	client *http.Client
	ts     oauth2.TokenSource
	labels map[string]string
}

func New(email string, cfg *oauth2.Config, store *TokenStore, log *slog.Logger) *Sink {
	return &Sink{email: email, cfg: cfg, store: store, log: log, baseURL: defaultBaseURL}
}

func (s *Sink) Check(ctx context.Context) error {
	token, err := s.store.Load(s.email)
	if err != nil {
		return describe(err)
	}
	client := oauth2.NewClient(ctx, s.cfg.TokenSource(ctx, token))
	defer client.CloseIdleConnections()
	check := session{s: s, client: client, dryRun: true}
	var result struct {
		Labels []gmailLabel `json:"labels"`
	}
	if err := check.do(ctx, http.MethodGet, "/gmail/v1/users/me/labels", "", nil, &result); err != nil {
		return describe(fmt.Errorf("list labels: %w", err))
	}
	return nil
}

// Reset drops the cached client so a freshly stored token is used.
func (s *Sink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client, s.ts, s.labels = nil, nil, nil
}

func (s *Sink) Open(ctx context.Context, dryRun bool) (sink.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		tok, err := s.store.Load(s.email)
		if err != nil {
			return nil, fmt.Errorf("gmail api: %w", err)
		}
		// The token source must outlive the polling run, so it gets its own context.
		s.ts = &savingTokenSource{
			base:  s.cfg.TokenSource(context.Background(), tok),
			last:  tok.AccessToken,
			save:  func(t *oauth2.Token) error { return s.store.Save(s.email, t) },
			onErr: func(err error) { s.log.Error("saving refreshed token failed", "error", err) },
		}
		s.client = &http.Client{
			Timeout:   5 * time.Minute,
			Transport: &oauth2.Transport{Source: s.ts},
		}
	}
	if _, err := s.ts.Token(); err != nil {
		return nil, describe(err)
	}
	return session{s: s, client: s.client, dryRun: dryRun}, nil
}

type session struct {
	s      *Sink
	client *http.Client
	dryRun bool
}

func (session) Close() error { return nil }

func (ss session) Import(ctx context.Context, raw []byte, label string) error {
	ids := []string{"INBOX", "UNREAD"}
	if label != "" {
		id, err := ss.labelID(ctx, label)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if ss.dryRun {
		return nil
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	meta, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err := json.NewEncoder(meta).Encode(map[string][]string{"labelIds": ids}); err != nil {
		return err
	}
	msg, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"message/rfc822"}})
	msg.Write(raw)
	mw.Close()

	q := url.Values{"uploadType": {"multipart"}, "internalDateSource": {"dateHeader"}}
	err := ss.do(ctx, http.MethodPost, "/upload/gmail/v1/users/me/messages/import?"+q.Encode(),
		"multipart/related; boundary="+mw.Boundary(), &body, nil)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && (ae.Code == http.StatusBadRequest || ae.Code == http.StatusRequestEntityTooLarge) {
			return sink.Rejected(describe(err))
		}
		return describe(err)
	}
	return nil
}

type gmailLabel struct {
	ID                    string `json:"id,omitempty"`
	Name                  string `json:"name"`
	LabelListVisibility   string `json:"labelListVisibility,omitempty"`
	MessageListVisibility string `json:"messageListVisibility,omitempty"`
}

// labelID resolves a label name; in dry-run mode a missing label is reported but not created.
func (ss session) labelID(ctx context.Context, name string) (string, error) {
	s := ss.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.labels[name]; ok {
		return id, nil
	}

	var list struct {
		Labels []gmailLabel `json:"labels"`
	}
	if err := ss.do(ctx, http.MethodGet, "/gmail/v1/users/me/labels", "", nil, &list); err != nil {
		return "", describe(fmt.Errorf("list labels: %w", err))
	}
	s.labels = make(map[string]string, len(list.Labels))
	for _, l := range list.Labels {
		s.labels[l.Name] = l.ID
	}
	if id, ok := s.labels[name]; ok {
		return id, nil
	}

	if ss.dryRun {
		s.log.Info("dry run: would create Gmail label", "label", name)
		s.labels[name] = ""
		return "", nil
	}
	b, _ := json.Marshal(gmailLabel{Name: name, LabelListVisibility: "labelShow", MessageListVisibility: "show"})
	var created gmailLabel
	if err := ss.do(ctx, http.MethodPost, "/gmail/v1/users/me/labels", "application/json", bytes.NewReader(b), &created); err != nil {
		return "", describe(fmt.Errorf("create label %q: %w", name, err))
	}
	s.log.Info("created Gmail label", "label", name)
	s.labels[name] = created.ID
	return created.ID, nil
}

func (ss session) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, ss.s.baseURL+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := ss.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return parseError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Code, http.StatusText(e.Code), e.Message)
}

func parseError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var g struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := string(b)
	if json.Unmarshal(b, &g) == nil && g.Error.Message != "" {
		msg = g.Error.Message
	}
	return &apiError{Code: resp.StatusCode, Message: msg}
}

// describe adds the "gmail api" prefix and turns OAuth failures into actionable messages.
func describe(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
		return errors.New("gmail api: authorization expired or revoked, re-authorize with gmail-poppers auth (is the OAuth app published \"In production\"?)")
	}
	return fmt.Errorf("gmail api: %w", err)
}
