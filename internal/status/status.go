// Package status keeps the latest run result of every source in memory.
package status

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"gmail-poppers/internal/config"
)

type Entry struct {
	Account       string
	Source        string
	Interval      time.Duration
	Timeout       time.Duration
	LastRun       time.Time
	LastSuccess   time.Time
	Duration      time.Duration
	Error         string
	Summary       string
	LastImported  int
	TotalImported int
	TotalSkipped  int
}

// Health evaluates the entry; message is "ok" when healthy.
func (e Entry) Health(now time.Time) (bool, string) {
	switch {
	case e.LastRun.IsZero():
		return false, "no run yet"
	case e.Error != "":
		return false, e.Error
	case now.Sub(e.LastRun) > 3*e.Interval+e.Timeout:
		return false, fmt.Sprintf("stale: last run at %s", e.LastRun.Format(time.RFC3339))
	default:
		return true, "ok"
	}
}

type Registry struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

func NewRegistry() *Registry { return &Registry{entries: map[string]*Entry{}} }

func (r *Registry) Register(s *config.Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[s.ID()] = &Entry{Account: s.Account, Source: s.Name, Interval: s.Interval, Timeout: s.Timeout}
}

func (r *Registry) Update(id string, fn func(*Entry)) Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	fn(e)
	return *e
}

func (r *Registry) Get(account, source string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[account+"/"+source]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

func (r *Registry) All() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Source < out[j].Source
	})
	return out
}
