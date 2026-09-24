package main

import (
	"encoding/json"
	"errors"
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
	// Recovered is set when Load had to replace an invalid state file.
	Recovered string
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

// Load reads the state file. A missing file or one that cannot be parsed is
// never fatal: a fresh state is (re)created instead. A corrupt file is moved
// aside (".corrupt-<timestamp>") so nothing is silently lost.
func (s *StateStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.S = newState()
			return s.saveLocked() // create it
		}
		return err
	}
	st, perr := parseState(b, s.path)
	if perr != nil {
		backup := s.path + ".corrupt-" + time.Now().Format("20060102-150405")
		if rerr := os.Rename(s.path, backup); rerr != nil {
			backup = "(could not back up: " + rerr.Error() + ")"
		}
		s.S = newState()
		if serr := s.saveLocked(); serr != nil {
			return serr
		}
		s.Recovered = "state file was invalid (" + perr.Error() + "); started a new one, old file kept as " + backup
		return nil
	}
	s.S = st
	return nil
}

func parseState(b []byte, path string) (State, error) {
	st := newState()
	if len(strings.TrimSpace(string(b))) == 0 {
		return st, errors.New("empty file")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return st, err
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
	if fi, err := os.Stat(path); err == nil {
		legacy = fi.ModTime().UTC().Format(time.RFC3339Nano)
	}
	for k, v := range q {
		if str, ok := v.(string); ok {
			st.QueuedReleases[k] = str
		} else {
			st.QueuedReleases[k] = legacy
		}
	}
	if st.FoundLinks == nil {
		st.FoundLinks = map[string]bool{}
	}
	for link := range st.FoundLinks {
		if m := legacyIDRe.FindStringSubmatch(link); m != nil {
			id := strings.ToUpper(m[1])
			if _, ok := st.QueuedReleases[id]; !ok {
				st.QueuedReleases[id] = legacy
			}
		}
	}
	if st.NotifiedReleases == nil {
		st.NotifiedReleases = map[string]string{}
	}
	if st.PendingNotifications == nil {
		st.PendingNotifications = map[string]bool{}
	}
	if st.PushoverDeliveries == nil {
		st.PushoverDeliveries = map[string]map[string]string{}
	}
	return st, nil
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
