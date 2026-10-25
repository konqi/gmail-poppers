package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/sink"
	"gmail-poppers/internal/source"
	"gmail-poppers/internal/state"
	"gmail-poppers/internal/status"
)

type fakeSink struct {
	openErr error
	dry     bool
}

func (f *fakeSink) Open(_ context.Context, dry bool) (sink.Session, error) {
	f.dry = dry
	if f.openErr != nil {
		return nil, f.openErr
	}
	return nopSession{}, nil
}

type nopSession struct{}

func (nopSession) Import(context.Context, []byte, string) error { return nil }
func (nopSession) Close() error                                 { return nil }

type fakeSource struct{ err error }

func (f fakeSource) Fetch(ctx context.Context, p *source.Processor) error {
	if f.err != nil {
		return f.err
	}
	return p.Handle(ctx, "1", []byte("Subject: x\r\n\r\n"))
}

type recReporter struct{ got []status.Entry }

func (r *recReporter) Report(_ context.Context, e status.Entry) { r.got = append(r.got, e) }

func newJob(t *testing.T, src source.Source, sk sink.Sink, dry bool) (*Job, *recReporter) {
	cfg := &config.Source{Account: "me@gmail.com", Name: "t", Interval: time.Minute, Timeout: time.Minute}
	reg := status.NewRegistry()
	reg.Register(cfg)
	st, _ := state.Open(t.TempDir(), "t")
	rep := &recReporter{}
	return &Job{Cfg: cfg, Source: src, Sink: sk, State: st, DryRun: dry, Status: reg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Reporters: []Reporter{rep}}, rep
}

func TestRunOnceSuccess(t *testing.T) {
	j, rep := newJob(t, fakeSource{}, &fakeSink{}, false)
	e := j.RunOnce(context.Background())
	if e.Error != "" || e.LastImported != 1 || e.LastSuccess.IsZero() || len(rep.got) != 1 {
		t.Fatalf("entry = %+v", e)
	}
}

func TestRunOnceErrorsAreReported(t *testing.T) {
	j, rep := newJob(t, fakeSource{}, &fakeSink{openErr: errors.New("gmail api: not authorized")}, false)
	if e := j.RunOnce(context.Background()); e.Error != "gmail api: not authorized" {
		t.Fatalf("entry = %+v", e)
	}
	j, _ = newJob(t, fakeSource{err: errors.New("pop3 login: bad")}, &fakeSink{}, false)
	if e := j.RunOnce(context.Background()); e.Error != "pop3 login: bad" {
		t.Fatalf("entry = %+v", e)
	}
	if len(rep.got) != 1 {
		t.Fatalf("reports = %d", len(rep.got))
	}
}

func TestRunOnceDryRun(t *testing.T) {
	sk := &fakeSink{}
	j, _ := newJob(t, fakeSource{}, sk, true)
	e := j.RunOnce(context.Background())
	if !sk.dry || !strings.HasPrefix(e.Summary, "dry run, would have imported 1") {
		t.Fatalf("dry = %v, summary = %q", sk.dry, e.Summary)
	}
}
