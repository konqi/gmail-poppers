// Package runner polls one source periodically and records the outcome.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/sink"
	"gmail-poppers/internal/source"
	"gmail-poppers/internal/state"
	"gmail-poppers/internal/status"
)

// Reporter is notified after every run, e.g. to push the result to Gatus.
type Reporter interface {
	Report(ctx context.Context, e status.Entry)
}

type Job struct {
	Cfg       *config.Source
	Source    source.Source
	Sink      sink.Sink
	State     *state.Store
	DryRun    bool
	Log       *slog.Logger
	Status    *status.Registry
	Reporters []Reporter
	Admission *Admission
}

type Admission struct{ slots chan struct{} }

func NewAdmission(limit int) *Admission { return &Admission{slots: make(chan struct{}, limit)} }

// Run blocks until ctx is cancelled.
func (j *Job) Run(ctx context.Context) {
	delay := time.Duration(rand.Int64N(int64(10 * time.Second)))
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		j.RunOnce(ctx)
		delay = j.Cfg.Interval
	}
}

func (j *Job) RunOnce(ctx context.Context) status.Entry {
	defer j.Status.Update(j.Cfg.ID(), func(entry *status.Entry) { entry.Phase = "idle" })
	queued := time.Now()
	if j.Admission != nil {
		j.Status.Update(j.Cfg.ID(), func(entry *status.Entry) { entry.Phase = "queued" })
		ticker := time.NewTicker(j.Cfg.Interval)
		defer ticker.Stop()
	wait:
		for {
			select {
			case <-ctx.Done():
				return status.Entry{}
			case j.Admission.slots <- struct{}{}:
				defer func() { <-j.Admission.slots }()
				break wait
			case <-ticker.C:
				entry, _ := j.Status.Get(j.Cfg.Account, j.Cfg.Name)
				for _, reporter := range j.Reporters {
					reporter.Report(ctx, entry)
				}
			}
		}
	}
	start := time.Now()
	j.Status.Update(j.Cfg.ID(), func(entry *status.Entry) { entry.Phase = "running"; entry.StartedAt = start })
	res, err := j.fetch(ctx)
	if ctx.Err() != nil {
		return status.Entry{}
	}
	d := time.Since(start)

	e := j.Status.Update(j.Cfg.ID(), func(e *status.Entry) {
		e.LastRun = start
		e.Phase = "idle"
		e.QueueDuration = start.Sub(queued)
		e.Duration = d
		e.LastImported = res.Imported
		e.TotalImported += res.Imported
		e.TotalSkipped += res.Skipped
		e.Summary = summary(res, j.DryRun)
		if err != nil {
			e.Error = err.Error()
		} else {
			e.Error = ""
			e.LastSuccess = start
		}
	})

	attrs := []any{"imported", res.Imported, "duplicates", res.Duplicates, "baseline", res.Baseline, "deleted", res.Deleted, "duration", d.Round(time.Millisecond)}
	if j.DryRun {
		attrs = append(attrs, "dry_run", true)
	}
	switch {
	case err != nil:
		j.Log.Error("run failed", append(attrs, "error", err)...)
	case res.Imported > 0 || res.Baseline > 0:
		j.Log.Info("run finished", attrs...)
	default:
		j.Log.Debug("run finished", attrs...)
	}

	for _, r := range j.Reporters {
		r.Report(ctx, e)
	}
	return e
}

func (j *Job) fetch(ctx context.Context) (source.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, j.Cfg.Timeout)
	defer cancel()

	sess, err := j.Sink.Open(ctx, j.DryRun)
	if err != nil {
		return source.Result{}, err
	}
	defer sess.Close()

	p := &source.Processor{
		State:       j.State,
		Sink:        sess,
		Label:       j.Cfg.LabelName(),
		InitialSync: j.Cfg.InitialSync,
		DryRun:      j.DryRun,
		Log:         j.Log,
	}
	if err := j.Source.Fetch(ctx, p); err != nil {
		return p.Result, err
	}
	return p.Result, p.Result.RejectedError()
}

func summary(r source.Result, dryRun bool) string {
	s := fmt.Sprintf("imported %d, duplicates %d, marked as seen %d, deleted %d, skipped %d",
		r.Imported, r.Duplicates, r.Baseline, r.Deleted, r.Skipped)
	if dryRun {
		return "dry run, would have " + s
	}
	return s
}
