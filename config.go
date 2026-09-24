package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// PushoverDest is one Pushover delivery target.
type PushoverDest struct {
	APIToken             string `json:"api_token"`
	UserKey              string `json:"user_key"`
	IncludeSensitiveData bool   `json:"include_sensitive_data"`
}

// Config mirrors the original Python config.json (same keys), plus a few
// optional web-UI / resilience settings.
type Config struct {
	MyJDEmail            string         `json:"myjd_email"`
	MyJDPassword         string         `json:"myjd_password"`
	PushoverDestinations []PushoverDest `json:"pushover_destinations"`
	AkibaBase            string         `json:"akiba_base"`
	AkibaReleasesPath    string         `json:"akiba_releases_path"`
	ForumBase            string         `json:"forum_base"`
	ForumThread          string         `json:"forum_thread"`
	ForumPages           int            `json:"forum_pages"`
	Port                 int            `json:"port"`
	ScheduleInterval     int            `json:"schedule_interval"`
	RSSFile              string         `json:"rss_file"`
	StateFile            string         `json:"state_file"`
	LogFile              string         `json:"log_file"`

	// New in the Go version (all optional).
	ListenHost    string `json:"listen_host"`     // default 0.0.0.0
	WebUser       string `json:"web_user"`        // enables HTTP basic auth on the control UI
	WebPassword   string `json:"web_password"`    //
	PublicBaseURL string `json:"public_base_url"` // used as channel link in RSS
	ForumRetries  int    `json:"forum_retries"`   // attempts per forum request (default 4)
	ReleasesFile  string `json:"releases_file"`   // cache of scraped release metadata
	HistoryFile   string `json:"history_file"`    // pipeline run history
}

// ConfigStore holds the live config and knows how to persist it back while
// preserving unknown keys.
type ConfigStore struct {
	mu   sync.RWMutex
	path string
	dir  string
	cfg  Config
}

func defaultConfig(dir string) Config {
	return Config{
		AkibaBase:         "https://www.akiba-web.com",
		AkibaReleasesPath: "/search/?narrow=2&sort=1",
		ForumBase:         "https://vipergirls.to",
		ForumThread:       "threads/5913172-Heroine-superheroine-JAV-movie-collection",
		ForumPages:        5,
		Port:              5000,
		ScheduleInterval:  3600,
		RSSFile:           "feed.rss",
		StateFile:         ".scraper_state.json",
		LogFile:           "scraper.log",
		ListenHost:        "0.0.0.0",
		ForumRetries:      4,
		ReleasesFile:      "releases.json",
		HistoryFile:       "history.json",
	}
}

func (c *Config) resolve(dir string) {
	for _, p := range []*string{&c.RSSFile, &c.StateFile, &c.LogFile, &c.ReleasesFile, &c.HistoryFile} {
		if *p == "" {
			continue
		}
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	if c.ForumPages < 1 {
		c.ForumPages = 1
	}
	if c.ScheduleInterval < 60 {
		c.ScheduleInterval = 60
	}
	if c.ForumRetries < 1 {
		c.ForumRetries = 1
	}
	if c.ListenHost == "" {
		c.ListenHost = "0.0.0.0"
	}
}

func LoadConfig(path string) (*ConfigStore, error) {
	dir := filepath.Dir(path)
	cs := &ConfigStore{path: path, dir: dir, cfg: defaultConfig(dir)}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cs.cfg.resolve(dir)
			return cs, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &cs.cfg); err != nil {
		// Same behaviour as the Python version: refuse to start on a bad file.
		return nil, fmt.Errorf("config load failed for %s: %w (refusing to start with blank fallback credentials)", path, err)
	}
	cs.cfg.resolve(dir)
	return cs, nil
}

func (cs *ConfigStore) Get() Config {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cfg
}

// RawJSON returns the config file as a generic map (unknown keys preserved).
func (cs *ConfigStore) rawMap() map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(cs.path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// Update applies a partial update (map of json keys) and persists it.
// Secret fields sent as "" or the mask are left unchanged.
func (cs *ConfigStore) Update(patch map[string]any) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	m := cs.rawMap()
	for k, v := range patch {
		if secretKeys[k] {
			if s, ok := v.(string); ok && (s == "" || s == secretMask) {
				continue
			}
		}
		if k == "pushover_destinations" {
			v = mergePushover(m["pushover_destinations"], v)
		}
		m[k] = v
	}
	// validate by round-tripping into Config
	b, _ := json.Marshal(m)
	next := defaultConfig(cs.dir)
	if err := json.Unmarshal(b, &next); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	next.resolve(cs.dir)
	out, _ := json.MarshalIndent(m, "", "  ")
	tmp := cs.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, cs.path); err != nil {
		return err
	}
	cs.cfg = next
	return nil
}

const secretMask = "••••••••"

var secretKeys = map[string]bool{"myjd_password": true, "web_password": true}

// mergePushover keeps existing secrets when the UI sends masked values.
func mergePushover(old, nu any) any {
	oldList, _ := old.([]any)
	newList, ok := nu.([]any)
	if !ok {
		return nu
	}
	out := make([]any, 0, len(newList))
	for i, it := range newList {
		d, _ := it.(map[string]any)
		if d == nil {
			continue
		}
		if i < len(oldList) {
			if od, _ := oldList[i].(map[string]any); od != nil {
				for _, f := range []string{"api_token", "user_key"} {
					if s, _ := d[f].(string); s == "" || s == secretMask {
						d[f] = od[f]
					}
				}
			}
		}
		out = append(out, d)
	}
	return out
}

// Redacted returns the config with secrets masked, for the UI.
func (cs *ConfigStore) Redacted() map[string]any {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	m := cs.rawMap()
	full := defaultConfig(cs.dir)
	fb, _ := json.Marshal(full)
	base := map[string]any{}
	_ = json.Unmarshal(fb, &base)
	for k, v := range base {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	for k := range secretKeys {
		if s, _ := m[k].(string); s != "" {
			m[k] = secretMask
		}
	}
	if l, ok := m["pushover_destinations"].([]any); ok {
		for _, it := range l {
			if d, ok := it.(map[string]any); ok {
				for _, f := range []string{"api_token", "user_key"} {
					if s, _ := d[f].(string); s != "" {
						d[f] = secretMask
					}
				}
			}
		}
	}
	return m
}
