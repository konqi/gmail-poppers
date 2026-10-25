// Package sink defines how messages are written into Gmail.
package sink

import (
	"context"
	"errors"
)

type Sink interface {
	// Open prepares a session for one polling run; it also validates credentials.
	// In dry-run mode the session only performs read operations.
	Open(ctx context.Context, dryRun bool) (Session, error)
}

type Session interface {
	// Import inserts a raw RFC 5322 message into the inbox as unread.
	// In dry-run mode it only checks the label and writes nothing.
	Import(ctx context.Context, raw []byte, label string) error
	Close() error
}

// ErrRejected marks errors where Gmail refused this particular message.
// Other errors are treated as transient and abort the run.
var ErrRejected = errors.New("message rejected")

type rejected struct{ err error }

func (r rejected) Error() string   { return r.err.Error() }
func (r rejected) Unwrap() []error { return []error{ErrRejected, r.err} }
func Rejected(err error) error     { return rejected{err} }
func IsRejected(err error) bool    { return errors.Is(err, ErrRejected) }
