// Package gmailimap writes messages to Gmail via IMAP APPEND using an app password.
package gmailimap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"gmail-poppers/internal/mailconn"
	"gmail-poppers/internal/sink"
	imapsrc "gmail-poppers/internal/source/imap"
)

type Sink struct {
	email       string
	appPassword string
	endpoint    mailconn.Endpoint
	log         *slog.Logger
}

func New(email, appPassword string, log *slog.Logger) *Sink {
	return &Sink{
		email:       email,
		appPassword: appPassword,
		endpoint:    mailconn.Endpoint{Host: "imap.gmail.com", Port: 993, Security: mailconn.Implicit},
		log:         log,
	}
}

func (s *Sink) Open(ctx context.Context) (sink.Session, error) {
	c, err := imapsrc.Dial(ctx, s.endpoint)
	if err != nil {
		return nil, fmt.Errorf("gmail imap connect: %w", err)
	}
	if err := c.Login(s.email, s.appPassword).Wait(); err != nil {
		c.Close()
		return nil, fmt.Errorf("gmail imap login (check the app password): %w", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		c.Close()
		return nil, fmt.Errorf("gmail imap select INBOX: %w", err)
	}
	return &session{c: c, log: s.log}, nil
}

type session struct {
	c       *imapclient.Client
	log     *slog.Logger
	folders map[string]bool
}

func (s *session) Close() error {
	_ = s.c.Logout().Wait()
	return s.c.Close()
}

func (s *session) Import(ctx context.Context, raw []byte, label string) error {
	stop := context.AfterFunc(ctx, func() { s.c.Close() })
	defer stop()

	cmd := s.c.Append("INBOX", int64(len(raw)), &imap.AppendOptions{Time: messageDate(raw)})
	if _, err := cmd.Write(raw); err != nil {
		return fmt.Errorf("gmail imap append: %w", err)
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("gmail imap append: %w", err)
	}
	data, err := cmd.Wait()
	if err != nil {
		err = fmt.Errorf("gmail imap append: %w", err)
		if imapsrc.IsServerNo(err) {
			return sink.Rejected(err)
		}
		return err
	}
	if label == "" {
		return nil
	}
	if data.UID == 0 {
		return errors.New("gmail imap: server did not return APPENDUID, cannot apply label")
	}
	if err := s.ensureFolder(label); err != nil {
		return err
	}
	// Copying into a label folder adds the label in Gmail.
	if _, err := s.c.Copy(imap.UIDSetNum(data.UID), label).Wait(); err != nil {
		return fmt.Errorf("gmail imap apply label %q: %w", label, err)
	}
	return nil
}

func (s *session) ensureFolder(name string) error {
	if s.folders == nil {
		list, err := s.c.List("", "*", nil).Collect()
		if err != nil {
			return fmt.Errorf("gmail imap list: %w", err)
		}
		s.folders = map[string]bool{}
		for _, m := range list {
			s.folders[m.Mailbox] = true
		}
	}
	if s.folders[name] {
		return nil
	}
	if err := s.c.Create(name, nil).Wait(); err != nil {
		var ie *imap.Error
		if !errors.As(err, &ie) || ie.Code != imap.ResponseCodeAlreadyExists {
			return fmt.Errorf("gmail imap create label %q: %w", name, err)
		}
	}
	s.log.Info("created Gmail label", "label", name)
	s.folders[name] = true
	return nil
}

func messageDate(raw []byte) time.Time {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err == nil {
		if d, err := m.Header.Date(); err == nil {
			return d
		}
	}
	return time.Now()
}
