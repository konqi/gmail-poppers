// Package source contains the logic shared by all mail sources.
package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"gmail-poppers/internal/sink"
	"gmail-poppers/internal/state"
)

type Source interface {
	Fetch(ctx context.Context, p *Processor) error
}

type Result struct {
	Imported   int
	Baseline   int
	Duplicates int
	Skipped    int
	// Rejected holds per-message errors; they do not abort the run.
	Rejected []error
}

// Processor imports single messages and keeps the state in sync.
type Processor struct {
	State       *state.Store
	Sink        sink.Session
	Label       string
	InitialSync string
	Log         *slog.Logger
	Now         func() time.Time
	Result      Result
}

// Done reports whether a key needs no further import attempts.
func (p *Processor) Done(key string) bool { return p.State.Status(key) != "" }

// MarkBaseline records an existing message as seen without importing it.
func (p *Processor) MarkBaseline(key string) {
	p.State.Mark(key, state.Baseline)
	p.Result.Baseline++
}

// Handle imports one message. A returned error is transient and should abort the run.
func (p *Processor) Handle(ctx context.Context, key string, raw []byte) error {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	hash := MessageIDHash(raw)
	if hash != "" && p.State.KnowsMessageID(hash) {
		p.State.Mark(key, state.Duplicate)
		p.Result.Duplicates++
		p.Log.Info("skipping duplicate message", "key", printableKey(key))
		return p.State.Save()
	}

	if err := p.Sink.Import(ctx, raw, p.Label); err != nil {
		if !sink.IsRejected(err) {
			return err
		}
		n := p.State.Fail(key)
		if n >= state.MaxAttempts {
			p.State.Mark(key, state.Skipped)
			p.Result.Skipped++
			err = fmt.Errorf("message %s skipped after %d attempts: %w", printableKey(key), n, err)
		} else {
			err = fmt.Errorf("message %s (attempt %d/%d): %w", printableKey(key), n, state.MaxAttempts, err)
		}
		p.Result.Rejected = append(p.Result.Rejected, err)
		p.Log.Warn("message rejected", "error", err)
		return p.State.Save()
	}

	p.State.Mark(key, state.Imported)
	if hash != "" {
		p.State.AddMessageID(hash, now())
	}
	p.Result.Imported++
	return p.State.Save()
}

// RejectedError summarizes per-message failures of a run, or nil.
func (r Result) RejectedError() error {
	if len(r.Rejected) == 0 {
		return nil
	}
	return fmt.Errorf("%d message(s) rejected: %w", len(r.Rejected), errors.Join(r.Rejected...))
}

// MessageIDHash returns a hash of the Message-ID header, or "" if absent.
func MessageIDHash(raw []byte) string {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return HashMessageID(m.Header.Get("Message-Id"))
}

// HashMessageID normalizes a Message-ID with or without angle brackets and hashes it.
func HashMessageID(id string) string {
	id = strings.ToLower(strings.Trim(strings.TrimSpace(id), "<>"))
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func printableKey(key string) string { return strings.ReplaceAll(key, "\x00", "/") }
