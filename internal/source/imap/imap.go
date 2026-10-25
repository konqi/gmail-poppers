// Package imap implements the IMAP source.
package imap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

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

// Dial connects to an IMAP endpoint, upgrading via STARTTLS when requested.
func Dial(ctx context.Context, e mailconn.Endpoint) (*imapclient.Client, error) {
	conn, err := mailconn.Dial(ctx, e, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if e.Security == mailconn.StartTLS {
		return imapclient.NewStartTLS(conn, &imapclient.Options{TLSConfig: mailconn.TLSConfig(e.Host)})
	}
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		c.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	return c, nil
}

func (s *Source) Fetch(ctx context.Context, p *source.Processor) error {
	cands := mailconn.Candidates(mailconn.IMAP, s.cfg.Server, s.cfg.Port, s.cfg.TLS)
	c, ep, err := mailconn.Connect(ctx, cands, func(e mailconn.Endpoint) (*imapclient.Client, error) {
		return Dial(ctx, e)
	})
	if err != nil {
		return fmt.Errorf("imap connect: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	defer c.Close()
	s.log.Debug("connected", "endpoint", ep.String())

	if err := c.Login(s.cfg.User, s.cfg.Password).Wait(); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}
	for _, folder := range s.cfg.Folders {
		if err := s.fetchFolder(ctx, c, folder, p); err != nil {
			return fmt.Errorf("imap folder %q: %w", folder, err)
		}
	}
	_ = c.Logout().Wait()
	return nil
}

func (s *Source) fetchFolder(ctx context.Context, c *imapclient.Client, folder string, p *source.Processor) error {
	sel, err := c.Select(folder, nil).Wait()
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}
	search, err := c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	uids := search.AllUIDs()

	prefix := folder + "\x00"
	key := func(uid imap.UID) string {
		return prefix + strconv.FormatUint(uint64(sel.UIDValidity), 10) + "\x00" + strconv.FormatUint(uint64(uid), 10)
	}
	present := make(map[string]bool, len(uids))
	for _, uid := range uids {
		present[key(uid)] = true
	}
	// A changed UIDVALIDITY drops all old keys; Message-ID dedupe prevents re-imports.
	p.State.Prune(prefix, present)

	if !p.State.Initialized(folder) {
		p.State.SetInitialized(folder)
		if p.InitialSync == config.InitialSyncNewOnly {
			if err := s.baseline(c, uids, key, p); err != nil {
				return err
			}
			s.log.Info("first run: existing messages marked as seen", "folder", folder, "count", len(uids))
			return p.State.Save()
		}
	}

	section := &imap.FetchItemBodySection{Peek: true}
	var toDelete []imap.UID
	for _, uid := range uids {
		k := key(uid)
		if !p.Done(k) {
			msgs, err := c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
				UID:         true,
				BodySection: []*imap.FetchItemBodySection{section},
			}).Collect()
			if err != nil {
				return fmt.Errorf("fetch: %w", err)
			}
			if len(msgs) == 0 {
				continue
			}
			raw := msgs[0].FindBodySection(section)
			if raw == nil {
				return fmt.Errorf("fetch: empty body for uid %d", uid)
			}
			if err := p.Handle(ctx, k, raw); err != nil {
				return err
			}
		}
		if s.cfg.DeleteAfterImport {
			if st := p.State.Status(k); st == state.Imported || st == state.Duplicate {
				toDelete = append(toDelete, uid)
			}
		}
	}
	if len(toDelete) > 0 {
		if err := s.delete(c, toDelete); err != nil {
			return err
		}
	}
	return p.State.Save()
}

func (s *Source) baseline(c *imapclient.Client, uids []imap.UID, key func(imap.UID) string, p *source.Processor) error {
	if len(uids) == 0 {
		return nil
	}
	msgs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if err != nil {
		return fmt.Errorf("fetch envelopes: %w", err)
	}
	now := time.Now()
	for _, m := range msgs {
		p.MarkBaseline(key(m.UID))
		if m.Envelope != nil {
			if h := source.HashMessageID(m.Envelope.MessageID); h != "" {
				p.State.AddMessageID(h, now)
			}
		}
	}
	return nil
}

func (s *Source) delete(c *imapclient.Client, uids []imap.UID) error {
	set := imap.UIDSetNum(uids...)
	err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close()
	if err != nil {
		return fmt.Errorf("store \\Deleted: %w", err)
	}
	if c.Caps().Has(imap.CapUIDPlus) {
		err = c.UIDExpunge(set).Close()
	} else {
		err = c.Expunge().Close()
	}
	if err != nil {
		return fmt.Errorf("expunge: %w", err)
	}
	return nil
}

// IsServerNo reports whether err is an IMAP NO/BAD response for a single command.
func IsServerNo(err error) bool {
	var ie *imap.Error
	return errors.As(err, &ie) && (ie.Type == imap.StatusResponseTypeNo || ie.Type == imap.StatusResponseTypeBad)
}
