package status

import (
	"strings"
	"testing"
	"time"

	"gmail-poppers/internal/config"
)

func TestHealth(t *testing.T) {
	now := time.Now()
	base := Entry{Interval: time.Minute, Timeout: time.Minute}
	cases := []struct {
		name string
		mod  func(*Entry)
		ok   bool
		msg  string
	}{
		{"pending", func(e *Entry) {}, false, "no run yet"},
		{"ok", func(e *Entry) { e.LastRun = now }, true, "ok"},
		{"error", func(e *Entry) { e.LastRun = now; e.Error = "pop3 login: nope" }, false, "pop3 login: nope"},
		{"stale", func(e *Entry) { e.LastRun = now.Add(-5 * time.Minute) }, false, "stale"},
	}
	for _, tc := range cases {
		e := base
		tc.mod(&e)
		ok, msg := e.Health(now)
		if ok != tc.ok || !strings.HasPrefix(msg, tc.msg) {
			t.Errorf("%s: got %v %q", tc.name, ok, msg)
		}
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(&config.Source{Account: "b@gmail.com", Name: "x"})
	r.Register(&config.Source{Account: "a@gmail.com", Name: "y"})
	r.Update("a@gmail.com/y", func(e *Entry) { e.TotalImported = 3 })
	if e, ok := r.Get("a@gmail.com", "y"); !ok || e.TotalImported != 3 {
		t.Fatalf("get = %+v %v", e, ok)
	}
	if all := r.All(); len(all) != 2 || all[0].Account != "a@gmail.com" {
		t.Fatalf("all = %+v", all)
	}
}
