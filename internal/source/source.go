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
	// In dry-run mode the counters describe what would have happened.
	Imported   int
	Baseline   int
	Duplicates int
	Skipped    int
	Deleted    int
	// Rejected holds per-message errors; they do not abort the run.
	Rejected []error
}

// Processor imports single messages and keeps the state in sync.
// With DryRun set it never mutates the state and the sink session is read-only.
type Processor struct {
	State       *state.Store
	Sink        sink.Session
	Label       string
	InitialSync string
	DryRun      bool
	Log         *slog.Logger
	Now         func() time.Time
	Result      Result

	dryHandled map[string]bool
}

// Done reports whether a key needs no further import attempts.
func (p *Processor) Done(key string) bool { return p.State.Status(key) != "" }

// Deletable reports whether a message was imported (or would have been) and may be removed from the server.
func (p *Processor) Deletable(key string) bool {
	st := p.State.Status(key)
	return st == state.Imported || st == state.Duplicate || p.dryHandled[key]
}

// Initialized reports whether the first run for scope already happened.
func (p *Processor) Initialized(scope string) bool { return p.State.Initialized(scope) }

func (p *Processor) SetInitialized(scope string) {
	if !p.DryRun {
		p.State.SetInitialized(scope)
	}
}

// MarkBaseline records an existing message as seen without importing it.
func (p *Processor) MarkBaseline(key, messageIDHash string) {
	p.Result.Baseline++
	if p.DryRun {
		return
	}
	p.State.Mark(key, state.Baseline)
	if messageIDHash != "" {
		p.State.AddMessageID(messageIDHash, p.now())
	}
}

func (p *Processor) Save() error {
	if p.DryRun {
		return nil
	}
	return p.State.Save()
}

func (p *Processor) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Handle imports one message. A returned error is transient and should abort the run.
// In dry-run mode raw may contain only the header.
func (p *Processor) Handle(ctx context.Context, key string, raw []byte) error {
	if p.DryRun {
		return p.dryHandle(ctx, key, raw)
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
		p.State.AddMessageID(hash, p.now())
	}
	p.Result.Imported++
	return p.State.Save()
}

func (p *Processor) dryHandle(ctx context.Context, key string, raw []byte) error {
	if p.dryHandled == nil {
		p.dryHandled = map[string]bool{}
	}
	p.dryHandled[key] = true
	if hash := MessageIDHash(raw); hash != "" && p.State.KnowsMessageID(hash) {
		p.Result.Duplicates++
		p.Log.Info("dry run: would skip duplicate", "key", printableKey(key))
		return nil
	}
	if err := p.Sink.Import(ctx, raw, p.Label); err != nil {
		return err
	}
	p.Result.Imported++
	attrs := []any{"key", printableKey(key), "label", p.Label}
	if m, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil {
		attrs = append(attrs, "from", m.Header.Get("From"), "subject", m.Header.Get("Subject"), "date", m.Header.Get("Date"))
	}
	p.Log.Info("dry run: would import", attrs...)
	return nil
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
