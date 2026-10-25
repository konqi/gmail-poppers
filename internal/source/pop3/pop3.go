package pop3

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/mailconn"
	"gmail-poppers/internal/source"
	"gmail-poppers/internal/state"
)

type Source struct {
	cfg *config.Source
	log *slog.Logger
}

func New(cfg *config.Source, log *slog.Logger) *Source { return &Source{cfg: cfg, log: log} }

func (s *Source) Fetch(ctx context.Context, p *source.Processor) error {
	cands := mailconn.Candidates(mailconn.POP3, s.cfg.Server, s.cfg.Port, s.cfg.TLS)
	c, ep, err := mailconn.Connect(ctx, cands, func(e mailconn.Endpoint) (*client, error) {
		return s.dial(ctx, e)
	})
	if err != nil {
		return fmt.Errorf("pop3 connect: %w", err)
	}
	stop := closeOnCancel(ctx, c.conn)
	defer stop()
	defer c.conn.Close()
	s.log.Debug("connected", "endpoint", ep.String())

	if err := c.login(s.cfg.User, s.cfg.Password); err != nil {
		return fmt.Errorf("pop3 login: %w", err)
	}
	list, err := c.uidl()
	if err != nil {
		return fmt.Errorf("pop3 uidl: %w", err)
	}

	present := make(map[string]bool, len(list))
	for _, l := range list {
		present[l.uid] = true
	}
	p.State.Prune("", present)

	if !p.State.Initialized("") {
		p.State.SetInitialized("")
		if p.InitialSync == config.InitialSyncNewOnly {
			for _, l := range list {
				p.MarkBaseline(l.uid)
			}
			s.log.Info("first run: existing messages marked as seen", "count", len(list))
			if err := p.State.Save(); err != nil {
				return err
			}
			return quit(c)
		}
	}

	for _, l := range list {
		if !p.Done(l.uid) {
			raw, err := c.retr(l.num)
			if err != nil {
				return fmt.Errorf("pop3 retr: %w", err)
			}
			if err := p.Handle(ctx, l.uid, raw); err != nil {
				return err
			}
		}
		if s.cfg.DeleteAfterImport && deletable(p.State.Status(l.uid)) {
			if err := c.dele(l.num); err != nil {
				return fmt.Errorf("pop3 dele: %w", err)
			}
		}
	}
	if err := p.State.Save(); err != nil {
		return err
	}
	return quit(c)
}

func (s *Source) dial(ctx context.Context, e mailconn.Endpoint) (*client, error) {
	conn, err := mailconn.Dial(ctx, e, 30*time.Second)
	if err != nil {
		return nil, err
	}
	c, err := newClient(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if e.Security == mailconn.StartTLS {
		if err := c.startTLS(mailconn.TLSConfig(e.Host)); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return c, nil
}

// quit commits pending deletions; POP3 only applies DELE on a clean QUIT.
func quit(c *client) error {
	if err := c.quit(); err != nil {
		return fmt.Errorf("pop3 quit: %w", err)
	}
	return nil
}

func deletable(status string) bool {
	return status == state.Imported || status == state.Duplicate
}
