package source

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"gmail-poppers/internal/sink"
	"gmail-poppers/internal/state"
)

type fakeSession struct {
	imported [][]byte
	err      error
}

func (f *fakeSession) Import(_ context.Context, raw []byte, _ string) error {
	if f.err != nil {
		return f.err
	}
	f.imported = append(f.imported, raw)
	return nil
}

func (f *fakeSession) Close() error { return nil }

func newProcessor(t *testing.T, s sink.Session) *Processor {
	st, err := state.Open(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	return &Processor{State: st, Sink: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

const msg = "Message-ID: <abc@example.com>\r\nSubject: hi\r\n\r\nbody\r\n"

func TestHandleImportsAndDedupes(t *testing.T) {
	fs := &fakeSession{}
	p := newProcessor(t, fs)
	ctx := context.Background()
	if err := p.Handle(ctx, "1", []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := p.Handle(ctx, "2", []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if len(fs.imported) != 1 || p.Result.Imported != 1 || p.Result.Duplicates != 1 {
		t.Fatalf("result = %+v, imported = %d", p.Result, len(fs.imported))
	}
	if !p.Done("1") || !p.Done("2") || p.State.Status("2") != state.Duplicate {
		t.Fatal("state not updated")
	}
}

func TestHandleRejectedIsSkippedAfterMaxAttempts(t *testing.T) {
	fs := &fakeSession{err: sink.Rejected(errors.New("too large"))}
	p := newProcessor(t, fs)
	for i := 0; i < state.MaxAttempts; i++ {
		if err := p.Handle(context.Background(), "1", []byte(msg)); err != nil {
			t.Fatalf("rejected error must not abort: %v", err)
		}
	}
	if p.State.Status("1") != state.Skipped || p.Result.Skipped != 1 || len(p.Result.Rejected) != state.MaxAttempts {
		t.Fatalf("result = %+v", p.Result)
	}
	if p.Result.RejectedError() == nil {
		t.Fatal("expected rejected error summary")
	}
}

func TestHandleTransientAborts(t *testing.T) {
	p := newProcessor(t, &fakeSession{err: errors.New("connection reset")})
	if err := p.Handle(context.Background(), "1", []byte(msg)); err == nil {
		t.Fatal("expected transient error")
	}
	if p.Done("1") {
		t.Fatal("transient failure must not mark message")
	}
}
