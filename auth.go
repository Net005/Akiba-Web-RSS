package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// User is the (single) administrator account. Passwords are bcrypt hashes.
type User struct {
	Username string `json:"username"`
	Hash     string `json:"hash"`
	Created  string `json:"created"`
}

type sessionRec struct {
	User     string    `json:"user"`
	Expires  time.Time `json:"expires"`
	Remember bool      `json:"remember"`
	Touched  time.Time `json:"touched"`
}

type authFile struct {
	Users    []User                `json:"users"`
	Sessions map[string]sessionRec `json:"sessions"` // key: sha256(token)
}

// AuthStore keeps the admin account and login sessions in auth.json.
// Session tokens are only stored hashed.
type AuthStore struct {
	mu       sync.Mutex
	path     string
	users    []User
	sessions map[string]sessionRec
}

const (
	sessionRememberTTL = 30 * 24 * time.Hour
	sessionShortTTL    = 12 * time.Hour
	minPasswordLen     = 8
)

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equalizer"), bcrypt.DefaultCost)

func NewAuthStore(path string) *AuthStore {
	return &AuthStore{path: path, sessions: map[string]sessionRec{}}
}

func (a *AuthStore) Load() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f authFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	a.users = f.Users
	a.sessions = f.Sessions
	if a.sessions == nil {
		a.sessions = map[string]sessionRec{}
	}
	return nil
}

func (a *AuthStore) saveLocked() error {
	_ = os.MkdirAll(filepath.Dir(a.path), 0o755)
	b, err := json.MarshalIndent(authFile{Users: a.users, Sessions: a.sessions}, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

func (a *AuthStore) HasUser() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.users) > 0
}

func (a *AuthStore) Username() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.users) == 0 {
		return ""
	}
	return a.users[0].Username
}

func validateCredentials(username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || utf8.RuneCountInString(username) > 64 {
		return errors.New("username must be 1–64 characters")
	}
	return validatePassword(password)
}

func validatePassword(password string) error {
	if len(password) < minPasswordLen {
		return errors.New("password must be at least 8 characters")
	}
	if len(password) > 72 {
		return errors.New("password must be at most 72 bytes")
	}
	return nil
}

// SetUser creates (or replaces) the administrator account and signs out
// every existing session.
func (a *AuthStore) SetUser(username, password string) error {
	if err := validateCredentials(username, password); err != nil {
		return err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.users = []User{{Username: strings.TrimSpace(username), Hash: string(h), Created: time.Now().UTC().Format(time.RFC3339)}}
	a.sessions = map[string]sessionRec{}
	return a.saveLocked()
}

// Verify checks credentials in (roughly) constant time whether or not the
// user exists.
func (a *AuthStore) Verify(username, password string) (string, bool) {
	a.mu.Lock()
	var u *User
	for i := range a.users {
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(a.users[i].Username)), []byte(strings.ToLower(strings.TrimSpace(username)))) == 1 {
			u = &a.users[i]
		}
	}
	hash := dummyHash
	name := ""
	if u != nil {
		hash = []byte(u.Hash)
		name = u.Username
	}
	a.mu.Unlock()
	ok := bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	return name, ok && u != nil
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (a *AuthStore) NewSession(user string, remember bool) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	ttl := sessionShortTTL
	if remember {
		ttl = sessionRememberTTL
	}
	now := time.Now()
	exp := now.Add(ttl)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked(now)
	a.sessions[hashToken(tok)] = sessionRec{User: user, Expires: exp, Remember: remember, Touched: now}
	return tok, exp, a.saveLocked()
}

func (a *AuthStore) pruneLocked(now time.Time) {
	for k, s := range a.sessions {
		if now.After(s.Expires) {
			delete(a.sessions, k)
		}
	}
}

// Lookup validates a session token and slides its expiry.
func (a *AuthStore) Lookup(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	k := hashToken(tok)
	s, ok := a.sessions[k]
	if !ok {
		return "", false
	}
	now := time.Now()
	if now.After(s.Expires) {
		delete(a.sessions, k)
		_ = a.saveLocked()
		return "", false
	}
	// the user may have been replaced
	found := false
	for _, u := range a.users {
		if u.Username == s.User {
			found = true
		}
	}
	if !found {
		return "", false
	}
	if now.Sub(s.Touched) > time.Hour {
		ttl := sessionShortTTL
		if s.Remember {
			ttl = sessionRememberTTL
		}
		s.Expires, s.Touched = now.Add(ttl), now
		a.sessions[k] = s
		_ = a.saveLocked()
	}
	return s.User, true
}

func (a *AuthStore) Destroy(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, hashToken(tok))
	_ = a.saveLocked()
}

// ChangePassword verifies the current password, sets the new one and signs
// out every session except keepTok.
func (a *AuthStore) ChangePassword(current, next, keepTok string) error {
	if err := validatePassword(next); err != nil {
		return err
	}
	a.mu.Lock()
	if len(a.users) == 0 {
		a.mu.Unlock()
		return errors.New("no account")
	}
	if bcrypt.CompareHashAndPassword([]byte(a.users[0].Hash), []byte(current)) != nil {
		a.mu.Unlock()
		return errors.New("current password is incorrect")
	}
	a.mu.Unlock()
	h, err := bcrypt.GenerateFromPassword([]byte(next), 12)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.users[0].Hash = string(h)
	keep := hashToken(keepTok)
	for k := range a.sessions {
		if k != keep {
			delete(a.sessions, k)
		}
	}
	return a.saveLocked()
}

// ── login throttling ───────────────────────────────────────────

type limiter struct {
	mu sync.Mutex
	m  map[string]*limEntry
}
type limEntry struct {
	fails int
	until time.Time
	last  time.Time
}

func newLimiter() *limiter { return &limiter{m: map[string]*limEntry{}} }

func (l *limiter) blocked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[key]
	if e == nil {
		return 0, false
	}
	if d := time.Until(e.until); d > 0 {
		return d, true
	}
	return 0, false
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, e := range l.m { // opportunistic cleanup
		if now.Sub(e.last) > time.Hour {
			delete(l.m, k)
		}
	}
	e := l.m[key]
	if e == nil {
		e = &limEntry{}
		l.m[key] = e
	}
	e.fails++
	e.last = now
	if e.fails >= 5 {
		d := time.Minute << uint(minInt(e.fails-5, 5)) // 1,2,4,8,16,32 min
		e.until = now.Add(d)
	}
}

func (l *limiter) ok(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func randomCode() string {
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	out := make([]byte, 0, 9)
	for i, x := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, alpha[int(x)%len(alpha)])
	}
	return string(out)
}
