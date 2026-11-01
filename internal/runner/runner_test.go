package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
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

type admissionSource func(context.Context, *source.Processor) error

func (fetch admissionSource) Fetch(ctx context.Context, processor *source.Processor) error {
	return fetch(ctx, processor)
}

type channelReporter chan status.Entry

func (reports channelReporter) Report(ctx context.Context, entry status.Entry) {
	select {
	case reports <- entry:
	case <-ctx.Done():
	}
}

func TestAdmissionCapsFetchesAndQueuesBeforeTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	admission := NewAdmission(2)
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	var active, peak atomic.Int32
	fetch := admissionSource(func(ctx context.Context, _ *source.Processor) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); count > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, count) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	jobs := make([]*Job, 3)
	for index := range jobs {
		jobs[index], _ = newJob(t, fetch, &fakeSink{}, false)
		jobs[index].Admission = admission
		jobs[index].Reporters = nil
	}
	queued := jobs[2]
	queued.Cfg.Timeout = 10 * time.Millisecond
	queued.Cfg.Interval = 20 * time.Millisecond
	queued.Status.Update(queued.Cfg.ID(), func(entry *status.Entry) {
		entry.Interval = queued.Cfg.Interval
		entry.Timeout = queued.Cfg.Timeout
		entry.LastRun = time.Now().Add(-time.Hour)
		entry.LastSuccess = entry.LastRun
	})
	reports := make(channelReporter, 16)
	queued.Reporters = []Reporter{reports}
	done := make(chan status.Entry, 3)
	for _, job := range jobs[:2] {
		go func() { done <- job.RunOnce(ctx) }()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("admitted fetch did not start")
		}
	}
	go func() { done <- queued.RunOnce(ctx) }()
	select {
	case entry := <-reports:
		if entry.Phase != "queued" {
			t.Fatalf("heartbeat phase = %q", entry.Phase)
		}
		if healthy, message := entry.Health(time.Now()); !healthy {
			t.Fatalf("queued successful source is unhealthy: %s", message)
		}
	case <-ctx.Done():
		t.Fatal("queued job did not emit a heartbeat")
	}
	select {
	case <-started:
		t.Fatal("third fetch exceeded admission capacity")
	default:
	}
	if entry, _ := jobs[0].Status.Get(jobs[0].Cfg.Account, jobs[0].Cfg.Name); entry.Phase != "running" || entry.StartedAt.IsZero() {
		t.Fatalf("active entry = %+v", entry)
	}
	close(release)
	for range jobs {
		select {
		case entry := <-done:
			if entry.Phase != "idle" || entry.Error != "" || entry.StartedAt.IsZero() || !entry.LastRun.Equal(entry.StartedAt) {
				t.Errorf("finished entry = %+v", entry)
			}
		case <-ctx.Done():
			t.Fatal("admitted jobs did not finish")
		}
	}
	entry, _ := queued.Status.Get(queued.Cfg.Account, queued.Cfg.Name)
	if entry.QueueDuration < queued.Cfg.Timeout {
		t.Fatalf("queue duration = %s, want at least %s", entry.QueueDuration, queued.Cfg.Timeout)
	}
	if peak.Load() != 2 || active.Load() != 0 || len(admission.slots) != 0 {
		t.Fatalf("peak = %d, active = %d, occupied slots = %d", peak.Load(), active.Load(), len(admission.slots))
	}
}

func TestAdmissionCancellationReleasesCapacity(t *testing.T) {
	for _, phase := range []string{"queued", "running"} {
		t.Run(phase, func(t *testing.T) {
			parent, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			admission := NewAdmission(1)
			if phase == "queued" {
				admission.slots <- struct{}{}
			}
			entered := make(chan struct{}, 1)
			job, _ := newJob(t, admissionSource(func(ctx context.Context, _ *source.Processor) error {
				entered <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}), &fakeSink{}, false)
			job.Admission = admission
			job.Cfg.Interval = time.Millisecond
			reports := make(channelReporter, 16)
			job.Reporters = []Reporter{reports}
			done := make(chan status.Entry, 1)
			go func() { done <- job.RunOnce(ctx) }()
			if phase == "queued" {
				select {
				case entry := <-reports:
					if entry.Phase != "queued" {
						t.Fatalf("heartbeat phase = %q", entry.Phase)
					}
				case <-parent.Done():
					t.Fatal("job did not queue")
				}
			} else {
				select {
				case <-entered:
				case <-parent.Done():
					t.Fatal("job did not start")
				}
			}
			cancel()
			select {
			case <-done:
			case <-parent.Done():
				t.Fatal("canceled job did not return")
			}
			if phase == "queued" {
				select {
				case <-entered:
					t.Error("canceled queued job fetched messages")
				default:
				}
				if len(admission.slots) != 1 {
					t.Fatal("queued cancellation released another job's slot")
				}
				<-admission.slots
			}
			if len(admission.slots) != 0 {
				t.Fatal("running cancellation leaked admission capacity")
			}
			successor, _ := newJob(t, fakeSource{}, &fakeSink{}, false)
			successor.Admission = admission
			if entry := successor.RunOnce(parent); entry.Error != "" || entry.LastImported != 1 {
				t.Fatalf("successor could not use released capacity: %+v", entry)
			}
			entry, _ := job.Status.Get(job.Cfg.Account, job.Cfg.Name)
			if entry.Phase != "idle" {
				t.Errorf("canceled %s job remained in phase %q", phase, entry.Phase)
			}
		})
	}
}
