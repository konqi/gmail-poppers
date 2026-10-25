package state

import (
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "me@gmail.com/telekom")
	if err != nil {
		t.Fatal(err)
	}
	s.SetInitialized("")
	s.Mark("a", Imported)
	s.Mark("b", Baseline)
	if n := s.Fail("c"); n != 1 {
		t.Fatalf("fail = %d", n)
	}
	now := time.Now()
	s.AddMessageID("old", now.Add(-400*24*time.Hour))
	s.AddMessageID("new", now)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir, "me@gmail.com/telekom")
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Initialized("") || s2.Status("a") != Imported || s2.Status("b") != Baseline || s2.Fail("c") != 2 {
		t.Fatalf("state not restored: %+v", s2.d)
	}
	if s2.KnowsMessageID("old") || !s2.KnowsMessageID("new") {
		t.Fatalf("message ids = %v", s2.d.MessageIDs)
	}

	s2.Prune("", map[string]bool{"a": true})
	if s2.Status("b") != "" || s2.Status("a") != Imported || s2.d.Failures["c"] != 0 {
		t.Fatalf("prune: %+v", s2.d)
	}
}

func TestPrunePrefix(t *testing.T) {
	s, _ := Open(t.TempDir(), "x")
	s.Mark("INBOX\x001\x005", Imported)
	s.Mark("Other\x001\x005", Imported)
	s.Prune("INBOX\x00", map[string]bool{})
	if s.Status("INBOX\x001\x005") != "" || s.Status("Other\x001\x005") != Imported {
		t.Fatalf("prefix prune: %v", s.d.Messages)
	}
}
