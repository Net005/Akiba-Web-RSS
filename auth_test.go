package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	cs, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := NewHub(100)
	log.out = &discard{}
	return NewApp(cs, log), dir
}

type tc struct {
	t   *testing.T
	c   *http.Client
	url string
}

func newClient(t *testing.T, app *App) (*tc, func()) {
	srv := httptest.NewServer(NewServer(app).Handler())
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &tc{t, c, srv.URL}, srv.Close
}

func (x *tc) do(method, path string, body any) (*http.Response, map[string]any) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, x.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := x.c.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	m := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp, m
}

func TestFeedIsPublicAndPanelRequiresLogin(t *testing.T) {
	app, dir := newTestApp(t)
	os.WriteFile(filepath.Join(dir, "feed.rss"), []byte("<rss/>"), 0o644)
	if err := app.auth.SetUser("rick", "correct horse"); err != nil {
		t.Fatal(err)
	}
	x, done := newClient(t, app)
	defer done()

	for _, p := range []string{"/giga/feed", "/giga/feed/", "/giga/feed/raw", "/health"} {
		if r, _ := x.do("GET", p, nil); r.StatusCode != 200 {
			t.Fatalf("%s should be public, got %d", p, r.StatusCode)
		}
	}
	if r, _ := x.do("GET", "/api/status", nil); r.StatusCode != 401 {
		t.Fatalf("api without session: %d", r.StatusCode)
	}
	if r, _ := x.do("GET", "/giga/feed/refresh", nil); r.StatusCode != 401 {
		t.Fatalf("legacy refresh must need a session: %d", r.StatusCode)
	}
	r, _ := x.do("GET", "/", nil)
	if r.StatusCode != 302 || !strings.HasPrefix(r.Header.Get("Location"), "/login") {
		t.Fatalf("panel should redirect to /login: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := x.do("GET", "/login", nil); r.StatusCode != 200 {
		t.Fatal("login page must be reachable")
	}
	if r.Header.Get("WWW-Authenticate") != "" {
		t.Fatal("no browser (basic) auth prompt allowed")
	}
}

func TestLoginLogoutAndThrottle(t *testing.T) {
	app, _ := newTestApp(t)
	app.auth.SetUser("rick", "correct horse")
	x, done := newClient(t, app)
	defer done()

	if r, _ := x.do("POST", "/api/auth/login", map[string]any{"username": "rick", "password": "nope"}); r.StatusCode != 401 {
		t.Fatal("wrong password must fail")
	}
	r, _ := x.do("POST", "/api/auth/login", map[string]any{"username": "Rick", "password": "correct horse", "remember": true})
	if r.StatusCode != 200 {
		t.Fatalf("login failed: %d", r.StatusCode)
	}
	ck := r.Header.Get("Set-Cookie")
	if !strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Lax") {
		t.Fatalf("cookie flags: %s", ck)
	}
	if r, _ := x.do("GET", "/api/status", nil); r.StatusCode != 200 {
		t.Fatalf("session should work: %d", r.StatusCode)
	}
	if r, _ := x.do("GET", "/", nil); r.StatusCode != 200 {
		t.Fatalf("panel with session: %d", r.StatusCode)
	}
	x.do("POST", "/api/auth/logout", map[string]any{})
	if r, _ := x.do("GET", "/api/status", nil); r.StatusCode != 401 {
		t.Fatal("logout must end the session")
	}
	// brute force → 429 after 5 failures
	var last int
	for i := 0; i < 7; i++ {
		r, _ := x.do("POST", "/api/auth/login", map[string]any{"username": "rick", "password": "bad"})
		last = r.StatusCode
	}
	if last != 429 {
		t.Fatalf("expected throttling, got %d", last)
	}
	if r, _ := x.do("POST", "/api/auth/login", map[string]any{"username": "rick", "password": "correct horse"}); r.StatusCode != 429 {
		t.Fatal("even the right password is refused while locked")
	}
}

