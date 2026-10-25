package gmailapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"gmail-poppers/internal/sink"
)

// Sink imports messages into one Gmail account. It is safe for concurrent use.
type Sink struct {
	email string
	cfg   *oauth2.Config
	store *TokenStore
	log   *slog.Logger

	mu     sync.Mutex
	svc    *gmail.Service
	ts     oauth2.TokenSource
	labels map[string]string
}

func New(email string, cfg *oauth2.Config, store *TokenStore, log *slog.Logger) *Sink {
	return &Sink{email: email, cfg: cfg, store: store, log: log}
}

// Reset drops the cached client so a freshly stored token is used.
func (s *Sink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.svc, s.ts, s.labels = nil, nil, nil
}

func (s *Sink) Open(ctx context.Context, dryRun bool) (sink.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.svc == nil {
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
		svc, err := gmail.NewService(context.Background(), option.WithTokenSource(s.ts))
		if err != nil {
			return nil, fmt.Errorf("gmail api: %w", err)
		}
		s.svc = svc
	}
	if _, err := s.ts.Token(); err != nil {
		return nil, describe(err)
	}
	return session{s: s, dryRun: dryRun}, nil
}

type session struct {
	s      *Sink
	dryRun bool
}

func (session) Close() error { return nil }

func (ss session) Import(ctx context.Context, raw []byte, label string) error {
	s := ss.s
	ids := []string{"INBOX", "UNREAD"}
	if label != "" {
		id, err := s.labelID(ctx, label, !ss.dryRun)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if ss.dryRun {
		return nil
	}
	s.mu.Lock()
	svc := s.svc
	s.mu.Unlock()
	if svc == nil {
		return errors.New("gmail api: client was reset, retrying next run")
	}
	_, err := svc.Users.Messages.Import("me", &gmail.Message{LabelIds: ids}).
		InternalDateSource("dateHeader").
		Media(bytes.NewReader(raw), googleapi.ContentType("message/rfc822")).
		Context(ctx).
		Do()
	if err != nil {
		err = describe(err)
		var ge *googleapi.Error
		if errors.As(err, &ge) && (ge.Code == http.StatusBadRequest || ge.Code == http.StatusRequestEntityTooLarge) {
			return sink.Rejected(err)
		}
		return err
	}
	return nil
}

func (s *Sink) labelID(ctx context.Context, name string, create bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.labels[name]; ok {
		return id, nil
	}
	if s.svc == nil {
		return "", errors.New("gmail api: client was reset, retrying next run")
	}
	if err := s.loadLabels(ctx); err != nil {
		return "", err
	}
	if id, ok := s.labels[name]; ok {
		return id, nil
	}
	if !create {
		s.log.Info("dry run: would create Gmail label", "label", name)
		s.labels[name] = ""
		return "", nil
	}
	l, err := s.svc.Users.Labels.Create("me", &gmail.Label{
		Name:                  name,
		LabelListVisibility:   "labelShow",
		MessageListVisibility: "show",
	}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("gmail api: create label %q: %w", name, describe(err))
	}
	s.log.Info("created Gmail label", "label", name)
	s.labels[name] = l.Id
	return l.Id, nil
}

func (s *Sink) loadLabels(ctx context.Context) error {
	resp, err := s.svc.Users.Labels.List("me").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("gmail api: list labels: %w", describe(err))
	}
	s.labels = make(map[string]string, len(resp.Labels))
	for _, l := range resp.Labels {
		s.labels[l.Name] = l.Id
	}
	return nil
}

// describe turns OAuth failures into actionable messages.
func describe(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
		return errors.New("gmail api: authorization expired or revoked, re-authorize in the web UI (is the OAuth app published \"In production\"?)")
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return fmt.Errorf("gmail api: %d %s: %w", ge.Code, http.StatusText(ge.Code), err)
	}
	return fmt.Errorf("gmail api: %w", err)
}
