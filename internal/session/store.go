// Package session manages Colab runtime session persistence and keepalive.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Session holds all state for a connected Colab runtime.
type Session struct {
	ID           string    `json:"id"`
	Label        string    `json:"label"`
	Endpoint     string    `json:"endpoint"`
	Variant      string    `json:"variant"`
	Accelerator  string    `json:"accelerator"`
	BaseURL      string    `json:"base_url"`
	Token        string    `json:"token"`
	TokenExpiry  time.Time `json:"token_expiry"`
	AssignedAt   time.Time `json:"assigned_at"`
	KeepalivePID int       `json:"keepalive_pid"`
	BgLogPath    string    `json:"bg_log_path,omitempty"` // log of the most recent `exec --bg` job
}

// Store persists sessions to a JSON file.
type Store struct {
	sessions map[string]*Session
	path     string
	mu       sync.RWMutex
}

// diskFormat is the on-disk JSON representation.
type diskFormat struct {
	Sessions []*Session `json:"sessions"`
}

// NewStore creates a store backed by the given file path.
func NewStore(path string) *Store {
	return &Store{
		sessions: make(map[string]*Session),
		path:     path,
	}
}

// Load reads the JSON file and populates the in-memory map.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	var df diskFormat
	if err := json.Unmarshal(data, &df); err != nil {
		return err
	}

	s.sessions = make(map[string]*Session, len(df.Sessions))
	for _, sess := range df.Sessions {
		s.sessions[sess.Label] = sess
	}
	return nil
}

// Save writes the current sessions to disk with 0600 permissions.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		list = append(list, sess)
	}

	df := diskFormat{Sessions: list}
	data, err := json.MarshalIndent(df, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// Add inserts or replaces a session keyed by its label.
func (s *Store) Add(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.Label] = sess
}

// Remove deletes the session with the given label.
func (s *Store) Remove(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, label)
}

// GetByLabel returns the session for the given label, or nil if not found.
func (s *Store) GetByLabel(label string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[label]
}

// GetDefault returns the most recently assigned session.
func (s *Store) GetDefault() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		list = append(list, sess)
	}
	if len(list) == 0 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].AssignedAt.After(list[j].AssignedAt)
	})
	return list[0]
}

// List returns all sessions in an unspecified order.
func (s *Store) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		list = append(list, sess)
	}
	return list
}