func TestFirstRunSetupNeedsCode(t *testing.T) {
	app, dir := newTestApp(t)
	if app.setupCode == "" {
		t.Fatal("setup code expected when no account exists")
	}
	x, done := newClient(t, app)
	defer done()
	if r, _ := x.do("GET", "/api/status", nil); r.StatusCode != 401 {
		t.Fatal("api locked before setup")
	}
	if r, _ := x.do("GET", "/", nil); r.Header.Get("Location") != "/setup" {
		t.Fatalf("expected redirect to /setup, got %q", r.Header.Get("Location"))
	}
	if r, _ := x.do("POST", "/api/auth/setup", map[string]any{"code": "WRONG-CODE", "username": "rick", "password": "correct horse"}); r.StatusCode != 403 {
		t.Fatal("wrong code must be refused")
	}
	if r, _ := x.do("POST", "/api/auth/setup", map[string]any{"code": strings.ToLower(app.setupCode), "username": "rick", "password": "short"}); r.StatusCode != 400 {
		t.Fatal("short password must be refused")
	}
	if r, _ := x.do("POST", "/api/auth/setup", map[string]any{"code": app.setupCode, "username": "rick", "password": "correct horse"}); r.StatusCode != 200 {
		t.Fatal("setup should succeed")
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal("settings.json must be created by setup")
	}
	if r, _ := x.do("GET", "/api/status", nil); r.StatusCode != 200 {
		t.Fatal("setup signs you in")
	}
	if r, _ := x.do("POST", "/api/auth/setup", map[string]any{"code": "X", "username": "evil", "password": "another pass"}); r.StatusCode != 409 {
		t.Fatal("setup must be one-shot")
	}
	select {
	case <-app.ready:
	default:
		t.Fatal("pipeline gate should be released after setup")
	}
}

func TestChangePasswordKeepsCurrentSession(t *testing.T) {
	app, _ := newTestApp(t)
	app.auth.SetUser("rick", "correct horse")
	a, d1 := newClient(t, app)
	defer d1()
	b, d2 := newClient(t, app)
	defer d2()
	a.do("POST", "/api/auth/login", map[string]any{"username": "rick", "password": "correct horse"})
	b.do("POST", "/api/auth/login", map[string]any{"username": "rick", "password": "correct horse"})
	if r, _ := a.do("POST", "/api/auth/password", map[string]any{"current": "wrong", "new": "brand new pass"}); r.StatusCode != 400 {
		t.Fatal("wrong current password")
	}
	if r, _ := a.do("POST", "/api/auth/password", map[string]any{"current": "correct horse", "new": "brand new pass"}); r.StatusCode != 200 {
		t.Fatal("change failed")
	}
	if r, _ := a.do("GET", "/api/status", nil); r.StatusCode != 200 {
		t.Fatal("current session should survive")
	}
	if r, _ := b.do("GET", "/api/status", nil); r.StatusCode != 401 {
		t.Fatal("other sessions must be signed out")
	}
}

func TestImportLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"myjd_email":"a@b.c","myjd_password":"pw","port":5000,"web_user":"admin","web_password":"hunter2hunter2"}`), 0o600)
	log := NewHub(50)
	log.out = &discard{}
	ok, err := ImportLegacyConfig(dir, log)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatal("config.json must be renamed")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json.imported")); err != nil {
		t.Fatal("config.json.imported missing")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if strings.Contains(string(b), "web_password") || !strings.Contains(string(b), "myjd_email") {
		t.Fatalf("settings.json wrong: %s", b)
	}
	auth := NewAuthStore(filepath.Join(dir, "auth.json"))
	auth.Load()
	if _, ok := auth.Verify("admin", "hunter2hunter2"); !ok {
		t.Fatal("web login should have been imported (hashed)")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("password must not be stored in clear text")
	}
	// second run is a no-op
	if ok, _ := ImportLegacyConfig(dir, log); ok {
		t.Fatal("must import only once")
	}
}

func TestImportInvalidLegacyConfigIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{oops`), 0o600)
	log := NewHub(50)
	log.out = &discard{}
	ok, err := ImportLegacyConfig(dir, log)
	if ok || err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("invalid config.json must not be touched")
	}
}

func TestStateMissingOrInvalidIsRecreated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".scraper_state.json")
	s := NewStateStore(p)
	if err := s.Load(); err != nil {
		t.Fatalf("missing file must not fail: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("state file should be created")
	}
	os.WriteFile(p, []byte(`{"found_links": {broken`), 0o600)
	if err := s.Load(); err != nil {
		t.Fatalf("invalid file must not fail: %v", err)
	}
	if s.Recovered == "" || len(s.S.FoundLinks) != 0 {
		t.Fatal("expected a fresh state + recovery note")
	}
	matches, _ := filepath.Glob(p + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatal("corrupt file should be kept aside")
	}
	b, _ := os.ReadFile(p)
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		t.Fatal("new state file must be valid JSON")
	}
	os.WriteFile(p, nil, 0o600) // empty file
	if err := s.Load(); err != nil || s.Recovered == "" {
		t.Fatal("empty file should be recreated too")
	}
}
