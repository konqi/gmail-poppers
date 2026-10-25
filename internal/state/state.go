// Package state persists per-source import progress as JSON files.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	Imported  = "imported"
	Baseline  = "baseline"
	Skipped   = "skipped"
	Duplicate = "duplicate"

	// MaxAttempts is how often a message may be rejected permanently before it is skipped.
	MaxAttempts = 3

	messageIDRetention = 365 * 24 * time.Hour
)

type data struct {
	Initialized map[string]bool   `json:"initialized"`
	Messages    map[string]string `json:"messages"`
	Failures    map[string]int    `json:"failures,omitempty"`
	MessageIDs  map[string]int64  `json:"message_ids"`
}

// Store is not safe for concurrent use; each source owns exactly one.
type Store struct {
	path string
	d    data
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9@._-]+`)

func Open(dir, id string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	name := unsafeChars.ReplaceAllString(strings.ReplaceAll(id, "/", "__"), "_") + ".json"
	s := &Store{path: filepath.Join(dir, name)}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("state: %w", err)
	default:
		if err := json.Unmarshal(b, &s.d); err != nil {
			return nil, fmt.Errorf("state: %s: %w", s.path, err)
		}
	}
	if s.d.Initialized == nil {
		s.d.Initialized = map[string]bool{}
	}
	if s.d.Messages == nil {
		s.d.Messages = map[string]string{}
	}
	if s.d.Failures == nil {
		s.d.Failures = map[string]int{}
	}
	if s.d.MessageIDs == nil {
		s.d.MessageIDs = map[string]int64{}
	}
	return s, nil
}

// Save writes the state atomically.
func (s *Store) Save() error {
	b, err := json.Marshal(&s.d)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	return nil
}

func (s *Store) Initialized(scope string) bool { return s.d.Initialized[scope] }

func (s *Store) SetInitialized(scope string) { s.d.Initialized[scope] = true }

// Status returns the recorded status of a message key, or "" if unknown.
func (s *Store) Status(key string) string { return s.d.Messages[key] }

func (s *Store) Mark(key, status string) {
	s.d.Messages[key] = status
	delete(s.d.Failures, key)
}

// Fail records a permanent rejection and returns the number of attempts so far.
func (s *Store) Fail(key string) int {
	s.d.Failures[key]++
	return s.d.Failures[key]
}

// Prune forgets keys with the given prefix that are no longer on the server.
func (s *Store) Prune(prefix string, present map[string]bool) {
	for k := range s.d.Messages {
		if strings.HasPrefix(k, prefix) && !present[k] {
			delete(s.d.Messages, k)
		}
	}
	for k := range s.d.Failures {
		if strings.HasPrefix(k, prefix) && !present[k] {
			delete(s.d.Failures, k)
		}
	}
}

func (s *Store) KnowsMessageID(hash string) bool {
	_, ok := s.d.MessageIDs[hash]
	return ok
}

func (s *Store) AddMessageID(hash string, now time.Time) {
	s.d.MessageIDs[hash] = now.Unix()
	cutoff := now.Add(-messageIDRetention).Unix()
	for k, t := range s.d.MessageIDs {
		if t < cutoff {
			delete(s.d.MessageIDs, k)
		}
	}
}
