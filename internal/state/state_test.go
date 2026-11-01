package state

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenHashesSourceIDWithoutCollisions(t *testing.T) {
	dir := t.TempDir()
	ids := []string{"me+tag@gmail.com/source", "me_tag@gmail.com/source"}
	for _, id := range ids {
		store, err := Open(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(id))))
		if store.path != want {
			t.Fatalf("path = %q, want %q", store.path, want)
		}
		store.Mark(id, Imported)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
	}
	for index, id := range ids {
		store, err := Open(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		if store.Status(id) != Imported || store.Status(ids[1-index]) != "" {
			t.Fatalf("source %q shares progress: %+v", id, store.d.Messages)
		}
	}
}

func TestBindTargetPersistsOnlyOnSave(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindTarget("first@gmail.com"); err != nil {
		t.Fatal(err)
	}
	if store.d.Target != "first@gmail.com" {
		t.Fatalf("in-memory target = %q", store.d.Target)
	}
	if _, err := os.Stat(store.path); !os.IsNotExist(err) {
		t.Fatalf("binding wrote state: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(dir, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.BindTarget("first@gmail.com"); err != nil {
		t.Fatalf("same persisted target rejected: %v", err)
	}
	if err := restored.BindTarget("second@gmail.com"); err == nil {
		t.Fatal("different persisted target accepted")
	}
	if restored.d.Target != "first@gmail.com" {
		t.Fatalf("rejected binding changed target to %q", restored.d.Target)
	}
	if err := restored.Save(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, "source")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.d.Target != "first@gmail.com" {
		t.Fatalf("persisted target = %q", reopened.d.Target)
	}
}

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
