package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	PublicBaseURL string `json:"public_base_url"` // used as channel link in RSS
	ForumRetries  int    `json:"forum_retries"`   // attempts per forum request (default 4)
	ReleasesFile  string `json:"releases_file"`   // cache of scraped release metadata
	HistoryFile   string `json:"history_file"`    // pipeline run history
}

// ConfigStore holds the live settings (settings.json, edited from the web UI)
// and persists them while preserving unknown keys.
type ConfigStore struct {
	mu     sync.RWMutex
	path   string
	dir    string
	cfg    Config
	exists bool
}

const settingsName = "settings.json"
const legacyConfigName = "config.json"

// Exists reports whether settings.json has been created (setup completed or
// a legacy config.json was imported).
func (cs *ConfigStore) Exists() bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.exists
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

// LoadConfig loads <dir>/settings.json. A missing file is not an error: the
// defaults are used and Exists() is false until settings are first saved.
func LoadConfig(dir string) (*ConfigStore, error) {
	path := filepath.Join(dir, settingsName)
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
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	cs.exists = true
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
	_ = os.MkdirAll(cs.dir, 0o755)
	tmp := cs.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, cs.path); err != nil {
		return err
	}
	cs.cfg = next
	cs.exists = true
	return nil
}

const secretMask = "••••••••"

var secretKeys = map[string]bool{"myjd_password": true}

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

// ImportLegacyConfig migrates an old <dir>/config.json (Python or earlier Go
// version) into settings.json + auth.json and renames it to
// config.json.imported. It does nothing if settings.json already exists or
// there is no config.json. An invalid config.json is left untouched and
// reported as an error.
func ImportLegacyConfig(dir string, log *Hub) (bool, error) {
	settings := filepath.Join(dir, settingsName)
	legacy := filepath.Join(dir, legacyConfigName)
	if _, err := os.Stat(settings); err == nil {
		return false, nil
	}
	b, err := os.ReadFile(legacy)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return false, fmt.Errorf("found %s but it is not valid JSON (%v); leaving it alone — fix it or delete it and use the setup page", legacy, err)
	}
	user, _ := m["web_user"].(string)
	pass, _ := m["web_password"].(string)
	delete(m, "web_user")
	delete(m, "web_password")
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(settings, out, 0o600); err != nil {
		return false, err
	}
	if user != "" && pass != "" {
		auth := NewAuthStore(filepath.Join(dir, "auth.json"))
		_ = auth.Load()
		if !auth.HasUser() {
			if err := auth.SetUser(user, pass); err != nil {
				log.Warn("Could not import web login from config.json (%v) — you will be asked to create an account", err)
			} else {
				log.Info("Imported web login %q from config.json", user)
			}
		}
	}
	target := legacy + ".imported"
	if _, err := os.Stat(target); err == nil {
		target = legacy + ".imported-" + time.Now().Format("20060102-150405")
	}
	if err := os.Rename(legacy, target); err != nil {
		return true, fmt.Errorf("imported, but could not rename %s: %w", legacy, err)
	}
	log.Info("Imported %s into %s and renamed it to %s", legacyConfigName, settingsName, filepath.Base(target))
	return true, nil
}
