package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// State is JSON-compatible with the Python .scraper_state.json.
type State struct {
	FoundLinks           map[string]bool              `json:"found_links"`
	QueuedReleases       map[string]string            `json:"queued_releases"`
	NotifiedReleases     map[string]string            `json:"notified_releases"`
	PendingNotifications map[string]bool              `json:"pending_notifications"`
	PushoverDeliveries   map[string]map[string]string `json:"pushover_deliveries"`
	RoundRobinIndex      int                          `json:"round_robin_index"`
	// New: last successfully discovered forum thread length (bug fix).
	ForumMaxPage int `json:"forum_max_page,omitempty"`
}

type StateStore struct {
	mu   sync.Mutex
	path string
	S    State
}

func newState() State {
	return State{
		FoundLinks:           map[string]bool{},
		QueuedReleases:       map[string]string{},
		NotifiedReleases:     map[string]string{},
		PendingNotifications: map[string]bool{},
		PushoverDeliveries:   map[string]map[string]string{},
	}
}

func NewStateStore(path string) *StateStore { return &StateStore{path: path, S: newState()} }

var legacyIDRe = regexp.MustCompile(`(?i)(?:^|[/_])([A-Z]{2,8}-\d{1,4})(?:_|\.|$)`)

func (s *StateStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	st := newState()
	// Tolerant decode: queued_releases values may be legacy non-strings.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	_ = json.Unmarshal(raw["found_links"], &st.FoundLinks)
	_ = json.Unmarshal(raw["notified_releases"], &st.NotifiedReleases)
	_ = json.Unmarshal(raw["pending_notifications"], &st.PendingNotifications)
	_ = json.Unmarshal(raw["pushover_deliveries"], &st.PushoverDeliveries)
	_ = json.Unmarshal(raw["round_robin_index"], &st.RoundRobinIndex)
	_ = json.Unmarshal(raw["forum_max_page"], &st.ForumMaxPage)
	var q map[string]any
	_ = json.Unmarshal(raw["queued_releases"], &q)
	legacy := time.Now().UTC().Format(time.RFC3339Nano)
	if fi, err := os.Stat(s.path); err == nil {
		legacy = fi.ModTime().UTC().Format(time.RFC3339Nano)
	}
	for k, v := range q {
		if str, ok := v.(string); ok {
			st.QueuedReleases[k] = str
		} else {
			st.QueuedReleases[k] = legacy
		}
	}
	for link := range st.FoundLinks {
		if m := legacyIDRe.FindStringSubmatch(link); m != nil {
			id := strings.ToUpper(m[1])
			if _, ok := st.QueuedReleases[id]; !ok {
				st.QueuedReleases[id] = legacy
			}
		}
	}
	if st.FoundLinks == nil {
		st.FoundLinks = map[string]bool{}
	}
	if st.PushoverDeliveries == nil {
		st.PushoverDeliveries = map[string]map[string]string{}
	}
	s.S = st
	return nil
}

func (s *StateStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.S, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	_ = f.Sync()
	f.Close()
	return os.Rename(tmp, s.path)
}

func (s *StateStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// With runs fn under the state lock and persists afterwards.
func (s *StateStore) With(fn func(st *State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.S)
	return s.saveLocked()
}

// Read runs fn under the state lock without saving.
func (s *StateStore) Read(fn func(st *State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.S)
}
